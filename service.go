package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type serverOptions struct {
	Name          string
	Root          string
	Sources       []librarySource
	Identity      string
	InterfaceName string
	Port          int
	ScanInterval  time.Duration
}

type runningServer struct {
	cancel   context.CancelFunc
	stopOnce sync.Once
	done     chan struct{}
	mu       sync.Mutex
	err      error
}

func startServer(parent context.Context, options serverOptions) (*runningServer, error) {
	if options.Name == "" {
		return nil, errors.New("server name cannot be empty")
	}
	if options.Port < 0 || options.Port > 65535 {
		return nil, fmt.Errorf("invalid port %d", options.Port)
	}
	if options.ScanInterval < 500*time.Millisecond {
		return nil, errors.New("scan interval must be at least 500ms")
	}
	var mediaLibrary *library
	var err error
	if options.Root != "" {
		mediaLibrary, err = newLibrary(options.Root)
	} else {
		mediaLibrary, err = newCatalog(options.Sources)
	}
	if err != nil {
		return nil, err
	}
	networkInterface, localIP, err := chooseInterface(options.InterfaceName)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: localIP, Port: options.Port})
	if err != nil {
		return nil, fmt.Errorf("listen on %s:%d: %w", localIP, options.Port, err)
	}
	hostname, _ := os.Hostname()
	identity := options.Identity
	if identity == "" {
		identity = filepath.Clean(options.Root)
	}
	udn := stableUUID(hostname + "\x00" + identity)
	server := newMediaServer(options.Name, udn, listener.Addr().String(), localIP, mediaLibrary)
	discovery, err := newSSDP(networkInterface, server.deviceURL(), udn)
	if err != nil {
		listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	running := &runningServer{cancel: cancel, done: make(chan struct{})}
	httpErrors := make(chan error, 1)
	go func() {
		if err := server.serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErrors <- err
		}
	}()
	ssdpDone := make(chan struct{})
	go func() {
		discovery.run(ctx)
		close(ssdpDone)
	}()
	go mediaLibrary.watch(ctx, options.ScanInterval, func(change libraryChange) {
		server.notify(change)
		discovery.republish(ctx)
	})
	go func() {
		var runError error
		select {
		case <-ctx.Done():
		case runError = <-httpErrors:
			cancel()
		}
		shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		if err := server.shutdown(shutdownContext); err != nil && runError == nil {
			runError = err
		}
		cancelShutdown()
		<-ssdpDone
		running.mu.Lock()
		running.err = runError
		running.mu.Unlock()
		close(running.done)
	}()
	slog.Info("DLNA server started", "name", options.Name, "address", listener.Addr(), "interface", networkInterface.Name)
	return running, nil
}

func (server *runningServer) Stop()                 { server.stopOnce.Do(server.cancel) }
func (server *runningServer) Done() <-chan struct{} { return server.done }
func (server *runningServer) Wait() error {
	<-server.done
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.err
}
