package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// RecordPublicMedia commits only a completed media delivery, never token or
// partial progress. The run must already own a confirmed source occurrence.
func (s *JSONLStore) RecordPublicMedia(ctx context.Context, key, runID, content string) error {
	if uuid.Validate(runID) != nil || content == "" || len(content) > PublicHistoryMessageBytes {
		return ErrPublicHistoryCoverage
	}
	for attempt := 0; attempt < 2; attempt++ {
		history, err := s.ReadPublicHistory(ctx, key)
		if err != nil {
			return err
		}
		var anchor *PublicHistoryEntry
		afterID := ""
		for index := range history.Entries {
			entry := &history.Entries[index]
			if entry.RunID != runID {
				continue
			}
			afterID = entry.ID
			if entry.ImportID == "" {
				anchor = entry
			}
		}
		if anchor == nil {
			return ErrPublicHistoryCoverage
		}
		digest := sha256.Sum256([]byte(runID + ":" + content))
		importID := "media:" + hex.EncodeToString(digest[:])
		entry := PublicHistoryEntry{ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(importID)).String(), RunID: runID, ImportID: importID, AnchorID: anchor.ID, AnchorOrder: anchor.Order, Message: providers.Message{Role: "assistant", Content: content}}
		_, err = s.ImportPublicHistory(ctx, PublicHistoryImport{Key: key, Generation: history.Generation, Revision: history.Revision, AfterID: afterID, Entries: []PublicHistoryEntry{entry}})
		if !errors.Is(err, ErrPublicHistoryCoverage) {
			return err
		}
	}
	return ErrPublicHistoryCoverage
}

// DeleteSession is an explicit owner action, distinct from automatic pruning.
// Resolve a pending commit first and remove the display source with its model
// source; a failed proof never authorizes resurrecting either one later.
func (s *JSONLStore) DeleteSession(ctx context.Context, key string) error {
	canonical, err := s.canonicalHistoryOrderKey(key)
	if err != nil {
		return err
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.recoverHistoryWrite(canonical); err != nil {
		return err
	}
	meta, err := s.readMeta(canonical)
	if err != nil {
		return err
	}
	if err := validateCanonicalHistoryKey(key, canonical, meta); err != nil {
		return err
	}
	for _, path := range []string{s.metaPath(canonical), s.jsonlPath(canonical), s.publicHistoryPath(canonical), s.historyRecoveryPath(canonical), s.historyDecisionPath(canonical), s.historyOrderWitnessPath(canonical), s.historyRecoveryRequiredPath(canonical)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	sanitized := sanitizeKey(canonical)
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	delete(s.metaCache, sanitized)
	delete(s.historyPending, sanitized)
	delete(s.historyMarkers, sanitized)
	delete(s.historyWitnesses, sanitized)
	return nil
}
