package memory

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicHistoryPreparationPreservesActiveContentRefs(t *testing.T) {
	s, e := NewJSONLStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	k := "sk_v1_prepare_active"
	for _, c := range []string{"a", "b"} {
		if e = s.AddMessage(t.Context(), k, "assistant", c); e != nil {
			t.Fatal(e)
		}
	}
	f := initializedContent(t, s, k)
	if len(f.Entries) != 2 || f.Entries[0].Ref == nil {
		t.Fatal("fixture identity")
	}
	m, e := s.readMeta(k)
	if e != nil {
		t.Fatal(e)
	}
	m.PublicHistoryVersion = 0
	m.PublicHistoryGeneration = ""
	m.PublicHistoryRevision = 0
	m.PublicHistoryBytes = 0
	m.PublicHistoryCount = 0
	m.PublicHistoryDigest = ""
	m.PublicHistoryComplete = false
	m.PublicHistoryPreparation = nil
	if e = s.writeMeta(k, m); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(s.publicHistoryPath(k)); e != nil {
		t.Fatal(e)
	}
	p, e := s.PreparePublicHistory(t.Context(), k, "")
	if e != nil {
		t.Fatal("begin:", e)
	}
	for i := 0; !p.Ready && i < 3; i++ {
		p, e = s.PreparePublicHistory(t.Context(), k, p.Cursor)
		if e != nil {
			t.Fatal("step:", e)
		}
	}
	h, e := s.ReadPublicHistory(t.Context(), k)
	if e != nil {
		t.Fatal("read:", e)
	}
	for _, x := range h.Entries {
		if x.Ref == nil {
			t.Fatal("prepared active identity entry lost ContentRef")
		}
	}
}

func TestPublicHistoryDeleteAcceptsLegacyIntentWithoutPublicSidecar(t *testing.T) {
	s, k := pauseOwnedDeletion(t)
	data, e := os.ReadFile(s.historyRecoveryPath(k))
	if e != nil {
		t.Fatal(e)
	}
	var r historyDeletionRecord
	if e = json.Unmarshal(data, &r); e != nil {
		t.Fatal(e)
	}
	files := r.Deletion.Files[:0]
	for _, f := range r.Deletion.Files {
		if f.Name != filepath.Base(s.publicHistoryPath(k)) {
			files = append(files, f)
		}
	}
	r.Deletion.Files = files
	r.Deletion.Fingerprint = historyDeletionFingerprint(r.Deletion)
	if e = os.Remove(s.publicHistoryPath(k)); e != nil && !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if e = s.validHistoryDeletion(k, r.Deletion); e != nil {
		t.Fatal("old valid intent rejected:", e)
	}
	if operationErr := os.WriteFile(
		s.publicHistoryPath(k),
		[]byte("new source without an old ownership descriptor"),
		0o600,
	); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := s.validHistoryDeletion(k, r.Deletion); !errors.Is(operationErr, ErrContentIdentityUnavailable) {
		t.Fatal("old intent adopted a newly present public source", operationErr)
	}
}

func TestPublicHistoryImportedOnlyScopeSurvivesEmptyPruning(t *testing.T) {
	s, k := legacyPublicFixture(t, 0)
	p, e := s.PreparePublicHistory(t.Context(), k, "")
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.ImportPublicHistory(
		t.Context(),
		PublicHistoryImport{
			Key:        k,
			Generation: p.Generation,
			Revision:   p.Revision,
			Entries: []PublicHistoryEntry{
				{
					ID:       uuid.NewString(),
					ImportID: "legacy",
					Message:  providers.Message{Role: "assistant", Content: "keep"},
				},
			},
		},
	)
	if e != nil {
		t.Fatal(e)
	}
	keys, e := s.PruneEmptySessions(time.Minute, time.Now().Add(time.Hour), nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, x := range keys {
		if x == k {
			t.Fatal("pruned imported public content")
		}
	}
}

func TestPublicHistoryPreparationAcceptsBlankAndUnconfirmedTail(t *testing.T) {
	for _, kind := range []string{"blank", "partial"} {
		t.Run(kind, func(t *testing.T) {
			s, k := legacyPublicFixture(t, 2)
			b, _ := os.ReadFile(s.jsonlPath(k))
			switch kind {
			case "blank":
				b = append([]byte("\n"), b...)
			case "partial":
				b = append(b, []byte(`{"role":"assistant","content":"incomplete`)...)
			}
			if e := os.WriteFile(s.jsonlPath(k), b, 0o600); e != nil {
				t.Fatal(e)
			}
			h, e := s.GetHistory(t.Context(), k)
			if e != nil {
				t.Fatal(e)
			}
			t.Log("legacy accepted messages", len(h))
			p, e := s.PreparePublicHistory(t.Context(), k, "")
			if e != nil {
				t.Fatal(e)
			}
			for i := 0; !p.Ready && i < 3; i++ {
				p, e = s.PreparePublicHistory(t.Context(), k, p.Cursor)
				if e != nil {
					t.Fatal("public preparation refused accepted legacy input:", e)
				}
			}
		})
	}
}

func TestPublicHistoryPreparationRetainsOversizedAndCorruptSource(t *testing.T) {
	for _, kind := range []string{"oversized", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			store, key := legacyPublicFixture(t, 1)
			source := []byte("{broken JSON\n")
			expected := ErrPublicHistoryCoverage
			if kind == "oversized" {
				var err error
				source, err = json.Marshal(
					providers.Message{Role: "assistant", Content: strings.Repeat("x", PublicHistoryMessageBytes+1)},
				)
				if err != nil {
					t.Fatal(err)
				}
				source = append(source, '\n')
				expected = ErrHistoryCapacity
			}
			if operationErr := os.WriteFile(store.jsonlPath(key), source, 0o600); operationErr != nil {
				t.Fatal(operationErr)
			}
			preparation, err := store.PreparePublicHistory(t.Context(), key, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, operationErr := store.PreparePublicHistory(
				t.Context(),
				key,
				preparation.Cursor,
			); !errors.Is(
				operationErr,
				expected,
			) {
				t.Fatal("preparation invented coverage outside the approved source budget", operationErr)
			}
			after, err := os.ReadFile(store.jsonlPath(key))
			if err != nil || string(source) != string(after) {
				t.Fatal("unproven source was discarded", err)
			}
		})
	}
}
