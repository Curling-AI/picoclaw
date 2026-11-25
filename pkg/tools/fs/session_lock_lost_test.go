//go:build unix

package fstools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/sipeed/picoclaw/pkg/internal/locktest"
	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
)

// failLockChecks makes every session lock's Check fail as after a lost NFS
// lease, through the fsync seam the store's lost-lease tests use. It returns
// what sessions/ held at each Check: the contents of its temp files.
func failLockChecks(t *testing.T) *[][]string {
	t.Helper()
	var checks [][]string
	locktest.Sync = func(lockFile *os.File) error {
		sessions := filepath.Dir(filepath.Dir(lockFile.Name()))
		temps, _ := filepath.Glob(filepath.Join(sessions, ".tmp-*"))
		contents := make([]string, len(temps))
		for i, temp := range temps {
			got, _ := os.ReadFile(temp)
			contents[i] = string(got)
		}
		checks = append(checks, contents)
		return syscall.EIO
	}
	t.Cleanup(func() { locktest.Sync = nil })
	return &checks
}

// requireCheckedRightBeforeTheRename fails unless every write ran one Check,
// with its temp file in sessions/ already holding the new content: the Check
// comes after the write is on disk and right before the rename publishes it.
func requireCheckedRightBeforeTheRename(t *testing.T, checks [][]string, what string, want ...string) {
	t.Helper()
	if len(checks) != len(want) {
		t.Fatalf("%s: %d Checks, want %d", what, len(checks), len(want))
	}
	for i, temps := range checks {
		if len(temps) != 1 || temps[0] != want[i] {
			t.Fatalf("%s: temp files at Check %d = %q, want one holding %q", what, i+1, temps, want[i])
		}
	}
}

// requireUnpublished fails unless path still holds want, byte for byte, and
// sessions holds nothing but path and the .locks folder: no temp file is left.
func requireUnpublished(t *testing.T, sessions, path, want, what string) {
	t.Helper()
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("%s: %s = %q, %v; want %q", what, filepath.Base(path), got, err, want)
	}
	entries, err := os.ReadDir(sessions)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != filepath.Base(path) && name != sessionlock.Dir {
			t.Fatalf("%s: %s left in sessions/", what, name)
		}
	}
}

// After a lost lease another process may hold the session's lock and have
// written the file since the tool read it: every file tool checks the lock
// right before it publishes, and on a lost lock leaves the file and no temp.
func TestFileToolsDoNotPublishWithALostSessionLock(t *testing.T) {
	checks := failLockChecks(t)
	written := map[string]string{"write": "changed", "edit": "changed", "append": "one changed"}
	for _, restricted := range []bool{false, true} {
		for _, op := range []string{"write", "edit", "append"} {
			*checks = nil
			workspace, sessions := sessionsFixture(t)
			path := filepath.Join(sessions, "agent_main_direct_x.jsonl")
			if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
				t.Fatal(err)
			}
			what := fmt.Sprintf("%s restricted=%v", op, restricted)
			res := runFileTool(workspace, restricted, op, path)
			if !res.IsError || !strings.Contains(res.ForLLM, sessionlock.ErrLost.Error()) {
				t.Fatalf("%s: result = %+v, want the lost lock", what, res)
			}
			requireCheckedRightBeforeTheRename(t, *checks, what, written[op])
			requireUnpublished(t, sessions, path, "one", what)
		}
	}
}

// The error is the store's own sentinel for a lost lock, not a busy session.
func TestEditAndAppendReturnTheLostSessionLock(t *testing.T) {
	checks := failLockChecks(t)
	for _, restricted := range []bool{false, true} {
		*checks = nil
		workspace, sessions := sessionsFixture(t)
		path := filepath.Join(sessions, "agent_main_direct_x.meta.json")
		if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
			t.Fatal(err)
		}
		sysFs := buildFs(workspace, restricted, nil)
		if _, _, err := editFile(sysFs, path, "one", "changed"); !errors.Is(err, sessionlock.ErrLost) ||
			errors.Is(err, sessionlock.ErrBusy) {
			t.Fatalf("restricted=%v: edit = %v, want ErrLost", restricted, err)
		}
		if err := appendFile(sysFs, path, " changed"); !errors.Is(err, sessionlock.ErrLost) {
			t.Fatalf("restricted=%v: append = %v, want ErrLost", restricted, err)
		}
		what := fmt.Sprintf("edit and append restricted=%v", restricted)
		requireCheckedRightBeforeTheRename(t, *checks, what, "changed", "one changed")
		requireUnpublished(t, sessions, path, "one", what)
	}
}

// A path outside sessions/ takes no session lock, so no Check stops it.
func TestFileToolsOutsideSessionsPublishWithoutACheck(t *testing.T) {
	checks := failLockChecks(t)
	for _, restricted := range []bool{false, true} {
		workspace, _ := sessionsFixture(t)
		path := filepath.Join(workspace, "notes.md")
		if res := runFileTool(workspace, restricted, "write", path); res.IsError {
			t.Fatalf("restricted=%v: %s", restricted, res.ForLLM)
		}
		if got, _ := os.ReadFile(path); string(got) != "changed" {
			t.Fatalf("restricted=%v: notes.md = %q, want changed", restricted, got)
		}
	}
	if len(*checks) != 0 {
		t.Fatalf("%d Checks outside sessions/, want none", len(*checks))
	}
}
