package memory

import (
	"context"
)

func (s *JSONLStore) AcknowledgeHistoryContentMutation(
	ctx context.Context,
	key string,
	ack ContentMutationAppliedAck,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	// Application is a writer and may finish its own prior metadata commit.
	if err := s.contentMutationWriterPreflight(ctx, key); err != nil {
		return err
	}
	meta, err := s.readContentMutationMeta(key)
	if err != nil {
		return err
	}
	state := meta.ContentIdentity
	digest := contentJSONDigest(ack)
	if state.Pending == nil {
		if state.Last != nil && state.Last.Mutation != nil && state.Last.Mutation.AckDigest == digest {
			s.retireAppliedContentImages(ctx, key, state.Last.Mutation.Retired)
			return nil
		}
		return ErrContentMutationAck
	}
	pending := state.Pending.Mutation
	if pending == nil || !matchingContentMutationAck(ack, pending.Transition) {
		return ErrContentMutationAck
	}
	// Exact current physical proof is required to release the retained Before
	// image. The ACK cannot compensate for tampered or unresolved source bytes.
	image, _, err := s.contentMutationImage(ctx, key)
	if err != nil {
		return err
	}
	next := copyMeta(image.Meta)
	next.ContentIdentity.Last = &contentAcknowledged{Mutation: &contentMutationAcknowledged{
		Transition: pending.Transition, PlanDigest: pending.PlanDigest,
		Ack: ack, AckDigest: digest, Retired: pending.Before.Image,
	}}
	next.ContentIdentity.Pending = nil
	if err = s.commitContentMetadata(ctx, key, image, next); err != nil {
		return err
	}
	// The source mutation and application are already durable. An optional
	// retirement failure preserves the descriptor/bytes and must not undo them.
	var previouslyRetired *contentPreimage
	if state.Last != nil && state.Last.Mutation != nil {
		previouslyRetired = state.Last.Mutation.Retired
	}
	s.retireAppliedContentImages(ctx, key, pending.Before.Image, previouslyRetired)
	return nil
}

var _ HistoryContentMutations = (*JSONLStore)(nil)
