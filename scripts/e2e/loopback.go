//go:build ignore

// loopback forwards TCP connections from loopback addresses to another
// address. The e2e harnesses run it in the Docker host's network namespace
// (docker run --network host) when they run the hub template's pipelines
// (--template, scripts/e2e/template-lib.sh): the CI jobs then run in
// containers of that namespace too, and reach the forge at its external
// URL, http://localhost or http://localhost:3000, the only plain-http host
// touchmark sends a credential to. A job container cannot
// share the forge's own network namespace instead: GitLab's Docker executor
// names every container's host, and Docker refuses a hostname with
// --network container:<name>.
//
// Usage: go run scripts/e2e/loopback.go -to HOST:PORT ADDR...
//
// It listens on every ADDR (such as 127.0.0.1:80 and [::1]:80; an IPv6
// address the namespace does not have is skipped), prints "loopback: ready"
// once all listen, and copies each connection to and from a new connection
// to HOST:PORT until both sides close. It stops on SIGINT or SIGTERM. It is
// test tooling: the build tag keeps it out of the module's packages.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	to := flag.String("to", "", "the HOST:PORT to forward to")
	flag.Parse()
	if *to == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: loopback -to HOST:PORT ADDR...")
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var listeners []net.Listener
	for _, addr := range flag.Args() {
		host, _, err := net.SplitHostPort(addr)
		if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
			fmt.Fprintf(os.Stderr, "loopback: %s is not a loopback IP address with a port\n", addr)
			os.Exit(2)
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			if net.ParseIP(host).To4() == nil && (errors.Is(err, syscall.EADDRNOTAVAIL) || errors.Is(err, syscall.EAFNOSUPPORT)) {
				fmt.Fprintf(os.Stderr, "loopback: skipping %s: %v\n", addr, err)
				continue
			}
			fmt.Fprintf(os.Stderr, "loopback: %v\n", err)
			os.Exit(1)
		}
		listeners = append(listeners, l)
	}
	if len(listeners) == 0 {
		fmt.Fprintln(os.Stderr, "loopback: no address to listen on")
		os.Exit(1)
	}
	fmt.Println("loopback: ready")
	var wg sync.WaitGroup
	for _, l := range listeners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serve(l, *to)
		}()
	}
	<-ctx.Done()
	for _, l := range listeners {
		_ = l.Close()
	}
	wg.Wait()
}

// serve accepts connections on l until it is closed and forwards each to
// to.
func serve(l net.Listener, to string) {
	for {
		c, err := l.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				fmt.Fprintf(os.Stderr, "loopback: %v\n", err)
			}
			return
		}
		go forward(c.(*net.TCPConn), to)
	}
}

// forward copies c to a new connection to to and back, closing each
// direction's write side when its reader ends.
func forward(c *net.TCPConn, to string) {
	defer c.Close()
	u, err := net.DialTimeout("tcp", to, 10*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loopback: %v\n", err)
		return
	}
	up := u.(*net.TCPConn)
	defer up.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(up, c)
		_ = up.CloseWrite()
		close(done)
	}()
	_, _ = io.Copy(c, up)
	_ = c.CloseWrite()
	<-done
}
