//go:build unix

package memory

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openContentArtifact(dir, name string, create bool) (*os.File, *os.File, error) {
	return openContentArtifactMode(dir, name, create, false)
}

func openContentArtifactWritable(dir, name string) (*os.File, *os.File, error) {
	return openContentArtifactMode(dir, name, false, true)
}

func openContentArtifactMode(dir, name string, create, writable bool) (*os.File, *os.File, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, nil, ErrContentIdentityUnavailable
	}
	parentFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open content directory", Path: dir, Err: err}
	}
	parent := os.NewFile(uintptr(parentFD), dir)
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if writable {
		flags = unix.O_RDWR | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	}
	if create {
		flags = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC
	}
	fd, err := unix.Openat(parentFD, name, flags, 0o600)
	if err != nil {
		parent.Close()
		return nil, nil, &os.PathError{Op: "open content artifact", Path: name, Err: err}
	}
	file := os.NewFile(uintptr(fd), filepath.Join(dir, name))
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err == nil && (stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1) {
		err = ErrContentIdentityUnavailable
	}
	if err != nil {
		return nil, nil, errors.Join(err, file.Close(), parent.Close())
	}
	return file, parent, nil
}

func removeContentArtifact(dir, name string, beforeSync ...func() error) error {
	file, parent, err := openContentArtifact(dir, name, false)
	if err != nil {
		return err
	}
	defer closeContentArtifact(file, parent)
	// Recheck the path against the opened inode immediately before unlink.
	// Source ownership excludes a second writer throughout this operation.
	var opened, current unix.Stat_t
	if err = unix.Fstat(int(file.Fd()), &opened); err != nil {
		return err
	}
	if err = unix.Fstatat(int(parent.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if opened.Dev != current.Dev || opened.Ino != current.Ino || current.Nlink != 1 ||
		current.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrContentIdentityUnavailable
	}
	if err = unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
		return err
	}
	for _, check := range beforeSync {
		if err = check(); err != nil {
			return err
		}
	}
	return parent.Sync()
}

func replaceContentArtifact(parent *os.File, temporary, target string) error {
	var current unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), target, &current, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err == nil && (current.Mode&unix.S_IFMT != unix.S_IFREG || current.Nlink != 1) {
		return ErrContentIdentityUnavailable
	}
	if err = unix.Renameat(int(parent.Fd()), temporary, int(parent.Fd()), target); err != nil {
		return err
	}
	return parent.Sync()
}
