package memory

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/memory/testdata/lineagefixture"
)

// overdueContent leaves key with a compaction whose archive confirmation is
// past its deadline, the state that used to lock a session for good.
func overdueContent(t *testing.T, key string) (*JSONLStore, ContentTransition) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, _, transition := pendingContent(t, s, key)
	if _, err = lineagefixture.LegacyArchiveDeadline(dir, key, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if s, err = NewJSONLStore(dir); err != nil {
		t.Fatal(err)
	}
	return s, transition
}

func compactActiveWindow(t *testing.T, s *JSONLStore, key string) (ContentTransition, error) {
	t.Helper()
	if err := s.AddMessage(t.Context(), key, "user", "after the overdue archive"); err != nil {
		t.Fatal(err)
	}
	frame, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	refs := frameRefs(frame)
	return s.CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
		MutationID: uuid.NewString(), DomainID: frame.DomainID, BeforeState: frame.StateToken,
		SummaryInputs: refs[:1], Retained: refs[1:], Summary: "summary after release",
	})
}

func TestOverdueCompactionIsReleasedByTheOperationThatMeetsIt(t *testing.T) {
	operations := map[string]func(*testing.T, *JSONLStore, string) error{
		"compaction": func(t *testing.T, s *JSONLStore, key string) error {
			_, err := compactActiveWindow(t, s, key)
			return err
		},
		"truncate": func(t *testing.T, s *JSONLStore, key string) error {
			return s.TruncateHistory(t.Context(), key, 1)
		},
		"set history": func(t *testing.T, s *JSONLStore, key string) error {
			return s.SetHistory(t.Context(), key, nil)
		},
		"delete": func(t *testing.T, s *JSONLStore, key string) error {
			return s.DeleteSession(t.Context(), key)
		},
		"edit": func(t *testing.T, s *JSONLStore, key string) error {
			frame, err := s.ReadHistoryContentFrame(t.Context(), key)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.CommitHistoryContentMutation(t.Context(), key, typedPrefixPlan(frame, 1))
			return err
		},
	}
	for name, operation := range operations {
		t.Run(name, func(t *testing.T) {
			key := "sk_v1_overdue_" + uuid.NewString()[:8]
			s, overdue := overdueContent(t, key)
			if err := operation(t, s, key); err != nil {
				t.Fatal("overdue archive still refused the operation", err)
			}
			pending, err := s.ReadPendingHistoryCompaction(context.Background(), key)
			if pending.MutationID == overdue.MutationID {
				t.Fatal("overdue archive still holds the session", err)
			}
		})
	}
}

func TestPendingCompactionWithinDeadlineStillHoldsTheSession(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_pending_within_deadline"
	pendingContent(t, s, key)
	if err = s.TruncateHistory(t.Context(), key, 1); err != ErrContentLineagePending {
		t.Fatal("a pending archive lost its confirmation window", err)
	}
	if _, err = compactActiveWindow(t, s, key); err != ErrContentLineagePending {
		t.Fatal("a second compaction overtook an archive within its deadline", err)
	}
}

func TestSaveKeepsTheRowsOfAReleasedCompaction(t *testing.T) {
	key := "sk_v1_overdue_save"
	s, _ := overdueContent(t, key)
	before, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	transition, err := compactActiveWindow(t, s, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, ContentArchiveAck{
		DomainID: transition.DomainID, MutationID: transition.MutationID,
		BeforeState: transition.BeforeState, AfterState: transition.AfterState,
		Archived: transition.Dropped, ArchiveCommitID: uuid.NewString(),
	}); err != nil {
		t.Fatal(err)
	}
	if err = s.CompactForSessionSave(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(after, before) {
		t.Fatal("Save dropped rows whose archive was never confirmed", err)
	}
}
