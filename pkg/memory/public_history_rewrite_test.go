package memory

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// The aborted-turn restore arrives without occurrence ids (the session
// backend strips them), but an exact suffix cut leaves every retained row at
// its line and bytes: their refs survive and the next compaction can commit.
func TestPublicHistoryExactRollbackKeepsContentIdentity(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, run := "sk_v1_review_abort", uuid.NewString()
	for _, m := range []providers.Message{
		{Role: "user", Content: "A"},
		{Role: "assistant", Content: "answer A"},
		{Role: "user", Content: "B"},
		{Role: "assistant", Content: "answer B"},
	} {
		if err = s.AddFullMessage(providers.WithHistoryRun(t.Context(), run), key, m); err != nil {
			t.Fatal(err)
		}
	}
	initial := initializedContent(t, s, key)
	old, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	for i := range old {
		old[i].HistoryID = ""
	}
	if _, err = s.AppendHistoryContent(
		providers.WithHistoryRun(t.Context(), uuid.NewString()),
		key,
		providers.Message{Role: "user", Content: "aborted C"},
	); err != nil {
		t.Fatal(err)
	}
	if err = s.SetHistory(t.Context(), key, old); err != nil {
		t.Fatal(err)
	}
	h, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil || len(h.Entries) != 4 {
		t.Fatalf("abort changed confirmed display: %+v %v", h, err)
	}
	refs := frameRefs(initial)
	for i, entry := range h.Entries {
		if entry.Ref == nil || *entry.Ref != refs[i] {
			t.Fatal("retained display lost its ref", entry)
		}
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || frame.IdentityStatus != ContentIdentityKnown || !reflect.DeepEqual(frameRefs(frame), refs) {
		t.Fatal("exact rollback lost content identity", frame, err)
	}
	if frame.StateToken == initial.StateToken {
		t.Fatal("rollback reused the pre-append state token")
	}
	readonly, err := ReadPublicHistoryFS(t.Context(), os.DirFS(s.dir), key)
	if err != nil || len(readonly.Entries) != len(h.Entries) {
		t.Fatal("read-only display disagrees after rollback", readonly, err)
	}
	if _, err = contentAPI(t, s).CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
		MutationID:    uuid.NewString(),
		DomainID:      frame.DomainID,
		BeforeState:   frame.StateToken,
		Retained:      refs[2:],
		SummaryInputs: refs[:2],
		Summary:       "turn A",
	}); err != nil {
		t.Fatal("compaction after rollback was refused", err)
	}
}

func TestPublicHistoryRewriteKeepsUnaffectedSupplement(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, run := "agent:web:review-supplement", uuid.NewString()
	for _, m := range []providers.Message{{Role: "user", Content: "A"}, {Role: "assistant", Content: "answer A"}} {
		if err = s.AddFullMessage(providers.WithHistoryRun(t.Context(), run), key, m); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.RecordPublicMedia(t.Context(), key, run, "[A](artifact:version-a.pdf)"); err != nil {
		t.Fatal(err)
	}
	before, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddMessage(t.Context(), key, "user", "aborted B"); err != nil {
		t.Fatal(err)
	}
	old, err := s.GetHistory(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetHistory(t.Context(), key, old[:2]); err != nil {
		t.Fatal(err)
	}
	after, err := s.ReadPublicHistory(t.Context(), key)
	if err != nil || len(after.Entries) != len(before.Entries) {
		t.Fatalf("unrelated cut discarded confirmed supplement: before=%+v after=%+v err=%v", before, after, err)
	}
	if after.Entries[2].ID != before.Entries[2].ID ||
		after.Entries[2].Message.Content != before.Entries[2].Message.Content {
		t.Fatal("supplement identity/content changed")
	}
}

func TestPublicHistoryRewritePreservesUncoveredPhysicalPrefix(t *testing.T) {
	for _, preparing := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "preparing"}[preparing], func(t *testing.T) {
			s, key := legacyPublicFixture(t, 80)
			if preparing {
				if _, operationErr := s.PreparePublicHistory(t.Context(), key, ""); operationErr != nil {
					t.Fatal(operationErr)
				}
			}
			meta, err := s.readMeta(key)
			if err != nil {
				t.Fatal(err)
			}
			meta.Skip = 79
			if err = writeMetaLocked(t, s, key, meta); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			s.SetPublicHistoryProjector(unavailablePublicVerifier{err: errors.New("historical artifact unavailable")})
			if err = s.SetHistory(
				t.Context(),
				key,
				[]providers.Message{{Role: "assistant", Content: "edited retained"}},
			); err != nil {
				t.Fatal("missing display proof blocked context replacement", err)
			}
			after, err := os.ReadFile(s.jsonlPath(key))
			prefix := before
			if err != nil || !bytes.HasPrefix(after, prefix) {
				t.Fatal("context replacement collected unproved prefix", err)
			}
			model, err := s.GetHistory(t.Context(), key)
			if err != nil || len(model) != 1 || model[0].Content != "edited retained" {
				t.Fatal("model replacement missing", model, err)
			}
			s.SetPublicHistoryProjector(nil)
			if _, err = s.ReadPublicHistory(t.Context(), key); !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("fallback invented public coverage", err)
			}
			if _, err = s.ReadHistoryPage(t.Context(), HistoryPageRequest{Key: key}); err == nil {
				t.Fatal("mixed lineage granted a raw cursor")
			}
			if err = s.Compact(t.Context(), key); !errors.Is(err, ErrPublicHistoryCoverage) {
				t.Fatal("fallback authorized GC", err)
			}
			retained, err := os.ReadFile(s.jsonlPath(key))
			if err != nil || !bytes.Equal(after, retained) {
				t.Fatal("refused GC changed raw source", err)
			}
			if preparing {
				if err = s.ClearHistory(t.Context(), key); err != nil {
					t.Fatal("fallback blocked explicit clear", err)
				}
				h, readErr := s.ReadPublicHistory(t.Context(), key)
				if readErr != nil || len(h.Entries) != 0 {
					t.Fatal("clear kept fallback display", h, readErr)
				}
			} else if err = s.DeleteSession(t.Context(), key); err != nil {
				t.Fatal("fallback blocked owning deletion", err)
			}
		})
	}
}

