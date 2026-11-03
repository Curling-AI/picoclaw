package agent

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

func TestWriteLongTermSharesCanonicalMemoryLock(t *testing.T) {
	workspace := t.TempDir()
	store := NewMemoryStore(workspace)
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(workspace, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	aliasedStore := NewMemoryStore(alias)
	locked, release := make(chan struct{}), make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- fileutil.WithFileLock(store.memoryFile, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	written := make(chan error, 1)
	var released, bypassed atomic.Bool
	go func() {
		err := aliasedStore.WriteLongTerm("agent content")
		bypassed.Store(!released.Load())
		written <- err
	}()
	select {
	case err := <-written:
		close(release)
		t.Fatalf("WriteLongTerm bypassed the canonical lock: %v", err)
	case <-time.After(2 * time.Second):
	}
	released.Store(true)
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if bypassed.Load() {
		t.Fatal("WriteLongTerm completed before the canonical lock was released")
	}
	if got := store.ReadLongTerm(); got != "agent content" {
		t.Fatalf("memory content = %q", got)
	}
}
