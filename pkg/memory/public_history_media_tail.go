package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
)

func (s *JSONLStore) publicMediaPrefix(key string, meta SessionMeta) (publicIndexHeader, error) {
	state := meta.PublicHistoryPreparation
	if state == nil || state.MediaCount <= 0 || state.MediaCount > meta.PublicHistoryCount ||
		state.MediaOffset < 0 || state.MediaOffset > meta.PublicHistoryBytes {
		return publicIndexHeader{}, ErrPublicHistoryCoverage
	}
	prefix := publicIndexMeta(meta)
	prefix.Bytes, prefix.Count = state.MediaOffset, meta.PublicHistoryCount-state.MediaCount
	prefix.Digest = state.MediaDigest
	if !validHistoryDigest(prefix.Digest) {
		return publicIndexHeader{}, ErrPublicHistoryCoverage
	}
	if prefix.Count == 0 {
		if prefix.Bytes != 0 || prefix.Digest != emptyHistoryDigest {
			return publicIndexHeader{}, ErrPublicHistoryCoverage
		}
		return prefix, nil
	}
	return prefix, nil
}

func (s *JSONLStore) readPublicMediaTail(
	ctx context.Context,
	key string,
	meta SessionMeta,
) ([]PublicHistoryEntry, []byte, publicIndexHeader, error) {
	prefix, err := s.publicMediaPrefix(key, meta)
	if err != nil {
		return nil, nil, prefix, err
	}
	size := meta.PublicHistoryBytes - prefix.Bytes
	if size <= 0 {
		return nil, nil, prefix, ErrPublicHistoryCoverage
	}
	file, err := os.Open(s.publicHistoryPath(key))
	if err != nil {
		return nil, nil, prefix, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != meta.PublicHistoryBytes {
		return nil, nil, prefix, errors.Join(ErrPublicHistoryCoverage, err)
	}
	data := make([]byte, size)
	for offset := int64(0); offset < size; {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, nil, prefix, contextErr
		}
		end := min(size, offset+int64(historyReadBufferBytes))
		if _, err = file.ReadAt(data[offset:end], prefix.Bytes+offset); err != nil {
			return nil, nil, prefix, err
		}
		offset = end
	}
	entries := make([]PublicHistoryEntry, 0)
	digest := prefix.Digest
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'}) {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, nil, prefix, contextErr
		}
		var entry PublicHistoryEntry
		if json.Unmarshal(line, &entry) != nil || !validPublicPageEntry(entry) || entry.ImportID == "" ||
			entry.BoundaryID == "" {
			return nil, nil, prefix, ErrPublicHistoryCoverage
		}
		digest = appendPublicDigest(digest, line)
		entries = append(entries, entry)
	}
	if int64(len(entries)) != meta.PublicHistoryPreparation.MediaCount || digest != meta.PublicHistoryDigest {
		return nil, nil, prefix, ErrPublicHistoryCoverage
	}
	return entries, data, prefix, nil
}

