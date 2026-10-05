//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package mcp

import (
	"errors"
	"os/exec"

	"golang.org/x/sys/unix"
)

// waitExitedUnreaped blocks until the server exits, through a kqueue
// NOTE_EXIT event that does not reap it, so its PID stays reserved until
// cmd.Wait does.
func waitExitedUnreaped(cmd *exec.Cmd) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer unix.Close(kq)

	var change unix.Kevent_t
	unix.SetKevent(&change, cmd.Process.Pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, []unix.Kevent_t{change}, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		// ESRCH: it had exited before the event was registered and, unreaped,
		// is still a zombie.
		if n == 1 && events[0].Flags&unix.EV_ERROR != 0 && unix.Errno(events[0].Data) != unix.ESRCH {
			return unix.Errno(events[0].Data)
		}
		return nil
	}
}
