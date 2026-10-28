package memory

import (
	"context"

	"github.com/google/uuid"
)

// Metadata replay is closed over the retained operation record. In particular,
// applied Last does not depend on a payload image already retired after ACK.
// This is not a content frame or a new receipt attestation.
func (s *JSONLStore) readContentMutationMeta(key string) (SessionMeta, error) {
	meta, _, err := s.readContentMeta(key)
	if err != nil {
		return SessionMeta{}, err
	}
	state := meta.ContentIdentity
	witness, present, err := s.readContentDomain(key)
	if err != nil {
		return SessionMeta{}, err
	}
	if state == nil || state.Unavailable || state.Version != ContentLineageVersion ||
		!validHistoryStreamID(state.DomainID) || !validHistoryStreamID(state.StateToken) ||
		!validContentMutationState(state, key) || !present || !contentWitnessMatches(witness, meta) ||
		state.Count != meta.Count || state.Skip != meta.Skip ||
		state.SummaryDigest != contentDigest([]byte(meta.Summary)) {
		return SessionMeta{}, ErrContentIdentityUnavailable
	}
	return meta, nil
}

func (s *JSONLStore) ReadHistoryContentMutation(
	ctx context.Context,
	key, mutationID, intentDigest string,
) (ContentMutationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if uuid.Validate(mutationID) != nil || !validHistoryDigest(intentDigest) {
		return ContentMutationResult{}, ErrContentStateChanged
	}
	return s.readHistoryContentMutation(ctx, key, mutationID, intentDigest, false, false)
}

func (s *JSONLStore) ReadPendingHistoryContentMutation(ctx context.Context, key string) (ContentMutationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.readHistoryContentMutation(ctx, key, "", "", true, false)
}

func (s *JSONLStore) ReadLatestHistoryContentMutation(ctx context.Context, key string) (ContentMutationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.readHistoryContentMutation(ctx, key, "", "", false, true)
}

func (s *JSONLStore) readHistoryContentMutation(
	ctx context.Context,
	key, mutationID, intentDigest string,
	onlyPending, latest bool,
) (ContentMutationResult, error) {
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return ContentMutationResult{}, err
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	meta, err := s.readContentMutationMeta(key)
	if err != nil {
		return ContentMutationResult{}, err
	}
	state := meta.ContentIdentity
	if state.Pending != nil {
		pending := state.Pending.Mutation
		if pending == nil {
			return ContentMutationResult{}, ErrContentMutationPendingKind
		}
		if !onlyPending && !latest && pending.Transition.MutationID != mutationID {
			return ContentMutationResult{}, contentPendingError(state.Pending)
		}
		if !onlyPending && !latest && pending.Transition.IntentDigest != intentDigest {
			return ContentMutationResult{}, ErrContentStateChanged
		}
		return pendingContentMutationResult(pending), ctx.Err()
	}
	if !onlyPending && state.Last != nil && state.Last.Mutation != nil {
		last := state.Last.Mutation
		if latest || last.Transition.MutationID == mutationID {
			if !latest && last.Transition.IntentDigest != intentDigest {
				return ContentMutationResult{}, ErrContentStateChanged
			}
			return appliedContentMutationResult(last), ctx.Err()
		}
	}
	return ContentMutationResult{}, ctx.Err()
}

func (s *JSONLStore) ReadHistoryContentMutationFrame(
	ctx context.Context,
	key, mutationID string,
	side ContentTransitionSide,
) (ContentFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return ContentFrame{}, err
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	meta, err := s.readContentMutationMeta(key)
	if err != nil {
		return ContentFrame{}, err
	}
	if meta.ContentIdentity.Pending == nil {
		return ContentFrame{}, ErrContentMutationNotRetained
	}
	pending := meta.ContentIdentity.Pending.Mutation
	if pending == nil {
		return ContentFrame{}, ErrContentMutationPendingKind
	}
	if pending.Transition.MutationID != mutationID {
		return ContentFrame{}, contentPendingError(meta.ContentIdentity.Pending)
	}
	// Validate the current physical prefix as well as the selected frozen
	// image; neither a current append nor a metadata replay proves its bytes.
	image, err := s.readContentImage(ctx, key)
	if err != nil {
		return ContentFrame{}, err
	}
	current, err := s.frameForContentImage(ctx, key, image)
	if err != nil {
		return ContentFrame{}, err
	}
	if current.IdentityStatus != ContentIdentityKnown {
		return ContentFrame{}, ErrContentIdentityUnavailable
	}
	var snapshot contentSnapshot
	var rows []contentRawRow
	switch side {
	case ContentBefore:
		snapshot = pending.Before
		data, readErr := s.readContentPreimage(ctx, key, pending.Transition.DomainID, mutationID, snapshot.Image)
		if readErr != nil {
			return ContentFrame{}, readErr
		}
		rows, err = contentRowsFromPreimage(ctx, data)
		if err != nil {
			return ContentFrame{}, err
		}
	case ContentAfter:
		snapshot, rows = pending.After, image.Rows
	default:
		return ContentFrame{}, ErrContentStateChanged
	}
	return frozenContentMutationFrame(ctx, key, meta.ContentIdentity.DomainID, snapshot, rows)
}

func frozenContentMutationFrame(
	ctx context.Context,
	key, domain string,
	snapshot contentSnapshot,
	rows []contentRawRow,
) (ContentFrame, error) {
	byLine := make(map[int]contentRawRow, len(rows))
	for _, row := range rows {
		byLine[row.Line] = row
	}
	frame := ContentFrame{
		Version: ContentLineageVersion, CanonicalKey: key, IdentityStatus: ContentIdentityKnown,
		DomainID: domain, StateToken: snapshot.StateToken, Summary: snapshot.Summary,
		Entries: make([]ContentEntry, len(snapshot.Rows)),
	}
	for i, locator := range snapshot.Rows {
		if err := ctx.Err(); err != nil {
			return ContentFrame{}, err
		}
		row, present := byLine[locator.Line]
		if !present || locator.Line <= snapshot.Skip || !locator.Ref.Valid() || locator.Ref.DomainID != domain ||
			contentDigest(row.Raw) != locator.Digest {
			return ContentFrame{}, ErrContentIdentityUnavailable
		}
		ref := locator.Ref
		// This frozen pair carries no live ReceiptBinding, including for an
		// unchanged core that still has genuine evidence in the current source.
		frame.Entries[i] = ContentEntry{Message: ordinaryContentMessage(row.Message), Ref: &ref}
	}
	return frame, nil
}
