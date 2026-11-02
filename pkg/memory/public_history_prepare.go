package memory

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// PublicHistoryPreparation is a small migration checkpoint over already
// confirmed source content, not a journal of model tokens or partial progress.
type PublicHistoryPreparation struct {
	FileGeneration string `json:"fileGeneration"`
	Generation     string `json:"generation"`
	Ceiling        int64  `json:"ceiling"`
	Count          int64  `json:"count"`
	Offset         int64  `json:"offset"`
	Line           int64  `json:"line"`
	Prefix         int64  `json:"prefix"`
}

type PublicHistoryPreparationResult struct {
	Generation string `json:"generation"`
	Revision   uint64 `json:"revision"`
	Cursor     string `json:"cursor"`
	Ready      bool   `json:"ready"`
	Complete   bool   `json:"complete"`
	Count      int64  `json:"count"`
}

func samePublicPreparation(a, b *PublicHistoryPreparation) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func preparationResult(key string, meta SessionMeta) PublicHistoryPreparationResult {
	result := PublicHistoryPreparationResult{
		Generation: meta.PublicHistoryGeneration,
		Revision:   meta.PublicHistoryRevision,
		Ready:      meta.PublicHistoryPreparation == nil,
		Complete:   meta.PublicHistoryComplete,
		Count:      meta.PublicHistoryCount,
	}
	if meta.PublicHistoryPreparation != nil {
		data, _ := json.Marshal(struct {
			Key        string                   `json:"key"`
			Generation string                   `json:"generation"`
			Revision   uint64                   `json:"revision"`
			State      PublicHistoryPreparation `json:"state"`
		}{key, meta.PublicHistoryGeneration, meta.PublicHistoryRevision, *meta.PublicHistoryPreparation})
		digest := sha256.Sum256(data)
		result.Cursor = hex.EncodeToString(digest[:])
	}
	return result
}

// PreparePublicHistory advances at most50 raw records/2MiB of display source.
// An empty cursor discovers the current checkpoint. It cannot create a scope
// whose owner metadata was removed, and never reads the entire legacy session.
func (s *JSONLStore) PreparePublicHistory(
	ctx context.Context,
	key, cursor string,
) (PublicHistoryPreparationResult, error) {
	canonical := key
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if operationErr := ctx.Err(); operationErr != nil {
		return PublicHistoryPreparationResult{}, operationErr
	}
	if _, operationErr1 := os.Stat(s.metaPath(canonical)); operationErr1 != nil {
		return PublicHistoryPreparationResult{}, operationErr1
	}
	if err := s.contentMutationWriterPreflight(ctx, canonical); err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	meta, _, canonicalErr := s.readContentMeta(canonical)
	if canonicalErr != nil {
		return PublicHistoryPreparationResult{}, canonicalErr
	}
	if operationErr2 := validateCanonicalHistoryKey(key, canonical, meta); operationErr2 != nil {
		return PublicHistoryPreparationResult{}, operationErr2
	}
	if operationErr3 := s.probeHistoryRecoveryMarker(canonical, &meta); operationErr3 != nil {
		return PublicHistoryPreparationResult{}, operationErr3
	}
	if meta.HistoryRecoveryRequired {
		return PublicHistoryPreparationResult{}, ErrHistoryRecoveryRequired
	}
	if operationErr4 := s.probeHistoryOrderWitness(canonical, &meta); operationErr4 != nil {
		return PublicHistoryPreparationResult{}, operationErr4
	}
	if meta.PublicHistoryVersion == 0 {
		if cursor != "" {
			return PublicHistoryPreparationResult{}, ErrPublicHistoryCoverage
		}
		return s.beginPublicPreparation(ctx, canonical, meta)
	}
	if meta.PublicHistoryVersion != PublicHistoryVersion {
		return PublicHistoryPreparationResult{}, ErrPublicHistoryCoverage
	}
	if meta.PublicHistoryPreparation == nil {
		return s.preparePublicIndexResult(ctx, canonical, meta)
	}
	state := *meta.PublicHistoryPreparation
	if state.FileGeneration != meta.HistoryFileGeneration || state.Generation != meta.HistoryGeneration ||
		state.Prefix != meta.HistoryPrefix {
		return PublicHistoryPreparationResult{}, ErrPublicHistoryCoverage
	}
	expected := preparationResult(canonical, meta)
	if cursor == "" {
		return expected, nil
	}
	if cursor != expected.Cursor {
		return PublicHistoryPreparationResult{}, ErrPublicHistoryCoverage
	}
	entries, next, canonicalErr := s.readPublicPreparation(ctx, canonical, meta, state)
	if canonicalErr != nil {
		return PublicHistoryPreparationResult{}, canonicalErr
	}
	change, canonicalErr := appendPreparedEntries(meta, entries)
	if canonicalErr != nil {
		return PublicHistoryPreparationResult{}, canonicalErr
	}
	meta.PublicHistoryBytes += int64(len(change.Data))
	meta.PublicHistoryCount += int64(len(entries))
	meta.PublicHistoryRevision++
	meta.PublicHistoryDigest, canonicalErr = publicEntriesDigest(meta.PublicHistoryDigest, entries)
	if canonicalErr != nil {
		return PublicHistoryPreparationResult{}, canonicalErr
	}
	if next.Offset == next.Ceiling {
		if meta.HistoryBytes != nil && *meta.HistoryBytes > next.Ceiling {
			next.Ceiling = *meta.HistoryBytes
			next.Count = int64(meta.Count)
		} else {
			meta.PublicHistoryPreparation = nil
			meta.PublicHistoryComplete = next.Prefix == 0
		}
	}
	if meta.PublicHistoryPreparation != nil {
		meta.PublicHistoryPreparation = &next
	}
	if operationErr5 := s.commitPublicChange(ctx, canonical, meta, change); operationErr5 != nil {
		return PublicHistoryPreparationResult{}, operationErr5
	}
	return s.publicPreparationResult(ctx, canonical, meta)
}

