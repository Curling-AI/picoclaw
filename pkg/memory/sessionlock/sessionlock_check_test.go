//go:build unix

package sessionlock

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/sipeed/picoclaw/pkg/internal/locktest"
)

// countLockWrites counts the pwrites of every Check, which still reach the disk.
func countLockWrites(t *testing.T) *int {
	t.Helper()
	calls := 0
	pwriteLockFile = func(fd int, p []byte, offset int64) (int, error) {
		calls++
		return unix.Pwrite(fd, p, offset)
	}
	t.Cleanup(func() { pwriteLockFile = unix.Pwrite })
	return &calls
}

// After a lost NFS lease the byte may be accepted and only its fsync fail: the
// lock is lost all the same, for good, and a lost lock removes nothing.
func TestCheckFailsWhenTheSyncFails(t *testing.T) {
	dir := t.TempDir()
	lock := mustAcquire(t, dir, "s")
	writes := countLockWrites(t)
	syncs := 0
	locktest.Sync = func(*os.File) error {
		syncs++
		return unix.EIO
	}
	t.Cleanup(func() { locktest.Sync = nil })

	err := lock.Check()
	if !errors.Is(err, ErrLost) || !errors.Is(err, unix.EIO) {
		t.Fatalf("Check after a failed fsync = %v, want ErrLost wrapping EIO", err)
	}
	if !lock.Lost() {
		t.Fatal("Lost() = false after a failed fsync")
	}
	if err := lock.Check(); !errors.Is(err, ErrLost) {
		t.Fatalf("second Check = %v, want ErrLost", err)
	}
	if *writes != 1 || syncs != 1 {
		t.Fatalf("writes = %d, syncs = %d; want 1 each: a lost lock fails without I/O", *writes, syncs)
	}
	lock.MarkRemove()
	release(t, lock)
	if _, err := os.Stat(Path(dir, "s")); err != nil {
		t.Fatalf("Release of a lock lost at fsync removed its path: %v", err)
	}
}

// Without the seam the Check's fsync is the real one, and passes on a held lock.
func TestCheckSyncsTheLockFileWithoutTheSeam(t *testing.T) {
	lock := mustAcquire(t, t.TempDir(), "s")
	if err := lock.Check(); err != nil {
		t.Fatalf("Check with the real fsync = %v, want nil", err)
	}
	release(t, lock)
}

// A deletion's Release unlinks the lock path only after a Check: a holder that
// lost its lease would unlink the path the next holder now locks, and a third
// process would lock a new file beside it.
func TestReleaseOfALockMarkedForRemovalChecksFirst(t *testing.T) {
	dir := t.TempDir()
	lock := mustAcquire(t, dir, "s")
	lock.MarkRemove()
	failLockWrites(t)

	release(t, lock)
	if _, err := os.Stat(Path(dir, "s")); err != nil {
		t.Fatalf("Release after a failed Check removed the lock path: %v", err)
	}
	if !lock.Lost() {
		t.Fatal("Lost() = false after the Check of Release failed")
	}
}