func (s *JSONLStore) preparePublicIndexTail(
	key string,
	original, next SessionMeta,
	change publicHistoryChange,
	header []byte,
) (publicHistoryChange, error) {
	prefix, err := s.publicMediaPrefix(key, original)
	if err != nil || prefix.Bytes != *change.Recovery.Offset {
		return publicHistoryChange{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	records, digest, err := publicIndexRecords(change.Data, prefix)
	if err != nil || digest != next.PublicHistoryDigest {
		return publicHistoryChange{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	index, err := s.openPublicIndex(key, original)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrPublicHistoryCoverage) {
		return change, nil // The existing bounded index-preparation step owns rebuilding it.
	}
	if err != nil {
		return publicHistoryChange{}, err
	}
	defer index.file.Close()
	offset := int64(publicIndexHeaderBytes) + prefix.Count*publicIndexRecordBytes
	old := make([]byte, original.PublicHistoryPreparation.MediaCount*publicIndexRecordBytes)
	if _, err = index.file.ReadAt(old, offset); err != nil {
		return publicHistoryChange{}, err
	}
	oldHeader, err := encodePublicIndexHeader(publicIndexMeta(original))
	if err != nil {
		return publicHistoryChange{}, err
	}
	recovery := *change.Recovery
	recovery.Index = &publicIndexRecovery{Header: oldHeader, Data: old, Offset: &offset, Existed: true}
	change.Recovery = &recovery
	change.Index = &publicIndexChange{Header: header, Records: records, Offset: &offset}
	return change, nil
}

func (s *JSONLStore) preparePublicMediaTail(
	ctx context.Context,
	key string,
	meta SessionMeta,
	state PublicHistoryPreparation,
) (PublicHistoryPreparationResult, error) {
	media, old, prefix, err := s.readPublicMediaTail(ctx, key, meta)
	if err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	if meta.HistoryBytes != nil && *meta.HistoryBytes > state.Ceiling {
		state.Ceiling, state.Count = *meta.HistoryBytes, int64(meta.Count)
	}
	if !meta.publicRefUnavailable && meta.ContentIdentity != nil {
		if err = s.hydrateContentStorage(ctx, key, &meta, true, true); err != nil {
			return PublicHistoryPreparationResult{}, err
		}
	}
	// The bounded step advances new RAW rows. Existing confirmed media is copied
	// separately, as in an explicit import/rewrite, and is not a read-page budget.
	entries, next, err := s.readPublicPreparation(ctx, key, meta, state)
	if err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	entries, err = s.dedupePreparedPublicEntries(ctx, key, prefix, entries)
	if err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	ready := make([]PublicHistoryEntry, 0, len(entries)+len(media))
	remaining := append([]PublicHistoryEntry(nil), media...)
	for _, entry := range entries {
		ready = append(ready, entry)
		for index := 0; index < len(remaining); {
			if remaining[index].BoundaryID != entry.ID {
				index++
				continue
			}
			ready = append(ready, remaining[index])
			remaining = append(remaining[:index], remaining[index+1:]...)
		}
	}
	if next.Offset == next.Ceiling && len(remaining) > 0 {
		return PublicHistoryPreparationResult{}, ErrPublicHistoryCoverage
	}
	readyData, err := encodePublicEntries(ready)
	if err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	data, err := encodePublicEntries(append(ready, remaining...))
	if err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	next.MediaOffset, next.MediaCount = prefix.Bytes+int64(len(readyData)), int64(len(remaining))
	next.MediaDigest, err = publicEntriesDigest(prefix.Digest, ready)
	if err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	if next.MediaCount == 0 {
		next.MediaOffset = 0
		next.MediaDigest = ""
	}
	meta.PublicHistoryPreparation = &next
	if next.Offset == next.Ceiling {
		meta.PublicHistoryPreparation = nil
		meta.PublicHistoryComplete = meta.PublicHistoryComplete || next.Prefix == 0
	}
	meta.PublicHistoryBytes = prefix.Bytes + int64(len(data))
	meta.PublicHistoryCount = prefix.Count + int64(len(ready)+len(remaining))
	meta.PublicHistoryRevision++
	meta.PublicHistoryDigest, err = publicEntriesDigest(prefix.Digest, append(ready, remaining...))
	if err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	if err = s.commitPublicChange(ctx, key, meta, publicHistoryChange{
		Data:     data,
		Recovery: &publicHistoryRecovery{Offset: &prefix.Bytes, Data: old, Existed: true},
	}); err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	return s.publicPreparationResult(ctx, key, meta)
}

func rebasePublicMediaTail(
	original SessionMeta,
	previous, kept []PublicHistoryEntry,
	state *PublicHistoryPreparation,
) error {
	if state.MediaCount == 0 {
		return nil
	}
	if state.MediaCount < 0 || state.MediaCount > int64(len(previous)) {
		return ErrPublicHistoryCoverage
	}
	deferred := make(map[string]bool, state.MediaCount)
	for _, entry := range previous[int64(len(previous))-state.MediaCount:] {
		deferred[entry.ID] = true
	}
	state.MediaCount = 0
	prefix := len(kept)
	for index, entry := range kept {
		if deferred[entry.ID] {
			prefix = min(prefix, index)
			state.MediaCount++
		} else if prefix < index {
			return ErrPublicHistoryCoverage
		}
	}
	state.MediaOffset = 0
	state.MediaDigest = ""
	if state.MediaCount > 0 {
		data, err := encodePublicEntries(kept[:prefix])
		if err != nil {
			return err
		}
		state.MediaOffset = int64(len(data))
		state.MediaDigest, err = publicEntriesDigest(emptyHistoryDigest, kept[:prefix])
		if err != nil {
			return err
		}
	}
	return nil
}

func mergePreparingPublicMedia(
	previous, source []PublicHistoryEntry,
	count int64,
) ([]PublicHistoryEntry, []PublicHistoryEntry, error) {
	if count <= 0 || count > int64(len(previous)) {
		return nil, nil, ErrPublicHistoryCoverage
	}
	prefix := previous[:int64(len(previous))-count]
	media := previous[int64(len(previous))-count:]
	known := make(map[string]bool, len(prefix))
	for _, entry := range prefix {
		known[entry.ID] = true
	}
	missing := make([]PublicHistoryEntry, 0, len(source))
	for _, entry := range source {
		if !known[entry.ID] {
			missing = append(missing, entry)
		}
	}
	ordered := append([]PublicHistoryEntry(nil), prefix...)
	consumed := make(map[string]bool, len(media))
	for _, entry := range missing {
		ordered = append(ordered, entry)
		for _, delivery := range media {
			if delivery.BoundaryID == entry.ID {
				ordered = append(ordered, delivery)
				consumed[delivery.ID] = true
			}
		}
	}
	if len(consumed) != len(media) {
		return nil, nil, ErrPublicHistoryCoverage
	}
	return ordered, nil, nil
}

// This is committed OLD writer input, separately from the bounded new RAW step.
// A streaming scan preserves cancellation without retaining another sidecar copy.
func (s *JSONLStore) readOwnedPublicMediaPrefix(
	ctx context.Context,
	key string,
	prefix publicIndexHeader,
) (map[string]PublicHistoryEntry, error) {
	file, err := os.Open(s.publicHistoryPath(key))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, prefix.Bytes))
	scanner.Buffer(make([]byte, historyReadBufferBytes), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	known := make(map[string]PublicHistoryEntry)
	offset, count, digest := int64(0), int64(0), emptyHistoryDigest
	for scanner.Scan() {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		offset += int64(len(scanner.Bytes()))
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\n'})
		if len(line) == 0 {
			continue
		}
		var entry PublicHistoryEntry
		if json.Unmarshal(line, &entry) != nil || !validPublicPageEntry(entry) {
			return nil, ErrPublicHistoryCoverage
		}
		count++
		digest = appendPublicDigest(digest, line)
		if entry.ImportID == "" {
			known[entry.ID] = entry
		}
	}
	if err = scanner.Err(); err != nil {
		return nil, err
	}
	if offset != prefix.Bytes || count != prefix.Count || digest != prefix.Digest {
		return nil, ErrPublicHistoryCoverage
	}
	return known, nil
}

// Rechecking an owned native row must verify its old immutable projection first,
// then preserve the already committed occurrence and proofs on an exact replay.
func (s *JSONLStore) dedupePreparedPublicEntries(
	ctx context.Context,
	key string,
	prefix publicIndexHeader,
	entries []PublicHistoryEntry,
) ([]PublicHistoryEntry, error) {
	if prefix.Count == 0 {
		return entries, nil
	}
	known, err := s.readOwnedPublicMediaPrefix(ctx, key, prefix)
	if err != nil {
		return nil, err
	}
	unseen := make([]PublicHistoryEntry, 0, len(entries))
	for _, entry := range entries {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if owned, exists := known[entry.ID]; exists {
			if owned.RunID != entry.RunID || contentJSONDigest(owned.Message) != contentJSONDigest(entry.Message) {
				return nil, ErrPublicHistoryCoverage
			}
			continue
		}
		unseen = append(unseen, entry)
	}
	return unseen, nil
}
