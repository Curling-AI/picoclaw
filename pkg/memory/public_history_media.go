package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

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
			if errors.Is(historyErr, ErrPublicHistoryCoverage) {
				historyErr = s.recordPreparingPublicMedia(ctx, key, runID, content)
				if errors.Is(historyErr, ErrPublicHistoryCoverage) {
					continue
				}
			}
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
		digest := sha256.Sum256([]byte(runID + ":" + anchor.ID + ":" + content))
		importID := "media:" + hex.EncodeToString(digest[:])
		legacyDigest := sha256.Sum256([]byte(runID + ":" + content))
		legacyID := "media:" + hex.EncodeToString(legacyDigest[:])
		for _, previous := range history.Entries {
			// Keep published retry identities only for their original native occurrence.
			if previous.ImportID == legacyID && previous.AnchorID == anchor.ID {
				importID = legacyID
				break
			}
		}
		entry := PublicHistoryEntry{
			ID:          uuid.NewSHA1(uuid.NameSpaceOID, []byte(importID)).String(),
			RunID:       runID,
			ImportID:    importID,
			AnchorID:    anchor.ID,
			AnchorRef:   anchor.Ref,
			AnchorOrder: anchor.Order,
			Message:     providers.Message{Role: "assistant", Content: content},
		}
		_, historyErr = s.importPublicHistory(
			ctx,
			PublicHistoryImport{
				Key:        key,
				Generation: history.Generation,
				Revision:   history.Revision,
				AfterID:    afterID,
				Entries:    []PublicHistoryEntry{entry},
			},
			publicImportNativeMedia,
		)
		if !errors.Is(historyErr, ErrPublicHistoryCoverage) {
			return historyErr
		}
	}
	return ErrPublicHistoryCoverage
}
