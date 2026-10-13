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
	YouTube       *youtubeBridge
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
	server.youtube = options.YouTube
	if cache, err := os.UserCacheDir(); err == nil {
		directory := filepath.Join(cache, "FolderDLNA")
		if err := os.MkdirAll(directory, 0700); err == nil {
			server.trace = &dlnaTrace{file: filepath.Join(directory, "dlna-requests.json")}
		}
	}
	discovery, err := newSSDP(networkInterface, server.deviceURL(), udn)
	if err != nil {
		listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	server.http.BaseContext = func(net.Listener) context.Context { return ctx }
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
	if options.YouTube != nil {
		go func() {
			ticker := time.NewTicker(options.ScanInterval)
			defer ticker.Stop()
			var previous uint32
			warmed := false
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
				if !warmed {
					entries, _ := options.YouTube.entries(options.YouTube.source().ID+"/recommended", false)
					if len(entries) > 0 {
						options.YouTube.prewarm(entries)
						warmed = true
					}
				}
				version := options.YouTube.version()
				if version == previous {
					continue
				}
				previous = version
				rel := options.YouTube.source().ID
				mediaLibrary.mu.Lock()
				mediaLibrary.updateID++
				mediaLibrary.containerUpdates[rel]++
				change := libraryChange{SystemID: mediaLibrary.updateID, Containers: map[string]uint32{rel: mediaLibrary.containerUpdates[rel]}}
				for _, feed := range youtubeFeeds {
					container := rel + "/" + feed.ID
					mediaLibrary.containerUpdates[container]++
					change.Containers[container] = mediaLibrary.containerUpdates[container]
				}
				mediaLibrary.mu.Unlock()
				server.notify(change)
				discovery.republish(ctx)
			}
		}()
	}
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
