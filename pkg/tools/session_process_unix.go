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
// exited and was reaped. A number stays taken while it names a group with
// members, so if pid answers again it belongs to another process now and our
// group is empty: its group is not ours to signal.
func killLeftoverGroup(pid int) error {
	if syscall.Kill(pid, 0) == nil {
		return errProcessGone
	}
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return errProcessGone
	}
	return err
}
