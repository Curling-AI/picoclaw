package memory

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
)

const contentArchiveDeadline = 45 * time.Second

// This caps only the one pending operation's identity metadata. Summaries
// remain payload, under the existing adapter's content budget; they do not
// steal capacity from a valid legacy content window.
const maxContentTransitionMetadata = 1 << 20

// Only the proof's exact leaf may turn optional physical GC into a no-op.
// errors.Is alone also matches joins that contain an actual I/O or recovery
// failure; those failures must remain visible to the persistence caller.
func onlyContentIdentityUnavailable(err error) bool {
	if err == nil {
		return false
	}
	switch err.(type) {
	case *os.PathError, *os.LinkError, *os.SyscallError:
		return false
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		causes := wrapped.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !onlyContentIdentityUnavailable(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyContentIdentityUnavailable(wrapped.Unwrap())
	default:
		return err == ErrContentIdentityUnavailable
	}
}

func contentPendingMetadataSize(pending *contentPending) (int, error) {
	metadata := *pending
	metadata.Before.Summary, metadata.After.Summary = "", ""
	if pending.Mutation != nil {
		mutation := *pending.Mutation
		mutation.Before.Summary, mutation.After.Summary = "", ""
		metadata.Mutation = &mutation
	}
	data, err := json.Marshal(metadata)
	return len(data), err
}

func refsForLocators(rows []contentLocator) []ContentRef {
	refs := make([]ContentRef, len(rows))
	for i, row := range rows {
		refs[i] = row.Ref
	}
	return refs
}

func (s *JSONLStore) contentMutationImage(ctx context.Context, key string) (contentImage, ContentFrame, error) {
	writer, err := s.readMeta(key)
	if err != nil {
		return contentImage{}, ContentFrame{}, err
	}
	image, err := s.readContentImage(ctx, key)
	if err != nil {
		return contentImage{}, ContentFrame{}, err
	}
	frame, err := s.frameForContentImage(ctx, key, image)
	if err != nil {
		return contentImage{}, ContentFrame{}, err
	}
	if frame.IdentityStatus != ContentIdentityKnown {
		return contentImage{}, ContentFrame{}, ErrContentIdentityUnavailable
	}
	// Preserve the writer's proof-loss overlay without turning a getter into a
	// marker write. The metadata publication durably retains the existing fence.
	image.WriterRecovery = writer.HistoryRecoveryRequired
	return image, frame, nil
}

func (s *JSONLStore) CommitHistoryCompaction(
	ctx context.Context,
	key string,
	plan ContentCompactionPlan,
) (ContentTransition, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return ContentTransition{}, err
	}
	image, frame, err := s.contentMutationImage(ctx, key)
	if err != nil {
		return ContentTransition{}, err
	}
	state := image.Meta.ContentIdentity
	if state.Pending != nil && state.Pending.Mutation != nil {
		return ContentTransition{}, ErrContentMutationPendingKind
	}
	digest := contentJSONDigest(plan)
	if state.Pending != nil && state.Pending.Transition.MutationID == plan.MutationID {
		if state.Pending.PlanDigest != digest {
			return ContentTransition{}, ErrContentStateChanged
		}
		return state.Pending.Transition, nil
	}
	if state.Last != nil && state.Last.Mutation != nil && state.Last.Mutation.Transition.MutationID == plan.MutationID {
		return ContentTransition{}, ErrContentStateChanged
	}
	if state.Last != nil && state.Last.Mutation == nil && state.Last.Transition.MutationID == plan.MutationID {
		if state.Last.PlanDigest != digest {
			return ContentTransition{}, ErrContentStateChanged
		}
		return state.Last.Transition, nil
	}
	if plan.DomainID != frame.DomainID || plan.BeforeState != frame.StateToken {
		return ContentTransition{}, ErrContentStateChanged
	}
	if state.Pending != nil && !overdueCompaction(state.Pending) {
		return ContentTransition{}, ErrContentLineagePending
	}
	if state.Pending != nil {
		logReleasedCompaction(ctx, state.Pending)
	}
	if uuid.Validate(plan.MutationID) != nil || len(plan.Retained) > len(state.Rows) {
		return ContentTransition{}, ErrContentStateChanged
	}
	dropped := len(state.Rows) - len(plan.Retained)
	// A single Skip can represent an unchanged suffix, never an arbitrary
	// selection. Check exact refs/revisions before writing summary or metadata.
	if !sameContentRefs(plan.Retained, refsForLocators(state.Rows[dropped:])) ||
		!sameContentRefs(plan.SummaryInputs, refsForLocators(state.Rows[:dropped])) {
		return ContentTransition{}, ErrContentStateChanged
	}
	next := copyMeta(image.Meta)
	next.Summary = plan.Summary
	next.UpdatedAt = time.Now()
	if dropped > 0 {
		next.Skip = state.Rows[dropped-1].Line
	}
	afterToken := uuid.NewString()
	transition := ContentTransition{
		Version:      ContentLineageVersion,
		CanonicalKey: key,
		DomainID:     state.DomainID,
		MutationID:   plan.MutationID,
		BeforeState:  state.StateToken,
		AfterState:   afterToken,
		Dropped:      refsForLocators(state.Rows[:dropped]),
		Retained:     refsForLocators(state.Rows[dropped:]),
		ExpiresAt:    time.Now().UTC().Add(contentArchiveDeadline),
	}
	after := contentSnapshot{
		StateToken: afterToken,
		Summary:    plan.Summary,
		Skip:       next.Skip,
		Rows:       append([]contentLocator(nil), state.Rows[dropped:]...),
	}
	next.ContentIdentity.StateToken = afterToken
	next.ContentIdentity.SummaryDigest = contentDigest([]byte(plan.Summary))
	next.ContentIdentity.Skip = next.Skip
	next.ContentIdentity.Rows = append([]contentLocator(nil), after.Rows...)
	next.ContentIdentity.Pending = &contentPending{
		Transition: transition,
		PlanDigest: digest,
		Before: contentSnapshot{
			StateToken: state.StateToken,
			Summary:    image.Meta.Summary,
			Skip:       image.Meta.Skip,
			Rows:       append([]contentLocator(nil), state.Rows...),
		},
		After: after,
	}
	if next.ContentIdentity.ArchivePolicy == nativePublicArchivePolicy {
		proof, proofErr := s.verifyNativeArchive(ctx, key, image.Meta, frame, transition)
		if proofErr != nil {
			return ContentTransition{}, proofErr
		}
		next.ContentIdentity.Pending.NativeArchiveFrontier = &proof.Frontier
	}
	size, err := contentPendingMetadataSize(next.ContentIdentity.Pending)
	if err != nil {
		return ContentTransition{}, err
	}
	if size > maxContentTransitionMetadata {
		return ContentTransition{}, ErrContentLineageCapacity
	}
	if err = s.commitContentMetadata(ctx, key, image, next); err != nil {
		return ContentTransition{}, err
	}
	if next.ContentIdentity.ArchivePolicy == nativePublicArchivePolicy {
		if err := s.prepareNativeHistoryArchiveLocked(ctx, key); err != nil {
			return transition, historyCommittedError(err)
		}
	}
	return transition, nil
}

