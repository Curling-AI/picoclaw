package memory

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestContentLineageCoveragePreimageAcceptsWritersExistingMutationUUIDContract(t *testing.T) {
	for _, mutation := range []string{uuid.Nil.String(), strings.ToUpper(uuid.NewString())} {
		t.Run(mutation, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_accepted_mutation"
			if err = s.AddMessage(t.Context(), key, "assistant", "public"); err != nil {
				t.Fatal(err)
			}
			before := initializedContent(t, s, key)
			transition, err := s.CommitHistoryCompaction(
				t.Context(),
				key,
				ContentCompactionPlan{
					MutationID:    mutation,
					DomainID:      before.DomainID,
					BeforeState:   before.StateToken,
					SummaryInputs: frameRefs(before),
					Summary:       "public summary",
				},
			)
			if err != nil {
				t.Fatal("writer rejected its established accepted UUID contract", err)
			}
			preimage, err := s.ReadHistoryCompactionFrame(t.Context(), key, transition.MutationID, ContentBefore)
			if err != nil || len(preimage.Entries) != 1 {
				t.Fatal("reader cannot serve a transition accepted by this writer", err)
			}
			ack := ContentArchiveCoverageAck{
				CoverageVersion: ContentArchiveCoverageVersion,
				PolicyID:        legacyPublicContentPolicy().ID,
				DomainID:        transition.DomainID,
				MutationID:      transition.MutationID,
				BeforeState:     transition.BeforeState,
				AfterState:      transition.AfterState,
				Archived:        transition.Dropped,
				ArchiveCommitID: uuid.NewString(),
			}
			if err = s.AcknowledgeHistoryCompactionCoverage(
				t.Context(),
				key,
				ack,
				legacyPublicContentPolicy(),
			); err != nil {
				t.Fatal("coverage cannot confirm a transition accepted by this writer", err)
			}
		})
	}
}
