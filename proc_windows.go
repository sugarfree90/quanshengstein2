//go:build windows

package main

import (
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {
	// Process groups on Windows are not required for standard execution
}

func killProcessCleanly(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

