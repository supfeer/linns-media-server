//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func configureYouTubeCommand(command *exec.Cmd) {
	// yt-dlp's standalone executable also starts a child Python process.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
}
