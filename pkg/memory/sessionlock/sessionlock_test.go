//go:build unix

package sessionlock

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const testWait = 5 * time.Second

func mustAcquire(t *testing.T, dir, name string) *Lock {
	t.Helper()
	lock, err := Acquire(dir, name, 0)
	if err != nil {
		t.Fatalf("Acquire(%q) = %v", name, err)
	}
	return lock
}

func release(t *testing.T, lock *Lock) {
	t.Helper()
	if err := lock.Release(); err != nil {
		t.Fatalf("Release() = %v", err)
	}
}

func releaseLocks(t *testing.T, locks []*Lock) {
	t.Helper()
	for _, lock := range locks {
		release(t, lock)
	}
}

// signalFirstPause makes the first retry pause close the returned channel: the
// waiter has opened the lock file and found it held.
func signalFirstPause(t *testing.T) <-chan struct{} {
	t.Helper()
	waiting := make(chan struct{})
	var once sync.Once
	pauseLockRetry = func(d time.Duration) {
		once.Do(func() { close(waiting) })
		time.Sleep(d)
	}
	t.Cleanup(func() { pauseLockRetry = time.Sleep })
	return waiting
}

func failLockWrites(t *testing.T) *int {
	t.Helper()
	calls := 0
	pwriteLockFile = func(int, []byte, int64) (int, error) {
		calls++
		return 0, unix.EIO
	}
	t.Cleanup(func() { pwriteLockFile = unix.Pwrite })
	return &calls
}

// stubRevalidateDir replaces the lock folder revalidation with fn and records
// each folder it gets. A local disk has no lookup cache, so fn stands for what
// the NFS client learns when it revalidates: another client's create or unlink.
func stubRevalidateDir(t *testing.T, fn func() error) *[]string {
	t.Helper()
	var dirs []string
	revalidateDir = func(dir string) error {
		dirs = append(dirs, dir)
		return fn()
	}
	t.Cleanup(func() { revalidateDir = openCloseDir })
	return &dirs
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) = %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestAcquireConflictsAcrossOpenFiles(t *testing.T) {
	dir := t.TempDir()
	holder := mustAcquire(t, dir, "s")
	start := time.Now()
	if _, err := Acquire(dir, "s", 50*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("Acquire while held = %v, want ErrBusy", err)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Fatalf("Acquire gave up after %v, before its 50ms wait", waited)
	}
	release(t, holder)
	next, err := Acquire(dir, "s", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire after release = %v", err)
	}
	release(t, next)
}

func TestTryAcquireReportsBusyWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	holder := mustAcquire(t, dir, "s")
	start := time.Now()
	if _, err := TryAcquire(dir, "s"); !errors.Is(err, ErrBusy) {
		t.Fatalf("TryAcquire while held = %v, want ErrBusy", err)
	}
	if took := time.Since(start); took >= 20*time.Millisecond {
		t.Fatalf("TryAcquire took %v, want no wait", took)
	}
	release(t, holder)
}

// POSIX locks drop when the process closes any fd of the file; flock does not.
func TestClosingAnotherDescriptorKeepsTheLock(t *testing.T) {
	dir := t.TempDir()
	holder := mustAcquire(t, dir, "s")
	other, err := os.Open(Path(dir, "s"))
	if err != nil {
		t.Fatalf("Open(lock file) = %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("Close(other fd) = %v", err)
	}
	if _, err := TryAcquire(dir, "s"); !errors.Is(err, ErrBusy) {
		t.Fatalf("TryAcquire after closing another fd = %v, want ErrBusy", err)
	}
	release(t, holder)
}

func TestAcquireCreatesOnlyTheLocksFolder(t *testing.T) {
	sessions := t.TempDir()
	release(t, mustAcquire(t, sessions, "s"))
	folder, err := os.Stat(filepath.Join(sessions, ".locks"))
	if err != nil || !folder.IsDir() || folder.Mode().Perm() != 0o700 {
		t.Fatalf("sessions/.locks = %v, %v; want a 0700 folder", folder, err)
	}
	file, err := os.Lstat(filepath.Join(sessions, ".locks", "s.lock"))
	if err != nil || !file.Mode().IsRegular() || file.Mode().Perm() != 0o600 {
		t.Fatalf("sessions/.locks/s.lock = %v, %v; want a 0600 file", file, err)
	}

	erased := filepath.Join(t.TempDir(), "sessions")
	if lock, err := Acquire(erased, "s", 0); err == nil {
		release(t, lock)
		t.Fatal("Acquire under an erased sessions folder succeeded")
	}
	if _, err := os.Lstat(erased); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Acquire brought the erased sessions folder back: Lstat = %v", err)
	}
}