func (s *JSONLStore) beginPublicPreparation(
	ctx context.Context,
	key string,
	meta SessionMeta,
) (PublicHistoryPreparationResult, error) {
	if _, operationErr6 := os.Stat(
		s.publicHistoryPath(key),
	); operationErr6 == nil ||
		!errors.Is(operationErr6, os.ErrNotExist) {
		return PublicHistoryPreparationResult{}, ErrPublicHistoryCoverage
	}
	ensureHistoryGeneration(&meta, key)
	ceiling := int64(0)
	info, infoErr := os.Stat(s.jsonlPath(key))
	if infoErr == nil {
		ceiling = info.Size()
	} else if !errors.Is(infoErr, os.ErrNotExist) || meta.Count != 0 {
		return PublicHistoryPreparationResult{}, infoErr
	}
	if meta.HistoryBytes != nil {
		if *meta.HistoryBytes > ceiling || *meta.HistoryBytes < 0 {
			return PublicHistoryPreparationResult{}, ErrPublicHistoryCoverage
		}
		ceiling = *meta.HistoryBytes
	}
	meta.PublicHistoryVersion = PublicHistoryVersion
	meta.PublicHistoryGeneration = newHistoryID()
	meta.PublicHistoryRevision = 1
	meta.PublicHistoryDigest = emptyHistoryDigest
	meta.PublicHistoryPreparation = &PublicHistoryPreparation{
		FileGeneration: meta.HistoryFileGeneration,
		Generation:     meta.HistoryGeneration,
		Ceiling:        ceiling,
		Count:          int64(meta.Count),
		Prefix:         meta.HistoryPrefix,
	}
	if ceiling == 0 && meta.Count == 0 {
		meta.PublicHistoryPreparation = nil
		meta.PublicHistoryComplete = meta.HistoryPrefix == 0
	}
	change := publicHistoryChange{Data: []byte{}, Recovery: &publicHistoryRecovery{Existed: false}}
	if operationErr7 := s.commitPublicChange(ctx, key, meta, change); operationErr7 != nil {
		return PublicHistoryPreparationResult{}, operationErr7
	}
	return s.publicPreparationResult(ctx, key, meta)
}

func appendPreparedEntries(meta SessionMeta, entries []PublicHistoryEntry) (publicHistoryChange, error) {
	data, dataErr := encodePublicEntries(entries)
	if dataErr != nil {
		return publicHistoryChange{}, dataErr
	}
	offset := meta.PublicHistoryBytes
	return publicHistoryChange{Data: data, Recovery: &publicHistoryRecovery{Offset: &offset, Existed: true}}, nil
}

