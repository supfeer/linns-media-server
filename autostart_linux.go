//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

func autostartPath() string {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(os.Getenv("HOME"), ".config", "autostart", "linns-media-server.desktop")
	}
	return filepath.Join(configDir, "autostart", "linns-media-server.desktop")
}

func autostartEnabled() (bool, error) {
	_, err := os.Stat(autostartPath())
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func setAutostart(enabled bool) error {
	path := autostartPath()
	if !enabled {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	entry := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=Linn's media server\nExec=%s\nX-GNOME-Autostart-enabled=true\n", strconv.Quote(executable))
	return os.WriteFile(path, []byte(entry), 0o644)
}
