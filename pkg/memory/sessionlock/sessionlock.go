// Package sessionlock is the per-session file lock that every writer of a
// sessions/ folder holds, so two processes sharing it (the two pods of one
// assistant during a rollout, on EFS) never write the same session at once.
// It is a leaf package, so writers outside pkg/memory take the same lock
// without importing the store.
package sessionlock

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Dir is the subfolder of sessions/ that holds the lock files. A subfolder
// keeps the locks out of every scan of sessions/, and a lock file is never
// replaced, so it keeps one inode while the session's own files change.
const Dir = ".locks"

var (
	// ErrBusy reports that another holder kept the lock past the wait.
	ErrBusy = errors.New("session locked by another process")
	// ErrLost reports that the lock can no longer be proven held, as after a
	// lost NFS lease: the holder stops writing and undoes nothing.
	ErrLost = errors.New("session lock lost")
)

// Path is the lock file of the session file name name (the sanitized key that
// also names its .jsonl) under the sessions folder dir.
func Path(dir, name string) string {
	return filepath.Join(dir, Dir, name+".lock")
}

func validName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("sessionlock: invalid session name %q", name)
	}
	return nil
}
