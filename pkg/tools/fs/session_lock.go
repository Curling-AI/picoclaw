package fstools

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
)

// sessionLockWait is how long a file tool waits for the session store or
// another process that holds a session's lock.
var sessionLockWait = 10 * time.Second

const sessionLocksFolder = ".locks"

// storeInternalPrefixes are the names in sessions/ that belong to the session
// store, not to a session: content artifacts, writes in flight and the
// checkpoint staging file. With the .locks folder (and anything in it) they
// are refused to the agent's file tools. The ethos SetFile/DeleteFile list
// (D4) mirrors this one.
var storeInternalPrefixes = []string{".pc-history-", ".tmp-", ".content-checkpoint-new."}

// isStoreInternalName reports whether first, the first path element under
// sessions/, is the .locks folder or a store-internal file.
func isStoreInternalName(first string) bool {
	if strings.EqualFold(first, sessionLocksFolder) {
		return true
	}
	for _, prefix := range storeInternalPrefixes {
		if len(first) >= len(prefix) && strings.EqualFold(first[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}

// realPath resolves symlinks of the deepest existing ancestor of path and
// appends the part that does not exist yet.
func realPath(path string) string {
	path = filepath.Clean(path)
	rest := ""
	for cur := path; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return path
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// withSessionWriteLock runs write under the session lock when path is a
// session file of workspace/sessions, so a tool write never interleaves with
// the session store's. The .locks folder belongs to the store: it is refused.
// Paths elsewhere take no session lock.
func withSessionWriteLock(workspace, path string, write func() error) error {
	if workspace == "" {
		return write()
	}
	absWorkspace, errWorkspace := filepath.Abs(workspace)
	absPath, errPath := filepath.Abs(path)
	if errWorkspace != nil || errPath != nil {
		return write() // outside sessions/: no lock
	}
	sessions := realPath(filepath.Join(absWorkspace, "sessions"))
	target := filepath.Join(realPath(filepath.Dir(absPath)), filepath.Base(absPath))
	rel, err := filepath.Rel(sessions, target)
	if err != nil || !filepath.IsLocal(rel) {
		return write()
	}
	first, _, nested := strings.Cut(filepath.ToSlash(rel), "/")
	if isStoreInternalName(first) {
		return errors.New("sessions/.locks and the store's internal files are managed by the session store")
	}
	name, ok := memory.SessionFileName(rel)
	if nested || !ok {
		return write()
	}
	lock, err := sessionlock.Acquire(sessions, name, sessionLockWait)
	if err != nil {
		if errors.Is(err, sessionlock.ErrBusy) {
			return fmt.Errorf("session %s is being written by the assistant; try again", name)
		}
		return err
	}
	defer func() { _ = lock.Release() }()
	return write()
}
