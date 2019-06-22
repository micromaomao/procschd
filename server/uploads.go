package main

import (
	"archive/tar"
	"context"
	"github.com/docker/docker/api/types"
	"io"
	"strings"
	"time"
)

type Upload struct {
	path   string
	data   io.ReadCloser
	length int64
}

func (t *Task) copyUploads(ctx context.Context) error {
	t.lock.RLock()
	containerId := t.containerId
	uploads := t.uploads
	docker := t.serverInstance.dockercli
	t.uploads_lock.RLock()
	defer t.uploads_lock.RUnlock()
	t.lock.RUnlock()

	if len(uploads) == 0 {
		return nil
	}

	outPipe, inPipe := io.Pipe()

	go func() {
		doneChan := ctx.Done()
		select {
		case <-doneChan:
			return
		default:
		}
		tarWriter := tar.NewWriter(inPipe)
		for _, upload := range uploads {
			select {
			case <-doneChan:
				return
			default:
			}
			err := tarWriter.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg,
				Name:     strings.TrimLeft(upload.path, "/"),
				Size:     upload.length,
				Mode:     0777,
				Uid:      0,
				Gid:      0,
				Uname:    "",
				Gname:    "",
				ModTime:  time.Now(),
			})
			select {
			case <-doneChan:
				return
			default:
			}
			if err != nil {
				inPipe.CloseWithError(err)
				return
			}
			_, err = io.Copy(tarWriter, upload.data)
			select {
			case <-doneChan:
				return
			default:
			}
			if err != nil {
				inPipe.CloseWithError(err)
				return
			}
		}
		select {
		case <-doneChan:
			return
		default:
		}
		err := tarWriter.Close()
		if err != nil {
			inPipe.CloseWithError(err)
			return
		}
		inPipe.Close()
	}()
	err := docker.CopyToContainer(ctx, containerId, "/", outPipe, types.CopyToContainerOptions{
		AllowOverwriteDirWithFile: true,
		CopyUIDGID:                false,
	})
	return err
}
