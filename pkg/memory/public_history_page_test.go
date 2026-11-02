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
	first, operationErr0 := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if operationErr0 != nil || first.Start != 1732 || first.End != 1752 || len(first.Entries) != 20 {
		t.Fatalf("recent page: %+v %v", first, operationErr0)
	}
	t.Logf(
		"1752-row recent page: source=%d index=%d lookahead=%d publicMessages=%d rows=%d",
		first.Stats.DataBytesRead,
		first.Stats.IndexBytesRead,
		first.Stats.LookaheadBytes,
		first.Stats.MessageBytes,
		first.Stats.RowsRead,
	)
	if operationErr1 := store.AddMessage(
		t.Context(),
		key,
		"assistant",
		"new response outside pinned view",
	); operationErr1 != nil {
		t.Fatal(operationErr1)
	}
	seen := make(map[string]bool)
	for _, entry := range first.Entries {
		seen[entry.ID] = true
	}
	page := first
	previousStart := first.Start
	for page.OlderCursor != "" {
		request := PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor}
		page, operationErr0 = store.ReadPublicHistoryPage(t.Context(), request)
		if operationErr0 != nil || page.End != previousStart || page.Start >= previousStart || len(page.Entries) > 50 ||
			page.Ceiling != 1752 || page.Stats.IndexBytesRead > 8192 || page.Stats.DataBytesRead > 128<<10 {
			t.Fatalf("unbounded or nonadvancing page: %+v %v", page, operationErr0)
		}
		for _, entry := range page.Entries {
			if seen[entry.ID] {
				t.Fatal("duplicate occurrence during older navigation")
			}
			seen[entry.ID] = true
		}
		retry, retryErr := store.ReadPublicHistoryPage(t.Context(), request)
		if retryErr != nil || !reflect.DeepEqual(retry, page) {
			t.Fatalf("retry changed pinned page: %v", retryErr)
		}
		previousStart = page.Start
		if page.Start < 100 {
			store, operationErr0 = NewJSONLStore(store.dir)
			if operationErr0 != nil {
				t.Fatal(operationErr0)
			}
		}
	}
	if len(seen) != 1752 || page.Start != 0 {
		t.Fatalf("lost confirmed source coverage: %d", len(seen))
	}
	end := page.End
	for page.NewerCursor != "" {
		page, operationErr0 = store.ReadPublicHistoryPage(
			t.Context(),
			PublicHistoryPageRequest{Key: key, Cursor: page.NewerCursor},
		)
		if operationErr0 != nil || page.Start != end || page.End <= end || page.End > 1752 {
			t.Fatalf("newer cursor failed strict progress: %+v %v", page, operationErr0)
		}
		end = page.End
	}
	if end != 1752 {
		t.Fatal("newer traversal did not reach the pinned frontier")
	}
	fresh, operationErr0 := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if operationErr0 != nil || fresh.End != 1753 ||
		fresh.Entries[len(fresh.Entries)-1].Message.Content != "new response outside pinned view" {
		t.Fatal("fresh read did not discover the append", operationErr0)
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
			if operationErr2 := store.SetHistory(t.Context(), key, []providers.Message{message}); operationErr2 != nil {
				t.Fatal(operationErr2)
			}
			before, operationErr3 := os.ReadFile(store.publicHistoryPath(key))
			if operationErr3 != nil {
				t.Fatal(operationErr3)
			}
			page, operationErr3 := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
			if size > PublicHistoryMessageBytes {
				if !errors.Is(operationErr3, ErrHistoryCapacity) {
					t.Fatalf("oversized public message accepted: %v", operationErr3)
				}
			} else if operationErr3 != nil || len(page.Entries) != 1 || page.Stats.MessageBytes != int64(size) {
				t.Fatalf("valid public boundary rejected: %+v %v", page.Stats, operationErr3)
			}
			after, _ := os.ReadFile(store.publicHistoryPath(key))
			if string(before) != string(after) {
				t.Fatal("capacity check changed the confirmed source")
			}
		})
	}
	store, key := pagedStore(t, 0)
	message := providers.Message{Role: "assistant", Content: strings.Repeat("x", 700<<10)}
	if operationErr4 := store.SetHistory(
		t.Context(),
		key,
		[]providers.Message{message, message, message, message},
	); operationErr4 != nil {
		t.Fatal(operationErr4)
	}
	page, operationErr5 := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if operationErr5 != nil || len(page.Entries) != 2 || page.Start != 2 || page.End != 4 || page.OlderCursor == "" ||
		page.Stats.DataBytesRead > PublicHistoryOperationBytes || page.Stats.MessageBytes+publicHistoryEnvelopeBytes > PublicHistoryOperationBytes {
		t.Fatalf("operation did not stop before the undelivered message: %+v %v", page, operationErr5)
	}
	t.Logf(
		"byte-limited page: source=%d index=%d lookahead=%d publicMessages=%d rows=%d",
		page.Stats.DataBytesRead,
		page.Stats.IndexBytesRead,
		page.Stats.LookaheadBytes,
		page.Stats.MessageBytes,
		page.Stats.RowsRead,
	)
	older, operationErr5 := store.ReadPublicHistoryPage(
		t.Context(),
		PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor},
	)
	if operationErr5 != nil || older.Start != 0 || older.End != 2 || len(older.Entries) != 2 {
		t.Fatalf("byte-limited continuation skipped content: %+v %v", older, operationErr5)
	}
}

