package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
)

// Older projectors latched -1. Recover only a canonical owned source into
// Preparing; neither the disabled version nor a current file proves old media.
func (s *JSONLStore) recoverDisabledPublicMedia(
	ctx context.Context,
	key, run string,
	meta SessionMeta,
) (SessionMeta, error) {
	if _, _, err := s.preparingMediaScope(ctx, key, run, meta, PublicHistoryEntry{}); err != nil {
		return meta, err
	}
	readMeta := meta
	readMeta.PublicHistoryVersion, readMeta.PublicHistoryPreparation = PublicHistoryVersion, nil
	entries, err := s.readPublicEntries(key, readMeta)
	if errors.Is(err, os.ErrNotExist) && meta.PublicHistoryBytes == 0 && meta.PublicHistoryCount == 0 {
		meta.PublicHistoryVersion = 0
		meta.PublicHistoryComplete = false
		if _, err = s.beginPublicPreparation(ctx, key, meta); err != nil {
			return meta, err
		}
		meta, _, err = s.readContentMeta(key)
		return meta, err
	}
	if err != nil {
		return meta, err
	}
	known := make(map[string]PublicHistoryEntry, len(entries))
	for _, entry := range entries {
		if entry.ImportID == "" {
			known[entry.ID] = entry
		}
	}
	file, err := os.Open(s.jsonlPath(key))
	if err != nil {
		return meta, err
	}
	defer file.Close()
	state := &PublicHistoryPreparation{
		Generation: meta.HistoryGeneration, FileGeneration: meta.HistoryFileGeneration,
		Count: int64(meta.Count), Ceiling: *meta.HistoryBytes, Prefix: meta.HistoryPrefix,
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, historyReadBufferBytes), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return meta, err
		}
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			state.Offset += int64(len(scanner.Bytes()))
			continue
		}
		var record historyRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			return meta, ErrPublicHistoryCoverage
		}
		public, visible := publicHistoryMessage(record.Message)
		if visible {
			previous, exists := known[record.ID]
			if !exists || previous.RunID != record.RunID {
				break
			}
			if record.PublicMessage != nil && validPublicProjection(record.Message, *record.PublicMessage) {
				public = *record.PublicMessage
			}
			if contentJSONDigest(public) != contentJSONDigest(previous.Message) {
				break
			}
			if projector := s.historyPublicProjector(); projector != nil {
				if err := projector.Verify(ctx, previous.Message); err != nil {
					break
				}
			}
		}
		state.Offset += int64(len(scanner.Bytes()))
		state.Line++
	}
	if err := scanner.Err(); err != nil {
		return meta, err
	}
	meta.PublicHistoryVersion, meta.PublicHistoryPreparation, meta.PublicHistoryComplete = PublicHistoryVersion, state, false
	if err := s.commitPublicChange(ctx, key, meta, publicHistoryChange{}); err != nil {
		return meta, err
	}
	return meta, nil
}
