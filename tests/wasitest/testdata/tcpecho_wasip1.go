//go:build wasip1

// tcpecho_wasip1 is a guest program for TestTCPEchoWasip1. It accepts a
// single TCP connection on a wasi-pre-opened socket fd, echoes the
// payload back, half-closes its write side, and exits.
package main

import (
	"errors"
	"net"
	"os"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		println(err.Error())
		os.Exit(1)
	}
}

func run() error {
	l, err := findListener()
	if err != nil {
		return err
	}
	if l == nil {
		return errors.New("no pre-opened sockets available")
	}
	defer l.Close()

	c, err := l.Accept()
	if err != nil {
		return err
	}
	defer c.Close()

	var buf [256]byte
	n, err := c.Read(buf[:])
	if err != nil {
		return err
	}
	if _, err := c.Write(buf[:n]); err != nil {
		return err
	}
	return c.(*net.TCPConn).CloseWrite()
}

// findListener walks pre-opened fds starting at 3 (0/1/2 are stdio),
// returning a TCPListener for the first one that is a socket. wasip1
// host runtimes hand pre-opened TCP listeners as raw fds with no
// in-band metadata, so probing via net.FileListener is the only path.
func findListener() (net.Listener, error) {
	for preopenFd := uintptr(3); ; preopenFd++ {
		f := os.NewFile(preopenFd, "")
		l, err := net.FileListener(f)
		f.Close()

		var se syscall.Errno
		if errors.As(err, &se) {
			switch se {
			case syscall.ENOTSOCK:
				continue
			case syscall.EBADF:
				return nil, nil
			}
		}
		return l, err
	}
}
