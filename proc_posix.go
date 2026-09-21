//go:build !windows

package main

import (
	"log"
	"os/exec"
	"syscall"
	"time"
)

func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killProcessCleanly(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	if pid <= 0 {
		return
	}

	myPgid, _ := syscall.Getpgid(0)
	pgid, err := syscall.Getpgid(pid)
	usePgid := (err == nil && pgid > 1 && pgid != myPgid)

	// 1. Send SIGTERM first for graceful release of sound cards/devices
	if usePgid {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = cmd.Process.Signal(syscall.SIGTERM)
	}

	// 2. Poll up to 250ms using signal 0 to see if process terminated
	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(30 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err != nil {
			// Process is already terminated
			return
		}
	}

	// 3. Process is still alive after 250ms -> Force kill with SIGKILL
	log.Printf("[Process] Process %d did not terminate cleanly, sending SIGKILL...", pid)
	if usePgid {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	} else {
		_ = cmd.Process.Kill()
	}
}

