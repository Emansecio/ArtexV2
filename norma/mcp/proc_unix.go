//go:build !windows

package mcp

import (
	"os"
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func terminateProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGTERM); err != nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}

func killProcessGroup(p *os.Process) {
	if p == nil {
		return
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		_ = p.Kill()
	}
}
