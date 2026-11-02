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
	data, err := os.ReadFile(s.publicIndexPreparationPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(data) > publicIndexPreparationMaxBytes || json.Unmarshal(data, &state) != nil {
		return state, ErrPublicHistoryCoverage
	}
	if state.Target.Generation != meta.PublicHistoryGeneration {
		return publicIndexPreparation{Target: publicIndexMeta(meta), Digest: emptyHistoryDigest}, nil
	}
	if _, err := encodePublicIndexHeader(state.Target); err != nil || state.Count < 0 ||
		state.Count > state.Target.Count || state.Byte < 0 || state.Byte > state.Target.Bytes ||
		state.Target.Count > meta.PublicHistoryCount || state.Target.Bytes > meta.PublicHistoryBytes ||
		state.Target.Revision > meta.PublicHistoryRevision || len(state.Digest) != 64 {
		return state, ErrPublicHistoryCoverage
	}
	return state, nil
}

const publicIndexPreparationMaxBytes = 4096

// Preparation runs only while no index exists. Each call advances at most one
// bounded batch and reports pending, including the call that publishes it.
func (s *JSONLStore) preparePublicIndexLocked(ctx context.Context, key string, meta SessionMeta) (resultErr error) {
	state, err := s.readPublicIndexPreparation(key, meta)
	if err != nil {
		return err
	}
	building, err := os.OpenFile(s.publicIndexBuildingPath(key), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer closePublicFile(building, &resultErr)
	if err := validatePublicIndexPreparation(building, state); err != nil {
		return err
	}
	if err := building.Truncate(publicIndexHeaderBytes + state.Count*publicIndexRecordBytes); err != nil {
		return err
	}
	source, err := os.Open(s.publicHistoryPath(key))
	if err != nil {
		return err
	}
	defer closePublicFile(source, &resultErr)
	info, err := source.Stat()
	if err != nil || info.Size() != meta.PublicHistoryBytes {
		return errors.Join(ErrPublicHistoryCoverage, err)
	}
	next, records, err := readPublicIndexPreparationBatch(ctx, source, state, meta)
	if err != nil {
		return err
	}
	if err := writePublicIndexBytes(building, records, publicIndexHeaderBytes+state.Count*publicIndexRecordBytes); err != nil {
		return err
	}
	if err := building.Sync(); err != nil {
		return err
	}
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if err := s.writeSessionFile(s.publicIndexPreparationPath(key), data, 0o600); err != nil {
		return err
	}
	if next.Count != meta.PublicHistoryCount || next.Byte != meta.PublicHistoryBytes {
		return ErrPublicHistoryIndexPreparing
	}
	if next.Digest != meta.PublicHistoryDigest {
		return ErrPublicHistoryCoverage
	}
	header, err := encodePublicIndexHeader(publicIndexMeta(meta))
	if err != nil {
		return err
	}
	if err := writePublicIndexBytes(building, header, 0); err != nil {
		return err
	}
	if err := building.Sync(); err != nil {
		return err
	}
	if err := building.Close(); err != nil {
		return err
	}
	if err := os.Rename(s.publicIndexBuildingPath(key), s.publicIndexPath(key)); err != nil {
		return err
	}
	if err := os.Remove(s.publicIndexPreparationPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
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
	if _, err := file.ReadAt(data, publicIndexHeaderBytes+(state.Count-1)*publicIndexRecordBytes); err != nil {
		return errors.Join(ErrPublicHistoryCoverage, err)
	}
	last, err := decodePublicIndexRecord(data)
	if err != nil || last.Offset < 0 || last.Length <= 1 || last.Offset+last.Length != state.Byte ||
		state.Digest != publicIndexRecordDigest(last) {
		return ErrPublicHistoryCoverage
	}
	return nil
}

func readPublicIndexPreparationBatch(ctx context.Context, source *os.File, state publicIndexPreparation, meta SessionMeta) (publicIndexPreparation, []byte, error) {
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
		if err := ctx.Err(); err != nil {
			return next, nil, err
		}
		line, err := readPublicIndexPreparationLine(reader)
		if errors.Is(err, io.EOF) {
			if next.Target.Bytes-state.Byte <= PublicHistoryOperationBytes {
				return next, nil, ErrPublicHistoryCoverage
			}
			if len(records) == 0 {
				return next, nil, ErrHistoryCapacity
			}
			break
		}
		if err != nil {
			return next, nil, err
		}
		var entry PublicHistoryEntry
		if json.Unmarshal(line, &entry) != nil || !validPublicPageEntry(entry) {
			return next, nil, ErrPublicHistoryCoverage
		}
		encoded, digest, err := publicIndexRecords(append(line, '\n'), next.Byte, next.Digest)
		if err != nil {
			return next, nil, err
		}
		records = append(records, encoded...)
		next.Byte += int64(len(line) + 1)
		next.Count++
		next.Digest = digest
	}
	if next.Count > next.Target.Count || next.Byte > next.Target.Bytes {
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
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > PublicHistoryOperationBytes {
			return nil, ErrHistoryCapacity
		}
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return line[:len(line)-1], nil
	}
}
