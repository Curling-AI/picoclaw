//go:build windows

package mcp

import (
	"os/exec"
	"syscall"
)

// Windows has no process-group signals; only the server itself is signaled.
func startInOwnProcessGroup(*exec.Cmd) {}

func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}
