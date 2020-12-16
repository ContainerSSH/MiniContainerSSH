package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"log"
	"net"
	"sync"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.org/x/crypto/ssh"
)

func main() {
	// Open listen socket
	listener, err := net.Listen("tcp", "0.0.0.0:2222")
	if err != nil {
		log.Fatalln(err)
	}
	for {
		// Accept TCP connection
		conn, err := listener.Accept()
		if err != nil {
			break
		}

		config := getServerConfig(err)

		// Perform SSH handshake
		_, newChannels, requests, err := ssh.NewServerConn(conn, config)
		if err != nil {
			_ = conn.Close()
			continue
		}
		// Handle non-channel requests
		go handleRequests(requests)
		// Handle new channels
		go handleChannels(newChannels)
	}
}

func getServerConfig(err error) *ssh.ServerConfig {
	config := &ssh.ServerConfig{
		NoClientAuth: true,
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalln(err)
	}
	hostKey, err := ssh.NewSignerFromKey(key)
	if err != nil {
		log.Fatalln(err)
	}
	config.AddHostKey(hostKey)
	return config
}

func handleChannels(channels <-chan ssh.NewChannel) {
	for {
		// When a new channel comes in, handle it
		newChannel, ok := <-channels
		if !ok {
			// Connection is closed
			break
		}
		go handleChannel(newChannel)
	}
}

func handleChannel(newChannel ssh.NewChannel) {
	// Accept all channels. Normally, we would check if it s a "session "channel
	channel, requests, err := newChannel.Accept()
	if err != nil {
		log.Println(err)
		return
	}

	tty := false
	for {
		req, ok := <-requests
		if !ok {
			break
		}
		switch req.Type {
		case "pty-req":
			// Interactive session
			tty = true
			if req.WantReply {
				_ = req.Reply(true, []byte{})
			}
		case "shell":
			if req.WantReply {
				_ = req.Reply(true, []byte{})
			}
			go launchShell(channel, tty)
		default:
			if req.WantReply {
				_ = req.Reply(false, []byte("unsupported request"))
			}
		}
	}
}

func launchShell(channel ssh.Channel, tty bool) {
	cli, err := client.NewClientWithOpts()
	if err != nil {
		log.Fatalln(err)
	}

	// region Create container
	cnt, err := cli.ContainerCreate(
		context.TODO(),
		&container.Config{
			Image:       "ubuntu",
			Tty:         tty,
			AttachStdin: true,
			StdinOnce:   true,
			OpenStdin:   true,
		},
		nil,
		nil,
		nil,
		"",
	)
	if err != nil {
		log.Fatalln(err)
	}
	// endregion

	// region Attach to container
	attach, err := cli.ContainerAttach(
		context.Background(),
		cnt.ID,
		types.ContainerAttachOptions{
			Stream: true,
			Stdin:  true,
			Stdout: true,
			Stderr: true,
			Logs:   true,
		},
	)
	if err != nil {
		log.Fatalln(err)
	}
	// endregion

	// region Start container
	err = cli.ContainerStart(context.TODO(), cnt.ID, types.ContainerStartOptions{})
	if err != nil {
		log.Fatalln(err)
	}
	// endregion

	closeChannel := func() {
		_ = channel.Close()
		_ = cli.ContainerRemove(context.TODO(), cnt.ID, types.ContainerRemoveOptions{})
	}

	var once sync.Once
	if tty {
		go func() {
			// Copy container output only to stdout (TTY is already multiplexed)
			_, _ = io.Copy(channel, attach.Reader)
			once.Do(closeChannel)
		}()
	} else {
		go func() {
			// Copy from to stdout and stderr
			_, _ = stdcopy.StdCopy(channel, channel.Stderr(), attach.Reader)
			once.Do(closeChannel)
		}()
	}
	go func() {
		// Copy stdin from channel to container
		_, _ = io.Copy(attach.Conn, channel)
		once.Do(closeChannel)
	}()
}

func handleRequests(requests <-chan *ssh.Request) {
	for {
		request, ok := <-requests
		if !ok {
			break
		}
		if request.WantReply {
			_ = request.Reply(false, []byte("request type not supported"))
		}
	}
}
