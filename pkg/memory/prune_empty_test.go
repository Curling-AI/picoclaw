package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"
	"time"
)

const ghostIdle = 24 * time.Hour

// age backdates the files of a session, as if nothing had written to them for
// that long.
func age(t *testing.T, store *JSONLStore, key string, idle time.Duration) {
	t.Helper()
	stamp := time.Now().Add(-idle)
	for _, path := range []string{store.metaPath(key), store.jsonlPath(key), store.historyOrderWitnessPath(key)} {
		if err := os.Chtimes(path, stamp, stamp); err != nil && !os.IsNotExist(err) {
			t.Fatalf("chtimes %s: %v", path, err)
		}
	}
}

// seedGhost leaves what a deleted conversation left behind before sessions had
// a delete: the clear that the pod fell back to (history and summary emptied).
func seedGhost(t *testing.T, store *JSONLStore, key string) {
	t.Helper()
	ctx := context.Background()
	if err := store.AddMessage(ctx, key, "user", "hello"); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := store.SetSummary(ctx, key, "a summary"); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	if err := store.SetHistory(ctx, key, nil); err != nil {
		t.Fatalf("SetHistory: %v", err)
	}
	if err := store.SetSummary(ctx, key, ""); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	if err := store.Compact(ctx, key); err != nil {
		t.Fatalf("Compact: %v", err)
	}
}

func keepAll(SessionMeta) error { return nil }

