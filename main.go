package main

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.org/x/crypto/ssh"
	"io"
	"io/ioutil"
	"log"
	"net"
	"sync"
)

func main() {
	listener, err := net.Listen("tcp", "0.0.0.0:2222")
	if err != nil {
		log.Fatalf("Failed to listen on 0.0.0.0:2222 (%s)", err)
	}
	log.Printf("Listening on 0.0.0.0:2222")

	sshConfig := &ssh.ServerConfig{}
	// region SSH authentication
	sshConfig.PasswordCallback = func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		if conn.User() == "foo" && string(password) == "bar" {
			return &ssh.Permissions{}, nil
		} else {
			return nil, fmt.Errorf("authentication failed")
		}
	}
	// endregion

	// region Host key
	hostKeyData, err := ioutil.ReadFile("ssh_host_rsa_key")
	if err != nil {
		log.Fatalf("failed to load host key (%s)", err)
	}
	signer, err := ssh.ParsePrivateKey(hostKeyData)
	if err != nil {
		log.Fatalf("failed to parse host key (%s)", err)
	}
	sshConfig.AddHostKey(signer)
	// endregion

	for {
		tcpConn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept incoming connection (%s)", err)
			// Continue with the next loop
			continue
		}

		sshConn, chans, reqs, err := ssh.NewServerConn(tcpConn, sshConfig)
		if err != nil {
			log.Printf("handshake failed (%s)", err)
			continue
		}

		go ssh.DiscardRequests(reqs)
		go handleChannels(sshConn, chans)
	}
}

func handleChannels(conn *ssh.ServerConn, chans <-chan ssh.NewChannel) {
	for newChannel := range chans {
		go handleChannel(conn, newChannel)
	}
}

type channelProperties struct {
	// Allocate pseudo-terminal for interactive sessions.
	pty bool
	// Store the container ID once it is started.
	containerId string
	// Environment variables passed from the SSH session.
	env map[string]string
	// Horizontal screen size
	cols uint
	// Vertical screen size
	rows uint
	// Context required by the Docker client.
	ctx context.Context
	// Docker client
	docker *client.Client
}

func handleChannel(conn *ssh.ServerConn, newChannel ssh.NewChannel) {
	if t := newChannel.ChannelType(); t != "session" {
		_ = newChannel.Reject(ssh.UnknownChannelType, fmt.Sprintf("unknown channel type: %s", t))
		return
	}

	docker, err := client.NewClient("tcp://127.0.0.1:2375", "", nil, make(map[string]string))
	if err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, fmt.Sprintf("error contacting backend (%s)", err))
		return
	}

	channelProps := &channelProperties{
		pty:         false,
		containerId: "",
		env:         map[string]string{},
		cols:        80,
		rows:        25,
		ctx:         context.Background(),
		docker:      docker,
	}

	channel, requests, err := newChannel.Accept()
	if err != nil {
		log.Printf("could not accept channel (%s)", err)
		err := docker.Close()
		if err != nil {
			log.Printf("eror while closing Docker connection (%s)", err)
		}
		return
	}

	//...
	removeContainer := func() {
		if channelProps.containerId != "" {
			//Remove container
			removeOptions := types.ContainerRemoveOptions{Force: true}
			err := docker.ContainerRemove(channelProps.ctx, channelProps.containerId, removeOptions)
			if err != nil {
				log.Printf("error while removing container (%s)", err)
			}
			channelProps.containerId = ""
		}
	}
	closeConnections := func() {
		removeContainer()
		//Close Docker connection
		err = docker.Close()
		if err != nil {
			log.Printf("error while closing Docker connection (%s)", err)
		}
		//Close SSH connection
		err := conn.Close()
		if err != nil {
			log.Printf("error while closing SSH channel (%s)", err)
		}
	}

	go func() {
		for req := range requests {
			reply := func(success bool, message []byte) {
				if req.WantReply {
					err := req.Reply(success, message)
					if err != nil {
						closeConnections()
					}
				}
			}
			handleRequest(
				channel,
				req,
				reply,
				closeConnections,
				removeContainer,
				channelProps,
			)
		}
	}()
}

type envRequestMsg struct {
	Name  string
	Value string
}

type windowChangeRequestMsg struct {
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
}

