package main

import (
	"os/exec"
	"strconv"
)

func configureYouTubeCommand(command *exec.Cmd) {
	command.Cancel = func() error {
		return exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(command.Process.Pid)).Run()
	}
}
