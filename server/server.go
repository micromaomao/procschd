package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Workiva/go-datastructures/queue"
	dockerClient "github.com/docker/docker/client"
)

type Server struct {
	taskQueue         *queue.Queue
	taskIdIncrement   uint64
	taskStore         sync.Map
	numThreads        uint32
	numThreadsDesired uint32
	threadAlterLock   sync.Mutex
	dockercli         *dockerClient.Client
	authToken         string
}

func NewServer(d *dockerClient.Client, authToken string) *Server {
	serv := &Server{}
	serv.taskIdIncrement = 0
	serv.taskQueue = queue.New(10)
	serv.taskStore = sync.Map{}
	serv.numThreads = 0
	serv.numThreadsDesired = 3
	serv.threadAlterLock = sync.Mutex{}
	serv.dockercli = d
	serv.authToken = authToken
	serv.EnsureRunnerThread()
	go func() {
		for {
			time.Sleep(time.Second * 5)
			if !serv.taskQueue.Disposed() {
				serv.EnsureRunnerThread()
			} else {
				return
			}
		}
	}()
	return serv
}

func (s *Server) CleanUp() {
	s.taskQueue.Dispose()
	s.taskStore.Range(func(k, v interface{}) bool {
		t := v.(*Task)
		t.cleanup()
		return true
	})
}

func (s *Server) getNewTaskId() uint64 {
	return atomic.AddUint64(&s.taskIdIncrement, 1)
}

func (s *Server) RunnerThread() {
	i := atomic.AddUint32(&s.numThreads, 1)
	defer atomic.AddUint32(&s.numThreads, ^uint32(0))
	log.Printf("Starting runner thread #%d...", i)
	for {
		taskArr, err := s.taskQueue.Poll(1, -1)
		if err == queue.ErrDisposed {
			break
		} else if err != nil {
			panic(err)
		}
		if len(taskArr) != 1 {
			panic(fmt.Sprintf("queue.Poll returned %d tasks, expected 1.", len(taskArr)))
		}
		task := taskArr[0].(*Task)
		log.Printf("Runner thread #%d doing task %d...", i, task.id)
		task.do(context.Background())
		log.Printf("Runner thread #%d done task %d...", i, task.id)
	}
}

