package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestContentLineageActivationReadsOnlyMetadataWithoutSideEffects(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		t.Run(map[bool]string{false: "uninitialized", true: "initialized"}[initialized], func(t *testing.T) {
			dir := t.TempDir()
			s, err := NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_content_activation"
			for range 2 {
				if err = s.AddMessage(t.Context(), key, "user", "content"); err != nil {
					t.Fatal(err)
				}
			}
			if initialized {
				initializedContent(t, s, key)
			}
			meta, err := s.GetSessionMeta(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			meta.Count, meta.Skip, meta.Aliases = 0, 1, []string{"activation-alias"}
			before, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(s.metaPath(key), before, 0o600); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(s.jsonlPath(key), s.jsonlPath(key)+".private-test-backup"); err != nil {
				t.Fatal(err)
			}
			if err = os.Mkdir(s.jsonlPath(key), 0o700); err != nil {
				t.Fatal(err)
			}
			s, err = NewJSONLStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			entriesBefore, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			active, err := s.HistoryContentIdentityActive(t.Context(), key)
			if err != nil || active != initialized {
				t.Fatal("activation queried payload or validated lost Count", active, err)
			}
			entriesAfter, err := os.ReadDir(dir)
			if err != nil || len(entriesAfter) != len(entriesBefore) || len(s.metaCache) != 0 {
				t.Fatal("activation initialized/recovered a source or populated its cache", err)
			}
			after, err := os.ReadFile(s.metaPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("activation repaired metadata", err)
			}
			if _, err = s.HistoryContentIdentityActive(
				t.Context(),
				"activation-alias",
			); !errors.Is(
				err,
				ErrHistoryOwnershipUnavailable,
			) {
				t.Fatal("activation relabeled an alias as an absent source", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err = s.HistoryContentIdentityActive(ctx, key); !errors.Is(err, context.Canceled) {
				t.Fatal("activation hid cancellation", err)
			}
			if err = os.Remove(s.metaPath(key)); err != nil {
				t.Fatal(err)
			}
			if err = os.Mkdir(s.metaPath(key), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err = s.HistoryContentIdentityActive(t.Context(), key); err == nil {
				t.Fatal("activation converted metadata I/O into feature absence")
			}
		})
	}
}
