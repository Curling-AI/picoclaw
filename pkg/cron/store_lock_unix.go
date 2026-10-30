//go:build !windows

package cron

import (
	"os"
	"path/filepath"
	"syscall"
)

// lockStore takes an exclusive lock shared by every process over storePath.
// flock is per open file, so it also arbitrates two services in one process.
func lockStore(storePath string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(storePath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(storePath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
