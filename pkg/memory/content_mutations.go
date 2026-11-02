package memory

import (
	"context"
	"errors"
	"time"
)

var (
	ErrContentMutationAck         = errors.New("history content application confirmation does not match")
	ErrContentMutationPendingKind = errors.New("history content has another pending mutation kind")
	ErrContentMutationNotRetained = errors.New("history content mutation payload is no longer retained")
)

type ContentMutationKind string

const (
	ContentMutationEdit         ContentMutationKind = "edit"
	ContentMutationRetainPrefix ContentMutationKind = "retain_prefix"
)

type ContentMutationEditIntent struct {
	Entries []ContentEntry `json:"entries"`
	Summary string         `json:"summary"`
}

type ContentMutationRetainPrefixIntent struct {
	Retained []ContentRef `json:"retained"`
}

// ContentMutationPlan is fixed by one command admission before its first
// commit. An absent replay result never authorizes replanning the same ID.
// Only a complete, exact plan can commit a new checked edit or prefix cut.
type ContentMutationPlan struct {
	MutationID   string                             `json:"mutationId"`
	IntentDigest string                             `json:"intentDigest"`
	DomainID     string                             `json:"domainId"`
	BeforeState  string                             `json:"beforeState"`
	Kind         ContentMutationKind                `json:"kind"`
	Edit         *ContentMutationEditIntent         `json:"edit,omitempty"`
	RetainPrefix *ContentMutationRetainPrefixIntent `json:"retainPrefix,omitempty"`
}

// ContentMutationTransition is a frozen content pair, independent of later
// appends and application status. It does not attest producer receipt order.
type ContentMutationTransition struct {
	Version      uint8               `json:"version"`
	CanonicalKey string              `json:"canonicalKey"`
	DomainID     string              `json:"domainId"`
	MutationID   string              `json:"mutationId"`
	IntentDigest string              `json:"intentDigest"`
	Kind         ContentMutationKind `json:"kind"`
	BeforeState  string              `json:"beforeState"`
	AfterState   string              `json:"afterState"`
	Before       []ContentRef        `json:"before"`
	After        []ContentRef        `json:"after"`
	ExpiresAt    time.Time           `json:"expiresAt"`
}

// ContentMutationAppliedAck confirms the authenticated consumer's existing
// projection/identity transaction. It is neither an archive claim for a
// discarded tail nor a user-read confirmation, and cannot release compaction.
type ContentMutationAppliedAck struct {
	IntentDigest    string              `json:"intentDigest"`
	DomainID        string              `json:"domainId"`
	MutationID      string              `json:"mutationId"`
	Kind            ContentMutationKind `json:"kind"`
	BeforeState     string              `json:"beforeState"`
	AfterState      string              `json:"afterState"`
	Before          []ContentRef        `json:"before"`
	After           []ContentRef        `json:"after"`
	AppliedCommitID string              `json:"appliedCommitId"`
}

type ContentMutationStatus string

const (
	ContentMutationPending ContentMutationStatus = "pending"
	ContentMutationApplied ContentMutationStatus = "applied"
)

// ContentMutationResult retains AppliedAck in the bounded Last slot. Its replay
// does not read a preimage that application already allowed the source to collect.
type ContentMutationResult struct {
	Transition ContentMutationTransition  `json:"transition"`
	Status     ContentMutationStatus      `json:"status"`
	AppliedAck *ContentMutationAppliedAck `json:"appliedAck,omitempty"`
}

// HistoryContentMutations is separately negotiated for initialized sources.
// Payload frames never carry ReceiptBinding. No remote call or wait for an ACK
// occurs under the source lock. The existing lineage interface is unchanged.
type HistoryContentMutations interface {
	CommitHistoryContentMutation(
		ctx context.Context,
		key string,
		plan ContentMutationPlan,
	) (ContentMutationResult, error)
	ReadHistoryContentMutation(ctx context.Context, key, mutationID, intentDigest string) (ContentMutationResult, error)
	ReadPendingHistoryContentMutation(ctx context.Context, key string) (ContentMutationResult, error)
	ReadLatestHistoryContentMutation(ctx context.Context, key string) (ContentMutationResult, error)
	ReadHistoryContentMutationFrame(
		ctx context.Context,
		key, mutationID string,
		side ContentTransitionSide,
	) (ContentFrame, error)
	AcknowledgeHistoryContentMutation(ctx context.Context, key string, ack ContentMutationAppliedAck) error
}
