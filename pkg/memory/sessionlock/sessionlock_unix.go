//go:build unix

package sessionlock

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sipeed/picoclaw/pkg/internal/locktest"
)

const (
	firstRetryPause = 10 * time.Millisecond
	maxRetryPause   = 100 * time.Millisecond
)

// Test seams: a lost NFS lease and a stale NFS lookup cannot be made on a
// local disk, and a test must catch a waiter between a failed try and its retry.
var (
	pwriteLockFile = unix.Pwrite
	pauseLockRetry = time.Sleep
	revalidateDir  = openCloseDir
)

// Lock is one held session lock. It belongs to the goroutine that took it.
type Lock struct {
	file                   *os.File
	path                   string
	remove, lost, released bool
}

// Acquire takes the lock of session name under dir, waiting up to wait for
// another holder. It creates the lock file, and the .locks folder one level
// deep, when missing.
func Acquire(dir, name string, wait time.Duration) (*Lock, error) {
	return acquire(dir, name, true, time.Now().Add(wait))
}

// TryAcquire takes the lock only if no one holds it, without waiting.
func TryAcquire(dir, name string) (*Lock, error) {
	return acquire(dir, name, true, time.Time{})
}

// AcquireAll takes the locks of several sessions in ascending name order, the
// order every process uses, so two holders of overlapping sets cannot deadlock.
// Each lock waits up to wait. Without create it locks only the sessions whose
// lock file exists and creates nothing; "exists" means visible after the
// .locks folder is revalidated, which on NFS shows another client's new files.
// On error it releases what it took.
func AcquireAll(dir string, names []string, create bool, wait time.Duration) ([]*Lock, error) {
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	locks := make([]*Lock, 0, len(sorted))
	for _, name := range sorted {
		lock, err := acquire(dir, name, create, time.Now().Add(wait))
		if !create && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, errors.Join(err, releaseAll(locks))
		}
		locks = append(locks, lock)
	}
	return locks, nil
}

func releaseAll(locks []*Lock) error {
	errs := make([]error, 0, len(locks))
	for _, lock := range slices.Backward(locks) {
		errs = append(errs, lock.Release())
	}
	return errors.Join(errs...)
}

// acquire polls LOCK_NB so a deadline holds: a blocking flock cannot be
// canceled, and on EFS a lost node keeps the lock until its lease ends.
func acquire(dir, name string, create bool, deadline time.Time) (*Lock, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	path := Path(dir, name)
	for {
		f, err := openLockFile(path, create)
		if err != nil {
			return nil, err
		}
		if err = flockUntil(f, deadline); err != nil {
			closeErr := f.Close()
			if errors.Is(err, ErrBusy) {
				return nil, fmt.Errorf("%w: %s", ErrBusy, name)
			}
			return nil, errors.Join(&os.PathError{Op: "flock", Path: path, Err: err}, closeErr)
		}
		// A deleter unlinks the path while holding it; a waiter that got the
		// old inode reopens the path, before its deadline only.
		same, err := samePath(f, path)
		if err == nil && same {
			return &Lock{file: f, path: path}, nil
		}
		if closeErr := f.Close(); err != nil {
			return nil, errors.Join(err, closeErr)
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %s", ErrBusy, name)
		}
	}
}

// flockUntil takes flock(2) LOCK_EX, never a POSIX lock: closing any other fd
// of the file drops a POSIX lock, and flock conflicts between NFS clients.
func flockUntil(f *os.File, deadline time.Time) error {
	fd := int(f.Fd())
	pause := firstRetryPause
	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return err
		}
		if !time.Now().Before(deadline) {
			return ErrBusy
		}
		pauseLockRetry(pause)
		pause = min(2*pause, maxRetryPause)
	}
}

// openLockFile opens the lock file without following a link. Creating, it
// makes the .locks folder one level only: after an erase removed sessions/,
// a late writer fails instead of bringing the assistant's folder back.
func openLockFile(path string, create bool) (*os.File, error) {
	flags := os.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if !create {
		if err := revalidateDir(filepath.Dir(path)); err != nil {
			return nil, err
		}
		return os.OpenFile(path, flags, 0)
	}
	flags |= os.O_CREATE
	f, err := os.OpenFile(path, flags, 0o600)
	if !errors.Is(err, fs.ErrNotExist) {
		return f, err
	}
	err = os.Mkdir(filepath.Dir(path), 0o700)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	return os.OpenFile(path, flags, 0o600)
}

// samePath reports whether path still names the inode f holds. A missing path
// counts as another inode: its holder removed it, and the caller reopens.
func samePath(f *os.File, path string) (bool, error) {
	held, err := f.Stat()
	if err != nil {
		return false, err
	}
	current, err := lstatFresh(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(held, current), nil
}

// lstatFresh revalidates the lock folder before the lookup, so a cached NFS
// entry cannot hide that another client replaced or removed the path.
func lstatFresh(path string) (fs.FileInfo, error) {
	if err := revalidateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return os.Lstat(path)
}

// openCloseDir opens and closes dir. On NFS, opening a directory makes the
// client fetch its attributes, and a lookup after that sees the names another
// client created there instead of the client's cached answer, which can last
// up to acdirmax (60 s by default).
func openCloseDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return d.Close()
}

// Check writes one byte on the lock's fd and syncs it, which must reach the
// server: after a lost NFS lease that write fails with EIO. A failure marks
// the lock lost for good, and later calls fail without I/O.
func (l *Lock) Check() error {
	if l.lost {
		return ErrLost
	}
	_, err := pwriteLockFile(int(l.file.Fd()), []byte{0}, 0)
	if err == nil {
		err = syncLockFile(l.file)
	}
	if err != nil {
		l.lost = true
		return fmt.Errorf("%w: %w", ErrLost, err)
	}
	return nil
}

// syncLockFile makes the Check's byte reach the server, through fsync(2). On
// darwin File.Sync is F_FULLFSYNC, which also flushes the drive's cache and
// costs milliseconds; fsync(2) already reaches an NFS server. Elsewhere File.Sync
// is fsync(2). The fsync's test seam is locktest.Sync.
func syncLockFile(f *os.File) error {
	if locktest.Sync != nil {
		return locktest.Sync(f)
	}
	if runtime.GOOS != "darwin" {
		return f.Sync()
	}
	err := unix.Fsync(int(f.Fd()))
	for errors.Is(err, unix.EINTR) {
		err = unix.Fsync(int(f.Fd()))
	}
	if err != nil {
		return &os.PathError{Op: "fsync", Path: f.Name(), Err: err}
	}
	return nil
}

// MarkRemove makes Release delete the lock file, at the session's removal.
func (l *Lock) MarkRemove() { l.remove = true }

// Lost reports whether a Check failed.
func (l *Lock) Lost() bool { return l.lost }

// Release drops the lock; closing the fd drops the flock. A lock marked for
// removal unlinks its path first, while held: unlinking after the release
// would let a waiter lock the old inode while a newcomer locks a new one. It
// unlinks only after a Check: a lost lock removes nothing, since another
// process may own the path by now, and a third would lock a new file beside it.
// Only the first call acts: by a second one, the path may be the next
// owner's lock.
func (l *Lock) Release() error {
	if l.released {
		return nil
	}
	l.released = true
	var err error
	if l.remove && l.Check() == nil {
		err = l.removeHeldPath()
	}
	return errors.Join(err, l.file.Close())
}

// removeHeldPath unlinks the lock path only while it names the held inode.
func (l *Lock) removeHeldPath() error {
	same, err := samePath(l.file, l.path)
	if err != nil || !same {
		return err
	}
	return os.Remove(l.path)
}
