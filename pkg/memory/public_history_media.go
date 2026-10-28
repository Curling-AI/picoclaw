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
		history, historyErr := s.ReadPublicHistory(ctx, key)
		if historyErr != nil {
			return historyErr
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
		entry := PublicHistoryEntry{
			ID:          uuid.NewSHA1(uuid.NameSpaceOID, []byte(importID)).String(),
			RunID:       runID,
			ImportID:    importID,
			AnchorID:    anchor.ID,
			AnchorOrder: anchor.Order,
			Message:     providers.Message{Role: "assistant", Content: content},
		}
		_, historyErr = s.ImportPublicHistory(
			ctx,
			PublicHistoryImport{
				Key:        key,
				Generation: history.Generation,
				Revision:   history.Revision,
				AfterID:    afterID,
				Entries:    []PublicHistoryEntry{entry},
			},
		)
		if !errors.Is(historyErr, ErrPublicHistoryCoverage) {
			return historyErr
		}
	}
	return ErrPublicHistoryCoverage
}

// DeleteSession is an explicit owner action, distinct from automatic pruning.
// Resolve a pending commit first and remove the display source with its model
// source; a failed proof never authorizes resurrecting either one later.
func (s *JSONLStore) DeleteSession(ctx context.Context, key string) error {
	canonical, canonicalErr := s.canonicalHistoryOrderKey(key)
	if canonicalErr != nil {
		return canonicalErr
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if operationErr := ctx.Err(); operationErr != nil {
		return operationErr
	}
	if operationErr1 := s.recoverHistoryWrite(canonical); operationErr1 != nil {
		return operationErr1
	}
	meta, canonicalErr := s.readMeta(canonical)
	if canonicalErr != nil {
		return canonicalErr
	}
	if operationErr2 := validateCanonicalHistoryKey(key, canonical, meta); operationErr2 != nil {
		return operationErr2
	}
	for _, path := range []string{s.metaPath(canonical), s.jsonlPath(canonical), s.publicHistoryPath(canonical), s.historyRecoveryPath(canonical), s.historyDecisionPath(canonical), s.historyOrderWitnessPath(canonical), s.historyRecoveryRequiredPath(canonical)} {
		if operationErr3 := os.Remove(path); operationErr3 != nil && !errors.Is(operationErr3, os.ErrNotExist) {
			return operationErr3
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
