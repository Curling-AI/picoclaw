package fileutil

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriteFileIfContent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		present  bool
		current  string
		expected FileContent
		conflict bool
	}{
		{name: "matching", present: true, current: "old\n", expected: FileContent{Content: []byte("old\n")}},
		{name: "raw bytes differ", present: true, current: "old\n", expected: FileContent{Content: []byte("old")}, conflict: true},
		{name: "create missing", expected: FileContent{Absent: true}},
		{name: "existing empty", present: true, expected: FileContent{}},
		{name: "empty is not absent", present: true, expected: FileContent{Absent: true}, conflict: true},
		{name: "absent is not empty", expected: FileContent{}, conflict: true},
		{name: "stale same size", present: true, current: "new", expected: FileContent{Content: []byte("old")}, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "memory", "MEMORY.md")
			if tc.present {
				if err := WriteFileAtomic(path, []byte(tc.current), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := WriteFileIfContent(path, []byte("replacement"), tc.expected, 0o600)
			if errors.Is(err, ErrFileChanged) != tc.conflict || (err != nil && !tc.conflict) {
				t.Fatalf("write result = %v, conflict = %v", err, tc.conflict)
			}
			got, readErr := os.ReadFile(path)
			if tc.conflict && !tc.present {
				if !errors.Is(readErr, os.ErrNotExist) {
					t.Fatalf("a rejected write created a file: %v", readErr)
				}
				return
			}
			want := "replacement"
			if tc.conflict {
				want = tc.current
			}
			if readErr != nil || string(got) != want {
				t.Fatalf("persisted content = %q, error = %v, want %q", got, readErr, want)
			}
		})
	}
}

func TestWriteFileIfContentRejectsSameSizeSameSecondMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := WriteFileAtomic(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	preimage, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileIfContent(
		path,
		[]byte("ui"),
		FileContent{Content: preimage},
		0o600,
	); !errors.Is(
		err,
		ErrFileChanged,
	) {
		t.Fatalf("same-size same-second edit was not detected: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("concurrent writer was overwritten: %q, %v", got, err)
	}
}

func TestWriteFileIfContentPreservesExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "MEMORY.md")
	if err := WriteFileAtomic(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileIfContent(path, []byte("new"), FileContent{Content: []byte("old")}, 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("existing private mode changed to %o", got)
	}
}

func TestWriteFileIfContentSharesLockThroughDirectoryAlias(t *testing.T) {
	dir := t.TempDir()
	resolved := filepath.Join(dir, "real")
	if err := os.MkdirAll(resolved, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(resolved, alias); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	path := filepath.Join(resolved, "memory", "MEMORY.md")
	aliasPath := filepath.Join(alias, "memory", "MEMORY.md")
	locked, release := make(chan struct{}), make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		writer <- WithFileLock(path, func() error {
			close(locked)
			<-release
			return WriteFileAtomic(path, []byte("agent"), 0o600)
		})
	}()
	<-locked
	compared := make(chan error, 1)
	go func() {
		compared <- WriteFileIfContent(aliasPath, []byte("ui"), FileContent{Absent: true}, 0o600)
	}()
	select {
	case err := <-compared:
		close(release)
		t.Fatalf("CAS bypassed the directory-alias writer lock: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	if err := <-compared; !errors.Is(err, ErrFileChanged) {
		t.Fatalf("CAS did not preserve the agent's new file: %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "agent" {
		t.Fatalf("agent bytes = %q, error = %v", got, err)
	}
}

func TestWriteFileIfContentRejectsFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	target, link := filepath.Join(dir, "target.md"), filepath.Join(dir, "MEMORY.md")
	if err := WriteFileAtomic(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("file symlinks unavailable: %v", err)
	}
	if err := WriteFileIfContent(
		link,
		[]byte("replacement"),
		FileContent{Content: []byte("original")},
		0o600,
	); !errors.Is(
		err,
		ErrFileChanged,
	) {
		t.Fatalf("final symlink was accepted: %v", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "original" {
		t.Fatalf("symlink target changed: %q, %v", got, err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("final symlink was replaced: %v", err)
	}
}

func TestWriteFileIfContentInRoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		present  bool
		current  string
		expected FileContent
		conflict bool
	}{
		{name: "matching private file", present: true, current: "old\n", expected: FileContent{Content: []byte("old\n")}},
		{name: "raw bytes differ", present: true, current: "old\n", expected: FileContent{Content: []byte("old")}, conflict: true},
		{name: "same size stale", present: true, current: "new", expected: FileContent{Content: []byte("old")}, conflict: true},
		{name: "create missing parent", expected: FileContent{Absent: true}},
		{name: "existing empty", present: true, expected: FileContent{}},
		{name: "empty is not absent", present: true, expected: FileContent{Absent: true}, conflict: true},
		{name: "absent is not empty", expected: FileContent{}, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			path := filepath.Join(workspace, "memory", "USER.md")
			if tc.present {
				if err := WriteFileAtomic(path, []byte(tc.current), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			err = WriteFileIfContentInRoot(root, "memory/USER.md", []byte("replacement"), tc.expected, 0o644)
			if errors.Is(err, ErrFileChanged) != tc.conflict || (err != nil && !tc.conflict) {
				t.Fatalf("root write result = %v, conflict = %v", err, tc.conflict)
			}
			if tc.conflict && !tc.present {
				if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("a rejected write created its parent directory: %v", err)
				}
				return
			}
			want := "replacement"
			if tc.conflict {
				want = tc.current
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != want {
				t.Fatalf("root persisted content = %q, error = %v, want %q", got, err, want)
			}
			if tc.present {
				if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("root write changed the existing private mode: %v", err)
				}
			}
			assertNoContentTemps(t, filepath.Dir(path))
		})
	}
}

func assertNoContentTemps(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatal("a content CAS temporary file was retained")
		}
	}
}

