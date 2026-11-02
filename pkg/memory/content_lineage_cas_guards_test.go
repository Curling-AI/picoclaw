package memory

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestContentLineageSummaryOnlyChangeRejectsStaleCompaction(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_summary_only_cas"
	for range 4 {
		if err = s.AddMessage(t.Context(), key, "user", "content"); err != nil {
			t.Fatal(err)
		}
	}
	frame := initializedContent(t, s, key)
	refs := frameRefs(frame)
	plan := ContentCompactionPlan{
		DomainID: frame.DomainID, BeforeState: frame.StateToken, MutationID: uuid.NewString(),
		SummaryInputs: refs[:2], Retained: refs[2:], Summary: "stale summary",
	}
	if err = s.SetSummary(t.Context(), key, "newer independent summary"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitHistoryCompaction(t.Context(), key, plan); !errors.Is(err, ErrContentStateChanged) {
		t.Fatal("same rows hid an intervening summary mutation", err)
	}
	after, err := os.ReadFile(s.metaPath(key))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("stale compaction changed the winning summary", err)
	}
}

func TestContentLineageV5ACKRejectsWrongArchivedRefs(t *testing.T) {
	for _, mode := range []string{"omitted", "retained", "reordered", "revision"} {
		t.Run(mode, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_ack_exact_refs"
			_, _, transition := pendingContent(t, s, key)
			archived := append([]ContentRef(nil), transition.Dropped...)
			switch mode {
			case "omitted":
				archived = archived[:len(archived)-1]
			case "retained":
				archived[0] = transition.Retained[0]
			case "reordered":
				archived[0], archived[1] = archived[1], archived[0]
			case "revision":
				archived[0].RevisionID = uuid.NewString()
			}
			before, err := os.ReadFile(s.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			ack := ContentArchiveAck{
				DomainID: transition.DomainID, MutationID: transition.MutationID,
				BeforeState: transition.BeforeState, AfterState: transition.AfterState,
				Archived: archived, ArchiveCommitID: uuid.NewString(),
			}
			if err = s.AcknowledgeHistoryCompaction(t.Context(), key, ack); !errors.Is(err, ErrContentArchiveAck) {
				t.Fatal("unrelated archive evidence released exact retained preimage", err)
			}
			after, err := os.ReadFile(s.metaPath(key))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected archive evidence changed pending state", err)
			}
		})
	}
}