func handleRequest(
	channel ssh.Channel,
	req *ssh.Request,
	reply func(success bool, message []byte),
	closeConnections func(),
	removeContainer func(),
	channelProps *channelProperties,
) {
	switch req.Type {
	case "env":
		if channelProps.containerId != "" {
			reply(false, []byte(fmt.Sprintf("cannot set env variables after shell or program already started")))
			return
		}
		request := envRequestMsg{}
		err := ssh.Unmarshal(req.Payload, request)
		if err != nil {
			reply(false, []byte(fmt.Sprintf("invalid payload (%s)", err)))
		}
		channelProps.env[request.Name] = request.Value
	case "pty-req":
		if channelProps.containerId != "" {
			reply(false, []byte(fmt.Sprintf("cannot set pty after shell or program already started")))
			return
		}
		channelProps.pty = true
	case "window-change":
		request := windowChangeRequestMsg{}
		err := ssh.Unmarshal(req.Payload, request)
		if err != nil {
			reply(false, []byte(fmt.Sprintf("invalid payload (%s)", err)))
			return
		}
		channelProps.cols = uint(request.Columns)
		channelProps.rows = uint(request.Rows)
		if channelProps.containerId != "" {
			err = channelProps.docker.ContainerResize(
				channelProps.ctx,
				channelProps.containerId,
				types.ResizeOptions{
					Height: channelProps.rows,
					Width:  channelProps.cols,
				},
			)
			if err != nil {
				reply(false, []byte(fmt.Sprintf("failed to set window size (%s)", err)))
				return
			}
		}
	case "shell":
		if channelProps.containerId != "" {
			reply(false, []byte(fmt.Sprintf("cannot launch a second shell")))
			break
		}

		//Pull the container image
		pullReader, err := channelProps.docker.ImagePull(channelProps.ctx, "docker.io/library/busybox", types.ImagePullOptions{})
		if err != nil {
			reply(false, []byte(fmt.Sprintf("could not pull busybox image (%s)", err)))
			return
		}
		_, err = ioutil.ReadAll(pullReader)
		if err != nil {
			reply(false, []byte(fmt.Sprintf("could not pull busybox image (%s)", err)))
			return
		}
		err = pullReader.Close()
		if err != nil {
			reply(false, []byte(fmt.Sprintf("could not pull busybox image (%s)", err)))
			return
		}

		//Create the container
		var env []string
		for key, value := range channelProps.env {
			env = append(env, fmt.Sprintf("%s=%s", key, value))
		}
		body, err := channelProps.docker.ContainerCreate(
			channelProps.ctx,
			&container.Config{
				Image:        "busybox",
				AttachStdout: true,
				AttachStderr: true,
				AttachStdin:  true,
				Tty:          channelProps.pty,
				StdinOnce:    true,
				OpenStdin:    true,
				Env:          env,
			},
			&container.HostConfig{},
			&network.NetworkingConfig{},
			"",
		)
		if err != nil {
			reply(false, []byte(fmt.Sprintf("failed to create container (%s)", err)))
			return
		}
		channelProps.containerId = body.ID

		//Attach the container
		attachResult, err := channelProps.docker.ContainerAttach(
			channelProps.ctx,
			channelProps.containerId,
			types.ContainerAttachOptions{
				Logs:   true,
				Stdin:  true,
				Stderr: true,
				Stdout: true,
				Stream: true,
			},
		)
		if err != nil {
			removeContainer()
			reply(false, []byte(fmt.Sprintf("failed to attach container (%s)", err)))
			return
		}

		//Start the container
		err = channelProps.docker.ContainerStart(
			channelProps.ctx,
			channelProps.containerId,
			types.ContainerStartOptions{},
		)
		if err != nil {
			removeContainer()
			reply(false, []byte(fmt.Sprintf("failed to launch container (%s)", err)))
			return
		}

		//Resize container from any previous `window-change` requests
		err = channelProps.docker.ContainerResize(
			channelProps.ctx,
			channelProps.containerId,
			types.ResizeOptions{
				Height: channelProps.rows,
				Width:  channelProps.cols,
			},
		)
		if err != nil {
			removeContainer()
			reply(false, []byte(fmt.Sprintf("failed to resize container (%s)", err)))
			return
		}

		//Start transferring data
		var once sync.Once
		if channelProps.pty {
			go func() {
				_, _ = io.Copy(channel, attachResult.Reader)
				once.Do(closeConnections)
			}()
		} else {
			go func() {
				//Demultiplex Docker stream into stdout/stderr
				_, _ = stdcopy.StdCopy(channel, channel.Stderr(), attachResult.Reader)
				once.Do(closeConnections)
			}()
		}
		go func() {
			_, _ = io.Copy(attachResult.Conn, channel)
			once.Do(closeConnections)
		}()
	default:
		reply(false, []byte(fmt.Sprintf("unsupported request type (%s)", req.Type)))
	}
}
