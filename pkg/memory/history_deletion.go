package memory

import (
	"context"
	"errors"
	"os"
	"strings"
)

// deleteSessionFilesLocked applies explicit owner intent to one physical source.
// Recover the source's own journal before deleting anything; automatic pruning
// and unqualified history replacements retain their stricter retention guards.
// The caller holds the source lock, including the pair lock used for aliases.
func (s *JSONLStore) deleteSessionFilesLocked(ctx context.Context, key string, ownedAlias ...bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
		return ErrHistoryOwnershipUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.recoverHistoryWrite(key); err != nil {
		return err
	}
	// A prior legacy delete may already have removed this alias's exact files.
	// Its canonical owner intentionally still names the alias until every
	// removal succeeds; that reference must not turn a physical no-op retry
	// into an attempt to resolve/delete the canonical source itself.
	absent := true
	for _, path := range s.historyDeletionSourcePaths(key) {
		if _, err := os.Lstat(path); err == nil {
			absent = false
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if absent {
		s.forgetDeletedHistory(key, false)
		return nil
	}
	legacy, err := s.ownedContentNeverInitialized(ctx, key)
	if err != nil {
		return err
	}
	if !legacy {
		if len(ownedAlias) > 0 && ownedAlias[0] {
			// The canonical owner's exact Aliases entry authorized this physical
			// legacy source under the promotion pair lock. Do not resolve it back
			// to that canonical owner when a failed legacy unlink left only its
			// own witness. Any existing physical metadata must still name key.
			own, present, readErr := s.readContentActivationMetadata(key)
			if readErr != nil {
				return readErr
			}
			if present && own.Key != "" && own.Key != key {
				return ErrHistoryOwnershipUnavailable
			}
		} else {
			if _, err = s.contentIdentityActiveLocked(ctx, key); err != nil {
				return err
			}
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
	artifacts, floorImages, err := s.contentStorageForDeletion(ctx, key, meta)
	if err != nil {
		return err
	}
	images = append(images, floorImages...)
	if len(images)+len(artifacts) > 0 {
		if err = s.beginHistoryDeletion(ctx, key, "owner", images, artifacts); err != nil {
			return err
		}
		return s.recoverHistoryWrite(key)
	}
	s.markHistoryPending(key)
	// Keep every exact ownership descriptor until all payload unlinks and
	// directory durability steps succeed. A failed attempt remains retryable
	// after reopening; never rediscover an unrooted payload by its filename.
	if err = s.deleteContentImages(key, images); err != nil {
		return err
	}
	if err = s.deleteContentStorage(artifacts); err != nil {
		return err
	}
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
