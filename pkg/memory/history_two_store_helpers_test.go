package memory

// Shared helpers for the two-store regression tests: two JSONLStore
// instances on one directory stand in for two pods sharing sessions/.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type twoStorePause struct {
	reached chan struct{}
	resume  chan bool // true: the writer dies here (runtime.Goexit)
	fired   atomic.Bool
}

// armTwoStorePause blocks the first write of path. afterWrite=true pauses
// right after the file is durable (journal case); false pauses before it.
func armTwoStorePause(s *JSONLStore, path string, afterWrite bool) *twoStorePause {
	p := &twoStorePause{reached: make(chan struct{}), resume: make(chan bool)}
	s.writeFile = func(target string, data []byte, mode os.FileMode) error {
		if target != path || !p.fired.CompareAndSwap(false, true) {
			return fileutil.WriteFileAtomic(target, data, mode)
		}
		if afterWrite {
			if err := fileutil.WriteFileAtomic(target, data, mode); err != nil {
				return err
			}
		}
		close(p.reached)
		if <-p.resume {
			runtime.Goexit()
		}
		if afterWrite {
			return nil
		}
		return fileutil.WriteFileAtomic(target, data, mode)
	}
	return p
}

type twoStoreCall struct {
	done     chan struct{}
	err      error
	returned bool
}

func runTwoStore(fn func() error) *twoStoreCall {
	c := &twoStoreCall{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		c.err = fn()
		c.returned = true
	}()
	return c
}

func waitTwoStore(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second): // guard only, not synchronization
		t.Fatalf("timed out waiting for %s", what)
	}
}

func openTwoStore(t *testing.T, dir string) *JSONLStore {
	t.Helper()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func addTwoStore(t *testing.T, s *JSONLStore, key string, contents ...string) {
	t.Helper()
	for _, content := range contents {
		if err := s.AddMessage(t.Context(), key, "user", content); err != nil {
			t.Fatal(err)
		}
	}
}

type twoStoreState struct {
	JSONLBytes   int64
	Lines        []string
	MetaCount    int
	MetaBytes    int64
	MetaRecovery bool
	Journal      bool
	Decision     bool
	Marker       bool
}

func (s twoStoreState) String() string {
	return fmt.Sprintf(
		"jsonl=%dB lines=%v | meta.count=%d meta.historyBytes=%d meta.recoveryRequired=%v | journal=%v decision=%v recovery-marker=%v",
		s.JSONLBytes,
		s.Lines,
		s.MetaCount,
		s.MetaBytes,
		s.MetaRecovery,
		s.Journal,
		s.Decision,
		s.Marker,
	)
}

func twoStoreExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readTwoStoreState(t *testing.T, dir, key string) twoStoreState {
	t.Helper()
	base := filepath.Join(dir, sanitizeKey(key))
	var state twoStoreState
	raw, err := os.ReadFile(base + ".jsonl")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	state.JSONLBytes = int64(len(raw))
	for len(raw) > 0 {
		end := bytes.IndexByte(raw, '\n')
		if end < 0 {
			state.Lines = append(state.Lines, fmt.Sprintf("<torn %dB>", len(raw)))
			break
		}
		var row struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(raw[:end], &row) != nil {
			row.Content = "<undecodable>"
		}
		state.Lines = append(state.Lines, row.Content)
		raw = raw[end+1:]
	}
	if data, err := os.ReadFile(base + ".meta.json"); err == nil {
		var meta SessionMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			t.Fatal(err)
		}
		state.MetaCount, state.MetaRecovery = meta.Count, meta.HistoryRecoveryRequired
		if meta.HistoryBytes != nil {
			state.MetaBytes = *meta.HistoryBytes
		} else {
			state.MetaBytes = -1
		}
	}
	state.Journal = twoStoreExists(base + ".jsonl.commit.json")
	state.Decision = twoStoreExists(base + ".jsonl.commit.decision")
	state.Marker = twoStoreExists(base + ".jsonl.recovery-required")
	return state
}

func twoStoreContents(t *testing.T, s *JSONLStore, key string) []string {
	t.Helper()
	messages, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	contents := make([]string, len(messages))
	for i, message := range messages {
		contents[i] = message.Content
	}
	return contents
}

func twoStorePage(t *testing.T, s *JSONLStore, key string) (HistoryPage, error) {
	t.Helper()
	return s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key, Refresh: true})
}

func twoStoreHas(lines []string, content string) bool {
	for _, line := range lines {
		if line == content {
			return true
		}
	}
	return false
}

