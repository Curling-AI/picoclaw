//go:build !windows

package tools

import (
	"errors"
	"syscall"
)

// killProcessGroup kills the group pid leads, or pid alone when it leads none.
// errProcessGone means there was nothing left to kill.
func killProcessGroup(pid int) error {
	groupErr := syscall.Kill(-pid, syscall.SIGKILL)
	if groupErr == nil {
		return nil
	}
	if syscall.Kill(pid, syscall.SIGKILL) == nil {
		return nil
	}
	if errors.Is(groupErr, syscall.ESRCH) {
		return errProcessGone
	}
	return groupErr
}

// killLeftoverGroup kills what is left in the group pid led, after pid itself
// exited. Never pid alone: by now the number may belong to another process.
func killLeftoverGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return errProcessGone
	}
	return err
}
