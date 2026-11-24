package fstools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
)

func sessionsFixture(t *testing.T) (workspace, sessions string) {
	t.Helper()
	workspace = t.TempDir()
	sessions = filepath.Join(workspace, "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, ".locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	return workspace, sessions
}

func runFileTool(workspace string, restricted bool, op, path string) *ToolResult {
	args := map[string]any{"path": path, "content": "changed", "overwrite": true}
	switch op {
	case "edit":
		args = map[string]any{"path": path, "old_text": "one", "new_text": "changed"}
	case "append":
		args = map[string]any{"path": path, "content": " changed"}
	}
	switch op {
	case "edit":
		return NewEditFileTool(workspace, restricted).Execute(context.Background(), args)
	case "append":
		return NewAppendFileTool(workspace, restricted).Execute(context.Background(), args)
	}
	return NewWriteFileTool(workspace, restricted).Execute(context.Background(), args)
}

func shortenSessionLockWait(t *testing.T) {
	t.Helper()
	old := sessionLockWait
	sessionLockWait = 50 * time.Millisecond
	t.Cleanup(func() { sessionLockWait = old })
}

func TestWriteFileWaitsForTheSessionLock(t *testing.T) {
	shortenSessionLockWait(t)
	for _, restricted := range []bool{false, true} {
		workspace, sessions := sessionsFixture(t)
		path := filepath.Join(sessions, "agent_main_direct_x.meta.json")
		if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
			t.Fatal(err)
		}
		lock, err := sessionlock.Acquire(sessions, "agent_main_direct_x", 0)
		if err != nil {
			t.Fatal(err)
		}
		// The tool waits sessionLockWait; the lock is held by this process's
		// other descriptor, so flock conflicts.
		res := runFileTool(workspace, restricted, "write", path)
		_ = lock.Release()
		if !res.IsError {
			t.Fatalf("restricted=%v: write under a held session lock succeeded", restricted)
		}
		if !strings.Contains(res.ForLLM, "being written by the assistant") {
			t.Fatalf("restricted=%v: unexpected error %q", restricted, res.ForLLM)
		}
		if got, _ := os.ReadFile(path); string(got) != "one" {
			t.Fatalf("restricted=%v: content = %q, want one", restricted, got)
		}
		if res := runFileTool(workspace, restricted, "write", path); res.IsError {
			t.Fatalf("restricted=%v: write after release failed: %s", restricted, res.ForLLM)
		}
	}
}

func TestFileToolsRefuseTheLocksFolder(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		for _, op := range []string{"write", "edit", "append"} {
			workspace, sessions := sessionsFixture(t)
			path := filepath.Join(sessions, ".locks", "x.lock")
			if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
				t.Fatal(err)
			}
			res := runFileTool(workspace, restricted, op, path)
			if !res.IsError || !strings.Contains(res.ForLLM, "managed by the session store") {
				t.Fatalf("%s restricted=%v: result = %+v", op, restricted, res)
			}
			if got, _ := os.ReadFile(path); string(got) != "one" {
				t.Fatalf("%s restricted=%v: lock file changed to %q", op, restricted, got)
			}
		}
	}
}

func TestFileToolsOutsideSessionsTakeNoSessionLock(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		workspace, sessions := sessionsFixture(t)
		lock, err := sessionlock.Acquire(sessions, "agent_main_direct_x", 0)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(workspace, "memory", "MEMORY.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		res := runFileTool(workspace, restricted, "write", path)
		_ = lock.Release()
		if res.IsError {
			t.Fatalf("restricted=%v: %s", restricted, res.ForLLM)
		}
	}
}

func TestFileToolsRefuseStoreInternalNames(t *testing.T) {
	for _, name := range []string{".pc-history-abc", ".tmp-abc", ".content-checkpoint-new.abc", ".locks/x.lock", ".LOCKS/x"} {
		for _, restricted := range []bool{false, true} {
			for _, op := range []string{"write", "edit", "append"} {
				workspace, sessions := sessionsFixture(t)
				path := filepath.Join(sessions, name)
				if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
					continue // e.g. .LOCKS/ on a case-sensitive filesystem
				}
				res := runFileTool(workspace, restricted, op, path)
				if !res.IsError || !strings.Contains(res.ForLLM, "managed by the session store") {
					t.Fatalf("%s %s restricted=%v: result = %+v", op, name, restricted, res)
				}
				if got, _ := os.ReadFile(path); string(got) != "one" {
					t.Fatalf("%s %s: content changed to %q", op, name, got)
				}
			}
		}
	}
}

func TestNormalSessionFileStillGoesThroughTheLock(t *testing.T) {
	workspace, sessions := sessionsFixture(t)
	path := filepath.Join(sessions, "agent_main_direct_y.json")
	if res := runFileTool(workspace, true, "write", path); res.IsError {
		t.Fatalf("write failed: %s", res.ForLLM)
	}
	if _, err := os.Stat(filepath.Join(sessions, ".locks", "agent_main_direct_y.lock")); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
}

func TestRelativePathsTakeTheSessionLockOrAreRefused(t *testing.T) {
	shortenSessionLockWait(t)
	workspace, sessions := sessionsFixture(t)
	t.Chdir(workspace)
	lock, err := sessionlock.Acquire(sessions, "agent_main_direct_z", 0)
	if err != nil {
		t.Fatal(err)
	}
	res := runFileTool(workspace, false, "write", "sessions/agent_main_direct_z.json")
	_ = lock.Release()
	if !res.IsError || !strings.Contains(res.ForLLM, "being written by the assistant") {
		t.Fatalf("relative write under a held lock = %+v", res)
	}
	res = runFileTool(workspace, false, "write", "sessions/.tmp-abc")
	if !res.IsError || !strings.Contains(res.ForLLM, "managed by the session store") {
		t.Fatalf("relative write to an internal name = %+v", res)
	}
}

func TestStoreInternalPrefixesIgnoreCase(t *testing.T) {
	for _, name := range []string{".PC-History-a", ".TMP-a", ".Content-Checkpoint-New.a"} {
		if !isStoreInternalName(name) {
			t.Fatalf("%s not refused", name)
		}
	}
}
