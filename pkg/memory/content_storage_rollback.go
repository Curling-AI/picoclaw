package memory

import "context"

// PrepareHistoryContentInlineRollback explicitly converts this source to the
// mutation-aware inline baseline (dfd1c2d), before an operator changes writers.
// It does not make V7 or pre-lineage binaries compatible. No runtime calls it
// automatically, and a missing/corrupt/opaque descriptor refuses conversion.
// Domain, tokens, refs, revisions, physical bytes, Order and retained ACKs keep
// their meanings; only the storage representation and its journaled floor vary.
func (s *JSONLStore) PrepareHistoryContentInlineRollback(ctx context.Context, key string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.contentMutationWriterPreflight(ctx, key); err != nil {
		return err
	}
	image, frame, err := s.contentMutationImage(ctx, key)
	if err != nil {
		return err
	}
	if frame.IdentityStatus != ContentIdentityKnown {
		return ErrContentIdentityUnavailable
	}
	if image.Meta.ContentIdentity.storage == nil {
		return nil
	}
	next := copyMeta(image.Meta)
	next.ContentIdentity.storage.InlineTarget = true
	return s.commitContentMetadata(ctx, key, image, next)
}
