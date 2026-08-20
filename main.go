package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := initializeLocalization(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
	mediaPath := flag.String("media", "", tr("linn.cli.media"))
	name := flag.String("name", tr("linn.cli.default_name"), tr("linn.cli.name"))
	interfaceName := flag.String("interface", "", tr("linn.cli.interface"))
	port := flag.Int("port", 1338, tr("linn.cli.port"))
	scanInterval := flag.Duration("scan-interval", 2*time.Second, tr("linn.cli.scan_interval"))
	flag.Parse()

	var err error
	if *mediaPath == "" {
		err = runDesktop(*interfaceName, *scanInterval)
	} else {
		err = runHeadless(*mediaPath, *name, *interfaceName, *port, *scanInterval)
	}
	if err != nil {
		slog.Error(tr("linn.log.application_stopped"), "error", err)
		os.Exit(1)
	}
}

func runHeadless(mediaPath, name, interfaceName string, port int, scanInterval time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	running, err := startServer(ctx, serverOptions{
		Name: name, Root: mediaPath, Identity: "headless:" + mediaPath,
		InterfaceName: interfaceName, Port: port, ScanInterval: scanInterval,
	})
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		running.Stop()
	case <-running.Done():
	}
	return running.Wait()
}
