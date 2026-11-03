package memory

import (
	"errors"
	"os"

	"github.com/sipeed/picoclaw/pkg/logger"
)

type publicIndexRecovery struct {
	Data    []byte `json:"data,omitempty"`
	Header  []byte `json:"header,omitempty"`
	Offset  *int64 `json:"offset,omitempty"`
	Existed bool   `json:"existed,omitempty"`
}

type publicIndexChange struct {
	Header  []byte
	Records []byte
	Offset  *int64
}

func (s *JSONLStore) preparePublicIndex(
	key string,
	original, next SessionMeta,
	change publicHistoryChange,
) (publicHistoryChange, error) {
	if change.Recovery == nil {
		return change, nil
	}
	header, operationErr0 := encodePublicIndexHeader(publicIndexMeta(next))
	if operationErr0 != nil {
		return publicHistoryChange{}, operationErr0
	}
	if change.Recovery.Offset != nil {
		if len(change.Recovery.Data) > 0 {
			return s.preparePublicIndexTail(key, original, next, change, header)
		}
		return s.preparePublicIndexAppend(key, original, next, change, header)
	}
	records, digest, operationErr0 := publicIndexRecords(
		change.Data,
		publicIndexHeader{Generation: next.PublicHistoryGeneration, Digest: emptyHistoryDigest},
	)
	if operationErr0 != nil || digest != next.PublicHistoryDigest {
		return publicHistoryChange{}, errors.Join(ErrPublicHistoryCoverage, operationErr0)
	}
	old, operationErr0 := os.ReadFile(s.publicIndexPath(key))
	existed := operationErr0 == nil
	if operationErr0 != nil && !errors.Is(operationErr0, os.ErrNotExist) {
		return publicHistoryChange{}, operationErr0
	}
	recovery := *change.Recovery
	recovery.Index = &publicIndexRecovery{Data: old, Existed: existed}
	change.Recovery = &recovery
	change.Index = &publicIndexChange{Header: header, Records: records}
	return change, nil
}

func (s *JSONLStore) preparePublicIndexAppend(
	key string,
	original, next SessionMeta,
	change publicHistoryChange,
	header []byte,
) (publicHistoryChange, error) {
	index, operationErr1 := s.openPublicIndex(key, original)
	if errors.Is(operationErr1, ErrPublicHistoryCoverage) {
		logger.WarnC("memory", "Public history index is invalid; explicit bounded preparation is required")
	}
	if errors.Is(operationErr1, os.ErrNotExist) || errors.Is(operationErr1, ErrPublicHistoryCoverage) {
		// A source installed before the index remains writable. Its explicit,
		// bounded preparation owns index creation; a read never scans it whole.
		return change, nil
	}
	if operationErr1 != nil {
		return publicHistoryChange{}, operationErr1
	}
	if operationErr2 := index.file.Close(); operationErr2 != nil {
		return publicHistoryChange{}, operationErr2
	}
	records, digest, operationErr1 := publicIndexRecords(
		change.Data,
		publicIndexMeta(original),
	)
	if operationErr1 != nil || digest != next.PublicHistoryDigest {
		return publicHistoryChange{}, errors.Join(ErrPublicHistoryCoverage, operationErr1)
	}
	oldHeader, operationErr1 := encodePublicIndexHeader(publicIndexMeta(original))
	if operationErr1 != nil {
		return publicHistoryChange{}, operationErr1
	}
	offset := int64(publicIndexHeaderBytes) + original.PublicHistoryCount*publicIndexRecordBytes
	recovery := *change.Recovery
	recovery.Index = &publicIndexRecovery{Header: oldHeader, Offset: &offset, Existed: true}
	change.Recovery = &recovery
	change.Index = &publicIndexChange{Header: header, Records: records, Offset: &offset}
	return change, nil
}

func (s *JSONLStore) writePublicIndex(key string, change *publicIndexChange) error {
	if change == nil {
		return nil
	}
	if change.Offset == nil {
		data := append(append([]byte(nil), change.Header...), change.Records...)
		return s.writeSessionFile(s.publicIndexPath(key), data, 0o600)
	}
	file, operationErr3 := os.OpenFile(s.publicIndexPath(key), os.O_WRONLY, 0o600)
	if operationErr3 != nil {
		return operationErr3
	}
	operationErr3 = file.Truncate(*change.Offset)
	if operationErr3 == nil {
		operationErr3 = writePublicIndexBytes(file, change.Records, *change.Offset)
	}
	if operationErr3 == nil {
		operationErr3 = writePublicIndexBytes(file, change.Header, 0)
	}
	if operationErr3 == nil {
		operationErr3 = file.Sync()
	}
	return errors.Join(operationErr3, file.Close())
}

func (s *JSONLStore) restorePublicIndex(key string, recovery *publicIndexRecovery) error {
	if recovery == nil {
		return nil
	}
	if recovery.Offset == nil {
		if recovery.Existed {
			return s.writeSessionFile(s.publicIndexPath(key), recovery.Data, 0o600)
		}
		operationErr4 := os.Remove(s.publicIndexPath(key))
		if errors.Is(operationErr4, os.ErrNotExist) {
			return nil
		}
		return operationErr4
	}
	return s.writePublicIndex(
		key,
		&publicIndexChange{Header: recovery.Header, Records: recovery.Data, Offset: recovery.Offset},
	)
}
