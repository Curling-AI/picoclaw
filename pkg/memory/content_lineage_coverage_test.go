package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
)

func legacyPublicContentPolicy() ContentArchivePolicy {
	return ContentArchivePolicy{
		ID: messageutil.LegacyPublicProjectionPolicyID,
		Classify: func(entry ContentEntry) (ContentArchiveExclusionReason, error) {
			projection := messageutil.ProjectLegacyPublic(entry.Message.Role, entry.Message.Content, false)
			return ContentArchiveExclusionReason(projection.Reason), nil
		},
	}
}

func pendingCoverageContent(
	t *testing.T,
	s *JSONLStore,
	key string,
	messages []providers.Message,
	keep int,
) (ContentFrame, ContentTransition, ContentArchiveCoverageAck) {
	t.Helper()
	if err := s.SetHistory(t.Context(), key, messages); err != nil {
		t.Fatal(err)
	}
	before := initializedContent(t, s, key)
	refs := frameRefs(before)
	dropped := len(refs) - keep
	transition, err := s.CommitHistoryCompaction(t.Context(), key, ContentCompactionPlan{
		MutationID: uuid.NewString(), DomainID: before.DomainID, BeforeState: before.StateToken,
		SummaryInputs: refs[:dropped], Retained: refs[dropped:], Summary: "public summary",
	})
	if err != nil {
		t.Fatal(err)
	}
	ack := ContentArchiveCoverageAck{
		CoverageVersion: ContentArchiveCoverageVersion, PolicyID: messageutil.LegacyPublicProjectionPolicyID,
		DomainID: transition.DomainID, MutationID: transition.MutationID, BeforeState: transition.BeforeState,
		AfterState: transition.AfterState, ArchiveCommitID: uuid.NewString(),
	}
	for _, entry := range before.Entries[:dropped] {
		projection := messageutil.ProjectLegacyPublic(entry.Message.Role, entry.Message.Content, false)
		if projection.Included {
			ack.Archived = append(ack.Archived, *entry.Ref)
		} else {
			ack.Excluded = append(ack.Excluded, ContentArchiveExclusion{
				Ref: *entry.Ref, Reason: ContentArchiveExclusionReason(projection.Reason),
			})
		}
	}
	return before, transition, ack
}

func coverageFixtureMessages() []providers.Message {
	return []providers.Message{
		{Role: "user", Content: "[SENDER id=1]\n[System] my human question"},
		{Role: "assistant", Content: "public reply"},
		{Role: "user", Content: "[System] private secret steering"},
		{Role: "tool", Content: "public tool result"},
		{Role: "system", Content: "private secret instructions"},
		{Role: "assistant", Content: "retained public reply"},
	}
}