func TestRemovedLockSendsTheWaiterToANewFile(t *testing.T) {
	dir := t.TempDir()
	holder := mustAcquire(t, dir, "s")
	holder.MarkRemove()
	// An open fd pins the removed inode: Linux reuses a freed inode number at once.
	pinned, err := os.Open(Path(dir, "s"))
	if err != nil {
		t.Fatalf("Open(lock file) = %v", err)
	}
	defer pinned.Close()
	removed, err := pinned.Stat()
	if err != nil {
		t.Fatalf("Stat(lock file) = %v", err)
	}
	waiting := signalFirstPause(t)
	type result struct {
		lock *Lock
		err  error
	}
	done := make(chan result, 1)
	go func() {
		lock, acquireErr := Acquire(dir, "s", testWait)
		done <- result{lock, acquireErr}
	}()
	select {
	case <-waiting:
	case got := <-done:
		t.Fatalf("waiter returned while the lock was held: %v", got.err)
	}
	release(t, holder)
	got := <-done
	if got.err != nil {
		t.Fatalf("waiter Acquire = %v", got.err)
	}
	held, err := got.lock.file.Stat()
	if err != nil {
		t.Fatalf("Stat(waiter fd) = %v", err)
	}
	current, err := os.Stat(Path(dir, "s"))
	if err != nil {
		t.Fatalf("lock path after the waiter took it: %v", err)
	}
	if !os.SameFile(held, current) {
		t.Fatal("waiter holds an inode the lock path no longer names")
	}
	if os.SameFile(held, removed) {
		t.Fatal("waiter holds the inode its holder removed")
	}
	release(t, got.lock)
}

func TestCheckFailsAfterTheLockIsLost(t *testing.T) {
	lock := mustAcquire(t, t.TempDir(), "s")
	if err := lock.Check(); err != nil {
		t.Fatalf("Check on a held lock = %v", err)
	}
	writes := failLockWrites(t)
	err := lock.Check()
	if !errors.Is(err, ErrLost) || !errors.Is(err, unix.EIO) {
		t.Fatalf("Check after EIO = %v, want ErrLost wrapping EIO", err)
	}
	if !lock.Lost() {
		t.Fatal("Lost() = false after a failed Check")
	}
	if err := lock.Check(); !errors.Is(err, ErrLost) {
		t.Fatalf("second Check = %v, want ErrLost", err)
	}
	if *writes != 1 {
		t.Fatalf("lock file writes = %d, want 1: a lost lock fails without I/O", *writes)
	}
	release(t, lock)
}

// Another process may own the path once the lease is gone.
func TestReleaseOfALostLockKeepsThePath(t *testing.T) {
	dir := t.TempDir()
	lock := mustAcquire(t, dir, "s")
	failLockWrites(t)
	if err := lock.Check(); !errors.Is(err, ErrLost) {
		t.Fatalf("Check after EIO = %v, want ErrLost", err)
	}
	lock.MarkRemove()
	release(t, lock)
	if _, err := os.Stat(Path(dir, "s")); err != nil {
		t.Fatalf("Release of a lost lock removed its path: %v", err)
	}
}

// The next owner may hold the path by the time a second Release comes.
func TestSecondReleaseLeavesTheNextOwnersLock(t *testing.T) {
	dir := t.TempDir()
	first := mustAcquire(t, dir, "s")
	first.MarkRemove()
	release(t, first)
	next := mustAcquire(t, dir, "s")
	secondErr := first.Release()
	if _, err := os.Stat(Path(dir, "s")); err != nil {
		t.Fatalf("second Release removed the next owner's lock file: %v", err)
	}
	if lock, err := TryAcquire(dir, "s"); !errors.Is(err, ErrBusy) {
		if err == nil {
			release(t, lock)
		}
		t.Fatalf("TryAcquire after a second Release = %v, want ErrBusy", err)
	}
	if secondErr != nil {
		t.Fatalf("second Release = %v, want a no-op", secondErr)
	}
	release(t, next)
}

