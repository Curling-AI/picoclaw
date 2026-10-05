//go:build !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package mcp

import (
	"errors"
	"os/exec"
)

// Without a wait that leaves the server unreaped, isolatedPipeRWC.Close
// reaps it with cmd.Wait and does not signal its group afterwards.
func waitExitedUnreaped(*exec.Cmd) error {
	return errors.ErrUnsupported
}
