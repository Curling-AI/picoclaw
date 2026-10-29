package memory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func detachedContentMutationPlan(plan ContentMutationPlan) (ContentMutationPlan, string, error) {
	data, err := json.Marshal(plan)
	if err != nil {
		return ContentMutationPlan{}, "", err
	}
	var detached ContentMutationPlan
	if err = json.Unmarshal(data, &detached); err != nil {
		return ContentMutationPlan{}, "", err
	}
	if detached.Edit != nil {
		for i := range detached.Edit.Entries {
			detached.Edit.Entries[i].Message = ordinaryContentMessage(detached.Edit.Entries[i].Message)
			detached.Edit.Entries[i].ReceiptBinding = nil
		}
	}
	return detached, contentJSONDigest(detached), nil
}

func validContentMutationPlan(plan ContentMutationPlan) bool {
	if uuid.Validate(plan.MutationID) != nil || !validHistoryDigest(plan.IntentDigest) ||
		!validHistoryStreamID(plan.DomainID) || !validHistoryStreamID(plan.BeforeState) {
		return false
	}
	switch plan.Kind {
	case ContentMutationEdit:
		return plan.Edit != nil && plan.RetainPrefix == nil
	case ContentMutationRetainPrefix:
		return plan.RetainPrefix != nil && plan.Edit == nil
	default:
		return false
	}
}

