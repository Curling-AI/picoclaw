package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentLineageForwardBaselineDoesNotRewriteOrReattestStrippedHistory(t *testing.T) {
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_stripped_baseline"
	for range 3 {
		if err = s.AddMessage(t.Context(), key, "assistant", "duplicate"); err != nil {
			t.Fatal(err)
		}
	}
	ordinary, err := s.ReadHistoryContent(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	stripped := make([]byte, 0, len(ordinary)*128)
	for _, message := range ordinary {
		data, encodeErr := json.Marshal(message)
		if encodeErr != nil {
			t.Fatal(encodeErr)
		}
		stripped = append(stripped, append(data, '\n')...)
	}
	if err = os.WriteFile(s.jsonlPath(key), stripped, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err = NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	frame := initializedContent(t, s, key)
	if frame.HistoricalBridge || frame.IdentityStatus != ContentIdentityKnown || len(frame.Entries) != 3 {
		t.Fatal("baseline failed to identify only current occurrences", frame)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(stripped, after) {
		t.Fatal("content baseline rewrote or imported private history evidence", err)
	}
	result, err := s.AppendHistoryContent(t.Context(), key, providers.Message{Role: "assistant", Content: "new only"})
	if err != nil || result.Entry.Ref == nil || result.Receipt != nil || result.Entry.ReceiptBinding != nil {
		t.Fatal("baseline repaired old order or blocked content append", result, err)
	}
	if _, err = s.ReadHistoryWithReceipts(t.Context(), key); !errors.Is(err, ErrHistoryReceiptUnavailable) {
		t.Fatal("forward content baseline restored receipt eligibility", err)
	}
}

func TestContentLineageExplicitEditAndEditBackNeverReuseRevision(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_edit_revisions"
	for range 2 {
		if err = s.AddMessage(t.Context(), key, "assistant", "same"); err != nil {
			t.Fatal(err)
		}
	}
	original := initializedContent(t, s, key)
	first := *original.Entries[0].Ref
	second := *original.Entries[1].Ref
	plan := ContentEditPlan{DomainID: original.DomainID, BeforeState: original.StateToken, Entries: original.Entries}
	plan.Entries[0].Message.Content = "changed"
	edited, err := s.CommitHistoryContentEdit(t.Context(), key, plan)
	if err != nil || edited.Entries[0].Ref.OccurrenceID != first.OccurrenceID ||
		edited.Entries[0].Ref.RevisionID == first.RevisionID ||
		*edited.Entries[1].Ref != second {
		t.Fatal("edit did not preserve exact occurrence or renew revision", edited, err)
	}
	plan = ContentEditPlan{DomainID: edited.DomainID, BeforeState: edited.StateToken, Entries: edited.Entries}
	plan.Entries[0].Message.Content = "same"
	back, err := s.CommitHistoryContentEdit(t.Context(), key, plan)
	if err != nil || back.Entries[0].Ref.OccurrenceID != first.OccurrenceID ||
		back.Entries[0].Ref.RevisionID == first.RevisionID ||
		back.Entries[0].Ref.RevisionID == edited.Entries[0].Ref.RevisionID ||
		*back.Entries[1].Ref != second {
		t.Fatal("edit-back revived an older revision", back, err)
	}
}

func TestContentLineageLargeLegacyWindowAndMetadataBudget(t *testing.T) {
	for _, count := range []int{1000, 3000} {
		t.Run(string(rune('0'+count/1000))+"000refs", func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_content_large_window"
			messages := make([]providers.Message, count)
			for i := range messages {
				body := "same"
				if count == 1000 {
					body = strings.Repeat("x", 7*1024)
				}
				messages[i] = providers.Message{Role: "user", Content: body}
			}
			if err = s.SetHistory(t.Context(), key, messages); err != nil {
				t.Fatal(err)
			}
			frame := initializedContent(t, s, key)
			refs := frameRefs(frame)
			plan := ContentCompactionPlan{
				MutationID:    uuid.NewString(),
				DomainID:      frame.DomainID,
				BeforeState:   frame.StateToken,
				SummaryInputs: refs[:count/2],
				Retained:      refs[count/2:],
				Summary:       "same content budget",
			}
			transition, err := s.CommitHistoryCompaction(t.Context(), key, plan)
			if count == 3000 {
				if !errors.Is(err, ErrContentLineageCapacity) {
					t.Fatal("operation metadata exceeded its bound without explicit failure", err)
				}
				current, readErr := s.ReadHistoryContentFrame(t.Context(), key)
				if readErr != nil || current.StateToken != frame.StateToken || len(current.Entries) != count {
					t.Fatal("capacity failure changed durable content", current.StateToken, readErr)
				}
				return
			}
			if err != nil || len(transition.Retained) != 500 {
				t.Fatal("new refs reduced a valid legacy 4-8MiB window", err)
			}
			pre, err := s.ReadHistoryCompactionFrame(t.Context(), key, transition.MutationID, ContentBefore)
			if err != nil || len(pre.Entries) != count || len(pre.Entries[999].Message.Content) != 7*1024 {
				t.Fatal("large preimage was truncated or unavailable", err)
			}
			encoded, err := json.Marshal(pre)
			if err != nil || len(encoded) < 4<<20 || len(encoded) > 8<<20 {
				t.Fatal("large frame fixture did not exercise its promised envelope", len(encoded), err)
			}
		})
	}
}

func TestContentLineageAppendCannotReuseCallerHistoryIdentity(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_content_append_identity"
	initializedContent(t, s, key)
	callerID := uuid.NewString()
	result, err := s.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "new occurrence", HistoryID: callerID},
	)
	if err != nil || result.Receipt == nil || result.Receipt.Message.HistoryID == callerID {
		t.Fatal("append accepted a caller's prior persistent identity", result, err)
	}
}

func TestContentLineagePreservesMessageAndOrdinaryDecoderBudgets(t *testing.T) {
	for _, bodySize := range []int{HistoryMessageBytes - 1024, 9 << 20} {
		t.Run(string(rune('0'+bodySize/(1<<20)))+"MiBbody", func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_content_body_budget"
			body := strings.Repeat("x", bodySize)
			if err = s.AddMessage(t.Context(), key, "tool", body); err != nil {
				t.Fatal(err)
			}
			frame := initializedContent(t, s, key)
			if len(frame.Entries) != 1 || frame.Entries[0].Message.Content != body {
				t.Fatal("new refs reduced the ordinary decoder or message budget")
			}
			encoded, err := json.Marshal(frame.Entries[0].Message)
			if err != nil || (bodySize < HistoryMessageBytes && len(encoded) > HistoryMessageBytes) {
				t.Fatal("refs consumed the ordinary message budget", len(encoded), err)
			}
		})
	}
}
