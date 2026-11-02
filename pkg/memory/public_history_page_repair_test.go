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
	data = []byte(strings.Replace(string(data), "assistant", "assistanz", 1))
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
