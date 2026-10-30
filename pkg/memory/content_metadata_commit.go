package memory

import (
	"context"
	"errors"
	"os"
)

// Legacy inline sources keep their existing inexpensive metadata path. Once
// typed semantics is active, every logical content change has one journaled
// metadata/checkpoint publication, even when it does not change JSONL bytes.
func (s *JSONLStore) commitLogicalContentMetadata(
	ctx context.Context,
	key string,
	before, next SessionMeta,
) error {
	if before.ContentIdentity == nil ||
		(before.ContentIdentity.MutationVersion == 0 && before.ContentIdentity.storage == nil) {
		return s.writeMeta(key, next)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	image, err := s.readContentImage(ctx, key)
	if err != nil {
		return err
	}
	if !contentCheckpointMatches(image.Meta, before) {
		return ErrContentStateChanged
	}
	frame, err := s.frameForContentImage(ctx, key, image)
	if err != nil {
		return err
	}
	if frame.IdentityStatus != ContentIdentityKnown && next.ContentIdentity != nil {
		next.ContentIdentity.Unavailable = true
	}
	image.WriterRecovery = before.HistoryRecoveryRequired
	return s.commitContentMetadata(ctx, key, image, next)
}

// A warm write-through cache is not authority to repair externally rolled-back
// semantic metadata. Rebase the content-only append on actual OLD metadata and
// latch lost order before writing; never reuse a cached counter to reattest it.
func (s *JSONLStore) contentAppendMetadata(
	key string,
	original, requested SessionMeta,
) (SessionMeta, SessionMeta, error) {
	witness, present, err := s.readContentDomain(key)
	if err != nil && !errors.Is(err, ErrContentIdentityUnavailable) {
		return SessionMeta{}, SessionMeta{}, err
	}
	fenced := original.ContentIdentity != nil && original.ContentIdentity.MutationVersion != 0
	if !fenced && (!present || witness.Version != contentCheckpointWitnessVersion) {
		if original.ContentIdentity != nil {
			if proofErr := s.prepareContentMutationOrderFence(key, &requested); proofErr != nil {
				return SessionMeta{}, SessionMeta{}, proofErr
			}
		}
		return original, requested, nil
	}
	actual, readErr := s.readRecoveryMeta(key)
	if errors.Is(readErr, os.ErrNotExist) {
		actual, readErr = SessionMeta{Key: key}, nil
	}
	if readErr != nil {
		return SessionMeta{}, SessionMeta{}, readErr
	}
	if actual.Key == "" {
		actual.Key = key
	}
	drift := !historyCheckpointMatches(actual, original) ||
		((actual.HistoryBytes == nil) != (original.HistoryBytes == nil)) ||
		(actual.HistoryBytes != nil && *actual.HistoryBytes != *original.HistoryBytes)
	if !drift && err == nil && present && contentWitnessMatches(witness, actual) {
		if proofErr := s.prepareContentMutationOrderFence(key, &requested); proofErr != nil {
			return SessionMeta{}, SessionMeta{}, proofErr
		}
		return original, requested, nil
	}
	updatedAt := requested.UpdatedAt
	requested = copyMeta(actual)
	requested.UpdatedAt = updatedAt
	protectedOrder := actual.ContentIdentity != nil && actual.ContentIdentity.MutationVersion != 0
	orderCurrent := err == nil && present && witness.Version == contentCheckpointWitnessVersion &&
		witness.OrderCheckpoint != "" && witness.OrderCheckpoint == contentOrderCheckpointDigest(actual)
	requested.HistoryRecoveryRequired = actual.HistoryRecoveryRequired ||
		((protectedOrder || witness.OrderCheckpoint != "") && !orderCurrent)
	if requested.CreatedAt.IsZero() {
		requested.CreatedAt = updatedAt
	}
	s.storeCachedMeta(sanitizeKey(key), actual)
	return actual, requested, nil
}