func TestPublicHistoryPagesRejectStructuralChangesCorruptionAndCancellation(t *testing.T) {
	store, key := pagedStore(t, 80)
	page, operationErr6 := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if operationErr6 != nil {
		t.Fatal(operationErr6)
	}
	history, operationErr6 := store.ReadPublicHistory(t.Context(), key)
	if operationErr6 != nil {
		t.Fatal(operationErr6)
	}
	anchor := history.Entries[5]
	entry := PublicHistoryEntry{
		ID: uuid.NewString(), ImportID: "historical-delivery", AnchorID: anchor.ID, AnchorOrder: anchor.Order,
		Message: providers.Message{Role: "assistant", Content: "inserted inside the prior view"},
	}
	if _, operationErr7 := store.ImportPublicHistory(t.Context(), PublicHistoryImport{
		Key: key, Generation: history.Generation,
		Revision: history.Revision, AfterID: anchor.ID, Entries: []PublicHistoryEntry{entry},
	}); operationErr7 != nil {
		t.Fatal(operationErr7)
	}
	if _, operationErr8 := store.ReadPublicHistoryPage(
		t.Context(),
		PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor},
	); !errors.Is(
		operationErr8,
		ErrHistoryCursor,
	) {
		t.Fatalf("structural insertion entered a frozen range: %v", operationErr8)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, operationErr9 := store.ReadPublicHistoryPage(
		ctx,
		PublicHistoryPageRequest{Key: key},
	); !errors.Is(
		operationErr9,
		context.Canceled,
	) {
		t.Fatalf("cancellation became a successful page: %v", operationErr9)
	}
	index, operationErr6 := os.OpenFile(store.publicIndexPath(key), os.O_WRONLY, 0o600)
	if operationErr6 != nil {
		t.Fatal(operationErr6)
	}
	if _, operationErr10 := index.WriteAt([]byte("corrupt"), 40); operationErr10 != nil {
		t.Fatal(operationErr10)
	}
	if operationErr11 := index.Close(); operationErr11 != nil {
		t.Fatal(operationErr11)
	}
	if _, operationErr12 := store.ReadPublicHistoryPage(
		t.Context(),
		PublicHistoryPageRequest{Key: key},
	); !errors.Is(
		operationErr12,
		ErrPublicHistoryCoverage,
	) {
		t.Fatalf("corrupt metadata selected a fallback reader: %v", operationErr12)
	}
}

func TestPublicHistoryIndexPreparationIsBoundedResumableAndKeepsAppends(t *testing.T) {
	store, key := pagedStore(t, 121)
	if operationErr13 := os.Remove(store.publicIndexPath(key)); operationErr13 != nil {
		t.Fatal(operationErr13)
	}
	for round := 0; round < 8; round++ {
		prepared, operationErr14 := store.PreparePublicHistory(t.Context(), key, "")
		if operationErr14 != nil {
			t.Fatal(operationErr14)
		}
		if prepared.Ready {
			if round < 3 {
				t.Fatal("index preparation scanned the full source in one request")
			}
			break
		}
		if round == 0 {
			state, operationErr15 := store.readPublicIndexPreparation(key, mustPublicMeta(t, store, key))
			if operationErr15 != nil || state.Count != 50 {
				t.Fatalf("unbounded preparation: %+v %v", state, operationErr15)
			}
			if operationErr16 := store.AddMessage(
				t.Context(),
				key,
				"assistant",
				"append during preparation",
			); operationErr16 != nil {
				t.Fatal(operationErr16)
			}
			store, operationErr15 = NewJSONLStore(store.dir)
			if operationErr15 != nil {
				t.Fatal(operationErr15)
			}
		}
	}
	page, operationErr17 := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if operationErr17 != nil || page.End != 122 ||
		page.Entries[len(page.Entries)-1].Message.Content != "append during preparation" {
		t.Fatalf("prepared source lost its concurrent append: %+v %v", page, operationErr17)
	}
}

