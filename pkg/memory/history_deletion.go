package memory

import (
	"context"
	"errors"
	"os"
	"strings"
)

// DeleteSession applies explicit owner intent to this single-writer source.
// Recover the source's own journal before deleting anything; automatic pruning
// and unqualified history replacements retain their stricter retention guards.
func (s *JSONLStore) DeleteSession(ctx context.Context, key string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
		return ErrHistoryOwnershipUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.recoverHistoryWrite(key); err != nil {
		return err
	}
	legacy, err := s.ownedContentNeverInitialized(ctx, key)
	if err != nil {
		return err
	}
	if !legacy {
		if _, err = s.contentIdentityActiveLocked(ctx, key); err != nil {
			return err
		}
	}
	meta, err := s.readRecoveryMeta(key)
	if errors.Is(err, os.ErrNotExist) {
		meta, err = SessionMeta{Key: key}, nil
	}
	if err != nil {
		return err
	}
	images, err := s.contentImagesForDeletion(ctx, key, meta)
	if err != nil {
		return err
	}
	s.markHistoryPending(key)
	// Keep the established launcher deletion order: remove the listing before
	// the payload, then retire every proof and journal belonging to this source.
	for _, path := range []string{
		s.metaPath(key), s.jsonlPath(key), s.historyRecoveryPath(key), s.historyDecisionPath(key),
		s.historyRecoveryRequiredPath(key), s.contentDomainPath(key), s.historyOrderWitnessPath(key),
	} {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
	}
	if err = s.deleteContentImages(key, images); err != nil {
		return err
	}
	if meta.ContentIdentity != nil && meta.ContentIdentity.MutationVersion != 0 {
		dir, openErr := os.Open(s.dir)
		if openErr != nil {
			return openErr
		}
		if syncErr := errors.Join(dir.Sync(), dir.Close()); syncErr != nil {
			return syncErr
		}
	}
	sanitized := sanitizeKey(key)
	s.metaMu.Lock()
	delete(s.metaCache, sanitized)
	delete(s.historyPending, sanitized)
	delete(s.historyMarkers, sanitized)
	delete(s.historyWitnesses, sanitized)
	s.metaMu.Unlock()
	return nil
}

// CheckHistoryDeletion resolves this key's pending write before owner-requested
// deletion. Missing capture proof does not override an explicit deletion intent.
// The caller must still exclude concurrent writers; this is not a transaction
// across processes.
func CheckHistoryDeletion(dir, key string) error {
	s := &JSONLStore{
		dir: dir, metaCache: make(map[string]SessionMeta), historyPending: make(map[string]bool),
	}
	return s.recoverHistoryWrite(key)
}

// DeleteJSONLSession lets an owner-facing file gateway reuse the complete
// source lifecycle without scanning or recovering other sessions. As with
// CheckHistoryDeletion, its caller must exclude another source writer.
func DeleteJSONLSession(ctx context.Context, dir, key string) error {
	s := &JSONLStore{
		dir: dir, metaCache: make(map[string]SessionMeta), historyPending: make(map[string]bool),
	}
	return s.DeleteSession(ctx, key)
}