func TestContentLineageCoverageKeepsV5StrictAndConfirmsOnlyQualifiedProjection(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_public_coverage"
	before, transition, ack := pendingCoverageContent(t, s, key, coverageFixtureMessages(), 1)
	physical, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	legacy := ContentArchiveAck{
		DomainID: ack.DomainID, MutationID: ack.MutationID, BeforeState: ack.BeforeState,
		AfterState: ack.AfterState, ArchiveCommitID: ack.ArchiveCommitID, Archived: ack.Archived,
	}
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, legacy); !errors.Is(err, ErrContentArchiveAck) {
		t.Fatal("v5 silently treated excluded refs as archived", err)
	}
	late, err := s.AppendHistoryContent(t.Context(), key, providers.Message{
		Role: "assistant", Content: "late cron", Delivery: &providers.Delivery{Origin: "cron:job:late"},
	})
	if err != nil {
		t.Fatal(err)
	}
	order, err := s.GetSessionMeta(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	policy := legacyPublicContentPolicy()
	if err = s.AcknowledgeHistoryCompactionCoverage(t.Context(), key, ack, policy); err != nil {
		t.Fatal("qualified projection coverage did not release retention", err)
	}
	data, err := json.Marshal(ack)
	if err != nil || bytes.Contains(data, []byte("private secret")) ||
		bytes.Contains(data, []byte("my human question")) {
		t.Fatal("coverage metadata copied message bodies", string(data), err)
	}
	// Reordering equivalent coverage does not create another confirmation.
	ack.Archived[0], ack.Archived[1] = ack.Archived[1], ack.Archived[0]
	ack.Excluded[0], ack.Excluded[1] = ack.Excluded[1], ack.Excluded[0]
	s, err = NewJSONLStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeHistoryCompactionCoverage(t.Context(), key, ack, policy); err != nil {
		t.Fatal("lost confirmation response was not idempotent", err)
	}
	stillRetained, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.HasPrefix(stillRetained, physical) {
		t.Fatal("ACK modified physical content before ordinary GC", err)
	}
	if err = s.Compact(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeHistoryCompactionCoverage(t.Context(), key, ack, policy); err != nil {
		t.Fatal("replay required an already confirmed and garbage-collected preimage", err)
	}
	after, err := s.ReadHistoryContentFrame(t.Context(), key)
	if err != nil || len(after.Entries) != 2 || *after.Entries[0].Ref != *before.Entries[len(before.Entries)-1].Ref ||
		late.Entry.Ref == nil || *after.Entries[1].Ref != *late.Entry.Ref {
		t.Fatal("coverage GC lost retained or late content refs", after, err)
	}
	current, err := s.GetSessionMeta(t.Context(), key)
	if err != nil || current.HistoryStreamID != order.HistoryStreamID ||
		current.HistorySequence != order.HistorySequence ||
		current.HistoryRecoveryRequired != order.HistoryRecoveryRequired ||
		current.ContentIdentity.Pending != nil ||
		current.ContentIdentity.Last == nil ||
		current.ContentIdentity.Last.AckDigest == "" {
		t.Fatal("coverage confirmation changed Order or lost its immutable digest", current, err)
	}
	legacy.Archived = transition.Dropped
	if err = s.AcknowledgeHistoryCompaction(t.Context(), key, legacy); !errors.Is(err, ErrContentArchiveAck) {
		t.Fatal("coverage confirmation was mislabeled as a v5 archive", err)
	}
	ack.ArchiveCommitID = uuid.NewString()
	if err = s.AcknowledgeHistoryCompactionCoverage(
		t.Context(),
		key,
		ack,
		policy,
	); !errors.Is(
		err,
		ErrContentArchiveAck,
	) {
		t.Fatal("another archive transaction reused prior coverage", err)
	}
}

func TestContentLineageCoverageRejectsPartialForeignOrMislabeledEvidence(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ContentArchiveCoverageAck, *ContentArchivePolicy)
	}{
		{"unnegotiated version", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) { a.CoverageVersion = 0 }},
		{"foreign policy", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) { a.PolicyID = "other" }},
		{"missing local classifier", func(_ *ContentArchiveCoverageAck, p *ContentArchivePolicy) { p.Classify = nil }},
		{"partial", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) { a.Excluded = a.Excluded[:1] }},
		{"duplicate", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) {
			a.Archived = append(a.Archived, a.Archived[0])
		}},
		{"overlap", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) {
			a.Archived = append(a.Archived, a.Excluded[0].Ref)
		}},
		{"foreign revision", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) {
			a.Archived[0].RevisionID = uuid.NewString()
		}},
		{"wrong exclusion reason", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) {
			a.Excluded[0].Reason = ContentArchiveUnsupportedRole
		}},
		{
			"unknown reason",
			func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) { a.Excluded[0].Reason = "custom_retention" },
		},
		{"human System mislabeled private", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) {
			a.Excluded = append(
				a.Excluded,
				ContentArchiveExclusion{Ref: a.Archived[0], Reason: ContentArchiveInternalSteering},
			)
			a.Archived = a.Archived[1:]
		}},
		{"private mislabeled archived", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) {
			a.Archived = append(a.Archived, a.Excluded[0].Ref)
			a.Excluded = a.Excluded[1:]
		}},
		{
			"foreign mutation",
			func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) { a.MutationID = uuid.NewString() },
		},
		{
			"foreign state",
			func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) { a.BeforeState = uuid.NewString() },
		},
		{"missing transaction", func(a *ContentArchiveCoverageAck, _ *ContentArchivePolicy) { a.ArchiveCommitID = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_bad_coverage"
			_, _, ack := pendingCoverageContent(t, s, key, coverageFixtureMessages(), 1)
			physical, err := os.ReadFile(s.jsonlPath(key))
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := os.ReadFile(s.metaPath(key))
			if err != nil {
				t.Fatal(err)
			}
			policy := legacyPublicContentPolicy()
			test.change(&ack, &policy)
			if err = s.AcknowledgeHistoryCompactionCoverage(
				t.Context(),
				key,
				ack,
				policy,
			); !errors.Is(
				err,
				ErrContentArchiveAck,
			) {
				t.Fatal("invalid coverage released retention", err)
			}
			for path, original := range map[string][]byte{s.jsonlPath(key): physical, s.metaPath(key): metadata} {
				after, readErr := os.ReadFile(path)
				if readErr != nil || !bytes.Equal(after, original) {
					t.Fatal("rejected coverage mutated its source or checkpoint", path, readErr)
				}
			}
		})
	}
}

