package memory

import (
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestPublicHistoryIndexRejectsInteriorRecordSplice(t *testing.T) {
	for _, corruption := range []string{"moved", "relabeled", "generation"} {
		t.Run(corruption, func(t *testing.T) {
			store, key := pagedStore(t, 1752)
			first, err := store.ReadPublicHistoryPage(t.Context(), PublicHistoryPageRequest{Key: key})
			if err != nil {
				t.Fatal(err)
			}
			request := PublicHistoryPageRequest{Key: key, Cursor: first.OlderCursor}
			control, err := store.ReadPublicHistoryPage(t.Context(), request)
			if err != nil || control.Start != 1682 || control.End != 1732 {
				t.Fatal("control did not read the requested window", err)
			}
			index, err := os.OpenFile(store.publicIndexPath(key), os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			block := make([]byte, 51*publicIndexRecordBytes)
			origin := int64(1000)
			if corruption == "generation" {
				origin = 1681
			}
			if _, operationErr := index.ReadAt(
				block,
				publicIndexHeaderBytes+origin*publicIndexRecordBytes,
			); operationErr != nil {
				t.Fatal(operationErr)
			}
			if corruption != "moved" {
				for row := 0; row < 51; row++ {
					start := row * publicIndexRecordBytes
					record, decodeErr := decodePublicIndexRecord(block[start : start+publicIndexRecordBytes])
					if decodeErr != nil {
						t.Fatal(decodeErr)
					}
					if corruption == "relabeled" {
						record.Position = int64(1681 + row)
					} else {
						record.Generation = uuid.New()
					}
					encoded, encodeErr := encodePublicIndexRecord(record)
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					copy(block[start:start+publicIndexRecordBytes], encoded)
				}
			}
			if _, operationErr := index.WriteAt(
				block,
				publicIndexHeaderBytes+1681*publicIndexRecordBytes,
			); operationErr != nil {
				t.Fatal(operationErr)
			}
			if operationErr := index.Close(); operationErr != nil {
				t.Fatal(operationErr)
			}
			page, err := store.ReadPublicHistoryPage(t.Context(), request)
			if !errors.Is(err, ErrPublicHistoryCoverage) {
				actualID := ""
				if len(page.Entries) > 0 {
					actualID = page.Entries[0].ID
				}
				t.Fatalf(
					"interior index splice published wrong occurrences: range=[%d,%d) first=%q expected=%q error=%v",
					page.Start,
					page.End,
					actualID,
					control.Entries[0].ID,
					err,
				)
			}
		})
	}
}
