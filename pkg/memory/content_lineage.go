package memory

import (
	"context"
	"errors"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ContentLineageVersion identifies content identity, never receipt eligibility.
const ContentLineageVersion = uint8(1)

var (
	ErrContentIdentityUnavailable = errors.New("history content identity unavailable")
	ErrContentStateChanged        = errors.New("history content state changed")
	ErrContentLineagePending      = errors.New("history content compaction awaits archive confirmation")
	ErrContentLineageExpired      = errors.New("history content compaction archive confirmation overdue")
	ErrContentLineageCapacity     = errors.New("history content lineage exceeds operation budget")
	ErrContentArchiveAck          = errors.New("history content archive confirmation does not match")
)

// ContentRef names one revision of an occurrence in an independent content
// domain. Its UUIDs are opaque: they neither encode positions nor prove order.
type ContentRef struct {
	DomainID     string `json:"domainId"`
	OccurrenceID string `json:"occurrenceId"`
	RevisionID   string `json:"revisionId"`
}

func (ref ContentRef) Valid() bool {
	return validHistoryStreamID(ref.DomainID) && validHistoryStreamID(ref.OccurrenceID) &&
		validHistoryStreamID(ref.RevisionID)
}

type ContentIdentityStatus string

const (
	ContentIdentityKnown         ContentIdentityStatus = "known"
	ContentIdentityUninitialized ContentIdentityStatus = "uninitialized"
	ContentIdentityUnavailable   ContentIdentityStatus = "unavailable"
)

// ContentReceiptBinding is optional existing evidence, not a receipt minted by
// content initialization. It is absent whenever Order is unavailable and for
// synthetic legacy IDs. A ContentRef alone can never populate this binding.
type ContentReceiptBinding struct {
	SourceID string            `json:"sourceId"`
	Order    HistoryOrderProof `json:"order"`
}

// ContentEntry wraps ordinary content for authenticated internal adapters.
// Message has the five old history fields cleared. Delivery.Origin remains
// complete (including the execution run), unlike the public job-only DTO.
// These wrappers must never be passed to a provider or a public session DTO.
type ContentEntry struct {
	Message        providers.Message      `json:"message"`
	Ref            *ContentRef            `json:"ref,omitempty"`
	ReceiptBinding *ContentReceiptBinding `json:"receiptBinding,omitempty"`
}

// ContentFrame is one immutable logical view. Observation permits strict CAS
// initialization even before identity exists; it is not an occurrence ID.
// StateToken is opaque and covers the active refs/revisions and summary.
type ContentFrame struct {
	Version          uint8                 `json:"version"`
	CanonicalKey     string                `json:"canonicalKey"`
	IdentityStatus   ContentIdentityStatus `json:"identityStatus"`
	DomainID         string                `json:"domainId,omitempty"`
	StateToken       string                `json:"stateToken,omitempty"`
	Observation      string                `json:"observation"`
	HistoricalBridge bool                  `json:"historicalBridge"`
	Summary          string                `json:"summary"`
	Entries          []ContentEntry        `json:"entries"`
}

// ContentCompactionPlan retains an exact unchanged suffix of a previously read
// frame. Any concurrent mutation rejects the whole plan, including its summary.
// Reusing MutationID replays the same committed result; it never applies twice.
type ContentCompactionPlan struct {
	MutationID    string       `json:"mutationId"`
	DomainID      string       `json:"domainId"`
	BeforeState   string       `json:"beforeState"`
	Retained      []ContentRef `json:"retained"`
	SummaryInputs []ContentRef `json:"summaryInputs"`
	Summary       string       `json:"summary"`
}

// ContentTransition carries metadata only. Payload for either immutable side
// is read separately, so an RPC need not duplicate large pre/post windows.
// Identity and archive confirmation never enable HistoryOrderProof or a read ACK.
type ContentTransition struct {
	Version      uint8        `json:"version"`
	CanonicalKey string       `json:"canonicalKey"`
	DomainID     string       `json:"domainId"`
	MutationID   string       `json:"mutationId"`
	BeforeState  string       `json:"beforeState"`
	AfterState   string       `json:"afterState"`
	Dropped      []ContentRef `json:"dropped"`
	Retained     []ContentRef `json:"retained"`
	ExpiresAt    time.Time    `json:"expiresAt"`
}

type ContentTransitionSide string

const (
	ContentBefore ContentTransitionSide = "before"
	ContentAfter  ContentTransitionSide = "after"
)

// ContentArchiveAck is supplied only after the authenticated consumer's legacy
// archive transaction commits ALL dropped refs. Projections that exclude refs
// require the separate negotiated HistoryContentArchiveCoverage capability.
// HTTP success alone is insufficient. ArchiveCommitID identifies that durable
// confirmation; it does not represent user viewing or producer receipt order.
type ContentArchiveAck struct {
	DomainID        string       `json:"domainId"`
	MutationID      string       `json:"mutationId"`
	BeforeState     string       `json:"beforeState"`
	AfterState      string       `json:"afterState"`
	Archived        []ContentRef `json:"archived"`
	ArchiveCommitID string       `json:"archiveCommitId"`
}

// ContentEditPlan identifies the exact revisions being replaced. An existing
// Ref keeps its occurrence and receives a fresh revision when content changes;
// the same occurrence layout is required. New occurrences use append, and
// arrays without refs are not checked edits.
type ContentEditPlan struct {
	DomainID    string         `json:"domainId"`
	BeforeState string         `json:"beforeState"`
	Entries     []ContentEntry `json:"entries"`
	Summary     string         `json:"summary"`
}

// ContentAppendResult retains the exact committed entry before callbacks can
// schedule other writes. Receipt is absent when the independent order fence is
// closed. DomainID/BeforeState/AfterState describe only this append, not a batch
// containing intervening commits. All three are absent with unproved identity.
type ContentAppendResult struct {
	Entry       ContentEntry             `json:"entry"`
	DomainID    string                   `json:"domainId,omitempty"`
	BeforeState string                   `json:"beforeState,omitempty"`
	AfterState  string                   `json:"afterState,omitempty"`
	Receipt     *CommittedHistoryMessage `json:"-"`
}

type HistoryContentAppender interface {
	AppendHistoryContent(ctx context.Context, key string, message providers.Message) (ContentAppendResult, error)
}

// HistoryContentInputAppender accepts input and may return its independently
// produced order receipt, but never a content Ref, binding or state interval.
// It is an optional wake-up lane; subsequent proof uses a strict content frame.
type HistoryContentInputAppender interface {
	AppendHistoryContentInput(ctx context.Context, key string, message providers.Message) (ContentAppendResult, error)
}

// HistoryContentActivation distinguishes an explicitly initialized source from
// an older caller that merely happens to implement the optional interfaces.
// It inspects metadata/witness presence and refuses unsettled journals; it never
// reads the transcript, repairs a checkpoint, initializes identity or validates
// legacy Count.
type HistoryContentActivation interface {
	HistoryContentIdentityActive(ctx context.Context, key string) (bool, error)
}

// HistoryContentLineage is opt-in; Store and SessionStore are unchanged.
// Getters must not initialize, recover a journal or mutate durable state.
// Initialization is forward-only and cannot reconstruct old delivery bindings.
// An active source holds at most one unacknowledged compaction transition.
type HistoryContentLineage interface {
	ReadHistoryContentFrame(ctx context.Context, key string) (ContentFrame, error)
	InitializeHistoryContentIdentity(ctx context.Context, key, observation string) (ContentFrame, error)
	CommitHistoryContentEdit(ctx context.Context, key string, plan ContentEditPlan) (ContentFrame, error)
	CommitHistoryCompaction(ctx context.Context, key string, plan ContentCompactionPlan) (ContentTransition, error)
	ReadPendingHistoryCompaction(ctx context.Context, key string) (ContentTransition, error)
	ReadHistoryCompactionFrame(
		ctx context.Context,
		key, mutation string,
		side ContentTransitionSide,
	) (ContentFrame, error)
	AcknowledgeHistoryCompaction(ctx context.Context, key string, ack ContentArchiveAck) error
}
