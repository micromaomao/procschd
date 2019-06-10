package main

import (
	"context"
	"errors"
	dockerClient "github.com/docker/docker/client"
	"github.com/docker/go-connections/sockets"
	"github.com/spf13/pflag"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	log.SetOutput(os.Stdout)
	var dockerAddr, listenAddr, dockerVersion string
	pflag.StringVar(&dockerAddr, "docker", "unix:/var/run/docker.sock", "Either: unix:/path/to/docker/socket\n    Or: tcp:host:port")
	pflag.StringVar(&listenAddr, "listen", "unix:/run/procschd.sock", "Either: unix:/path/to/bind/point\n    Or: tcp:addr:port")
	pflag.StringVar(&dockerVersion, "docker-version", "", "Specify API version used by docker. Leave empty for latest.")
	pflag.Parse()
	dockercli, err := initDockerClient(dockerAddr, dockerVersion)
	if err != nil {
		log.Fatalf("When creating docker client: " + err.Error())
	}
	err = serve(listenAddr, dockercli)
	if err != nil {
		log.Fatalf(err.Error())
	}
}

func initDockerClient(dockerAddr, dockerVersion string) (client *dockerClient.Client, err error) {
	proto, host, err := protoHostFromAddr(dockerAddr)
	if err != nil {
		return
	}
	httpClient := &http.Client{}
	httpTransport := &http.Transport{}
	sockets.ConfigureTransport(httpTransport, proto, host)
	httpTransport.DisableKeepAlives = false
	httpTransport.MaxIdleConns = 50
	httpTransport.IdleConnTimeout = 0
	httpTransport.ResponseHeaderTimeout = 10 * time.Second
	httpTransport.TLSClientConfig = nil
	httpClient.Transport = httpTransport
	host = proto + "://" + host
	client, err = dockerClient.NewClient(host, dockerVersion, httpClient, make(map[string]string))
	ctx, ctxCancel := context.WithTimeout(context.Background(), time.Second*5)
	info, err := client.Info(ctx)
	ctxCancel()
	ctx = nil
	if err != nil {
		client = nil
		return
	}
	log.Printf("Docker %v running on %v", info.ServerVersion, info.Name)
	return
}

func serve(listenAddr string, d *dockerClient.Client) (err error) {
	proto, host, err := protoHostFromAddr(listenAddr)
	if err != nil {
		return
	}
	if proto == "unix" {
		fi, err := os.Stat(host)
		if err == nil {
			if fi.Mode()&os.ModeSocket > 0 {
				err = os.Remove(host)
				if err != nil {
					return err
				}
			} else {
				return errors.New(host + " exists, but is not a socket. Not overwriting.")
			}
		}
	}
	httpSrv := http.Server{}
	srv := NewServer(d)
	httpSrv.Handler = srv
	l, err := net.Listen(proto, host)
	if err != nil {
		return
	}
	err = httpSrv.Serve(l)
	return
}

func protoHostFromAddr(addr string) (proto, host string, err error) {
	sliced := strings.SplitN(addr, ":", 2)
	switch sliced[0] {
	case "unix":
		return "unix", sliced[1], nil
	case "tcp":
		return "tcp", sliced[1], nil
	default:
		err = errors.New("docker address must either start with unix: or tcp:")
		return
	}
}
