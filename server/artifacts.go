package main

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/ioutil"
	"log"
	"os"
)

type ArtifactStream struct {
	file   *os.File
	length int64
}

func (as *ArtifactStream) getStream() (io.ReadCloser, error) {
	fname := as.file.Name()
	return os.Open(fname)
}

// Ensure container is stopped before calling this.
func (t *Task) getArtifacts(ctx context.Context) (artifactsMap map[string]ArtifactStream, err error) {
	t.lock.RLock()
	docker := t.serverInstance.dockercli
	containerId := t.containerId
	artifacts := make([]string, len(t.artifacts))
	copy(artifacts, t.artifacts)
	t.lock.RUnlock()
	artifactsMap = make(map[string]ArtifactStream)
	for _, artifactPath := range artifacts {
		reader, stat, err := docker.CopyFromContainer(ctx, containerId, artifactPath)
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		tmpFile, err := ioutil.TempFile("", "procschd-artifact-temp-")
		var length int64 = -1
		if err != nil {
			return nil, errors.New("Error creating temp file: " + err.Error())
		}
		if stat.Mode.IsDir() {
			length, err = io.Copy(tmpFile, reader)
			if err != nil {
				return nil, errors.New("Error on " + artifactPath + ": " + err.Error())
			}
		} else if stat.Mode.IsRegular() {
			tarRead := tar.NewReader(reader)
			hdr, err := tarRead.Next()
			if err != nil {
				return nil, errors.New("Error on " + artifactPath + ": " + err.Error())
			}
			log.Printf("tar: name = %v", hdr.Name)
			length = hdr.Size
			realLength, err := io.Copy(tmpFile, tarRead)
			if err != nil {
				return nil, errors.New("Error on " + artifactPath + ": " + err.Error())
			}
			if realLength != length {
				panic("realLength != length ???")
			}
		} else {
			tmpFile.Close()
			return nil, errors.New(artifactPath + " is not a regular file")
		}
		_, err = tmpFile.Seek(0, os.SEEK_SET)
		if err != nil {
			return nil, errors.New("Unable to seek on temp file: " + err.Error())
		}
		artifactsMap[artifactPath] = ArtifactStream{
			file:   tmpFile,
			length: length,
		}
	}
	return artifactsMap, nil
}