func (s *JSONLStore) CommitHistoryContentMutation(
	ctx context.Context,
	key string,
	plan ContentMutationPlan,
) (ContentMutationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	plan, digest, err := detachedContentMutationPlan(plan)
	if err != nil {
		return ContentMutationResult{}, err
	}
	if !validContentMutationPlan(plan) {
		return ContentMutationResult{}, ErrContentStateChanged
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	// A writer may settle its own interrupted commit. Metadata replay below
	// does not reopen an old preimage or rebuild a plan against the current tail.
	if err = s.contentMutationWriterPreflight(ctx, key); err != nil {
		return ContentMutationResult{}, err
	}
	meta, err := s.readContentMutationMeta(key)
	if err != nil {
		return ContentMutationResult{}, err
	}
	if result, found, replayErr := replayContentMutationPlan(meta, plan, digest); found || replayErr != nil {
		return result, replayErr
	}
	image, frame, err := s.contentMutationImage(ctx, key)
	if err != nil {
		return ContentMutationResult{}, err
	}
	if frame.DomainID != plan.DomainID || frame.StateToken != plan.BeforeState {
		return ContentMutationResult{}, ErrContentStateChanged
	}
	next, prepared, err := s.prepareTypedContentMutation(ctx, key, plan, image, frame)
	if err != nil {
		return ContentMutationResult{}, err
	}
	// Build new physical locators BEFORE any durable publication. The rewrite
	// helper will recompute the same locators from these exact prepared rows.
	data, err := marshalHistoryJSONL(prepared, next)
	if err != nil {
		return ContentMutationResult{}, err
	}
	rows, err := contentLocatorsForData(data, next.ContentIdentity.DomainID, 0,
		refsForLocators(next.ContentIdentity.Rows))
	if err != nil {
		return ContentMutationResult{}, err
	}
	next.ContentIdentity.Rows = rows
	transition := ContentMutationTransition{
		Version: ContentLineageVersion, CanonicalKey: key, DomainID: frame.DomainID,
		MutationID: plan.MutationID, IntentDigest: plan.IntentDigest, Kind: plan.Kind,
		BeforeState: frame.StateToken, AfterState: next.ContentIdentity.StateToken,
		Before: refsForLocators(image.Meta.ContentIdentity.Rows), After: refsForLocators(rows),
		ExpiresAt: time.Now().UTC().Add(contentArchiveDeadline),
	}
	previous, existed, err := s.readRawJSONL(key)
	if err != nil {
		return ContentMutationResult{}, err
	}
	if !existed || !contentPreimageMatchesImage(key, image, previous) {
		return ContentMutationResult{}, ErrContentStateChanged
	}
	preimage := &contentPreimage{
		Version: 1, Key: key, DomainID: frame.DomainID, MutationID: plan.MutationID, ID: uuid.NewString(),
		Bytes: int64(len(previous)), Digest: contentDigest(previous),
	}
	pending := &contentMutationPending{
		Transition: transition, PlanDigest: digest,
		Before: contentSnapshot{
			StateToken: frame.StateToken, Summary: image.Meta.Summary, Skip: image.Meta.Skip,
			Rows: append([]contentLocator(nil), image.Meta.ContentIdentity.Rows...), Image: preimage,
		},
		After: contentSnapshot{
			StateToken: transition.AfterState, Summary: next.Summary,
			Rows: append([]contentLocator(nil), rows...),
		},
	}
	next.ContentIdentity.Pending = &contentPending{Mutation: pending}
	size, err := contentPendingMetadataSize(next.ContentIdentity.Pending)
	if err != nil {
		return ContentMutationResult{}, err
	}
	if size > maxContentTransitionMetadata {
		return ContentMutationResult{}, ErrContentLineageCapacity
	}
	if err = ctx.Err(); err != nil {
		return ContentMutationResult{}, err
	}
	// A crash before the source journal preserves an unowned orphan. Only this
	// exact descriptor, later rooted by NEW, authorizes serving or retiring it.
	if err = s.createContentPreimage(ctx, *preimage, previous); err != nil {
		return ContentMutationResult{}, err
	}
	if err = s.commitHistoryRewriteFor(key, prepared, next, true); err != nil {
		return ContentMutationResult{}, err
	}
	// Do not reread a possibly cleanup-fenced source and turn confirmed NEW
	// into an error that would invite a second cut. The pair was frozen above.
	return pendingContentMutationResult(pending), nil
}

func (s *JSONLStore) contentMutationWriterPreflight(ctx context.Context, key string) error {
	// The caller owns this physical source's lock. Resolve ownership before
	// allowing the writer to settle its own journal; pure getters never do so.
	if _, err := s.contentIdentityActiveLocked(ctx, key); err != nil {
		return err
	}
	if _, err := s.readMeta(key); err != nil {
		return err
	}
	return s.canonicalContentKey(ctx, key)
}

func replayContentMutationPlan(
	meta SessionMeta,
	plan ContentMutationPlan,
	digest string,
) (ContentMutationResult, bool, error) {
	state := meta.ContentIdentity
	if state.Pending != nil {
		pending := state.Pending.Mutation
		if pending == nil {
			return ContentMutationResult{}, false, ErrContentMutationPendingKind
		}
		if pending.Transition.MutationID == plan.MutationID {
			if pending.PlanDigest != digest {
				return ContentMutationResult{}, true, ErrContentStateChanged
			}
			return pendingContentMutationResult(pending), true, nil
		}
		return ContentMutationResult{}, false, contentPendingError(state.Pending)
	}
	if state.Last != nil {
		if last := state.Last.Mutation; last != nil && last.Transition.MutationID == plan.MutationID {
			if last.PlanDigest != digest {
				return ContentMutationResult{}, true, ErrContentStateChanged
			}
			return appliedContentMutationResult(last), true, nil
		}
		if state.Last.Transition.MutationID == plan.MutationID {
			return ContentMutationResult{}, true, ErrContentStateChanged
		}
	}
	return ContentMutationResult{}, false, nil
}

func (s *JSONLStore) prepareTypedContentMutation(
	ctx context.Context,
	key string,
	plan ContentMutationPlan,
	image contentImage,
	frame ContentFrame,
) (SessionMeta, []providers.Message, error) {
	meta := copyMeta(image.Meta)
	meta.HistoryRecoveryRequired = meta.HistoryRecoveryRequired || image.WriterRecovery
	if err := s.prepareContentMutationOrderFence(key, &meta); err != nil {
		return SessionMeta{}, nil, err
	}
	if !meta.HistoryRecoveryRequired {
		ordered, _, proofErr := s.readCommittedHistoryOrderOrigins(key, meta, 0, false)
		if proofErr == nil {
			proofErr = validateHistoryMessageOrders(ordered, meta)
		}
		if proofErr != nil {
			if !onlyContentMutationOrderLoss(proofErr) {
				return SessionMeta{}, nil, proofErr
			}
			if err := s.markHistoryRecoveryRequired(key, &meta); err != nil {
				return SessionMeta{}, nil, err
			}
		}
	}
	var entries []ContentEntry
	switch plan.Kind {
	case ContentMutationEdit:
		entries = plan.Edit.Entries
		meta.Summary = plan.Edit.Summary
		if len(entries) != len(frame.Entries) {
			return SessionMeta{}, nil, ErrContentStateChanged
		}
	case ContentMutationRetainPrefix:
		retained := plan.RetainPrefix.Retained
		if len(retained) > len(frame.Entries) ||
			!sameContentRefs(retained, refsForLocators(meta.ContentIdentity.Rows[:len(retained)])) {
			return SessionMeta{}, nil, ErrContentStateChanged
		}
		entries = frame.Entries[:len(retained)]
	default:
		return SessionMeta{}, nil, ErrContentStateChanged
	}
	prepared := make([]providers.Message, len(entries))
	refs := make([]contentLocator, len(entries))
	active := image.activeRows()
	for i, entry := range entries {
		if err := ctx.Err(); err != nil {
			return SessionMeta{}, nil, err
		}
		if entry.Ref == nil || *entry.Ref != *frame.Entries[i].Ref {
			return SessionMeta{}, nil, ErrContentStateChanged
		}
		ref := *entry.Ref
		if contentJSONDigest(ordinaryContentMessage(entry.Message)) != contentJSONDigest(frame.Entries[i].Message) {
			ref.RevisionID = uuid.NewString()
		}
		refs[i].Ref = ref
		prepared[i] = ordinaryContentMessage(entry.Message)
		var raw historyRecord
		if json.Unmarshal(active[i].Raw, &raw) != nil {
			return SessionMeta{}, nil, ErrContentIdentityUnavailable
		}
		prepared[i].HistoryID = decodedHistoryID(
			active[i].Raw,
			filepath.Base(s.jsonlPath(key)),
			int64(active[i].Line-1),
		)
		prepared[i].HistoryRunID = decodedHistoryRunID(active[i].Raw)
		// This helper preserves real Order only for unchanged cores; changed
		// content allocates genuinely or stays without proof in recovery.
		prepared[i] = prepareHistoryMessage(prepared[i], time.Now(), providers.HistoryRun(ctx))
	}
	if err := s.prepareReplacementHistoryOrder(key, prepared, &meta); err != nil {
		return SessionMeta{}, nil, err
	}
	physicalPrefix, err := unchangedPhysicalPrefix(s.jsonlPath(key), meta.Skip, len(active))
	if err != nil {
		return SessionMeta{}, nil, err
	}
	common := 0
	for i, message := range prepared {
		if !sameHistoryCore(message, frame.Entries[i].Message) {
			break
		}
		common++
	}
	ensureHistoryGeneration(&meta, key)
	meta.HistoryParentGeneration = meta.HistoryGeneration
	meta.HistoryPrefix += int64(meta.Skip)
	meta.HistoryRetainedUntil = meta.HistoryPrefix + int64(min(common, physicalPrefix, max(0, meta.Count-meta.Skip)))
	meta.HistoryMutationRunID = providers.HistoryRun(ctx)
	resetHistoryGeneration(&meta)
	meta.Skip, meta.Count = 0, len(prepared)
	meta.UpdatedAt = time.Now()
	meta.HistoryDigest, err = historyDigest(prepared)
	if err != nil {
		return SessionMeta{}, nil, err
	}
	meta.HistoryTip = ""
	if len(prepared) > 0 {
		meta.HistoryTip = prepared[len(prepared)-1].HistoryID
	}
	meta.ContentIdentity.Rows = refs
	meta.ContentIdentity.MutationVersion = contentMutationVersion
	meta.ContentIdentity.StateToken = uuid.NewString()
	meta.ContentIdentity.SummaryDigest = contentDigest([]byte(meta.Summary))
	meta.ContentIdentity.Skip, meta.ContentIdentity.Count = 0, meta.Count
	return meta, prepared, nil
}
