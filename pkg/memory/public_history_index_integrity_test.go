package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPublicHistoryIndexRejectsIncoherentEmptyHeaderAndKeepsLegitimateEmptySource(t *testing.T) {
	store, key := pagedStore(t, 0)
	if _, prepareErr := store.PreparePublicHistory(t.Context(), key, ""); prepareErr != nil {
		t.Fatal(prepareErr)
	}
	page, err := ReadPublicHistoryPageFS(t.Context(), os.DirFS(store.dir), PublicHistoryPageRequest{Key: key})
	if err != nil || len(page.Entries) != 0 || page.Ceiling != 0 {
		t.Fatal("legitimate empty source was rejected", err)
	}
	for _, corruption := range []string{"nonempty_data", "nonempty_digest"} {
		t.Run(corruption, func(t *testing.T) {
			count := 0
			if corruption == "nonempty_data" {
				count = 80
			}
			source, sourceKey := pagedStore(t, count)
			if _, prepareErr := source.PreparePublicHistory(t.Context(), sourceKey, ""); prepareErr != nil {
				t.Fatal(prepareErr)
			}
			meta := mustPublicMeta(t, source, sourceKey)
			index, readErr := os.ReadFile(source.publicIndexPath(sourceKey))
			if readErr != nil {
				t.Fatal(readErr)
			}
			header := index[:publicIndexHeaderBytes]
			meta.PublicHistoryCount = 0
			binary.BigEndian.PutUint64(header[40:48], 0)
			if corruption == "nonempty_digest" {
				meta.PublicHistoryDigest = strings.Repeat("a", sha256.Size*2)
				digest, decodeErr := hex.DecodeString(meta.PublicHistoryDigest)
				if decodeErr != nil {
					t.Fatal(decodeErr)
				}
				copy(header[48:80], digest)
			}
			checksum := sha256.Sum256(header[:80])
			copy(header[80:112], checksum[:])
			metadata, marshalErr := json.Marshal(meta)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if writeErr := os.WriteFile(source.metaPath(sourceKey), metadata, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			if writeErr := os.WriteFile(source.publicIndexPath(sourceKey), header, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			before := publicPageFiles(t, source.dir)
			_, readErr = ReadPublicHistoryPageFS(
				t.Context(),
				os.DirFS(source.dir),
				PublicHistoryPageRequest{Key: sourceKey},
			)
			if !errors.Is(readErr, ErrPublicHistoryCoverage) ||
				!reflect.DeepEqual(before, publicPageFiles(t, source.dir)) {
				t.Fatal("incoherent empty header became successful or changed source", readErr)
			}
		})
	}
}

func TestPublicHistoryIndexPreparationRejectsIncoherentFrontierInsteadOfPendingForever(t *testing.T) {
	for _, corruption := range []string{"count_after_append", "positive_count_zero_bytes"} {
		t.Run(corruption, func(t *testing.T) {
			store, key := pagedStore(t, 121)
			if err := os.Remove(store.publicIndexPath(key)); err != nil {
				t.Fatal(err)
			}
			if corruption == "count_after_append" {
				if result, err := store.PreparePublicHistory(t.Context(), key, ""); err != nil || result.Ready {
					t.Fatal("control did not prepare one bounded batch", err)
				}
			}
			state, err := store.readPublicIndexPreparation(key, mustPublicMeta(t, store, key))
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "count_after_append" {
				if operationErr := store.AddMessage(
					t.Context(),
					key,
					"assistant",
					"append outside preparation target",
				); operationErr != nil {
					t.Fatal(operationErr)
				}
				state.Target.Count++
			} else {
				state.Target.Bytes = 0
			}
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if operationErr := os.WriteFile(store.publicIndexPreparationPath(key), data, 0o600); operationErr != nil {
				t.Fatal(operationErr)
			}
			before, err := os.ReadFile(store.publicHistoryPath(key))
			if err != nil {
				t.Fatal(err)
			}
			rejected := false
			for round := 0; round < 5; round++ {
				result, prepareErr := store.PreparePublicHistory(t.Context(), key, "")
				if prepareErr != nil {
					if !errors.Is(prepareErr, ErrPublicHistoryCoverage) {
						t.Fatal("corrupt frontier returned another failure", prepareErr)
					}
					rejected = true
				} else if result.Ready || rejected {
					t.Fatal("corrupt frontier became ready or pending after failure", result)
				}
			}
			if !rejected {
				t.Fatal("corrupt frontier remained pending across repeated calls")
			}
			after, err := os.ReadFile(store.publicHistoryPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("corrupt preparation changed confirmed source", err)
			}
		})
	}
}

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
