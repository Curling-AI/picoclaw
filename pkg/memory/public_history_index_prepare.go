package memory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
)

var ErrPublicHistoryIndexPreparing = errors.New("public history index preparation pending")

type publicIndexPreparation struct {
	Target publicIndexHeader `json:"target"`
	Byte   int64             `json:"byte"`
	Count  int64             `json:"count"`
	Digest string            `json:"digest"`
}

func (s *JSONLStore) publicIndexPreparationPath(key string) string {
	return s.publicIndexPath(key) + ".prepare"
}

func (s *JSONLStore) publicIndexBuildingPath(key string) string {
	return s.publicIndexPath(key) + ".building"
}

func (s *JSONLStore) readPublicIndexPreparation(key string, meta SessionMeta) (publicIndexPreparation, error) {
	state := publicIndexPreparation{Target: publicIndexMeta(meta), Digest: emptyHistoryDigest}
	data, operationErr0 := os.ReadFile(s.publicIndexPreparationPath(key))
	if errors.Is(operationErr0, os.ErrNotExist) {
		return state, nil
	}
	if operationErr0 != nil {
		return state, operationErr0
	}
	if len(data) > publicIndexPreparationMaxBytes || json.Unmarshal(data, &state) != nil {
		return state, ErrPublicHistoryCoverage
	}
	if state.Target.Generation != meta.PublicHistoryGeneration {
		return publicIndexPreparation{Target: publicIndexMeta(meta), Digest: emptyHistoryDigest}, nil
	}
	if _, operationErr1 := encodePublicIndexHeader(state.Target); operationErr1 != nil || state.Count < 0 ||
		state.Count > state.Target.Count || state.Byte < 0 || state.Byte > state.Target.Bytes ||
		state.Target.Count > meta.PublicHistoryCount || state.Target.Bytes > meta.PublicHistoryBytes ||
		state.Target.Revision > meta.PublicHistoryRevision || len(state.Digest) != 64 ||
		(state.Count == state.Target.Count) != (state.Byte == state.Target.Bytes) {
		return state, ErrPublicHistoryCoverage
	}
	return state, nil
}

const publicIndexPreparationMaxBytes = 4096

func (s *JSONLStore) preparePublicIndexResult(
	ctx context.Context,
	key string,
	meta SessionMeta,
) (PublicHistoryPreparationResult, error) {
	result, readinessErr := s.publicPreparationResult(ctx, key, meta)
	if readinessErr != nil || result.Ready {
		return result, readinessErr
	}
	if operationErr3 := s.preparePublicIndexLocked(
		ctx,
		key,
		meta,
	); !errors.Is(
		operationErr3,
		ErrPublicHistoryIndexPreparing,
	) {
		return PublicHistoryPreparationResult{}, operationErr3
	}
	result.Ready = false
	return result, nil
}

func (s *JSONLStore) publicPreparationResult(
	ctx context.Context,
	key string,
	meta SessionMeta,
) (PublicHistoryPreparationResult, error) {
	result := preparationResult(key, meta)
	if !result.Ready {
		return result, nil
	}
	if err := ctx.Err(); err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	index, err := s.openPublicIndex(key, meta)
	if errors.Is(err, ErrPublicHistoryCoverage) {
		// Only the derived index is discarded; bounded preparation verifies the source digest.
		if removeErr := os.Remove(s.publicIndexPath(key)); removeErr != nil {
			return PublicHistoryPreparationResult{}, removeErr
		}
		err = os.ErrNotExist
	}
	if errors.Is(err, os.ErrNotExist) {
		result.Ready = false
		return result, nil
	}
	if err != nil {
		return PublicHistoryPreparationResult{}, err
	}
	return result, index.file.Close()
}

// Preparation runs only while no index exists. Each call advances at most one
// bounded batch and reports pending, including the call that publishes it.
func (s *JSONLStore) preparePublicIndexLocked(ctx context.Context, key string, meta SessionMeta) (resultErr error) {
	state, operationErr4 := s.readPublicIndexPreparation(key, meta)
	if operationErr4 != nil {
		return operationErr4
	}
	building, operationErr4 := os.OpenFile(s.publicIndexBuildingPath(key), os.O_RDWR|os.O_CREATE, 0o600)
	if operationErr4 != nil {
		return operationErr4
	}
	defer closePublicFile(building, &resultErr)
	if operationErr5 := validatePublicIndexPreparation(building, state); operationErr5 != nil {
		return operationErr5
	}
	if operationErr6 := building.Truncate(
		publicIndexHeaderBytes + state.Count*publicIndexRecordBytes,
	); operationErr6 != nil {
		return operationErr6
	}
	source, operationErr7 := os.Open(s.publicHistoryPath(key))
	if operationErr7 != nil {
		return operationErr7
	}
	defer closePublicFile(source, &resultErr)
	info, operationErr7 := source.Stat()
	if operationErr7 != nil || info.Size() != meta.PublicHistoryBytes {
		return errors.Join(ErrPublicHistoryCoverage, operationErr7)
	}
	next, records, operationErr7 := readPublicIndexPreparationBatch(ctx, source, state, meta)
	if operationErr7 != nil {
		return operationErr7
	}
	if operationErr8 := writePublicIndexBytes(
		building,
		records,
		publicIndexHeaderBytes+state.Count*publicIndexRecordBytes,
	); operationErr8 != nil {
		return operationErr8
	}
	if operationErr9 := building.Sync(); operationErr9 != nil {
		return operationErr9
	}
	data, operationErr7 := json.Marshal(next)
	if operationErr7 != nil {
		return operationErr7
	}
	if operationErr10 := s.writeSessionFile(s.publicIndexPreparationPath(key), data, 0o600); operationErr10 != nil {
		return operationErr10
	}
	if next.Count != meta.PublicHistoryCount || next.Byte != meta.PublicHistoryBytes {
		return ErrPublicHistoryIndexPreparing
	}
	if next.Digest != meta.PublicHistoryDigest {
		return ErrPublicHistoryCoverage
	}
	header, operationErr7 := encodePublicIndexHeader(publicIndexMeta(meta))
	if operationErr7 != nil {
		return operationErr7
	}
	if operationErr11 := writePublicIndexBytes(building, header, 0); operationErr11 != nil {
		return operationErr11
	}
	if operationErr12 := building.Sync(); operationErr12 != nil {
		return operationErr12
	}
	if operationErr13 := building.Close(); operationErr13 != nil {
		return operationErr13
	}
	if operationErr14 := os.Rename(s.publicIndexBuildingPath(key), s.publicIndexPath(key)); operationErr14 != nil {
		return operationErr14
	}
	if operationErr15 := os.Remove(
		s.publicIndexPreparationPath(key),
	); operationErr15 != nil &&
		!errors.Is(operationErr15, os.ErrNotExist) {
		return operationErr15
	}
	return ErrPublicHistoryIndexPreparing
}

