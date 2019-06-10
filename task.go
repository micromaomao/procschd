package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"
	"io"
	"log"
	"runtime"
	"strconv"
	"sync"
	"time"
)

type Task struct {
	lock           *sync.RWMutex
	id             uint64
	serverInstance *Server
	started        bool
	completed      bool
	cancelFunc     context.CancelFunc
	cancelChan     <-chan struct{}

	containerId string
	imageId     string
	stdin       io.Reader
	startTime   time.Time

	err    interface{}
	stdout []byte
	stderr []byte
}

// This function return *Task with its lock locked.
func (s *Server) newTask() *Task {
	newId := s.getNewTaskId()
	t := &Task{
		lock:           &sync.RWMutex{},
		id:             newId,
		serverInstance: s,
		started:        false,
		completed:      false,
		cancelFunc:     nil,

		stdout: []byte{},
		stderr: []byte{},
	}
	t.lock.Lock()
	return t
}

func (t *Task) do(ctx context.Context) {
	t.lock.Lock()
	if t.completed {
		t.lock.Unlock()
		return
	}
	if t.imageId == "" {
		t.completed = true
		t.lock.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second*20)
	t.startTime = time.Now()
	t.started = true
	t.cancelFunc = cancel
	t.cancelChan = ctx.Done()
	t.lock.Unlock()

	defer func() {
		p := recover()
		if p != nil {
			buf := make([]byte, 64<<10)
			log.Printf("Panic! %v: %s", p, buf[:runtime.Stack(buf, false)])
			go func() {
				t.cancel()
				t.lock.Lock()
				t.err = p
				t.lock.Unlock()
				cancel()
			}()
			return
		} else {
			cancel()
		}
	}()

	err := t.createContainer(ctx)
	if err != nil {
		t.lock.Lock()
		t.err = errors.New("Error during container creation: " + err.Error())
		t.completed = true
		t.lock.Unlock()
		return
	}
	conn, err := t.attachContainer(ctx)
	if err != nil {
		t.lock.Lock()
		t.err = errors.New("Error attaching to container: " + err.Error())
		t.completed = true
		t.lock.Unlock()
		return
	}
	t.lock.RLock()
	if t.stdin != nil {
		go func() {
			io.Copy(conn.Conn, t.stdin)
			conn.CloseWrite()
		}()
	}
	containerId := t.containerId
	docker := t.serverInstance.dockercli
	t.lock.RUnlock()

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
		docker.ContainerRemove(ctx, containerId, types.ContainerRemoveOptions{
			RemoveVolumes: true,
			RemoveLinks:   true,
			Force:         true,
		})
		cancel()
	}()

	containerWaitChan, errChan := docker.ContainerWait(ctx, containerId, container.WaitConditionNextExit)
	err = docker.ContainerStart(ctx, containerId, types.ContainerStartOptions{})
	if err != nil {
		t.lock.Lock()
		t.err = errors.New("Error starting container: " + err.Error())
		t.completed = true
		t.lock.Unlock()
		go func() {
			conn.Close()
			<-containerWaitChan
		}()
		return
	}
	stdioWg := sync.WaitGroup{}
	stdioWg.Add(1)
	go func() {
		stdoutBuf := bytes.NewBuffer(make([]byte, 0, 2<<20))
		stderrBuf := bytes.NewBuffer(make([]byte, 0, 2<<20))
		n, err := stdcopy.StdCopy(stdoutBuf, stderrBuf, conn.Reader)
		if err == nil {
			log.Printf("Got %v bytes from stdio.", n)
		} else {
			log.Printf("Error from StdCopy: %v", err)
		}
		t.lock.Lock()
		t.stdout = stdoutBuf.Bytes()
		t.stderr = stderrBuf.Bytes()
		t.err = err
		t.lock.Unlock()
		stdioWg.Done()
	}()
	log.Printf("Waiting")
	select {
	case err := <-errChan:
		if err != nil {
			t.lock.Lock()
			t.err = errors.New("Error returned by ContainerWait: " + err.Error())
			t.completed = true
			t.lock.Unlock()
			return
		}
	case wBody := <-containerWaitChan:
		err := wBody.Error
		if err != nil {
			t.lock.Lock()
			t.err = errors.New("Error returned by ContainerWait: " + err.Message)
			t.completed = true
			t.lock.Unlock()
			return
		} else {
			stdioWg.Wait()
			if wBody.StatusCode != 0 {
				t.lock.Lock()
				t.err = errors.New("container exited with status " + strconv.FormatInt(wBody.StatusCode, 10))
				t.completed = true
				t.lock.Unlock()
			} else {
				t.lock.Lock()
				// TODO: final work
				// Here: t.err may had been set by the goroutine doing StdCopy, otherwise it is nil.
				t.completed = true
				t.lock.Unlock()
			}
		}
	}
}

func (t *Task) cancel() {
	t.lock.Lock()
	if !t.started {
		t.completed = true
		// At this point, t.do() will do nothing.
		t.lock.Unlock()
		return
	} else {
		t.lock.Unlock()
		t.cancelFunc()
		t.lock.Lock()
		t.completed = true
		t.lock.Unlock()
		return
	}
}

func (t *Task) wait() {
	t.lock.RLock()
	if t.completed {
		t.lock.RUnlock()
		return
	}
	cancelChan := t.cancelChan
	t.lock.RUnlock()
	if cancelChan == nil {
		return
	} else {
		for {
			<-cancelChan
			t.lock.RLock()
			completed := t.completed
			t.lock.RUnlock()
			if completed {
				return
			} else {
				runtime.Gosched()
			}
		}
	}
}

func (t *Task) createContainer(ctx context.Context) (err error) {
	t.lock.RLock()
	docker := t.serverInstance.dockercli
	labels := make(map[string]string)
	labels["procschd-task-id"] = strconv.FormatUint(t.id, 10)
	imageId := t.imageId
	t.lock.RUnlock()
	one := 1
	res, err := docker.ContainerCreate(ctx, &container.Config{
		User:            "nobody:nobody",
		AttachStdin:     true,
		AttachStdout:    true,
		AttachStderr:    true,
		StdinOnce:       true,
		OpenStdin:       true,
		Image:           imageId,
		NetworkDisabled: true,
		Labels:          labels,
		StopSignal:      "SIGKILL",
		StopTimeout:     &one,
	}, &container.HostConfig{
		AutoRemove: true,
	}, nil, "")
	if err != nil {
		return
	}
	t.lock.Lock()
	t.containerId = res.ID
	t.lock.Unlock()
	for _, s := range res.Warnings {
		log.Printf("Container creation warning: %s\n    when creating container with image %s.", s, imageId)
	}
	return
}

func (t *Task) attachContainer(ctx context.Context) (conn types.HijackedResponse, err error) {
	t.lock.RLock()
	docker := t.serverInstance.dockercli
	containerId := t.containerId
	t.lock.RUnlock()
	conn, err = docker.ContainerAttach(ctx, containerId, types.ContainerAttachOptions{
		Stdin:  true,
		Stdout: true,
		Stderr: true,
		Stream: true,
	})
	return
}
