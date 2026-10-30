//go:build !windows

package cron

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const storeLockRetry = 100 * time.Millisecond

// lockStore takes an exclusive lock shared by every process over storePath.
// flock is per open file, so it also arbitrates two services in one process.
// It gives up with errStoreLocked after timeout: on EFS a node lost while
// holding it keeps the lock until its lease expires.
func lockStore(storePath string, timeout time.Duration) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(storePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errStoreLocked
		}
		time.Sleep(storeLockRetry)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
