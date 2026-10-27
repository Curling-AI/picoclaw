package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func seedSession(t *testing.T, store *JSONLStore, key string) {
	t.Helper()
	ctx := context.Background()
	if err := store.AddMessage(ctx, key, "user", "hello"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := store.AddMessage(ctx, key, "assistant", "hi"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := store.SetSummary(ctx, key, "greetings"); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	scope := json.RawMessage(`{"version":1,"channel":"telegram","values":{"chat":"direct:42"}}`)
	if err := store.UpsertSessionMeta(ctx, key, scope, nil); err != nil {
		t.Fatalf("UpsertSessionMeta: %v", err)
	}
}

func TestDeleteSession_RemovesTheSessionFromDiskAndListings(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedSession(t, store, "agent:main:web-1")
	seedSession(t, store, "agent:main:web-2")

	if err := store.DeleteSession(ctx, "agent:main:web-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	for _, path := range []string{store.metaPath("agent:main:web-1"), store.jsonlPath("agent:main:web-1")} {
		if fileExists(path) {
			t.Errorf("%s survived the delete", filepath.Base(path))
		}
	}
	if keys := store.ListSessions(); !slices.Equal(keys, []string{"agent:main:web-2"}) {
		t.Errorf("ListSessions() = %v, want only the other session", keys)
	}
	for _, meta := range store.ListSessionMetas() {
		if meta.Key == "agent:main:web-1" {
			t.Error("ListSessionMetas() still lists the deleted session")
		}
	}
	history, err := store.GetHistory(ctx, "agent:main:web-1")
	if err != nil || len(history) != 0 {
		t.Errorf("GetHistory() = %v, %v; want an empty history", history, err)
	}
	if summary, _ := store.GetSummary(ctx, "agent:main:web-1"); summary != "" {
		t.Errorf("GetSummary() = %q, want empty", summary)
	}
	meta, err := store.GetSessionMeta(ctx, "agent:main:web-1")
	if err != nil || len(meta.Scope) != 0 {
		t.Errorf("GetSessionMeta() = %+v, %v; want no scope left", meta, err)
	}
}

func TestDeleteSession_LeavesOtherSessionsAlone(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedSession(t, store, "agent:main:web-1")
	seedSession(t, store, "agent:main:web-2")

	if err := store.DeleteSession(ctx, "agent:main:web-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	history, err := store.GetHistory(ctx, "agent:main:web-2")
	if err != nil || len(history) != 2 {
		t.Fatalf("GetHistory() = %d messages, %v; want the 2 it had", len(history), err)
	}
	if summary, _ := store.GetSummary(ctx, "agent:main:web-2"); summary != "greetings" {
		t.Errorf("GetSummary() = %q, want the summary it had", summary)
	}
}

// The meta cache must not outlive the files: a message written to the same key
// afterwards would otherwise inherit the deleted session's count and summary.
func TestDeleteSession_TheKeyStartsFreshAfterwards(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedSession(t, store, "agent:main:web-1")

	if err := store.DeleteSession(ctx, "agent:main:web-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if err := store.AddMessage(ctx, "agent:main:web-1", "user", "again"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	meta, err := store.GetSessionMeta(ctx, "agent:main:web-1")
	if err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	if meta.Count != 1 || meta.Summary != "" {
		t.Errorf("meta = count %d, summary %q; want a fresh session (1, \"\")", meta.Count, meta.Summary)
	}
	history, _ := store.GetHistory(ctx, "agent:main:web-1")
	if len(history) != 1 || history[0].Content != "again" {
		t.Errorf("history = %v, want only the new message", history)
	}
}

func TestDeleteSession_UnknownKeyIsANoop(t *testing.T) {
	store := newTestStore(t)

	if err := store.DeleteSession(context.Background(), "agent:main:never-existed"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	entries, err := os.ReadDir(store.dir)
	if err != nil || len(entries) != 0 {
		t.Errorf("directory = %v, %v; want it left empty", entries, err)
	}
}

func TestDeleteSession_RejectsABlankKey(t *testing.T) {
	store := newTestStore(t)
	seedSession(t, store, "agent:main:web-1")

	for _, key := range []string{"", "  "} {
		if err := store.DeleteSession(context.Background(), key); err == nil {
			t.Errorf("DeleteSession(%q) = nil, want an error", key)
		}
	}
	if len(store.ListSessions()) != 1 {
		t.Error("a blank key deleted something")
	}
}

// Delete takes the session's lock. Without it the delete lands between an
// append and the meta update that follows, and the transcript and its meta stop
// agreeing on how many messages the session has.
func TestDeleteSession_ConcurrentAppendsKeepTranscriptAndMetaInStep(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const key = "agent:main:web-1"

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if err := store.AddMessage(ctx, key, "user", "x"); err != nil {
					t.Errorf("AddMessage: %v", err)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 10 {
			if err := store.DeleteSession(ctx, key); err != nil {
				t.Errorf("DeleteSession: %v", err)
			}
		}
	}()
	wg.Wait()

	meta, err := store.GetSessionMeta(ctx, key)
	if err != nil {
		t.Fatalf("GetSessionMeta: %v", err)
	}
	raw, err := os.ReadFile(store.jsonlPath(key))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read transcript: %v", err)
	}
	lines := strings.Count(string(raw), "\n")
	if meta.Count != lines {
		t.Errorf("meta counts %d messages, the transcript has %d", meta.Count, lines)
	}
}

// A delete that removed the meta and then failed on the transcript must not
// leave the meta in the cache: the next write would bring back its count,
// summary and scope.
func TestDeleteSession_AFailedDeleteLeavesNoMetaInTheCache(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedSession(t, store, "agent:main:web-1")
	// A directory with something in it cannot be removed like a file.
	transcript := store.jsonlPath("agent:main:web-1")
	if err := os.Remove(transcript); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(transcript, "stuck"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteSession(ctx, "agent:main:web-1"); err == nil {
		t.Fatal("DeleteSession reported success with the transcript still there")
	}

	if _, cached := store.cachedMeta(sanitizeKey("agent:main:web-1")); cached {
		t.Error("the deleted meta stayed in the cache")
	}
}

// ListSessionMetas fills the cache without the session's lock. A meta it read
// before a delete must not come back after it, and one it read before a write
// must not replace what the write cached.
func TestListSessionMetas_AFillNeverUndoesADeleteOrAWrite(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedSession(t, store, "agent:main:web-1")
	sanitized := sanitizeKey("agent:main:web-1")
	readBefore, _ := store.cachedMeta(sanitized)

	removals := store.metaRemovalCount()
	if err := store.DeleteSession(ctx, "agent:main:web-1"); err != nil {
		t.Fatal(err)
	}
	store.fillCachedMeta(sanitized, readBefore, removals)
	if _, cached := store.cachedMeta(sanitized); cached {
		t.Fatal("a meta read before the delete was cached after it")
	}

	seedSession(t, store, "agent:main:web-1")
	newer, _ := store.cachedMeta(sanitized)
	store.fillCachedMeta(sanitized, SessionMeta{Key: "agent:main:web-1"}, store.metaRemovalCount())
	if got, _ := store.cachedMeta(sanitized); got.Count != newer.Count || got.Summary != newer.Summary {
		t.Fatalf("a fill replaced the cached meta: %+v, want %+v", got, newer)
	}
}
