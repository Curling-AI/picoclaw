//go:build windows

package memory

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
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
	parent, err := os.Open(dir)
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, name)
	name16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		parent.Close()
		return nil, nil, err
	}
	access, disposition := uint32(windows.GENERIC_READ), uint32(windows.OPEN_EXISTING)
	if writable {
		access = windows.GENERIC_READ | windows.GENERIC_WRITE
	}
	if create {
		access, disposition = windows.GENERIC_WRITE, windows.CREATE_NEW
	}
	handle, err := windows.CreateFile(name16, access, windows.FILE_SHARE_READ, nil, disposition,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		parent.Close()
		return nil, nil, &os.PathError{Op: "open content artifact", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(handle, &info); err == nil &&
		(info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
			info.NumberOfLinks != 1) {
		err = ErrContentIdentityUnavailable
	}
	if err != nil {
		return nil, nil, errors.Join(err, file.Close(), parent.Close())
	}
	return file, parent, nil
}

func removeContentArtifact(dir, name string, beforeSync ...func() error) error {
	return removeContentArtifactIdentity(dir, name, nil, beforeSync...)
}

func contentArtifactFileIdentity(file *os.File) (contentFileIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return contentFileIdentity{}, err
	}
	return contentFileIdentity{
		Volume: uint64(info.VolumeSerialNumber),
		File:   uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

func removeContentArtifactIdentity(dir, name string, expected *contentFileIdentity, beforeSync ...func() error) error {
	file, parent, err := openContentArtifact(dir, name, false)
	if err != nil {
		return err
	}
	if expected != nil {
		actual, identityErr := contentArtifactFileIdentity(file)
		if identityErr != nil || actual != *expected {
			return errors.Join(ErrContentIdentityUnavailable, identityErr, file.Close(), parent.Close())
		}
	}
	if err = file.Close(); err == nil {
		err = os.Remove(filepath.Join(dir, name))
	}
	if err == nil {
		for _, check := range beforeSync {
			if err = check(); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = parent.Sync()
	}
	return errors.Join(err, parent.Close())
}

func replaceContentArtifact(parent *os.File, temporary, target string) error {
	file, dir, err := openContentArtifact(parent.Name(), target, false)
	if err == nil {
		err = closeContentArtifact(file, dir)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return err
	}
	if err = os.Rename(filepath.Join(parent.Name(), temporary), filepath.Join(parent.Name(), target)); err != nil {
		return err
	}
	return parent.Sync()
}
