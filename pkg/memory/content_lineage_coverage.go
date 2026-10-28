package memory

import (
	"context"
	"encoding/json"
)

const ContentArchiveCoverageVersion = uint8(1)

type ContentArchiveExclusionReason string

const (
	ContentArchiveUnsupportedRole  ContentArchiveExclusionReason = "unsupported_role"
	ContentArchiveInternalSteering ContentArchiveExclusionReason = "internal_steering"
)

type ContentArchiveExclusion struct {
	Ref    ContentRef                    `json:"ref"`
	Reason ContentArchiveExclusionReason `json:"reason"`
}

// ContentArchiveCoverageAck distinguishes content archived by the existing
// public projection from refs excluded by that same projection. Excluded refs
// carry metadata only and must never be called archived content.
type ContentArchiveCoverageAck struct {
	CoverageVersion uint8                     `json:"coverageVersion"`
	PolicyID        string                    `json:"policyId"`
	DomainID        string                    `json:"domainId"`
	MutationID      string                    `json:"mutationId"`
	BeforeState     string                    `json:"beforeState"`
	AfterState      string                    `json:"afterState"`
	Archived        []ContentRef              `json:"archived"`
	Excluded        []ContentArchiveExclusion `json:"excluded"`
	ArchiveCommitID string                    `json:"archiveCommitId"`
}

// ContentArchivePolicy is selected from trusted local configuration by the
// adapter. It is never supplied by RPC. Classify must be pure, non-reentrant
// and operate on the producer's immutable RAW preimage; an empty reason means
// included by the existing projection. It must not trust remote provenance.
type ContentArchivePolicy struct {
	ID       string                                                    `json:"-"`
	Classify func(ContentEntry) (ContentArchiveExclusionReason, error) `json:"-"`
}

// HistoryContentArchiveCoverage is an optional negotiated capability. The v5
// all-archived ACK and the broad session/store interfaces remain unchanged.
type HistoryContentArchiveCoverage interface {
	AcknowledgeHistoryCompactionCoverage(
		ctx context.Context,
		key string,
		ack ContentArchiveCoverageAck,
		policy ContentArchivePolicy,
	) error
}

func validContentExclusion(reason ContentArchiveExclusionReason) bool {
	return reason == ContentArchiveUnsupportedRole || reason == ContentArchiveInternalSteering
}

func normalizeContentCoverage(
	ack ContentArchiveCoverageAck,
	transition ContentTransition,
) (ContentArchiveCoverageAck, map[ContentRef]ContentArchiveExclusionReason, error) {
	if ack.ArchiveCommitID == "" || len(ack.ArchiveCommitID) > 256 || ack.DomainID != transition.DomainID ||
		ack.MutationID != transition.MutationID || ack.BeforeState != transition.BeforeState ||
		ack.AfterState != transition.AfterState {
		return ContentArchiveCoverageAck{}, nil, ErrContentArchiveAck
	}
	coverage := make(map[ContentRef]ContentArchiveExclusionReason, len(ack.Archived)+len(ack.Excluded))
	for _, ref := range ack.Archived {
		if _, exists := coverage[ref]; exists || !ref.Valid() || ref.DomainID != ack.DomainID {
			return ContentArchiveCoverageAck{}, nil, ErrContentArchiveAck
		}
		coverage[ref] = ""
	}
	for _, excluded := range ack.Excluded {
		if _, exists := coverage[excluded.Ref]; exists || !excluded.Ref.Valid() ||
			excluded.Ref.DomainID != ack.DomainID || !validContentExclusion(excluded.Reason) {
			return ContentArchiveCoverageAck{}, nil, ErrContentArchiveAck
		}
		coverage[excluded.Ref] = excluded.Reason
	}
	if len(coverage) != len(transition.Dropped) {
		return ContentArchiveCoverageAck{}, nil, ErrContentArchiveAck
	}
	normalized := ack
	normalized.Archived = make([]ContentRef, 0, len(ack.Archived))
	normalized.Excluded = make([]ContentArchiveExclusion, 0, len(ack.Excluded))
	seen := make(map[ContentRef]bool, len(transition.Dropped))
	for _, ref := range transition.Dropped {
		reason, exists := coverage[ref]
		if !exists || seen[ref] {
			return ContentArchiveCoverageAck{}, nil, ErrContentArchiveAck
		}
		seen[ref] = true
		if reason == "" {
			normalized.Archived = append(normalized.Archived, ref)
		} else {
			normalized.Excluded = append(normalized.Excluded, ContentArchiveExclusion{Ref: ref, Reason: reason})
		}
	}
	return normalized, coverage, nil
}

func (s *JSONLStore) AcknowledgeHistoryCompactionCoverage(
	ctx context.Context,
	key string,
	ack ContentArchiveCoverageAck,
	policy ContentArchivePolicy,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if ack.CoverageVersion != ContentArchiveCoverageVersion || ack.PolicyID == "" || len(ack.PolicyID) > 128 ||
		ack.PolicyID != policy.ID || policy.Classify == nil {
		return ErrContentArchiveAck
	}
	metadata, err := json.Marshal(ack)
	if err != nil {
		return err
	}
	if len(metadata) > maxContentTransitionMetadata {
		return ErrContentLineageCapacity
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err = s.canonicalContentKey(ctx, key); err != nil {
		return err
	}
	image, _, err := s.contentMutationImage(ctx, key)
	if err != nil {
		return err
	}
	state := image.Meta.ContentIdentity
	if state.Pending == nil {
		if state.Last == nil || state.Last.Mutation != nil {
			return ErrContentArchiveAck
		}
		normalized, _, normalizeErr := normalizeContentCoverage(ack, state.Last.Transition)
		if normalizeErr != nil || state.Last.AckDigest != contentJSONDigest(normalized) {
			return ErrContentArchiveAck
		}
		return nil
	}
	if state.Pending.Mutation != nil {
		return ErrContentArchiveAck
	}
	transition := state.Pending.Transition
	normalized, coverage, err := normalizeContentCoverage(ack, transition)
	if err != nil {
		return err
	}
	before, err := s.contentCompactionSnapshot(ctx, key, image, state.Pending, ContentBefore)
	if err != nil {
		return err
	}
	if len(before.Entries) < len(transition.Dropped) ||
		!sameContentRefs(frameEntryRefs(before.Entries[:len(transition.Dropped)]), transition.Dropped) {
		return ErrContentIdentityUnavailable
	}
	for _, entry := range before.Entries[:len(transition.Dropped)] {
		if err = ctx.Err(); err != nil {
			return err
		}
		reason, classifyErr := policy.Classify(entry)
		if classifyErr != nil {
			return classifyErr
		}
		if (reason != "" && !validContentExclusion(reason)) || coverage[*entry.Ref] != reason {
			return ErrContentArchiveAck
		}
	}
	return s.finalizeContentArchiveAck(ctx, key, image, contentJSONDigest(normalized))
}

func frameEntryRefs(entries []ContentEntry) []ContentRef {
	refs := make([]ContentRef, len(entries))
	for i, entry := range entries {
		if entry.Ref != nil {
			refs[i] = *entry.Ref
		}
	}
	return refs
}

var _ HistoryContentArchiveCoverage = (*JSONLStore)(nil)