// staleCacheOverDisk caches an empty meta for key and then puts body on disk
// behind the cache's back.
func staleCacheOverDisk(t *testing.T, store *JSONLStore, key, body string) {
	t.Helper()
	if err := store.UpsertSessionMeta(context.Background(), key, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.metaPath(key), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPruneEmptySessions_RemovesAbandonedGhosts(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	seedGhost(t, store, "agent:main:web-cleared")
	scope := json.RawMessage(`{"version":1,"channel":"web"}`)
	if err := store.UpsertSessionMeta(ctx, "agent:main:web-never-used", scope, nil); err != nil {
		t.Fatalf("UpsertSessionMeta: %v", err)
	}
	age(t, store, "agent:main:web-cleared", 2*ghostIdle)
	age(t, store, "agent:main:web-never-used", 2*ghostIdle)

	var seen []string
	removed, err := store.PruneEmptySessions(ghostIdle, time.Now(), func(meta SessionMeta) error {
		seen = append(seen, meta.Key)
		return nil
	})
	if err != nil {
		t.Fatalf("PruneEmptySessions: %v", err)
	}

	want := []string{"agent:main:web-cleared", "agent:main:web-never-used"}
	slices.Sort(removed)
	if !slices.Equal(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	slices.Sort(seen)
	if !slices.Equal(seen, want) {
		t.Errorf("before-remove hook saw %v, want %v", seen, want)
	}
	for _, key := range want {
		if fileExists(store.metaPath(key)) || fileExists(store.jsonlPath(key)) {
			t.Errorf("files of %s survived", key)
		}
		if _, cached := store.cachedMeta(sanitizeKey(key)); cached {
			t.Errorf("meta of %s stayed in the cache", key)
		}
	}
	if keys := store.ListSessions(); len(keys) != 0 {
		t.Errorf("ListSessions() = %v, want none", keys)
	}
}

// The sweep deletes files in production storage, so everything that is not
// plainly an abandoned empty session has to stay.
func TestPruneEmptySessions_NeverRemovesWhatMayHoldSomething(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, store *JSONLStore, key string)
		idle  time.Duration
		// witnessFresh keeps the order witness's own, recent write time.
		witnessFresh bool
	}{
		{
			name: "a message",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				if err := store.AddMessage(ctx, key, "user", "still here"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a summary and no message",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				if err := store.SetSummary(ctx, key, "what was said"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a transcript the meta does not count",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				if err := store.UpsertSessionMeta(ctx, key, nil, nil); err != nil {
					t.Fatal(err)
				}
				line := `{"role":"user","content":"written before the meta caught up"}` + "\n"
				if err := os.WriteFile(store.jsonlPath(key), []byte(line), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "every message truncated away",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				if err := store.AddMessage(ctx, key, "user", "old"); err != nil {
					t.Fatal(err)
				}
				if err := store.TruncateHistory(ctx, key, 0); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a meta that still skips lines of the transcript",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				body := []byte(`{"key":"agent:main:web-1","count":0,"skip":3}`)
				if err := os.WriteFile(store.metaPath(key), body, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "an empty session written to a moment ago",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				if err := store.UpsertSessionMeta(ctx, key, nil, nil); err != nil {
					t.Fatal(err)
				}
			},
			idle: time.Hour,
		},
		{
			name: "an empty session from the future",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				if err := store.UpsertSessionMeta(ctx, key, nil, nil); err != nil {
					t.Fatal(err)
				}
			},
			idle: -time.Hour,
		},
		// The listing comes from the meta cache; these sessions look empty there
		// and are not on disk, which the sweep reads again before removing.
		{
			name: "a message on disk the cache has not seen",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				staleCacheOverDisk(t, store, key, `{"key":"agent:main:web-1","count":1}`)
			},
		},
		{
			name: "a summary on disk the cache has not seen",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				staleCacheOverDisk(t, store, key, `{"key":"agent:main:web-1","summary":"what was said"}`)
			},
		},
		{
			name: "a meta on disk that is no longer this session's",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				staleCacheOverDisk(t, store, key, `{"key":"agent:main:somebody-else"}`)
			},
		},
		{
			name: "a history write still to recover",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				if err := store.UpsertSessionMeta(ctx, key, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store.historyRecoveryPath(key), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				stamp := time.Now().Add(-30 * ghostIdle)
				if err := os.Chtimes(store.historyRecoveryPath(key), stamp, stamp); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "an order witness written a moment ago",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				if err := store.UpsertSessionMeta(ctx, key, nil, nil); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(store.historyOrderWitnessPath(key), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			witnessFresh: true,
		},
		{
			name: "a meta that cannot be read",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				if err := os.WriteFile(store.metaPath(key), []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a meta whose key is another session's",
			setup: func(t *testing.T, store *JSONLStore, key string) {
				body := []byte(`{"key":"agent:main:somebody-else","count":0}`)
				if err := os.WriteFile(store.metaPath(key), body, 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			const key = "agent:main:web-1"
			tc.setup(t, store, key)
			idle := tc.idle
			if idle == 0 {
				idle = 30 * ghostIdle
			}
			age(t, store, key, idle)
			if tc.witnessFresh {
				now := time.Now()
				if err := os.Chtimes(store.historyOrderWitnessPath(key), now, now); err != nil {
					t.Fatal(err)
				}
			}

			removed, err := store.PruneEmptySessions(ghostIdle, time.Now(), func(meta SessionMeta) error {
				t.Errorf("before-remove hook called for %q", meta.Key)
				return nil
			})
			if err != nil {
				t.Fatalf("PruneEmptySessions: %v", err)
			}
			if len(removed) != 0 {
				t.Errorf("removed = %v, want nothing", removed)
			}
			if !fileExists(store.metaPath(key)) {
				t.Error("the meta file was removed")
			}
		})
	}
}

func TestPruneEmptySessions_NeedsAnIdleWindow(t *testing.T) {
	for _, idle := range []time.Duration{0, -time.Hour} {
		store := newTestStore(t)
		if err := store.UpsertSessionMeta(context.Background(), "agent:main:web-1", nil, nil); err != nil {
			t.Fatal(err)
		}
		age(t, store, "agent:main:web-1", 30*ghostIdle)

		removed, err := store.PruneEmptySessions(idle, time.Now(), keepAll)
		if err != nil || len(removed) != 0 {
			t.Errorf("PruneEmptySessions(%v) = %v, %v; want it disabled", idle, removed, err)
		}
		if !fileExists(store.metaPath("agent:main:web-1")) {
			t.Errorf("PruneEmptySessions(%v) removed a session", idle)
		}
	}
}

// What the hook owns is removed first: when it fails the session stays, so the
// next sweep finds it again instead of leaving the hook's data without a session.
func TestPruneEmptySessions_KeepsTheSessionWhenTheHookFails(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, key := range []string{"agent:main:web-a", "agent:main:web-b"} {
		if err := store.UpsertSessionMeta(ctx, key, nil, nil); err != nil {
			t.Fatal(err)
		}
		age(t, store, key, 2*ghostIdle)
	}
	boom := errors.New("index unavailable")

	removed, err := store.PruneEmptySessions(ghostIdle, time.Now(), func(meta SessionMeta) error {
		if meta.Key == "agent:main:web-a" {
			return boom
		}
		return nil
	})

	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to carry the hook's", err)
	}
	if !slices.Equal(removed, []string{"agent:main:web-b"}) {
		t.Errorf("removed = %v, want only the one whose hook succeeded", removed)
	}
	if !fileExists(store.metaPath("agent:main:web-a")) {
		t.Error("the session whose hook failed was removed")
	}
}

// The caller may keep a session the store would remove, for what it holds
// about the session itself; that is a decision, not a failure.
func TestPruneEmptySessions_TheHookCanKeepASession(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for _, key := range []string{"agent:main:web-a", "agent:main:web-b"} {
		if err := store.UpsertSessionMeta(ctx, key, nil, nil); err != nil {
			t.Fatal(err)
		}
		age(t, store, key, 2*ghostIdle)
	}

	removed, err := store.PruneEmptySessions(ghostIdle, time.Now(), func(meta SessionMeta) error {
		if meta.Key == "agent:main:web-a" {
			return ErrKeepSession
		}
		return nil
	})
	if err != nil {
		t.Errorf("error = %v, want none for a kept session", err)
	}
	if !slices.Equal(removed, []string{"agent:main:web-b"}) {
		t.Errorf("removed = %v, want only the session the hook let go", removed)
	}
	if !fileExists(store.metaPath("agent:main:web-a")) {
		t.Error("the session the hook kept was removed")
	}
}

// The hook runs under the session's lock, which is what holds writes off
// during the removal: the store does not answer for that session until the hook
// returns, so the hook gets the meta it needs instead of asking for it.
func TestPruneEmptySessions_TheHookRunsUnderTheSessionLockWithItsMeta(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	const key = "agent:main:whatsapp:direct:42"
	scope := json.RawMessage(`{"version":1,"channel":"whatsapp"}`)
	if err := store.UpsertSessionMeta(ctx, key, scope, nil); err != nil {
		t.Fatal(err)
	}
	age(t, store, key, 2*ghostIdle)

	var given SessionMeta
	_, err := store.PruneEmptySessions(ghostIdle, time.Now(), func(meta SessionMeta) error {
		given = meta
		read := make(chan struct{})
		go func() {
			_, _ = store.GetSessionMeta(ctx, key)
			close(read)
		}()
		select {
		case <-read:
			t.Error("the store answered for the session while the hook held its lock")
		case <-time.After(100 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatalf("PruneEmptySessions: %v", err)
	}
	var givenScope map[string]any
	if given.Key != key || json.Unmarshal(given.Scope, &givenScope) != nil || givenScope["channel"] != "whatsapp" {
		t.Errorf("hook got %+v, want the session's meta", given)
	}
}
