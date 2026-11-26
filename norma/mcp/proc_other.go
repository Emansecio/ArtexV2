//go:build windows

package mcp

import (
	"os"
	"os/exec"
)

func setProcessGroup(cmd *exec.Cmd) {}

func terminateProcessGroup(p *os.Process) {
	killProcessGroup(p)
}

func killProcessGroup(p *os.Process) {
	if p != nil {
		_ = p.Kill()
	}
}
