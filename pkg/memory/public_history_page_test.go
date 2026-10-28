package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicHistoryPagesSeekBidirectionallyAcrossRestartAndConcurrentAppend(t *testing.T) {
	store, key := pagedStore(t, 1752)
	first, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if err != nil || first.Start != 1732 || first.End != 1752 || len(first.Entries) != 20 {
		t.Fatalf("recent page: %+v %v", first, err)
	}
	t.Logf("1752-row recent page: source=%d index=%d lookahead=%d publicMessages=%d rows=%d",
		first.Stats.DataBytesRead, first.Stats.IndexBytesRead, first.Stats.LookaheadBytes, first.Stats.MessageBytes, first.Stats.RowsRead)
	if err := store.AddMessage(t.Context(), key, "assistant", "new response outside pinned view"); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, entry := range first.Entries {
		seen[entry.ID] = true
	}
	page := first
	previousStart := first.Start
	for page.OlderCursor != "" {
		request := PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor}
		page, err = store.ReadPublicHistoryPage(t.Context(), request)
		if err != nil || page.End != previousStart || page.Start >= previousStart || len(page.Entries) > 50 ||
			page.Ceiling != 1752 || page.Stats.IndexBytesRead > 8192 || page.Stats.DataBytesRead > 128<<10 {
			t.Fatalf("unbounded or nonadvancing page: %+v %v", page, err)
		}
		for _, entry := range page.Entries {
			if seen[entry.ID] {
				t.Fatal("duplicate occurrence during older navigation")
			}
			seen[entry.ID] = true
		}
		retry, err := store.ReadPublicHistoryPage(t.Context(), request)
		if err != nil || !reflect.DeepEqual(retry, page) {
			t.Fatalf("retry changed pinned page: %v", err)
		}
		previousStart = page.Start
		if page.Start < 100 {
			store, err = NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(seen) != 1752 || page.Start != 0 {
		t.Fatalf("lost confirmed source coverage: %d", len(seen))
	}
	end := page.End
	for page.NewerCursor != "" {
		page, err = store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key, Cursor: page.NewerCursor})
		if err != nil || page.Start != end || page.End <= end || page.End > 1752 {
			t.Fatalf("newer cursor failed strict progress: %+v %v", page, err)
		}
		end = page.End
	}
	if end != 1752 {
		t.Fatal("newer traversal did not reach the pinned frontier")
	}
	fresh, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if err != nil || fresh.End != 1753 || fresh.Entries[len(fresh.Entries)-1].Message.Content != "new response outside pinned view" {
		t.Fatal("fresh read did not discover the append", err)
	}
}

func TestPublicHistoryPagesRespectPublicMessageAndOperationBudgets(t *testing.T) {
	for _, size := range []int{393 << 10, PublicHistoryMessageBytes, PublicHistoryMessageBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, key := pagedStore(t, 0)
			created := time.Unix(1, 0).UTC()
			message := providers.Message{Role: "assistant", CreatedAt: &created}
			fixed, _ := json.Marshal(message)
			message.Content = strings.Repeat("x", size-len(fixed))
			if err := store.SetHistory(t.Context(), key, []providers.Message{message}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(store.publicHistoryPath(key))
			if err != nil {
				t.Fatal(err)
			}
			page, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
			if size > PublicHistoryMessageBytes {
				if !errors.Is(err, ErrHistoryCapacity) {
					t.Fatalf("oversized public message accepted: %v", err)
				}
			} else if err != nil || len(page.Entries) != 1 || page.Stats.MessageBytes != int64(size) {
				t.Fatalf("valid public boundary rejected: %+v %v", page.Stats, err)
			}
			after, _ := os.ReadFile(store.publicHistoryPath(key))
			if string(before) != string(after) {
				t.Fatal("capacity check changed the confirmed source")
			}
		})
	}
	store, key := pagedStore(t, 0)
	message := providers.Message{Role: "assistant", Content: strings.Repeat("x", 700<<10)}
	if err := store.SetHistory(t.Context(), key, []providers.Message{message, message, message, message}); err != nil {
		t.Fatal(err)
	}
	page, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if err != nil || len(page.Entries) != 2 || page.Start != 2 || page.End != 4 || page.OlderCursor == "" ||
		page.Stats.LookaheadBytes == 0 || page.Stats.MessageBytes+publicHistoryEnvelopeBytes > PublicHistoryOperationBytes {
		t.Fatalf("operation did not stop before the undelivered message: %+v %v", page, err)
	}
	t.Logf("byte-limited page: source=%d index=%d lookahead=%d publicMessages=%d rows=%d",
		page.Stats.DataBytesRead, page.Stats.IndexBytesRead, page.Stats.LookaheadBytes, page.Stats.MessageBytes, page.Stats.RowsRead)
	older, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor})
	if err != nil || older.Start != 0 || older.End != 2 || len(older.Entries) != 2 {
		t.Fatalf("byte-limited continuation skipped content: %+v %v", older, err)
	}
}

