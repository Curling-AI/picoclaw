package memory

import (
	"errors"
	"os"
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

func (s *JSONLStore) preparePublicIndex(key string, original, next SessionMeta, change publicHistoryChange) (publicHistoryChange, error) {
	if change.Recovery == nil {
		return change, nil
	}
	header, err := encodePublicIndexHeader(publicIndexMeta(next))
	if err != nil {
		return publicHistoryChange{}, err
	}
	if change.Recovery.Offset != nil {
		return s.preparePublicIndexAppend(key, original, next, change, header)
	}
	records, digest, err := publicIndexRecords(change.Data, 0, emptyHistoryDigest)
	if err != nil || digest != next.PublicHistoryDigest {
		return publicHistoryChange{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	old, err := os.ReadFile(s.publicIndexPath(key))
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return publicHistoryChange{}, err
	}
	recovery := *change.Recovery
	recovery.Index = &publicIndexRecovery{Data: old, Existed: existed}
	change.Recovery = &recovery
	change.Index = &publicIndexChange{Header: header, Records: records}
	return change, nil
}

func (s *JSONLStore) preparePublicIndexAppend(key string, original, next SessionMeta, change publicHistoryChange, header []byte) (publicHistoryChange, error) {
	index, err := s.openPublicIndex(key, original)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrPublicHistoryCoverage) {
		// A source installed before the index remains writable. Its explicit,
		// bounded preparation owns index creation; a read never scans it whole.
		return change, nil
	}
	if err != nil {
		return publicHistoryChange{}, err
	}
	if err := index.file.Close(); err != nil {
		return publicHistoryChange{}, err
	}
	records, digest, err := publicIndexRecords(change.Data, original.PublicHistoryBytes, original.PublicHistoryDigest)
	if err != nil || digest != next.PublicHistoryDigest {
		return publicHistoryChange{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	oldHeader, err := encodePublicIndexHeader(publicIndexMeta(original))
	if err != nil {
		return publicHistoryChange{}, err
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
	file, err := os.OpenFile(s.publicIndexPath(key), os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	err = file.Truncate(*change.Offset)
	if err == nil {
		err = writePublicIndexBytes(file, change.Records, *change.Offset)
	}
	if err == nil {
		err = writePublicIndexBytes(file, change.Header, 0)
	}
	if err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

func (s *JSONLStore) restorePublicIndex(key string, recovery *publicIndexRecovery) error {
	if recovery == nil {
		return nil
	}
	if recovery.Offset == nil {
		if recovery.Existed {
			return s.writeSessionFile(s.publicIndexPath(key), recovery.Data, 0o600)
		}
		err := os.Remove(s.publicIndexPath(key))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return s.writePublicIndex(key, &publicIndexChange{Header: recovery.Header, Offset: recovery.Offset})
}