func validatePublicIndexPreparation(file *os.File, state publicIndexPreparation) error {
	if state.Count == 0 {
		if state.Byte != 0 || state.Digest != emptyHistoryDigest {
			return ErrPublicHistoryCoverage
		}
		return nil
	}
	data := make([]byte, publicIndexRecordBytes)
	if _, operationErr16 := file.ReadAt(
		data,
		publicIndexHeaderBytes+(state.Count-1)*publicIndexRecordBytes,
	); operationErr16 != nil {
		return errors.Join(ErrPublicHistoryCoverage, operationErr16)
	}
	last, operationErr17 := decodePublicIndexRecord(data)
	if operationErr17 != nil || last.Position != state.Count-1 || last.Generation.String() != state.Target.Generation ||
		last.Offset < 0 || last.Length <= 1 || last.Offset+last.Length != state.Byte ||
		state.Digest != publicIndexRecordDigest(last) {
		return ErrPublicHistoryCoverage
	}
	return nil
}

func readPublicIndexPreparationBatch(
	ctx context.Context,
	source io.ReaderAt,
	state publicIndexPreparation,
	meta SessionMeta,
) (publicIndexPreparation, []byte, error) {
	next := state
	if state.Count == state.Target.Count && state.Byte == state.Target.Bytes {
		if state.Digest != state.Target.Digest {
			return next, nil, ErrPublicHistoryCoverage
		}
		next.Target = publicIndexMeta(meta)
	}
	reader := bufio.NewReaderSize(io.NewSectionReader(source, next.Byte,
		min(next.Target.Bytes-next.Byte, PublicHistoryOperationBytes)), historyReadBufferBytes)
	var records []byte
	for rows := 0; rows < PublicHistoryImportMessages && next.Byte < next.Target.Bytes; rows++ {
		if operationErr18 := ctx.Err(); operationErr18 != nil {
			return next, nil, operationErr18
		}
		line, operationErr19 := readPublicIndexPreparationLine(reader)
		if errors.Is(operationErr19, io.EOF) {
			if next.Target.Bytes-state.Byte <= PublicHistoryOperationBytes {
				return next, nil, ErrPublicHistoryCoverage
			}
			if len(records) == 0 {
				return next, nil, ErrHistoryCapacity
			}
			break
		}
		if operationErr19 != nil {
			return next, nil, operationErr19
		}
		var entry PublicHistoryEntry
		if json.Unmarshal(line, &entry) != nil || !validPublicPageEntry(entry) {
			return next, nil, ErrPublicHistoryCoverage
		}
		encoded, digest, operationErr19 := publicIndexRecords(append(line, '\n'), publicIndexHeader{
			Generation: next.Target.Generation, Bytes: next.Byte, Count: next.Count, Digest: next.Digest,
		})
		if operationErr19 != nil {
			return next, nil, operationErr19
		}
		records = append(records, encoded...)
		next.Byte += int64(len(line) + 1)
		next.Count++
		next.Digest = digest
	}
	if next.Count > next.Target.Count || next.Byte > next.Target.Bytes ||
		(next.Count == next.Target.Count) != (next.Byte == next.Target.Bytes) {
		return next, nil, ErrPublicHistoryCoverage
	}
	if next.Count == next.Target.Count && next.Byte == next.Target.Bytes && next.Digest != next.Target.Digest {
		return next, nil, ErrPublicHistoryCoverage
	}
	return next, records, nil
}

func readPublicIndexPreparationLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, operationErr20 := reader.ReadSlice('\n')
		if len(line)+len(part) > PublicHistoryOperationBytes {
			return nil, ErrHistoryCapacity
		}
		line = append(line, part...)
		if errors.Is(operationErr20, bufio.ErrBufferFull) {
			continue
		}
		if operationErr20 != nil {
			return nil, operationErr20
		}
		return line[:len(line)-1], nil
	}
}
