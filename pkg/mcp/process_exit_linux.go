//go:build linux

package mcp

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

// waitExitedUnreaped blocks until the server exits and leaves it a zombie
// (WNOWAIT), so its PID stays reserved until cmd.Wait reaps it.
func waitExitedUnreaped(cmd *exec.Cmd) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