func mustPublicMeta(t *testing.T, store *JSONLStore, key string) SessionMeta {
	t.Helper()
	meta, operationErr18 := store.readMeta(key)
	if operationErr18 != nil {
		t.Fatal(operationErr18)
	}
	return meta
}

func TestPublicHistoryIndexRollsBackWithItsConfirmedSource(t *testing.T) {
	store, key := pagedStore(t, 60)
	before, operationErr19 := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if operationErr19 != nil {
		t.Fatal(operationErr19)
	}
	fail := true
	store.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == store.metaPath(key) && fail {
			fail = false
			return errors.New("controlled metadata commit failure")
		}
		return os.WriteFile(path, data, mode)
	}
	if operationErr20 := store.AddMessage(t.Context(), key, "assistant", "must roll back"); operationErr20 == nil {
		t.Fatal("expected failed transaction")
	}
	store.writeFile = nil
	reopened, operationErr19 := NewJSONLStore(store.dir)
	if operationErr19 != nil {
		t.Fatal(operationErr19)
	}
	after, operationErr19 := reopened.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if operationErr19 != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("index did not follow OLD recovery: %+v %v", after, operationErr19)
	}
}

func TestPublicHistoryPageKeepsViewAcrossModelCompactionButRejectsEdits(t *testing.T) {
	store, key := pagedStore(t, 80)
	page, operationErr21 := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if operationErr21 != nil {
		t.Fatal(operationErr21)
	}
	request := PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor}
	before, operationErr21 := store.ReadPublicHistoryPage(t.Context(), request)
	if operationErr21 != nil {
		t.Fatal(operationErr21)
	}
	if operationErr22 := store.TruncateHistory(t.Context(), key, 2); operationErr22 != nil {
		t.Fatal(operationErr22)
	}
	if operationErr23 := store.Compact(t.Context(), key); operationErr23 != nil {
		t.Fatal(operationErr23)
	}
	after, operationErr21 := store.ReadPublicHistoryPage(t.Context(), request)
	if operationErr21 != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("model compaction changed confirmed public coverage", operationErr21)
	}
	model, operationErr21 := store.GetHistory(t.Context(), key)
	if operationErr21 != nil {
		t.Fatal(operationErr21)
	}
	model[len(model)-1].Content = "edited confirmed answer"
	if operationErr24 := store.SetHistory(t.Context(), key, model); operationErr24 != nil {
		t.Fatal(operationErr24)
	}
	if _, operationErr25 := store.ReadPublicHistoryPage(
		t.Context(),
		request,
	); !errors.Is(
		operationErr25,
		ErrHistoryCursor,
	) {
		t.Fatalf("edited view accepted old cursor: %v", operationErr25)
	}
}

func TestPublicHistoryPageRejectsWarmMetadataAndSourceCorruption(t *testing.T) {
	for _, scenario := range []string{"metadata", "source"} {
		t.Run(scenario, func(t *testing.T) {
			store, key := pagedStore(t, 80)
			if _, operationErr26 := store.ReadPublicHistoryPage(
				t.Context(),
				PublicHistoryPageRequest{Key: key},
			); operationErr26 != nil {
				t.Fatal(operationErr26)
			}
			path := store.publicHistoryPath(key)
			var data []byte
			if scenario == "metadata" {
				meta := mustPublicMeta(t, store, key)
				meta.PublicHistoryRevision++
				var operationErr27 error
				data, operationErr27 = json.Marshal(meta)
				if operationErr27 != nil {
					t.Fatal(operationErr27)
				}
				path = store.metaPath(key)
			} else {
				var operationErr28 error
				data, operationErr28 = os.ReadFile(path)
				if operationErr28 != nil {
					t.Fatal(operationErr28)
				}
				position := strings.Index(string(data), "synthetic-79")
				if position < 0 {
					t.Fatal("missing corruption target")
				}
				data[position] = 'S'
			}
			if operationErr29 := os.WriteFile(path, data, 0o600); operationErr29 != nil {
				t.Fatal(operationErr29)
			}
			if _, operationErr30 := store.ReadPublicHistoryPage(
				t.Context(),
				PublicHistoryPageRequest{Key: key},
			); !errors.Is(
				operationErr30,
				ErrPublicHistoryCoverage,
			) {
				t.Fatalf("corruption became successful/fallback history: %v", operationErr30)
			}
		})
	}
}