func TestReleaseRemovesOnlyTheInodeItHolds(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "s")
	lock := mustAcquire(t, dir, "s")
	lock.MarkRemove()
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(lock file) = %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile(lock file) = %v", err)
	}
	replaced, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(replaced lock file) = %v", err)
	}
	release(t, lock)
	current, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Release removed a lock file it did not hold: %v", err)
	}
	if !os.SameFile(replaced, current) {
		t.Fatal("lock path changed inode across Release")
	}

	gone := mustAcquire(t, dir, "t")
	gone.MarkRemove()
	if err := os.Remove(Path(dir, "t")); err != nil {
		t.Fatalf("Remove(lock file) = %v", err)
	}
	release(t, gone)
}

// On NFS a lookup can answer from a cached entry; samePath must revalidate the
// folder before it looks the path up, or it misses another client's new file.
func TestSamePathRevalidatesTheFolderBeforeTheLookup(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "s")
	lock := mustAcquire(t, dir, "s")
	dirs := stubRevalidateDir(t, func() error {
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.WriteFile(path, nil, 0o600)
	})
	same, err := samePath(lock.file, path)
	if err != nil || same {
		t.Fatalf("samePath after the folder showed a new file = %v, %v; want false", same, err)
	}
	if !slices.Equal(*dirs, []string{filepath.Join(dir, Dir)}) {
		t.Fatalf("revalidated %q, want the lock folder once", *dirs)
	}
	release(t, lock)
}

func TestAcquireReturnsAFolderRevalidationError(t *testing.T) {
	dir := t.TempDir()
	stubRevalidateDir(t, func() error { return unix.EIO })
	if lock, err := Acquire(dir, "s", 0); !errors.Is(err, unix.EIO) {
		if err == nil {
			release(t, lock)
		}
		t.Fatalf("Acquire with a failing folder revalidation = %v, want EIO", err)
	}
}

// The open without O_CREAT goes through the same cache: revalidating first
// lets it see a lock file another client just created.
func TestAcquireAllWithoutCreateRevalidatesTheFolderBeforeOpening(t *testing.T) {
	dir := t.TempDir()
	release(t, mustAcquire(t, dir, "other"))
	created := false
	dirs := stubRevalidateDir(t, func() error {
		if created {
			return nil
		}
		created = true
		return os.WriteFile(Path(dir, "s"), nil, 0o600)
	})
	locks, err := AcquireAll(dir, []string{"s"}, false, 0)
	if err != nil || len(locks) != 1 {
		t.Fatalf("AcquireAll without create after the folder showed s = %v, %v; want s locked", locks, err)
	}
	if len(*dirs) == 0 || (*dirs)[0] != filepath.Join(dir, Dir) {
		t.Fatalf("revalidated %q, want the lock folder first", *dirs)
	}
	releaseLocks(t, locks)
}

func TestAcquireAllWithoutCreateReturnsAFolderRevalidationError(t *testing.T) {
	dir := t.TempDir()
	release(t, mustAcquire(t, dir, "s"))
	failed := false
	stubRevalidateDir(t, func() error {
		if failed {
			return nil
		}
		failed = true
		return unix.EIO
	})
	if locks, err := AcquireAll(dir, []string{"s"}, false, 0); !errors.Is(err, unix.EIO) {
		releaseLocks(t, locks)
		t.Fatalf("AcquireAll without create, failing folder revalidation = %v, want EIO", err)
	}
}

