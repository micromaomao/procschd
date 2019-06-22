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
	"os"
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
	startWaitChan  chan struct{}

	containerId  string
	imageId      string
	artifacts    []string
	stdin        io.ReadCloser
	uploads      []Upload
	uploads_lock *sync.RWMutex
	startTime    time.Time

	err          interface{}
	stdout       []byte
	stderr       []byte
	artifactsMap map[string]ArtifactStream
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

		startWaitChan: make(chan struct{}, 1), // so that those who wait()ed before task was picked up by a runner thread can wait for the task to complete.

		uploads:      make([]Upload, 0),
		uploads_lock: &sync.RWMutex{},

		stdout: []byte{},
		stderr: []byte{},
	}
	t.lock.Lock()
	return t
}

func (t *Task) do(ctx context.Context) {
	t.lock.Lock()
	close(t.startWaitChan)
	t.startWaitChan = nil
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
			t.lock.RLock()
			if t.err != nil {
				log.Printf("Task %v failed: %v", t.id, t.err)
			}
			t.lock.RUnlock()
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

	err = t.copyUploads(ctx)
	if err != nil {
		t.lock.Lock()
		t.err = errors.New("Unable to copy uploads into container: " + err.Error())
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
		stdin := t.stdin
		go func() {
			io.Copy(conn.Conn, stdin)
			conn.CloseWrite()
			stdin.Close()
		}()
	}
	containerId := t.containerId
	docker := t.serverInstance.dockercli
	t.lock.RUnlock()

	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*2)
		err := docker.ContainerRemove(ctx, containerId, types.ContainerRemoveOptions{
			RemoveVolumes: true,
			Force:         true,
		})
		if err != nil {
			log.Printf("Unable to remove container %v: %v", containerId, err.Error())
		}
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
		_, err := stdcopy.StdCopy(stdoutBuf, stderrBuf, conn.Reader)
		if err != nil {
			log.Printf("Error from StdCopy: %v", err)
		}
		t.lock.Lock()
		t.stdout = stdoutBuf.Bytes()
		t.stderr = stderrBuf.Bytes()
		t.lock.Unlock()
		stdioWg.Done()
	}()
	select {
	case err := <-errChan:
		if err != nil {
			snd := time.Second
			stopErr := docker.ContainerStop(context.Background(), containerId, &snd)
			if stopErr == nil {
				stdioWg.Wait() // wait for stdout/stderr to be populated.
			} else {
				log.Printf("Error stopping container %v: %v", containerId, stopErr.Error())
			}
			t.lock.Lock()
			t.err = errors.New("Error returned by ContainerWait: " + err.Error())
			t.completed = true
			t.lock.Unlock()
			return
		}
	case wBody := <-containerWaitChan:
		err := wBody.Error
		if err != nil {
			snd := time.Second
			stopErr := docker.ContainerStop(context.Background(), containerId, &snd)
			if stopErr == nil {
				stdioWg.Wait() // wait for stdout/stderr to be populated.
			} else {
				log.Printf("Error stopping container %v: %v", containerId, stopErr.Error())
			}
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
				// Here: t.err may had been set by the goroutine doing StdCopy, otherwise it is nil.
				am, err := t.getArtifacts(ctx)
				t.lock.Lock()
				t.completed = true
				if err != nil {
					t.err = errors.New("Failed to get artifacts: " + err.Error())
				} else {
					t.artifactsMap = am
				}
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

func (t *Task) cleanup() {
	t.cancel()
	t.lock.Lock()
	defer t.lock.Unlock()
	if t.artifactsMap != nil {
		for _, as := range t.artifactsMap {
			os.Remove(as.file.Name())
			as.file.Close()
		}
		t.artifactsMap = nil
	}
	if t.stdin != nil {
		t.stdin.Close()
	}
	for _, f := range t.uploads {
		if f.data != nil {
			f.data.Close()
		}
	}
	t.uploads = nil
}

func (t *Task) wait() {
	t.lock.RLock()
	if t.completed {
		t.lock.RUnlock()
		return
	}
	startWaitChan := t.startWaitChan
	cancelChan := t.cancelChan
	t.lock.RUnlock()
	if cancelChan == nil {
		if startWaitChan == nil {
			return
		} else {
			<-startWaitChan
			t.wait()
			return
		}
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
	ten := int64(10)
	res, err := docker.ContainerCreate(ctx, &container.Config{
		User:            "65534:65534",
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
		Resources: container.Resources{
			Memory:     1 << 30, // 1 GiB
			MemorySwap: 1 << 30,
			NanoCPUs:   1000000000, // 10^9 nCPUs = 1 CPU. This is not some sort of time limit, but limit on how many CPUs to allocate to the container.
			PidsLimit:  &ten,
		},
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
