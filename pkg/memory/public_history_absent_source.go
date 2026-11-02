package memory

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Only this journal's exact OLD/NEW display may be retired after both owner
// metadata and model source were removed externally. An unrelated file stays.
func (s *JSONLStore) retireAbsentHistorySource(ctx context.Context, key string, recovery historyWriteRecovery) error {
	if recovery.Meta.Key != key || (recovery.Next.Key != "" && recovery.Next.Key != key) {
		return ErrHistoryOwnershipUnavailable
	}
	if recovery.PublicMediaCleanup && !s.requiresPublicHistoryCleanup() {
		return errPublicHistoryCleanupUnavailable
	}
	file, dir, err := openContentArtifact(s.dir, filepath.Base(s.publicHistoryPath(key)), false)
	if err == nil {
		defer closeContentArtifact(file, dir)
		identity, identityErr := contentArtifactFileIdentity(file)
		if identityErr != nil {
			return identityErr
		}
		data, readErr := io.ReadAll(
			io.LimitReader(file, max(0, recovery.Meta.PublicHistoryBytes, recovery.Next.PublicHistoryBytes)+1),
		)
		if readErr != nil {
			return readErr
		}
		owned := false
		for _, meta := range []SessionMeta{recovery.Meta, recovery.Next} {
			if meta.Key != key || meta.PublicHistoryVersion != PublicHistoryVersion {
				continue
			}
			if _, decodeErr := decodePublicEntries(data, meta); decodeErr == nil {
				owned = true
				break
			}
		}
		if !owned {
			return ErrPublicHistoryCoverage
		}
		if err = removeContentArtifactIdentity(s.dir, filepath.Base(s.publicHistoryPath(key)), &identity); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	journal, directory, openErr := openContentArtifact(s.dir, filepath.Base(s.historyRecoveryPath(key)), false)
	if openErr != nil {
		return openErr
	}
	defer closeContentArtifact(journal, directory)
	if err = directory.Sync(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if recovery.PublicMediaCleanup {
		if err = s.deletePublicHistoryArtifacts(key); err != nil {
			return err
		}
	}
	return s.finishHistoryWrite(key, s.historyDecisionPath(key))
}
