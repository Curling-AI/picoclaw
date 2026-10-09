//go:build windows

package tools

import (
	"os/exec"
	"strconv"
)

func killProcessGroup(pid int) error {
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	return nil
}

// killLeftoverGroup: Windows has no process group outliving its leader to
// signal; taskkill /T already took the tree while the leader ran.
func killLeftoverGroup(int) error {
	return errProcessGone
}
