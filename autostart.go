package main

import (
	"errors"
	"os"
	"path/filepath"
)

func ensureDefaultAutostart() error {
	configPath, err := appConfigPath()
	if err != nil {
		return err
	}
	marker := filepath.Join(filepath.Dir(configPath), ".autostart-configured")
	if _, err := os.Stat(marker); err == nil {
		enabled, err := autostartEnabled()
		if err != nil || !enabled {
			return err
		}
		return setAutostart(true)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := setAutostart(true); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return err
	}
	return os.WriteFile(marker, nil, 0o600)
}