func sameContentRefs(a, b []ContentRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *JSONLStore) ReadPendingHistoryCompaction(ctx context.Context, key string) (ContentTransition, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return ContentTransition{}, err
	}
	image, err := s.readContentImage(ctx, key)
	if err != nil {
		return ContentTransition{}, err
	}
	frame, err := s.frameForContentImage(ctx, key, image)
	if err != nil {
		return ContentTransition{}, err
	}
	if frame.IdentityStatus != ContentIdentityKnown {
		return ContentTransition{}, ErrContentIdentityUnavailable
	}
	if image.Meta.ContentIdentity.Pending == nil {
		return ContentTransition{}, nil
	}
	if image.Meta.ContentIdentity.Pending.Mutation != nil {
		return ContentTransition{}, ErrContentMutationPendingKind
	}
	transition := image.Meta.ContentIdentity.Pending.Transition
	if time.Now().After(transition.ExpiresAt) {
		return transition, ErrContentLineageExpired
	}
	return transition, nil
}

func (s *JSONLStore) ReadHistoryCompactionFrame(
	ctx context.Context,
	key, mutation string,
	side ContentTransitionSide,
) (ContentFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return ContentFrame{}, err
	}
	image, err := s.readContentImage(ctx, key)
	if err != nil {
		return ContentFrame{}, err
	}
	frame, err := s.frameForContentImage(ctx, key, image)
	if err != nil {
		return ContentFrame{}, err
	}
	if frame.IdentityStatus != ContentIdentityKnown {
		return ContentFrame{}, ErrContentIdentityUnavailable
	}
	state := image.Meta.ContentIdentity
	pending := state.Pending
	if pending != nil && pending.Mutation != nil {
		return ContentFrame{}, ErrContentMutationPendingKind
	}
	if pending == nil || pending.Transition.MutationID != mutation {
		return ContentFrame{}, ErrContentArchiveAck
	}
	return s.contentCompactionSnapshot(ctx, key, image, pending, side)
}