func TestRejectsNamesThatLeaveTheFolder(t *testing.T) {
	dir := t.TempDir()
	release(t, mustAcquire(t, dir, "s"))
	for _, name := range []string{"", ".", "..", "a/b", `a\b`} {
		if lock, err := Acquire(dir, name, 0); err == nil {
			release(t, lock)
			t.Errorf("Acquire(%q) succeeded", name)
		}
		if lock, err := TryAcquire(dir, name); err == nil {
			release(t, lock)
			t.Errorf("TryAcquire(%q) succeeded", name)
		}
		if locks, err := AcquireAll(dir, []string{name}, true, 0); err == nil {
			releaseLocks(t, locks)
			t.Errorf("AcquireAll(%q) succeeded", name)
		}
	}
	if got := entryNames(t, filepath.Join(dir, Dir)); !slices.Equal(got, []string{"s.lock"}) {
		t.Fatalf("sessions/.locks = %q, want only s.lock", got)
	}
	if got := entryNames(t, dir); !slices.Equal(got, []string{Dir}) {
		t.Fatalf("sessions = %q, want only %s", got, Dir)
	}
}

func TestAcquireAllLocksInAscendingNameOrder(t *testing.T) {
	dir := t.TempDir()
	holder := mustAcquire(t, dir, "b")
	waiting := signalFirstPause(t)
	var locks []*Lock
	done := make(chan error, 1)
	go func() {
		var err error
		locks, err = AcquireAll(dir, []string{"b", "a"}, true, testWait)
		done <- err
	}()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("AcquireAll returned while b was held: %v", err)
	}
	if lock, err := TryAcquire(dir, "a"); !errors.Is(err, ErrBusy) {
		if err == nil {
			release(t, lock)
		}
		t.Fatalf("TryAcquire(a) while AcquireAll waits for b = %v, want ErrBusy", err)
	}
	release(t, holder)
	if err := <-done; err != nil {
		t.Fatalf("AcquireAll = %v", err)
	}
	if len(locks) != 2 || locks[0].path != Path(dir, "a") || locks[1].path != Path(dir, "b") {
		t.Fatalf("AcquireAll locks = %v, want a then b", locks)
	}
	releaseLocks(t, locks)
}

// flock is per open file: a second open of the same name in one call would wait on itself.
func TestAcquireAllTakesARepeatedNameOnce(t *testing.T) {
	locks, err := AcquireAll(t.TempDir(), []string{"a", "a"}, true, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("AcquireAll(a, a) = %v", err)
	}
	if len(locks) != 1 {
		t.Fatalf("AcquireAll(a, a) took %d locks, want 1", len(locks))
	}
	releaseLocks(t, locks)
}

func TestAcquireAllWithoutCreateLocksOnlyExistingFiles(t *testing.T) {
	dir := t.TempDir()
	locks, err := AcquireAll(dir, []string{"a"}, false, 0)
	if err != nil || len(locks) != 0 {
		t.Fatalf("AcquireAll without .locks = %v, %v; want no locks", locks, err)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, Dir)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("AcquireAll without create made .locks: Lstat = %v", statErr)
	}
	release(t, mustAcquire(t, dir, "a"))
	locks, err = AcquireAll(dir, []string{"a", "b"}, false, 0)
	if err != nil || len(locks) != 1 || locks[0].path != Path(dir, "a") {
		t.Fatalf("AcquireAll(a, b) = %v, %v; want only a", locks, err)
	}
	if _, err := os.Lstat(Path(dir, "b")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("AcquireAll without create made b's lock file: Lstat = %v", err)
	}
	if _, err := TryAcquire(dir, "a"); !errors.Is(err, ErrBusy) {
		t.Fatalf("TryAcquire(a) while AcquireAll holds it = %v, want ErrBusy", err)
	}
	releaseLocks(t, locks)
}

func TestAcquireAllReleasesWhatItTookWhenOneIsBusy(t *testing.T) {
	dir := t.TempDir()
	holder := mustAcquire(t, dir, "b")
	if _, err := AcquireAll(dir, []string{"a", "b"}, true, 20*time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("AcquireAll with b held = %v, want ErrBusy", err)
	}
	lock, err := TryAcquire(dir, "a")
	if err != nil {
		t.Fatalf("TryAcquire(a) after AcquireAll failed = %v, want a released", err)
	}
	release(t, lock)
	release(t, holder)
}
