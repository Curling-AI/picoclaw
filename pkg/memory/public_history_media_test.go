package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicHistoryMediaIdentityFollowsTheNativeAnchor(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "new_delivery", true: "existing_delivery"}[legacy], func(t *testing.T) {
			store := newTestStore(t)
			key, run := "agent:web:repeat-media", uuid.NewString()
			ctx := providers.WithHistoryRun(t.Context(), run)
			if err := store.AddFullMessage(ctx, key, providers.Message{Role: "user", Content: "report"}); err != nil {
				t.Fatal(err)
			}
			content := "[report](artifact:immutable.pdf)"
			if legacy {
				history, err := store.ReadPublicHistory(ctx, key)
				if err != nil {
					t.Fatal(err)
				}
				anchor := history.Entries[0]
				digest := sha256.Sum256([]byte(run + ":" + content))
				importID := "media:" + hex.EncodeToString(digest[:])
				_, err = store.ImportPublicHistory(ctx, PublicHistoryImport{
					Key: key, Generation: history.Generation, Revision: history.Revision, AfterID: anchor.ID,
					Entries: []PublicHistoryEntry{{
						ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(importID)).String(), RunID: run, ImportID: importID,
						AnchorID: anchor.ID, AnchorRef: anchor.Ref, AnchorOrder: anchor.Order,
						Message: providers.Message{Role: "assistant", Content: content},
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
			} else if err := store.RecordPublicMedia(ctx, key, run, content); err != nil {
				t.Fatal(err)
			}
			before, err := store.ReadPublicHistory(ctx, key)
			if err != nil || len(before.Entries) != 2 {
				t.Fatal("missing initial delivery", before, err)
			}
			initializedContent(t, store, key)
			store, err = NewJSONLStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.RecordPublicMedia(ctx, key, run, content); err != nil {
				t.Fatal("retry after identity activation and restart failed", err)
			}
			retried, err := store.ReadPublicHistory(ctx, key)
			if err != nil || len(retried.Entries) != 2 || retried.Entries[1].ID != before.Entries[1].ID ||
				retried.Entries[1].ImportID != before.Entries[1].ImportID || retried.Entries[1].AnchorRef == nil {
				t.Fatal("retry duplicated or rebound an existing delivery", retried, err)
			}
			if err = store.AddFullMessage(
				ctx,
				key,
				providers.Message{Role: "tool", Content: "file delivered"},
			); err != nil {
				t.Fatal(err)
			}
			if err = store.RecordPublicMedia(ctx, key, run, content); err != nil {
				t.Fatal("same file from another native occurrence failed", err)
			}
			after, err := store.ReadPublicHistory(ctx, key)
			if err != nil || len(after.Entries) != 4 || after.Entries[1].AnchorID != after.Entries[0].ID ||
				after.Entries[3].AnchorID != after.Entries[2].ID || after.Entries[3].ID == after.Entries[1].ID ||
				after.Entries[3].ImportID == after.Entries[1].ImportID ||
				!samePublicRef(after.Entries[3].AnchorRef, after.Entries[2].Ref) {
				t.Fatal("delivery identity did not follow its native occurrence", after, err)
			}
			if err = store.RecordPublicMedia(ctx, key, run, content); err != nil {
				t.Fatal("retry on the second native occurrence failed", err)
			}
			repeated, err := store.ReadPublicHistory(ctx, key)
			if err != nil || !reflect.DeepEqual(after, repeated) {
				t.Fatal("retry changed the confirmed display", repeated, err)
			}
		})
	}
}