func (s *JSONLStore) contentCompactionSnapshot(
	ctx context.Context,
	key string,
	image contentImage,
	pending *contentPending,
	side ContentTransitionSide,
) (ContentFrame, error) {
	state := image.Meta.ContentIdentity
	transition := pending.Transition
	if pending.Mutation != nil {
		return ContentFrame{}, ErrContentMutationPendingKind
	}
	if transition.Version != ContentLineageVersion || transition.CanonicalKey != key ||
		transition.DomainID != state.DomainID || uuid.Validate(transition.MutationID) != nil ||
		!validHistoryStreamID(transition.BeforeState) || !validHistoryStreamID(transition.AfterState) {
		return ContentFrame{}, ErrContentIdentityUnavailable
	}
	var snapshot contentSnapshot
	switch side {
	case ContentBefore:
		snapshot = pending.Before
	case ContentAfter:
		snapshot = pending.After
	default:
		return ContentFrame{}, ErrContentStateChanged
	}
	if (side == ContentBefore && snapshot.StateToken != pending.Transition.BeforeState) ||
		(side == ContentAfter && snapshot.StateToken != pending.Transition.AfterState) {
		return ContentFrame{}, ErrContentIdentityUnavailable
	}
	byLine := make(map[int]contentRawRow, len(image.Rows))
	for _, row := range image.Rows {
		byLine[row.Line] = row
	}
	result := ContentFrame{
		Version:        ContentLineageVersion,
		CanonicalKey:   key,
		IdentityStatus: ContentIdentityKnown,
		DomainID:       state.DomainID,
		StateToken:     snapshot.StateToken,
		Summary:        snapshot.Summary,
		Entries:        make([]ContentEntry, len(snapshot.Rows)),
	}
	deliveryCounts := image.DeliveryIDCounts
	for i, locator := range snapshot.Rows {
		if err := ctx.Err(); err != nil {
			return ContentFrame{}, err
		}
		row, exists := byLine[locator.Line]
		if !exists || !locator.Ref.Valid() || locator.Ref.DomainID != state.DomainID ||
			contentDigest(row.Raw) != locator.Digest {
			return ContentFrame{}, ErrContentIdentityUnavailable
		}
		ref := locator.Ref
		result.Entries[i] = ContentEntry{Message: checkedDeliveryIdentity(row.Message, deliveryCounts), Ref: &ref}
	}
	return result, nil
}

func (s *JSONLStore) AcknowledgeHistoryCompaction(ctx context.Context, key string, ack ContentArchiveAck) error {
	if ctx == nil {
		ctx = context.Background()
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return err
	}
	image, _, err := s.contentMutationImage(ctx, key)
	if err != nil {
		return err
	}
	state := image.Meta.ContentIdentity
	digest := contentJSONDigest(ack)
	if state.Pending == nil {
		if state.Last != nil && state.Last.Mutation == nil && state.Last.AckDigest == digest {
			return nil
		}
		return ErrContentArchiveAck
	}
	if state.Pending.Mutation != nil {
		return ErrContentArchiveAck
	}
	transition := state.Pending.Transition
	if ack.ArchiveCommitID == "" || len(ack.ArchiveCommitID) > 256 || ack.DomainID != transition.DomainID ||
		ack.MutationID != transition.MutationID ||
		ack.BeforeState != transition.BeforeState ||
		ack.AfterState != transition.AfterState ||
		!sameContentRefs(ack.Archived, transition.Dropped) {
		return ErrContentArchiveAck
	}
	return s.finalizeContentArchiveAck(ctx, key, image, digest)
}