func (s *JSONLStore) readPublicPreparation(
	ctx context.Context,
	key string,
	meta SessionMeta,
	state PublicHistoryPreparation,
) (result []PublicHistoryEntry, next PublicHistoryPreparation, resultErr error) {
	file, fileErr := os.Open(s.jsonlPath(key))
	if fileErr != nil {
		return nil, state, fileErr
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, fileErr := file.Stat()
	if fileErr != nil {
		return nil, state, fileErr
	}
	if state.Offset < 0 || state.Offset > state.Ceiling || state.Ceiling > info.Size() {
		return nil, state, ErrPublicHistoryCoverage
	}
	reader := bufio.NewReaderSize(
		io.NewSectionReader(file, state.Offset, state.Ceiling-state.Offset),
		historyReadBufferBytes,
	)
	entries := make([]PublicHistoryEntry, 0, PublicHistoryImportMessages)
	bytes := 0
	for consumed := 0; consumed < PublicHistoryImportMessages && state.Offset < state.Ceiling; consumed++ {
		if operationErr8 := ctx.Err(); operationErr8 != nil {
			return nil, state, operationErr8
		}
		line, lineErr := readPublicPreparationLine(reader)
		if lineErr != nil {
			return nil, state, lineErr
		}
		entry, visible, lineErr := s.preparedPublicEntry(line, key, meta, state)
		if lineErr != nil {
			return nil, state, lineErr
		}
		if visible {
			message, _ := json.Marshal(entry.Message)
			if len(message) > PublicHistoryMessageBytes {
				return nil, state, ErrHistoryCapacity
			}
			encoded, _ := json.Marshal(entry)
			if bytes+len(encoded)+1 > PublicHistoryOperationBytes {
				break
			}
			bytes += len(encoded) + 1
			entries = append(entries, entry)
		}
		state.Offset += int64(len(line))
		state.Line++
	}
	if state.Offset == state.Ceiling && state.Line != state.Count {
		return nil, state, ErrPublicHistoryCoverage
	}
	return entries, state, nil
}

func readPublicPreparationLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, partErr := reader.ReadSlice('\n')
		if len(line)+len(part) > maxLineSize {
			return nil, ErrHistoryCapacity
		}
		line = append(line, part...)
		if errors.Is(partErr, bufio.ErrBufferFull) {
			continue
		}
		if partErr != nil {
			return nil, fmt.Errorf("prepare confirmed public history: %w", partErr)
		}
		return line, nil
	}
}

func (s *JSONLStore) preparedPublicEntry(
	line []byte,
	key string,
	meta SessionMeta,
	state PublicHistoryPreparation,
) (PublicHistoryEntry, bool, error) {
	var record historyRecord
	if json.Unmarshal(line, &record) != nil || !historyLineageMatches(line, meta, key) {
		return PublicHistoryEntry{}, false, ErrPublicHistoryCoverage
	}
	order, orderErr := decodedHistoryOrder(line)
	if orderErr != nil {
		return PublicHistoryEntry{}, false, orderErr
	}
	legacy, orderErr := decodedLegacyHistoryStream(line)
	if orderErr != nil {
		return PublicHistoryEntry{}, false, orderErr
	}
	fromFence := meta.HistoryFileGeneration == meta.HistoryLegacyFileGeneration &&
		state.Offset+int64(len(line)) <= meta.HistoryLegacyBytes
	if order != (HistoryOrderProof{}) {
		if legacy != "" || order.StreamID != meta.HistoryStreamID || order.Sequence > meta.HistorySequence {
			return PublicHistoryEntry{}, false, ErrPublicHistoryCoverage
		}
	} else if meta.HistoryOrderVersion == HistoryOrderVersion && legacy != meta.HistoryStreamID && !fromFence {
		return PublicHistoryEntry{}, false, ErrPublicHistoryCoverage
	}
	message := messageWithHistoryOrder(record.Message, order)
	message.HistoryID = decodedHistoryID(line, sanitizeKey(key)+".jsonl", state.Line)
	message.HistoryRunID = decodedHistoryRunID(line)
	message.HistoryPublicMessage = record.PublicMessage
	projected, projectionErr := s.projectLegacyPublic(message)
	if projectionErr != nil {
		return PublicHistoryEntry{}, false, projectionErr
	}
	projected.HistoryID, projected.HistoryRunID = message.HistoryID, message.HistoryRunID
	projected = messageWithHistoryOrder(projected, order)
	message = projected
	entries := publicHistoryEntriesAt([]providers.Message{message}, meta, int(state.Line))
	if len(entries) == 0 {
		return PublicHistoryEntry{}, false, nil
	}
	return entries[0], true, nil
}
