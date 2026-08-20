//go:build darwin

package main

import (
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
)

func autostartPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", "dev.linns-media-server.plist")
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
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.linns-media-server</string>
<key>ProgramArguments</key><array><string>%s</string></array>
<key>RunAtLoad</key><true/>
</dict></plist>
`, html.EscapeString(executable))
	return os.WriteFile(path, []byte(plist), 0o644)
}
