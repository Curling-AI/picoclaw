package memory

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicHistoryIndexPreparationRepairsDerivedCorruptionWithoutLosingAppend(t *testing.T) {
	for _, damage := range []string{"checksum", "empty", "truncated"} {
		t.Run(damage, func(t *testing.T) {
			store, key := pagedStore(t, 80)
			data, readErr := os.ReadFile(store.publicIndexPath(key))
			if readErr != nil {
				t.Fatal(readErr)
			}
			switch damage {
			case "checksum":
				data[0] ^= 1
			case "empty":
				data = nil
			case "truncated":
				data = data[:30]
			}
			if err := os.WriteFile(store.publicIndexPath(key), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ReadPublicHistoryPage(
				t.Context(),
				PublicHistoryPageRequest{Key: key},
			); !errors.Is(
				err,
				ErrPublicHistoryCoverage,
			) {
				t.Fatalf("corrupt index was not typed coverage: %v", err)
			}
			if err := store.AddMessage(t.Context(), key, "assistant", "append during repair"); err != nil {
				t.Fatal("derived index blocked durable append", err)
			}
			ready := false
			for range 5 {
				result, err := store.PreparePublicHistory(t.Context(), key, "")
				if err != nil {
					t.Fatal("intact source could not rebuild index", err)
				}
				if result.Ready {
					ready = true
					break
				}
			}
			if !ready {
				t.Fatal("bounded repair never became ready")
			}
			page, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
			if err != nil || page.Ceiling != 81 ||
				page.Entries[len(page.Entries)-1].Message.Content != "append during repair" {
				t.Fatal("repair lost confirmed append", page, err)
			}
		})
	}
}

func TestPublicHistoryPageBoundsSourceReadsBeforeLookahead(t *testing.T) {
	store, key := pagedStore(t, 0)
	message := providers.Message{Role: "assistant", Content: strings.Repeat("x", 700<<10)}
	if err := store.SetHistory(t.Context(), key, []providers.Message{message, message, message, message}); err != nil {
		t.Fatal(err)
	}
	trace := &publicPageTraceFS{FS: os.DirFS(store.dir)}
	page, err := ReadPublicHistoryPageFS(t.Context(), trace, PublicHistoryPageRequest{Key: key})
	if err != nil || len(page.Entries) != 2 || trace.sourceBytes > PublicHistoryOperationBytes ||
		page.Stats.DataBytesRead != trace.sourceBytes {
		t.Fatalf(
			"source budget exceeded before response: bytes=%d stats=%+v err=%v",
			trace.sourceBytes,
			page.Stats,
			err,
		)
	}
	older, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key, Cursor: page.OlderCursor})
	if err != nil || len(older.Entries) != 2 || older.Start != 0 {
		t.Fatal("budget cutoff skipped a retained occurrence", older, err)
	}
}

func TestPublicHistoryIndexRepairNeverAttestsCorruptSource(t *testing.T) {
	store, key := pagedStore(t, 80)
	if err := os.WriteFile(store.publicIndexPath(key), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	data, readErr := os.ReadFile(store.publicHistoryPath(key))
	if readErr != nil {
		t.Fatal(readErr)
	}
	// Keep valid JSON and length while changing confirmed bytes beneath the metadata digest.
	data = []byte(strings.Replace(string(data), "synthetic-0", "synthetic-X", 1))
	if err := os.WriteFile(store.publicHistoryPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	rejected := false
	for range 5 {
		result, err := store.PreparePublicHistory(t.Context(), key, "")
		if result.Ready {
			t.Fatal("rebuilt index attested altered confirmed source")
		}
		if errors.Is(err, ErrPublicHistoryCoverage) {
			rejected = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if !rejected {
		t.Fatal("corrupt source was not rejected at digest verification")
	}
}

func TestPublicHistoryPageKeepsDisplayButDropsUnavailableIndependentProofs(t *testing.T) {
	for _, failure := range []string{"order", "domain"} {
		t.Run(failure, func(t *testing.T) {
			store, key := pagedStore(t, 2)
			initializedContent(t, store, key)
			before, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
			if err != nil || len(before.Entries) != 2 || before.Entries[0].Ref == nil ||
				before.Entries[0].SourceDigest == "" ||
				!before.Entries[0].Order.Valid() {
				t.Fatal("fixture lacks independent proofs", before, err)
			}
			witness := store.historyOrderWitnessPath(key)
			if failure == "domain" {
				witness = store.contentDomainPath(key)
			}
			if removeErr := os.Remove(witness); removeErr != nil {
				t.Fatal(removeErr)
			}
			page, err := ReadPublicHistoryPageFS(t.Context(), os.DirFS(store.dir), PublicHistoryPageRequest{Key: key})
			if err != nil || len(page.Entries) != 2 ||
				page.Entries[0].Message.Content != before.Entries[0].Message.Content {
				t.Fatal("proof loss suppressed display", page, err)
			}
			if page.Entries[0].Order.Valid() || page.Entries[0].SourceDigest != "" {
				t.Fatal("proof loss resurrected raw receipt authority", page.Entries[0])
			}
			if failure == "domain" && page.Entries[0].Ref != nil {
				t.Fatal("missing domain resurrected content authority", page.Entries[0])
			}
		})
	}
}

func TestPublicHistoryPageTailSupplementKeepsItsTransactionalIndex(t *testing.T) {
	store, key := newTestStore(t), "sk_v1_page_tail_index"
	run := "11111111-1111-4111-8111-111111111111"
	if err := store.AddMessage(
		providers.WithHistoryRun(t.Context(), run),
		key,
		"assistant",
		"confirmed prefix",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordPublicMedia(t.Context(), key, run, "media supplement"); err != nil {
		t.Fatal(err)
	}
	page, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
	if err != nil || len(page.Entries) != 2 || page.Entries[1].Message.Content != "media supplement" {
		t.Fatal("tail append desynchronized public index", page, err)
	}
}