// killAWithTornSuffix makes A die after its journal and an append of two more
// rows, then cuts A's last row in half like a crash during the write.
func killAWithTornSuffix(t *testing.T, a *JSONLStore, key string) {
	t.Helper()
	messages, err := a.GetHistory(t.Context(), key)
	if err != nil || len(messages) != 2 {
		t.Fatal(err, messages)
	}
	messages = append(messages,
		providers.Message{Role: "user", Content: "three-A"},
		providers.Message{Role: "assistant", Content: "four-A"},
	)
	pause := armTwoStorePause(a, a.metaPath(key), false)
	call := runTwoStore(func() error { return a.SetHistory(t.Context(), key, messages) })
	waitTwoStore(t, pause.reached, "A before its meta publish")
	pause.resume <- true // A dies here
	waitTwoStore(t, call.done, "A goroutine exit")
	if call.returned {
		t.Fatal("A was expected to die, not return", call.err)
	}
	raw, err := os.ReadFile(a.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	lastStart := bytes.LastIndexByte(raw[:len(raw)-1], '\n') + 1
	if err := os.Truncate(a.jsonlPath(key), int64(lastStart+(len(raw)-lastStart)/2)); err != nil {
		t.Fatal(err)
	}
}

// recordJournalOverwrite captures the journal bytes on disk when s first
// writes the journal of key.
func recordJournalOverwrite(s *JSONLStore, key string) *[]byte {
	var overwritten []byte
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.historyRecoveryPath(key) && overwritten == nil {
			overwritten, _ = os.ReadFile(path)
			if overwritten == nil {
				overwritten = []byte{}
			}
		}
		return fileutil.WriteFileAtomic(path, data, mode)
	}
	return &overwritten
}

type twoStoreStaleCacheResult struct {
	stale, disk SessionMeta
	afterRead   twoStoreState
	readErr     error
	pageRead    HistoryPage
	appendErr   error
	afterAppend twoStoreState
	page        HistoryPage
	pageErr     error
}

// twoStoreStaleCache: A writes X, B warms its cache (WarmSessionListing), A appends
// three-A, then B optionally reads a page and appends four-B from its cache.
func twoStoreStaleCache(t *testing.T, witness, readFirst bool) twoStoreStaleCacheResult {
	return twoStoreStaleCacheWith(t, witness, readFirst, false)
}

func twoStoreStaleCacheWith(t *testing.T, witness, readFirst, receipt bool) twoStoreStaleCacheResult {
	t.Helper()
	dir := t.TempDir()
	key := "agent:main:direct:stale-cache"
	a := openTwoStore(t, dir)
	if witness {
		if err := a.InitializeNewHistoryContentIdentity(t.Context(), key); err != nil {
			t.Fatal(err)
		}
	}
	addTwoStore(t, a, key, "one", "two")
	domain, present, domainErr := a.readContentDomain(key)
	t.Logf("content witness present=%v version=%d err=%v", present, domain.Version, domainErr)

	b := openTwoStore(t, dir)
	b.ListSessionMetas() // WarmSessionListing at B's boot, A still serving
	addTwoStore(t, a, key, "three-A")
	var r twoStoreStaleCacheResult
	r.stale, _ = b.cachedMeta(sanitizeKey(key))
	var err error
	if r.disk, err = b.readRecoveryMeta(key); err != nil {
		t.Fatal(err)
	}
	t.Logf(
		"B cached meta: count=%d historyBytes=%d seq=%d | disk meta: count=%d historyBytes=%d seq=%d",
		r.stale.Count,
		*r.stale.HistoryBytes,
		r.stale.HistorySequence,
		r.disk.Count,
		*r.disk.HistoryBytes,
		r.disk.HistorySequence,
	)
	if readFirst {
		page, readErr := b.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key})
		r.readErr, r.pageRead = readErr, page
		r.afterRead = readTwoStoreState(t, dir, key)
		t.Logf("B ReadHistoryPage (stale cache): entries=%d err=%v", len(page.Entries), readErr)
		t.Logf("after B read:          %v", r.afterRead)
	}
	if receipt {
		_, r.appendErr = b.AppendHistoryReceipt(t.Context(), key, providers.Message{Role: "user", Content: "four-B"})
		t.Logf("B AppendHistoryReceipt(four-B) returned: %v", r.appendErr)
	} else {
		r.appendErr = b.AddMessage(t.Context(), key, "user", "four-B")
		t.Logf("B AddMessage(four-B) returned: %v", r.appendErr)
	}
	r.afterAppend = readTwoStoreState(t, dir, key)
	t.Logf("after B append:        %v", r.afterAppend)
	fresh := openTwoStore(t, dir)
	r.page, r.pageErr = twoStorePage(t, fresh, key)
	t.Logf("fresh store GetHistory: %v", twoStoreContents(t, fresh, key))
	t.Logf(
		"fresh store ReadHistoryPage: entries=%d complete=%v err=%v",
		len(r.page.Entries),
		r.page.Complete,
		r.pageErr,
	)
	return r
}

func requireTwoStoreStale(t *testing.T, r twoStoreStaleCacheResult) {
	t.Helper()
	if r.stale.Count != 2 || r.disk.Count != 3 {
		t.Fatal("precondition: B's cache must be stale", r.stale.Count, r.disk.Count)
	}
}