func (s *Server) EnsureRunnerThread() {
	s.threadAlterLock.Lock()
	currentNumThreadApprox := atomic.LoadUint32(&s.numThreads)
	for currentNumThreadApprox < s.numThreadsDesired {
		go s.RunnerThread()
		currentNumThreadApprox++
	}
	s.threadAlterLock.Unlock()
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	log.Printf("%v %v", req.Method, req.URL.RequestURI())
	if s.authToken != "" {
		authHeader := req.Header["Authorization"]
		if len(authHeader) != 1 {
			rw.WriteHeader(401)
			return
		}
		authHeaderStrs := strings.Split(authHeader[0], " ")
		if len(authHeaderStrs) != 2 || authHeaderStrs[0] != "Bearer" {
			rw.WriteHeader(401)
			return
		}
		clientToken := authHeaderStrs[1]
		if subtle.ConstantTimeCompare([]byte(clientToken), []byte(s.authToken)) != 1 {
			rw.WriteHeader(401)
			return
		}
	}
	defer func() {
		p := recover()
		if p != nil {
			rw.WriteHeader(500)
			rw.Header()["Content-Type"] = []string{"text/plain"}
			rw.Write([]byte(fmt.Sprintf("Server panic! %v", p)))
			buf := make([]byte, 64<<10)
			buf = buf[:runtime.Stack(buf, false)]
			log.Printf("On %v: Panic: %v\n%s", req.URL.RequestURI(), p, buf)
		}
	}()
	path := req.URL.Path
	switch req.Method {
	case "HEAD":
		fallthrough
	case "GET":
		if path == "/stat" {
			jsonData, err := json.Marshal(StatResponse{
				NumThreadsDesired: s.numThreadsDesired,
				NumThreads:        atomic.LoadUint32(&s.numThreads),
				NumTasksPending:   s.taskQueue.Len(),
			})
			if err != nil {
				responseWithError(err, req, rw)
				return
			}
			rw.Header()["Content-Type"] = []string{"text/json"}
			rw.WriteHeader(200)
			rw.Write(jsonData)
			return
		}
		if path == "/image" {
			query := req.URL.Query()
			imageId := query.Get("id")
			ispect, _, err := s.dockercli.ImageInspectWithRaw(context.Background(), imageId)
			if err != nil {
				if dockerClient.IsErrNotFound(err) {
					rw.WriteHeader(404)
					rw.Write([]byte("Image not found."))
					return
				}
				responseWithError(err, req, rw)
				return
			}
			rw.Header()["Content-Type"] = []string{"text/plain"}
			rw.WriteHeader(200)
			rw.Write([]byte(ispect.Created))
			return
		}
		if path == "/task" {
			query := req.URL.Query()
			id, err := strconv.ParseUint(query.Get("id"), 10, 64)
			if err != nil {
				rw.WriteHeader(400)
				rw.Write([]byte(err.Error()))
				return
			}
			qWaitArr := query["wait"]
			doWait := false
			if len(qWaitArr) == 1 {
				if qWaitArr[0] != "" {
					doWait = true
				}
			} else if len(qWaitArr) > 1 {
				rw.WriteHeader(400)
				rw.Write([]byte("Expected only one ?wait."))
				return
			}
			_t, ok := s.taskStore.Load(id)
			if !ok {
				rw.WriteHeader(404)
				return
			}
			t := _t.(*Task)
			if doWait {
				t.wait()
			}
			t.lock.RLock()
			defer t.lock.RUnlock()
			rw.Header()["Content-Type"] = []string{"text/json"}
			rw.WriteHeader(200)
			tErr := ""
			if t.err != nil {
				if err, ok := t.err.(error); ok {
					tErr = err.Error()
				} else if err, ok := t.err.(string); ok {
					tErr = err
				} else {
					tErr = "Unknow error"
				}
			}
			jsonBytes, err := json.Marshal(TaskResponse{
				Id:        t.id,
				Completed: t.completed,
				Error:     tErr,
				Stdout:    string(t.stdout),
				Stderr:    string(t.stderr),
				Artifacts: t.artifacts,
			})
			if err != nil {
				responseWithError(err, req, rw)
				return
			}
			rw.Write(jsonBytes)
			return
		}
		if path == "/task/artifact" {
			query := req.URL.Query()
			id, err := strconv.ParseUint(query.Get("id"), 10, 64)
			if err != nil {
				rw.WriteHeader(400)
				rw.Write([]byte(err.Error()))
				return
			}
			_t, ok := s.taskStore.Load(id)
			if !ok {
				rw.WriteHeader(404)
				rw.Write([]byte("No such task."))
				return
			}
			t := _t.(*Task)
			artifactPath := query.Get("path")
			t.lock.RLock()
			defer t.lock.RUnlock()
			if !t.completed {
				rw.WriteHeader(404)
				rw.Write([]byte("Task not completed."))
				return
			}
			artifactStream, ok := t.artifactsMap[artifactPath]
			if !ok {
				if t.err != nil {
					rw.WriteHeader(404)
					rw.Write([]byte("Task failed, so artifacts not available."))
					return
				}
				rw.WriteHeader(404)
				rw.Write([]byte("No such artifact."))
				return
			}
			stream, err := artifactStream.getStream()
			if err != nil {
				rw.WriteHeader(500)
				rw.Write([]byte(err.Error()))
				return
			}
			rw.Header()["Content-Type"] = []string{} // Do not send Content-Type because we don't know the type.
			rw.Header()["Content-Length"] = []string{strconv.FormatInt(artifactStream.length, 10)}
			rw.WriteHeader(200)
			io.Copy(rw, stream)
			stream.Close()
			return
		}
		rw.WriteHeader(404)
		return
	case "POST":
		if path == "/task" {
			var imageId string
			var stdin io.ReadCloser
			var artifacts []string = make([]string, 0)
			contentTypeArr := req.Header["Content-Type"]
			if len(contentTypeArr) != 1 {
				rw.WriteHeader(400)
				rw.Write([]byte("Expected 1 Content-Type header."))
				return
			}
			contentType := contentTypeArr[0]
			contentType = strings.SplitN(contentType, ";", 2)[0]
			var uploadsArr []*multipart.FileHeader = nil
			switch contentType {
			case "application/x-www-form-urlencoded":
				err := req.ParseForm()
				if err != nil {
					rw.WriteHeader(400)
					rw.Write([]byte(err.Error()))
					return
				}

				imageIdArr := req.Form["imageId"]
				if len(imageIdArr) != 1 {
					rw.WriteHeader(400)
					rw.Write([]byte("Expected imageId to be a string."))
					return
				}
				imageId = imageIdArr[0]

				stdinStrArr := req.Form["stdin"]
				if len(stdinStrArr) > 1 {
					rw.WriteHeader(400)
					rw.Write([]byte("Expected stdin to be a string."))
					return
				} else if len(stdinStrArr) == 1 {
					stdin = ioutil.NopCloser(bytes.NewReader([]byte(stdinStrArr[0])))
				}

				artifacts = req.Form["artifacts"]
			case "multipart/form-data":
				err := req.ParseMultipartForm(1 << 24) // 16 MB
				defer req.MultipartForm.RemoveAll()    // Deleting files on disk don't close fd on linux, so saved streams can still be read later.
				if err != nil {
					rw.WriteHeader(400)
					rw.Write([]byte(err.Error()))
					return
				}

				imageIdArr := req.MultipartForm.Value["imageId"]
				if len(imageIdArr) != 1 {
					rw.WriteHeader(400)
					rw.Write([]byte("Expected imageId to be a string."))
					return
				}
				imageId = imageIdArr[0]

				stdinArr := req.MultipartForm.File["stdin"]
				if len(stdinArr) == 1 {
					stdin, err = stdinArr[0].Open()
					if err != nil {
						rw.WriteHeader(400)
						rw.Write([]byte(err.Error()))
						return
					}
					if len(req.MultipartForm.Value["stdin"]) != 0 {
						rw.WriteHeader(400)
						rw.Write([]byte("Stdin provided as file, but also provided as string. Which should I use?"))
						return
					}
				} else if len(stdinArr) == 0 {
					stdinStrArr := req.MultipartForm.Value["stdin"]
					if len(stdinStrArr) == 1 {
						stdin = ioutil.NopCloser(bytes.NewReader([]byte(stdinStrArr[0])))
					} else if len(stdinArr) != 0 {
						rw.WriteHeader(400)
						rw.Write([]byte("Expected stdin to be a string."))
						return
					}
				} else {
					rw.WriteHeader(400)
					rw.Write([]byte("Expected stdin to be one file."))
					return
				}

				artifacts = req.MultipartForm.Value["artifacts"]
				uploadsArr = req.MultipartForm.File["uploads"]
			default:
				rw.WriteHeader(400)
				rw.Write([]byte("Invalid Content-Type. Expected either application/x-www-form-urlencoded or multipart/form-data."))
				return
			}
			if len(imageId) == 0 {
				rw.WriteHeader(400)
				rw.Write([]byte("Empty imageId?"))
				return
			}
			if artifacts == nil {
				artifacts = make([]string, 0)
			}
			for _, artifact := range artifacts {
				if len(artifact) == 0 || artifact[0] != '/' {
					rw.WriteHeader(400)
					rw.Write([]byte("Artifact paths must be absolute."))
					return
				}
			}
			t := s.newTask()
			t.imageId = imageId
			t.artifacts = artifacts
			t.stdin = stdin
			if stdin == nil {
				t.stdin = ioutil.NopCloser(bytes.NewReader([]byte{}))
			}
			if uploadsArr != nil {
				t.uploads_lock.Lock()
				for _, uploadFile := range uploadsArr {
					_, params, err := mime.ParseMediaType(uploadFile.Header.Get("Content-Disposition"))
					if err != nil {
						rw.WriteHeader(400)
						rw.Write([]byte("Failed to parse Content-Disposition"))
						t.uploads_lock.Unlock()
						t.lock.Unlock()
						t.cleanup()
						return
					}
					path := params["filename"]
					if len(path) == 0 || path[0] != '/' {
						rw.WriteHeader(400)
						rw.Write([]byte("Paths must be absolute."))
						t.uploads_lock.Unlock()
						t.lock.Unlock()
						t.cleanup()
						return
					}
					data, err := uploadFile.Open()
					if err != nil {
						rw.WriteHeader(500)
						rw.Write([]byte(err.Error()))
						t.uploads_lock.Unlock()
						t.lock.Unlock()
						t.cleanup()
						return
					}
					t.uploads = append(t.uploads, Upload{
						path:   path,
						data:   data,
						length: uploadFile.Size,
					})
				}
				t.uploads_lock.Unlock()
			}
			err := s.taskQueue.Put(t)
			if err != nil {
				rw.WriteHeader(500)
				rw.Write([]byte(err.Error()))
				t.lock.Unlock()
				return
			}
			log.Printf("Created task %v running %v", t.id, t.imageId)
			s.taskStore.Store(t.id, t)
			rw.Header()["Content-Type"] = []string{"text/plain"}
			rw.WriteHeader(200)
			rw.Write([]byte(strconv.FormatUint(t.id, 10)))
			t.lock.Unlock()
			return
		} else {
			rw.WriteHeader(404)
			return
		}
	default:
		rw.WriteHeader(404)
		return
	}
}

func responseWithError(err error, req *http.Request, rw http.ResponseWriter) {
	rw.WriteHeader(500)
	rw.Write([]byte(err.Error()))
	log.Printf("[500] %v on request %v", err.Error(), req.URL.RequestURI())
}
