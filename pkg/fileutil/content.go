package fileutil

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ErrFileChanged means the current file no longer matches the captured content.
var ErrFileChanged = errors.New("file changed since it was read")

// FileContent distinguishes a missing file from an existing empty file.
type FileContent struct {
	Content []byte
	Absent  bool
}

var fileLocks = struct {
	sync.Mutex
	entries map[string]*fileLock
}{entries: make(map[string]*fileLock)}

type fileLock struct {
	sync.Mutex
	users int
}

// WithFileLock serializes cooperating in-process writers to the same directory
// entry. External scripts and processes do not participate in this lock.
func WithFileLock(path string, write func() error) error {
	return withFileLock(path, func(string) error { return write() })
}

func withFileLock(path string, write func(string) error) error {
	canonical, err := canonicalFilePath(path)
	if err != nil {
		return err
	}
	fileLocks.Lock()
	lock := fileLocks.entries[canonical]
	if lock == nil {
		lock = &fileLock{}
		fileLocks.entries[canonical] = lock
	}
	lock.users++
	fileLocks.Unlock()
	lock.Lock()
	defer func() {
		lock.Unlock()
		fileLocks.Lock()
		lock.users--
		if lock.users == 0 {
			delete(fileLocks.entries, canonical)
		}
		fileLocks.Unlock()
	}()
	return write(canonical)
}

// canonicalFilePath resolves parent aliases without following the final entry:
// atomic rename writes that entry, rather than the target of a final symlink.
func canonicalFilePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve file path: %w", err)
	}
	directory, suffix := filepath.Dir(absolute), filepath.Base(absolute)
	for {
		resolved, err := filepath.EvalSymlinks(directory)
		if err == nil {
			return filepath.Join(resolved, suffix), nil
		}
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(directory) == directory {
			return "", fmt.Errorf("resolve file directory: %w", err)
		}
		suffix = filepath.Join(filepath.Base(directory), suffix)
		directory = filepath.Dir(directory)
	}
}

// WriteFileIfContentInRoot keeps every data operation beneath root, even when a
// directory alias changes after the caller validated the workspace path.
// Creation honors umask; replacement preserves the existing permissions.
// Unsupported atomic operations use a serialized direct write, as WriteFileAtomic does.
func WriteFileIfContentInRoot(root *os.Root, path string, data []byte, expected FileContent, perm os.FileMode) error {
	relative := filepath.Clean(path)
	if root == nil || relative == "." || !filepath.IsLocal(relative) {
		return fmt.Errorf("root and local file path required: %w", fs.ErrInvalid)
	}
	return withFileLock(filepath.Join(root.Name(), relative), func(string) error {
		directory := filepath.ToSlash(filepath.Dir(relative))
		parent, err := root.OpenRoot(directory)
		if errors.Is(err, fs.ErrNotExist) {
			if !expected.Absent {
				return ErrFileChanged
			}
			if mkdirErr := root.MkdirAll(directory, 0o755); mkdirErr != nil {
				return fmt.Errorf("create file directory: %w", mkdirErr)
			}
			parent, err = root.OpenRoot(directory)
		}
		if err != nil {
			return fmt.Errorf("open file directory: %w", err)
		}
		defer parent.Close()

		name, mode := filepath.Base(relative), perm
		info, err := parent.Lstat(name)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if !expected.Absent {
				return ErrFileChanged
			}
		case err != nil:
			return fmt.Errorf("stat file: %w", err)
		case expected.Absent || !info.Mode().IsRegular():
			return ErrFileChanged
		default:
			mode = info.Mode().Perm()
			current, err := parent.ReadFile(name)
			if err != nil {
				return fmt.Errorf("read file: %w", err)
			}
			if !bytes.Equal(current, expected.Content) {
				return ErrFileChanged
			}
		}
		return writeFileAtomicInRoot(parent, name, data, mode)
	})
}

type rootAtomicOps struct {
	chmod  func(*os.File, os.FileMode) error
	rename func(string, string) error
	remove func(string) error
}

func writeFileAtomicInRoot(root *os.Root, name string, data []byte, perm os.FileMode) error {
	return writeFileAtomicInRootWith(root, name, data, perm, rootAtomicOps{
		chmod:  (*os.File).Chmod,
		rename: root.Rename,
		remove: root.Remove,
	})
}

func writeFileAtomicInRootWith(
	root *os.Root,
	name string,
	data []byte,
	perm os.FileMode,
	ops rootAtomicOps,
) (err error) {
	existing, err := root.Lstat(name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat atomic write target: %w", err)
	}
	temporary := fmt.Sprintf(".tmp-%d-%d", os.Getpid(), time.Now().UnixNano())
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	renamed := false
	defer func() {
		_ = file.Close()
		if !renamed {
			if cleanupErr := ops.remove(temporary); cleanupErr != nil && !errors.Is(cleanupErr, fs.ErrNotExist) {
				if err != nil {
					err = errors.Join(err, fmt.Errorf("remove temp file: %w", cleanupErr))
				} else {
					slog.Warn(
						"could not remove committed write's temporary file",
						"file", temporary, "error", cleanupErr,
					)
				}
			}
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if existing != nil {
		if err := ops.chmod(file, perm); err != nil {
			return writeRootAfterUnsupported(root, name, data, perm, fmt.Errorf("set temp file permissions: %w", err))
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := ops.rename(temporary, name); err != nil {
		return writeRootAfterUnsupported(root, name, data, perm, fmt.Errorf("rename temp file: %w", err))
	}
	renamed = true
	if directory, err := root.Open("."); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

// Mountpoint cannot chmod or rename ordinary S3 objects; the caller still holds the CAS writer lock.
func writeRootAfterUnsupported(root *os.Root, name string, data []byte, perm os.FileMode, atomicErr error) error {
	if !errors.Is(atomicErr, syscall.EPERM) && !errors.Is(atomicErr, syscall.EINVAL) &&
		!errors.Is(atomicErr, syscall.ENOSYS) && !errors.Is(atomicErr, syscall.EOPNOTSUPP) {
		return atomicErr
	}
	info, err := root.Lstat(name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(atomicErr, fmt.Errorf("stat direct write target: %w", err))
	}
	if info != nil && !info.Mode().IsRegular() {
		return errors.Join(atomicErr, ErrFileChanged)
	}
	if err := root.WriteFile(name, data, perm); err != nil {
		return errors.Join(atomicErr, fmt.Errorf("write file without rename: %w", err))
	}
	return nil
}