func TestWriteFileIfContentInRootAcceptsInternalDirectoryAlias(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "memory", "USER.md")
	if err := WriteFileAtomic(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("memory", filepath.Join(workspace, "alias")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := WriteFileIfContentInRoot(
		root,
		"alias/USER.md",
		[]byte("new"),
		FileContent{Content: []byte("old")},
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatalf("internal alias content = %q, error = %v", got, err)
	}
}

func TestWriteFileIfContentInRootRejectsEscapesAndFinalSymlink(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "escape")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := WriteFileAtomic(filepath.Join(workspace, "target.md"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.md", filepath.Join(workspace, "alias.md")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, path := range []string{"../escaped.md", filepath.Join(outside, "absolute.md"), "escape/new.md"} {
		if err := WriteFileIfContentInRoot(
			root,
			path,
			[]byte("unexpected"),
			FileContent{Absent: true},
			0o600,
		); err == nil {
			t.Fatal("an escaping root CAS was accepted")
		}
	}
	if err := WriteFileIfContentInRoot(
		root,
		"alias.md",
		[]byte("unexpected"),
		FileContent{Content: []byte("original")},
		0o600,
	); !errors.Is(
		err,
		ErrFileChanged,
	) {
		t.Fatalf("final symlink was accepted by root CAS: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("root CAS changed the outside directory: count=%d, error=%v", len(entries), err)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "target.md")); err != nil || string(got) != "original" {
		t.Fatalf("root CAS changed a final symlink target: %q, %v", got, err)
	}
}

func TestWriteFileIfContentInRootKeepsOpenedRootAfterNameReplacement(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	workspace, moved := filepath.Join(base, "workspace"), filepath.Join(base, "moved-workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Rename(workspace, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, workspace); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := WriteFileIfContentInRoot(
		root,
		"memory/USER.md",
		[]byte("inside"),
		FileContent{Absent: true},
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "memory", "USER.md")); err != nil || string(got) != "inside" {
		t.Fatalf("the opened root lost its namespace: %q, %v", got, err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("root.Name redirected an absent CAS outside: count=%d, error=%v", len(entries), err)
	}
}

func TestWriteFileAtomicInRootPinsParentAndCleansFailedRename(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	parentPath := filepath.Join(workspace, "memory")
	if err := os.Mkdir(parentPath, 0o700); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	moved := filepath.Join(workspace, "pinned-memory")
	if err := os.Rename(parentPath, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parentPath); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := writeFileAtomicInRoot(parent, "USER.md", []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(moved, "USER.md")); err != nil || string(got) != "inside" {
		t.Fatalf("parent replacement redirected the atomic write: %q, %v", got, err)
	}
	if err := parent.Mkdir("cannot-replace", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomicInRoot(parent, "cannot-replace", []byte("rejected"), 0o600); err == nil {
		t.Fatal("atomic rename over a directory was accepted")
	}
	assertNoContentTemps(t, moved)
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("pinned parent wrote outside: count=%d, error=%v", len(entries), err)
	}
}

func TestWriteFileIfContentInRootHonorsUmaskOnCreation(t *testing.T) {
	if os.Getenv("PICOCLAW_CAS_UMASK_CHILD") != "1" {
		switch runtime.GOOS {
		case "windows", "plan9", "js", "wasip1":
			t.Skip("umask applies to Unix file creation")
		}
		shell, err := exec.LookPath("sh")
		if err != nil {
			t.Skip("a Unix shell is required for the isolated umask test")
		}
		binary, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		child := exec.Command(
			shell,
			"-c",
			`umask 077; exec "$@"`,
			"cas-umask-test",
			binary,
			"-test.run=^TestWriteFileIfContentInRootHonorsUmaskOnCreation$",
		)
		child.Env = []string{"PICOCLAW_CAS_UMASK_CHILD=1"}
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("isolated umask test failed: %v\n%s", err, output)
		}
		return
	}
	for _, present := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "create requested 0644 becomes 0600", true: "existing 0644 remains exact"}[present],
			func(t *testing.T) {
				workspace := t.TempDir()
				path := filepath.Join(workspace, "USER.md")
				expected := FileContent{Absent: true}
				wantMode := os.FileMode(0o600)
				if present {
					if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(path, 0o644); err != nil {
						t.Fatal(err)
					}
					expected, wantMode = FileContent{Content: []byte("old")}, 0o644
				}
				root, err := os.OpenRoot(workspace)
				if err != nil {
					t.Fatal(err)
				}
				defer root.Close()
				if writeErr := WriteFileIfContentInRoot(
					root,
					"USER.md",
					[]byte("new"),
					expected,
					0o644,
				); writeErr != nil {
					t.Fatal(writeErr)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if got := info.Mode().Perm(); got != wantMode {
					t.Fatalf("CAS under umask 077 produced mode %04o, want %04o", got, wantMode)
				}
			},
		)
	}
}