// Each ACK protocol validates its own evidence before this retention-only
// metadata commit. A coverage exclusion never becomes a v5 archived ref.
func (s *JSONLStore) finalizeContentArchiveAck(
	ctx context.Context,
	key string,
	image contentImage,
	digest string,
) error {
	if err := s.verifyPublicPrefix(ctx, key, image.Meta); err != nil {
		return err
	}
	return s.commitContentArchiveAck(ctx, key, image, digest)
}

func (s *JSONLStore) commitContentArchiveAck(ctx context.Context, key string, image contentImage, digest string) error {
	state := image.Meta.ContentIdentity
	next := copyMeta(image.Meta)
	next.ContentIdentity.Last = &contentAcknowledged{
		Transition: state.Pending.Transition,
		PlanDigest: state.Pending.PlanDigest,
		AckDigest:  digest,
	}
	next.ContentIdentity.Pending = nil
	// ACK changes retention only. Its own atomic metadata commit does not mint a
	// content revision, receipt, read watermark or substitute the current tail.
	if err := s.commitContentMetadata(ctx, key, image, next); err != nil {
		return err
	}
	if state.Last != nil && state.Last.Mutation != nil {
		s.retireAppliedContentImages(ctx, key, state.Last.Mutation.Retired)
	}
	return nil
}

// overdueCompaction reports a compaction whose archive confirmation is past its
// deadline. Content sessions never compact physically, so releasing it keeps
// every row on disk and only gives up the archive copy.
func overdueCompaction(pending *contentPending) bool {
	return pending != nil && pending.Mutation == nil && time.Now().After(pending.Transition.ExpiresAt)
}

func logReleasedCompaction(ctx context.Context, pending *contentPending) {
	slog.WarnContext(ctx, "overdue history compaction released",
		"mutation_id", pending.Transition.MutationID,
		"expired_at", pending.Transition.ExpiresAt)
}

// releaseOverdueCompaction clears an overdue compaction so the operation that
// met it proceeds instead of failing. The caller holds the session lock and
// rereads its metadata when this reports a release.
func (s *JSONLStore) releaseOverdueCompaction(ctx context.Context, key string, meta SessionMeta) (bool, error) {
	if meta.ContentIdentity == nil || len(meta.ContentIdentity.opaque) != 0 ||
		!overdueCompaction(meta.ContentIdentity.Pending) {
		return false, nil
	}
	image, _, err := s.contentMutationImage(ctx, key)
	if err != nil {
		return false, err
	}
	pending := image.Meta.ContentIdentity.Pending
	if !overdueCompaction(pending) {
		return false, nil
	}
	next := copyMeta(image.Meta)
	next.ContentIdentity.Pending = nil
	if err = s.commitContentMetadata(ctx, key, image, next); err != nil {
		return false, err
	}
	logReleasedCompaction(ctx, pending)
	return true, nil
}

// metaAfterOverdueCompaction is releaseOverdueCompaction for readMeta callers.
func (s *JSONLStore) metaAfterOverdueCompaction(
	ctx context.Context,
	key string,
	meta SessionMeta,
) (SessionMeta, error) {
	released, err := s.releaseOverdueCompaction(ctx, key, meta)
	if err != nil || !released {
		return meta, err
	}
	return s.readMeta(key)
}

func (s *JSONLStore) contentPendingGuard(meta SessionMeta) error {
	if meta.ContentIdentity != nil && len(meta.ContentIdentity.opaque) != 0 {
		return ErrContentIdentityUnavailable
	}
	if meta.ContentIdentity == nil || meta.ContentIdentity.Pending == nil {
		return nil
	}
	return contentPendingError(meta.ContentIdentity.Pending)
}

func contentPendingError(pending *contentPending) error {
	if time.Now().After(pendingContentDeadline(pending)) {
		return ErrContentLineageExpired
	}
	return ErrContentLineagePending
}

func isContentIdentityLoss(err error) bool { return errors.Is(err, ErrContentIdentityUnavailable) }
