package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentLineageNullMarkerDoesNotPermitNewBaseline(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_null_marker"
	if err = s.AddMessage(t.Context(), key, "assistant", "durable"); err != nil {
		t.Fatal(err)
	}
	initializedContent(t, s, key)
	data, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]json.RawMessage
	if err = json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta["content_identity"] = json.RawMessage("null")
	data, err = json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(s.contentDomainPath(key)); err != nil {
		t.Fatal(err)
	}
	active, err := s.HistoryContentIdentityActive(t.Context(), key)
	if err != nil || !active {
		t.Fatal("fixture has no activation marker", active, err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if frame.IdentityStatus != ContentIdentityUnavailable {
		t.Error("known null marker was relabeled never-initialized", frame.IdentityStatus)
	}
	if _, err = s.InitializeHistoryContentIdentity(
		t.Context(),
		key,
		frame.Observation,
	); !errors.Is(
		err,
		ErrContentIdentityUnavailable,
	) {
		t.Error("Initialize silently replaced a lost domain", err)
	}
	after, err := os.ReadFile(s.metaPath(key))
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("refused initialization changed preserved metadata", err)
	}
}

func TestContentLineageNullMarkerSurvivesAppendAndReopen(t *testing.T) {
	for _, typed := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "typed"}[typed], func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_null_marker_append"
			_, _, _ = pendingContent(t, s, key)
			original, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(s.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			var meta map[string]json.RawMessage
			if err = json.Unmarshal(data, &meta); err != nil {
				t.Fatal(err)
			}
			meta["content_identity"] = json.RawMessage("null")
			data, err = json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
				t.Fatal(err)
			}
			for _, p := range []string{s.contentDomainPath(key), s.historyOrderWitnessPath(key)} {
				if err = os.Remove(p); err != nil {
					t.Fatal(err)
				}
			}
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			if typed {
				result, appendErr := s.AppendHistoryContent(
					t.Context(),
					key,
					providers.Message{Role: "assistant", Content: "new durable"},
				)
				if appendErr != nil {
					t.Fatal(appendErr)
				}
				if result.Entry.Ref != nil || result.Receipt != nil || result.Entry.ReceiptBinding != nil {
					t.Fatal("lost identity/order was reattested", result)
				}
			} else {
				if err = s.AddMessage(t.Context(), key, "assistant", "new durable"); err != nil {
					t.Fatal(err)
				}
			}
			s, err = NewJSONLStore(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			active, err := s.HistoryContentIdentityActive(t.Context(), key)
			if err != nil || !active {
				t.Error("append erased conservative identity-loss marker", active, err)
			}
			frame, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			if frame.IdentityStatus != ContentIdentityUnavailable {
				t.Error("reopen forgot prior identity loss", frame.IdentityStatus)
			}
			if err = s.Compact(t.Context(), key); err == nil {
				t.Error("automatic physical GC accepted an unknown archive state")
			}
			after, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.HasPrefix(after, original) {
				t.Error("unknown archive state discarded original physical rows", err)
			}
			if _, err = s.InitializeHistoryContentIdentity(
				t.Context(),
				key,
				frame.Observation,
			); !errors.Is(
				err,
				ErrContentIdentityUnavailable,
			) {
				t.Error("append reopened ordinary initialization after known loss", err)
			}
		})
	}
}

func TestContentLineageUnavailableArchiveStatePreservesPruning(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "agent:cron-null-marker:run"
	pendingContent(t, s, key)
	meta, err := s.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	meta.ContentIdentity = &contentIdentityState{Unavailable: true}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	physical, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err = os.Chtimes(filepath.Join(s.dir, entry.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := s.PruneCronRuns(time.Hour, now)
	if err != nil || removed != 0 {
		t.Fatal("automatic pruning removed an unknown archive state", removed, err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(physical, after) {
		t.Fatal("pruning discarded the unproved source", err)
	}
}

func TestContentLineageUnavailableClearDoesNotCreateReplacementDomain(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_unavailable_clear"
	if err = s.AddMessage(t.Context(), key, "assistant", "durable"); err != nil {
		t.Fatal(err)
	}
	initializedContent(t, s, key)
	meta, err := s.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	meta.ContentIdentity = &contentIdentityState{Unavailable: true}
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(s.metaPath(key), data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetHistory(t.Context(), key, nil); err != nil {
		t.Fatal("unavailable identity blocked an explicit legacy content clear", err)
	}
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || len(frame.Entries) != 0 || frame.IdentityStatus != ContentIdentityUnavailable ||
		frame.DomainID != "" {
		t.Fatal("ordinary clear silently established a new identity domain", frame, err)
	}
}
