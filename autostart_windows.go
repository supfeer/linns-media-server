//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

const windowsRunKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`

func autostartEnabled() (bool, error) {
	err := exec.Command("reg", "query", windowsRunKey, "/v", "LinnsMediaServer").Run()
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return false, nil
	}
	return false, err
}

func setAutostart(enabled bool) error {
	if !enabled {
		return exec.Command("reg", "delete", windowsRunKey, "/v", "LinnsMediaServer", "/f").Run()
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.Command("reg", "add", windowsRunKey, "/v", "LinnsMediaServer", "/t", "REG_SZ", "/d", fmt.Sprintf(`"%s"`, executable), "/f").Run()
}