func TestPublicHistoryReplacementAcceptsNilContext(t *testing.T) {
	store, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_nil_replacement"
	if operationErr := store.AddMessage(t.Context(), key, "assistant", "confirmed"); operationErr != nil {
		t.Fatal(operationErr)
	}
	if operationErr := store.SetHistory(
		nil,
		key,
		[]providers.Message{{Role: "assistant", Content: "changed"}},
	); operationErr != nil {
		t.Fatal("legacy nil context was rejected", operationErr)
	}
	history, err := store.GetHistory(t.Context(), key)
	if err != nil || len(history) != 1 || history[0].Content != "changed" {
		t.Fatal("nil replacement lost intended content", history, err)
	}
}

// Intact row locators do not stand in for lost or stale continuity proof: the
// rollback settles with identity unavailable and ordinary appends keep working.
func TestPublicHistoryExactRollbackWithoutContentProofStaysUnavailable(t *testing.T) {
	for _, mode := range []string{"missing witness", "older witness"} {
		t.Run(mode, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key, run := "sk_v1_review_abort_proof", uuid.NewString()
			for _, m := range []providers.Message{{Role: "user", Content: "A"}, {Role: "assistant", Content: "answer A"}} {
				if err = s.AddFullMessage(providers.WithHistoryRun(t.Context(), run), key, m); err != nil {
					t.Fatal(err)
				}
			}
			initializedContent(t, s, key)
			restore, err := s.GetHistory(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			for i := range restore {
				restore[i].HistoryID = ""
			}
			older, err := os.ReadFile(s.contentDomainPath(key))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.AppendHistoryContent(
				providers.WithHistoryRun(t.Context(), uuid.NewString()),
				key,
				providers.Message{Role: "user", Content: "aborted B"},
			); err != nil {
				t.Fatal(err)
			}
			if mode == "missing witness" {
				err = os.Remove(s.contentDomainPath(key))
			} else {
				err = os.WriteFile(s.contentDomainPath(key), older, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if s, err = NewJSONLStore(s.dir); err != nil {
				t.Fatal(err)
			}
			if err = s.SetHistory(t.Context(), key, restore); err != nil {
				t.Fatal("rollback did not settle", err)
			}
			frame, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil || frame.IdentityStatus != ContentIdentityUnavailable {
				t.Fatal("rollback reattested refs without content proof", frame, err)
			}
			if err = s.AddMessage(t.Context(), key, "user", "after rollback"); err != nil {
				t.Fatal("append blocked after rollback", err)
			}
			history, err := s.GetHistory(t.Context(), key)
			if err != nil || len(history) != 3 {
				t.Fatal("rollback or append lost rows", history, err)
			}
		})
	}
}