func TestPublicHistoryPagesRejectStructuralChangesCorruptionAndCancellation(t *testing.T) {
	store, key := pagedStore(t, 80)
	page, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	history, err := store.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	anchor := history.Entries[5]
	entry := PublicHistoryEntry{ID: uuid.NewString(), ImportID: "historical-delivery", AnchorID: anchor.ID, AnchorOrder: anchor.Order,
		Message: providers.Message{Role: "assistant", Content: "inserted inside the prior view"}}
	if _, err := store.ImportPublicHistory(t.Context(), PublicHistoryImport{Key: key, Generation: history.Generation,
		Revision: history.Revision, AfterID: anchor.ID, Entries: []PublicHistoryEntry{entry}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor}); !errors.Is(err, ErrHistoryCursor) {
		t.Fatalf("structural insertion entered a frozen range: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.ReadPublicHistoryPage(ctx, PublicHistoryPageRequest{Key: key}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation became a successful page: %v", err)
	}
	index, err := os.OpenFile(store.publicIndexPath(key), os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.WriteAt([]byte("corrupt"), 40); err != nil {
		t.Fatal(err)
	}
	if err := index.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key}); !errors.Is(err, ErrPublicHistoryCoverage) {
		t.Fatalf("corrupt metadata selected a fallback reader: %v", err)
	}
}

func TestPublicHistoryIndexPreparationIsBoundedResumableAndKeepsAppends(t *testing.T) {
	store, key := pagedStore(t, 121)
	if err := os.Remove(store.publicIndexPath(key)); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 8; round++ {
		_, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
		if err == nil {
			if round < 3 {
				t.Fatal("index preparation scanned the full source in one request")
			}
			break
		}
		if !errors.Is(err, ErrPublicHistoryIndexPreparing) {
			t.Fatal(err)
		}
		if round == 0 {
			state, err := store.readPublicIndexPreparation(key, mustPublicMeta(t, store, key))
			if err != nil || state.Count != 50 {
				t.Fatalf("unbounded preparation: %+v %v", state, err)
			}
			if err := store.AddMessage(t.Context(), key, "assistant", "append during preparation"); err != nil {
				t.Fatal(err)
			}
			store, err = NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	page, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if err != nil || page.End != 122 || page.Entries[len(page.Entries)-1].Message.Content != "append during preparation" {
		t.Fatalf("prepared source lost its concurrent append: %+v %v", page, err)
	}
}

func mustPublicMeta(t *testing.T, store *JSONLStore, key string) SessionMeta {
	t.Helper()
	meta, err := store.readMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

func TestPublicHistoryIndexRollsBackWithItsConfirmedSource(t *testing.T) {
	store, key := pagedStore(t, 60)
	before, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == store.metaPath(key) && fail {
			fail = false
			return errors.New("controlled metadata commit failure")
		}
		return os.WriteFile(path, data, mode)
	}
	if err := store.AddMessage(t.Context(), key, "assistant", "must roll back"); err == nil {
		t.Fatal("expected failed transaction")
	}
	store.writeFile = nil
	reopened, err := NewJSONLStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	after, err := reopened.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("index did not follow OLD recovery: %+v %v", after, err)
	}
}
