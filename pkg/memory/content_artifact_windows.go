//go:build windows

package memory

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func openContentArtifact(dir, name string, create bool) (*os.File, *os.File, error) {
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

func removeContentArtifact(dir, name string) error {
	file, parent, err := openContentArtifact(dir, name, false)
	if err != nil {
		return err
	}
	if err = file.Close(); err == nil {
		err = os.Remove(filepath.Join(dir, name))
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