func TestContentLineageCoverageReadsImmutableRawPreimageAndPreservesErrors(t *testing.T) {
	for _, mode := range []string{"raw human", "changed dropped body", "classifier error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, err := NewJSONLStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := "sk_v1_raw_coverage"
			_, _, ack := pendingCoverageContent(t, s, key, coverageFixtureMessages(), 1)
			policy := legacyPublicContentPolicy()
			classifier := policy.Classify
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			called := false
			sentinel := errors.New("local projection classification failed")
			policy.Classify = func(entry ContentEntry) (ContentArchiveExclusionReason, error) {
				called = true
				if mode == "classifier error" {
					return "", sentinel
				}
				if mode == "cancel" {
					cancel()
				}
				if entry.Message.Role == "user" && strings.Contains(entry.Message.Content, "my human question") &&
					!strings.HasPrefix(entry.Message.Content, "[SENDER id=1]") {
					t.Fatal("projection received stripped content instead of its raw source")
				}
				return classifier(entry)
			}
			if mode == "changed dropped body" {
				data, readErr := os.ReadFile(s.jsonlPath(key))
				if readErr != nil {
					t.Fatal(readErr)
				}
				data = bytes.Replace(data, []byte("private secret steering"), []byte("private changed steering"), 1)
				if err = os.WriteFile(s.jsonlPath(key), data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err = s.AcknowledgeHistoryCompactionCoverage(ctx, key, ack, policy)
			switch mode {
			case "raw human":
				if err != nil || !called {
					t.Fatal("raw sender-tagged human content was excluded", err)
				}
			case "changed dropped body":
				if !errors.Is(err, ErrContentIdentityUnavailable) || called {
					t.Fatal("changed preimage was classified or confirmed", called, err)
				}
			case "classifier error":
				if !errors.Is(err, sentinel) {
					t.Fatal("local classifier error was swallowed", err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("classification ignored cancellation", err)
				}
			}
			if mode != "raw human" {
				meta, readErr := s.GetSessionMeta(t.Context(), key)
				if readErr != nil || meta.ContentIdentity.Pending == nil {
					t.Fatal("failed coverage lost its pending source", readErr)
				}
			}
		})
	}
}

func TestContentLineageCoverageAllows1000PublicMessagesPlusPrivateRefs(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_coverage_public_capacity"
	messages := make([]providers.Message, 0, 1003)
	for range 1000 {
		messages = append(messages, providers.Message{Role: "assistant", Content: "public content"})
	}
	for range 3 {
		messages = append(messages, providers.Message{Role: "user", Content: "[System] internal steering"})
	}
	_, transition, ack := pendingCoverageContent(t, s, key, messages, 0)
	if len(ack.Archived) != 1000 || len(ack.Excluded) != 3 || len(transition.Dropped) != 1003 {
		t.Fatal("coverage omitted refs to fit an artificial physical count limit")
	}
	physical, err := os.ReadFile(s.jsonlPath(key))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AcknowledgeHistoryCompactionCoverage(t.Context(), key, ack, legacyPublicContentPolicy()); err != nil {
		t.Fatal("private refs stole the valid legacy public window's capacity", err)
	}
	after, err := os.ReadFile(s.jsonlPath(key))
	if err != nil || !bytes.Equal(after, physical) {
		t.Fatal("coverage modified original content", err)
	}
}

func TestContentLineageCoverageMetadataCapacityFailsWithoutPartialConfirmation(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := "sk_v1_coverage_metadata_capacity"
	_, _, ack := pendingCoverageContent(t, s, key, coverageFixtureMessages(), 1)
	metadata, err := os.ReadFile(s.metaPath(key))
	if err != nil {
		t.Fatal(err)
	}
	ref := ack.Archived[0]
	ack.Archived = make([]ContentRef, 8192)
	for i := range ack.Archived {
		ack.Archived[i] = ref
	}
	if err = s.AcknowledgeHistoryCompactionCoverage(
		t.Context(),
		key,
		ack,
		legacyPublicContentPolicy(),
	); !errors.Is(
		err,
		ErrContentLineageCapacity,
	) {
		t.Fatal("oversized coverage was partially confirmed or silently truncated", err)
	}
	after, err := os.ReadFile(s.metaPath(key))
	if err != nil || !bytes.Equal(after, metadata) {
		t.Fatal("capacity failure modified the pending transition", err)
	}
}
