package fileutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRootedCASFallsBackForUnsupportedAtomicOperations(t *testing.T) {
	for _, operation := range []string{"chmod", "rename"} {
		for _, unsupported := range []error{syscall.EPERM, syscall.EINVAL, syscall.ENOSYS, syscall.EOPNOTSUPP} {
			t.Run(operation+"/"+unsupported.Error(), func(t *testing.T) {
				workspace := t.TempDir()
				root, rootErr := os.OpenRoot(workspace)
				if rootErr != nil {
					t.Fatal(rootErr)
				}
				defer root.Close()
				if err := root.WriteFile("MEMORY.md", []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				ops := rootAtomicOps{chmod: (*os.File).Chmod, rename: root.Rename, remove: root.Remove}
				if operation == "chmod" {
					ops.chmod = func(*os.File, os.FileMode) error { return unsupported }
				} else {
					ops.rename = func(string, string) error { return unsupported }
				}
				err := WithFileLock(filepath.Join(workspace, "MEMORY.md"), func() error {
					return writeFileAtomicInRootWith(root, "MEMORY.md", []byte("new"), 0o600, ops)
				})
				if err != nil {
					t.Fatal(err)
				}
				if got, err := root.ReadFile("MEMORY.md"); err != nil || string(got) != "new" {
					t.Fatalf("fallback bytes = %q, error = %v", got, err)
				}
				if info, err := root.Stat("MEMORY.md"); err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("fallback changed private mode: %v", err)
				}
				if err := WriteFileIfContentInRoot(
					root,
					"MEMORY.md",
					[]byte("stale"),
					FileContent{Content: []byte("old")},
					0o600,
				); !errors.Is(
					err,
					ErrFileChanged,
				) {
					t.Fatalf("stale preimage after fallback was accepted: %v", err)
				}
				assertNoContentTemps(t, workspace)
			})
		}
	}
}

func TestRootedCASReportsCommittedFallbackWhenTempCleanupFails(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.WriteFile("MEMORY.md", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops := rootAtomicOps{
		chmod:  (*os.File).Chmod,
		rename: func(string, string) error { return syscall.EPERM },
		remove: func(string) error { return syscall.EPERM },
	}
	writeErr := writeFileAtomicInRootWith(root, "MEMORY.md", []byte("new"), 0o600, ops)
	if got, err := root.ReadFile("MEMORY.md"); err != nil || string(got) != "new" {
		t.Fatalf("fallback did not commit: bytes = %q, error = %v", got, err)
	}
	if writeErr != nil {
		t.Fatalf("committed fallback reported a write failure: %v", writeErr)
	}
}

func TestRootedCASDoesNotFallbackOnPermissionOrIOFailure(t *testing.T) {
	for _, operation := range []string{"chmod", "rename"} {
		for _, cause := range []error{syscall.EACCES, syscall.EIO, syscall.ENOSPC} {
			t.Run(operation+"/"+cause.Error(), func(t *testing.T) {
				workspace := t.TempDir()
				root, err := os.OpenRoot(workspace)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				if err := root.WriteFile("MEMORY.md", []byte("old"), 0o600); err != nil {
					t.Fatal(err)
				}
				ops := rootAtomicOps{chmod: (*os.File).Chmod, rename: root.Rename, remove: root.Remove}
				if operation == "chmod" {
					ops.chmod = func(*os.File, os.FileMode) error { return cause }
				} else {
					ops.rename = func(string, string) error { return cause }
				}
				if err := writeFileAtomicInRootWith(
					root,
					"MEMORY.md",
					[]byte("new"),
					0o600,
					ops,
				); !errors.Is(
					err,
					cause,
				) {
					t.Fatalf("original operation failure was masked: %v", err)
				}
				if got, err := root.ReadFile("MEMORY.md"); err != nil || string(got) != "old" {
					t.Fatalf("failed operation overwrote bytes: %q, %v", got, err)
				}
				assertNoContentTemps(t, workspace)
			})
		}
	}
}

func TestRootedCASFallbackRejectsAReplacedFinalSymlink(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "outside.md")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ops := rootAtomicOps{
		chmod:  (*os.File).Chmod,
		remove: root.Remove,
		rename: func(string, string) error {
			if err := os.Symlink(target, filepath.Join(workspace, "MEMORY.md")); err != nil {
				t.Fatal(err)
			}
			return syscall.ENOSYS
		},
	}
	if err := writeFileAtomicInRootWith(
		root,
		"MEMORY.md",
		[]byte("unexpected"),
		0o600,
		ops,
	); !errors.Is(
		err,
		ErrFileChanged,
	) {
		t.Fatalf("replacement symlink was accepted: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "untouched" {
		t.Fatalf("fallback wrote outside its root: %q, %v", got, err)
	}
	assertNoContentTemps(t, workspace)
}

func TestRootedCASFallbackKeepsPinnedParentAfterItsNameChanges(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	parentPath, moved := filepath.Join(workspace, "memory"), filepath.Join(workspace, "moved")
	if err := os.Mkdir(parentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(parentPath, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parentPath); err != nil {
		t.Skipf("directory aliases unavailable: %v", err)
	}
	ops := rootAtomicOps{
		chmod:  (*os.File).Chmod,
		rename: func(string, string) error { return syscall.EPERM },
		remove: root.Remove,
	}
	if err := writeFileAtomicInRootWith(root, "MEMORY.md", []byte("inside"), 0o600, ops); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "MEMORY.md")); err != nil || string(got) != "inside" {
		t.Fatalf("fallback lost the pinned parent: %q, %v", got, err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("fallback wrote outside the pinned parent: %v", err)
	}
	assertNoContentTemps(t, moved)
}
