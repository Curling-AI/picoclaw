package memory

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestContentInputKeepsQualificationExplicitAndOrderIndependent(t *testing.T) {
	s, key, before := typedMutationSource(t)
	input, err := s.AppendHistoryContentInput(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "accepted input"},
	)
	if err != nil || input.Entry.Message.Content != "accepted input" || input.Entry.Ref != nil ||
		input.Entry.ReceiptBinding != nil ||
		input.DomainID != "" ||
		input.BeforeState != "" ||
		input.AfterState != "" ||
		input.Receipt == nil ||
		!input.Receipt.Order.Valid() {
		t.Fatal("accepted input claimed prefix proof or lost genuine order", input, err)
	}
	current, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || current.IdentityStatus != ContentIdentityKnown || len(current.Entries) != 5 ||
		current.Entries[4].Ref == nil || current.Entries[4].Ref.OccurrenceID == before.Entries[0].Ref.OccurrenceID {
		t.Fatal("strict qualification lost an owned new occurrence", current, err)
	}
	qualified, err := s.AppendHistoryContent(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "qualified input"},
	)
	if err != nil || qualified.Entry.Ref == nil || qualified.Entry.ReceiptBinding == nil ||
		qualified.BeforeState != current.StateToken ||
		qualified.AfterState == current.StateToken ||
		qualified.DomainID != before.DomainID {
		t.Fatal("explicit qualified append was downgraded", qualified, err)
	}
}

func TestContentInputPreservesNewBodyAndClosesFirstTamperedProofOrCut(t *testing.T) {
	s, key, _ := typedMutationSource(t)
	data, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(data, []byte(`"content":"same"`), []byte(`"content":"evil"`), 1)
	if bytes.Equal(data, tampered) || len(data) != len(tampered) {
		t.Fatal("not a same-size earlier edit")
	}
	if err = os.WriteFile(s.jsonlPath(key), tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := s.AppendHistoryContentInput(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "new body remains"},
	)
	if err != nil || input.Entry.Message.Content != "new body remains" || input.Entry.Ref != nil ||
		input.Entry.ReceiptBinding != nil ||
		input.DomainID != "" ||
		input.BeforeState != "" ||
		input.AfterState != "" {
		t.Fatal("ordinary input reattested the old prefix", input, err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityUnavailable || len(frame.Entries) != 5 {
		t.Fatal("first strict read accepted earlier tamper", frame, err)
	}
	for _, entry := range frame.Entries {
		if entry.Ref != nil {
			t.Fatal("tampered prefix issued a qualified ref", entry)
		}
	}
	beforeBody, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	beforeMeta, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.TruncateHistory(t.Context(), key, 2); !errors.Is(err, ErrContentIdentityUnavailable) {
		t.Fatal("first cut hid the unproved prefix", err)
	}
	afterBody, _ := os.ReadFile(s.jsonlPath(key))
	afterMeta, _ := os.ReadFile(s.metaPath(key))
	if !bytes.Equal(beforeBody, afterBody) || !bytes.Equal(beforeMeta, afterMeta) ||
		!bytes.HasPrefix(afterBody, tampered) {
		t.Fatal("failed proof/cut lost old or new content")
	}
}

func TestContentInputWithLostOrderKeepsBodyWithoutReconstructingCounter(t *testing.T) {
	s, key, _ := typedMutationSource(t)
	before, err := s.readRecoveryMeta(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(s.historyOrderWitnessPath(key)); err != nil {
		t.Fatal(err)
	}
	input, err := s.AppendHistoryContentInput(
		t.Context(),
		key,
		providers.Message{Role: "assistant", Content: "visible without receipt"},
	)
	if err != nil || input.Entry.Message.Content != "visible without receipt" || input.Receipt != nil ||
		input.Entry.Ref != nil ||
		input.Entry.ReceiptBinding != nil {
		t.Fatal("missing order blocked content or fabricated confirmation", input, err)
	}
	after, err := s.readRecoveryMeta(key)
	if err != nil || after.HistorySequence != before.HistorySequence || !after.HistoryRecoveryRequired {
		t.Fatal("ordinary lane reconstructed lost highwater", after, err)
	}
	history, err := s.GetHistory(t.Context(), key)
	if err != nil || len(history) != 5 || history[4].Content != "visible without receipt" {
		t.Fatal("unproved durable input disappeared", history, err)
	}
}

func TestContentInputCapabilityDoesNotInitializeLegacySource(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_accepted_never_initialized"
	input, err := s.AppendHistoryContentInput(
		t.Context(),
		key,
		providers.Message{Role: "user", Content: "ordinary legacy body"},
	)
	if err != nil || input.Entry.Ref != nil || input.DomainID != "" {
		t.Fatal(input, err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityUninitialized {
		t.Fatal("capability presence initialized a source", frame, err)
	}
	if _, err = os.Lstat(s.contentDomainPath(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ordinary input created a content witness", err)
	}
}
