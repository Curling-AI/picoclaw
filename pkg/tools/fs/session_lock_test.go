package fstools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory"
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

// emptyKeyLock writes the session of the empty key through the store and
// returns the one lock file name the store made for it.
func emptyKeyLock(t *testing.T, sessions string) string {
	t.Helper()
	store, err := memory.NewJSONLStore(sessions)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AddMessage(context.Background(), "", "user", "one"); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	locks, err := os.ReadDir(filepath.Join(sessions, sessionlock.Dir))
	if err != nil || len(locks) != 1 {
		t.Fatalf("lock files of the empty key's session = %v, %v; want one", locks, err)
	}
	return strings.TrimSuffix(locks[0].Name(), ".lock")
}

// The store keeps the session of the empty key in .jsonl and .meta.json and
// locks it under a name of its own: while the store holds that lock the file
// tools wait for it and fail busy, and with it free they write.
func TestAppendFileOnTheEmptyKeysFilesWaitsForTheStoresLock(t *testing.T) {
	shortenSessionLockWait(t)
	for _, restricted := range []bool{false, true} {
		workspace, sessions := sessionsFixture(t)
		lock, err := sessionlock.Acquire(sessions, emptyKeyLock(t, sessions), 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{".jsonl", ".meta.json"} {
			path := filepath.Join(sessions, name)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			res := runFileTool(workspace, restricted, "append", path)
			if !res.IsError || !strings.Contains(res.ForLLM, "being written by the assistant") {
				t.Fatalf("restricted=%v: append to %s under the store's lock = %+v", restricted, name, res)
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Fatalf("restricted=%v: %s changed under the store's lock", restricted, name)
			}
		}
		// A file of no session there takes no lock, the empty key's neither.
		if res := runFileTool(workspace, restricted, "append", filepath.Join(sessions, "notes.txt")); res.IsError {
			t.Fatalf("restricted=%v: append to notes.txt under the store's lock: %s", restricted, res.ForLLM)
		}
		if err := lock.Release(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(sessions, ".jsonl")
		if res := runFileTool(workspace, restricted, "append", path); res.IsError {
			t.Fatalf("restricted=%v: append with the lock free: %s", restricted, res.ForLLM)
		}
		if got, _ := os.ReadFile(path); !strings.HasSuffix(string(got), " changed") {
			t.Fatalf("restricted=%v: .jsonl = %q, want the append", restricted, got)
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
