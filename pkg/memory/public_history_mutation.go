package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type publicHistoryRecovery struct {
	Offset  *int64 `json:"offset,omitempty"`
	Data    []byte `json:"data,omitempty"`
	Existed bool   `json:"existed,omitempty"`
}

type publicHistoryChange struct {
	Data     []byte
	Recovery *publicHistoryRecovery
}

func (s *JSONLStore) preparePublicAppend(key string, original SessionMeta, next *SessionMeta, messages []providers.Message) (publicHistoryChange, error) {
	if next.HistoryRecoveryRequired {
		return publicHistoryChange{}, nil
	}
	newEntries := publicHistoryEntries(messages, *next)
	if original.PublicHistoryVersion == 0 {
		old, err := s.readCommittedHistory(key, original, 0)
		if err != nil {
			return publicHistoryChange{}, err
		}
		oldEntries := publicHistoryEntries(old, original)
		next.PublicHistoryComplete = original.HistoryPrefix == 0
		next.PublicHistoryVersion = PublicHistoryVersion
		next.PublicHistoryGeneration = newHistoryID()
		return s.replacePublicEntries(key, append(oldEntries, newEntries...), next)
	}
	if original.PublicHistoryVersion != PublicHistoryVersion {
		return publicHistoryChange{}, ErrPublicHistoryCoverage
	}
	data, err := encodePublicEntries(newEntries)
	if err != nil {
		return publicHistoryChange{}, err
	}
	next.PublicHistoryDigest, err = publicEntriesDigest(original.PublicHistoryDigest, newEntries)
	if err != nil {
		return publicHistoryChange{}, err
	}
	offset := original.PublicHistoryBytes
	next.PublicHistoryBytes = offset + int64(len(data))
	next.PublicHistoryCount = original.PublicHistoryCount + int64(len(newEntries))
	next.PublicHistoryRevision++
	return publicHistoryChange{Data: data, Recovery: &publicHistoryRecovery{Offset: &offset, Existed: true}}, nil
}

func (s *JSONLStore) preparePublicRewrite(key string, original SessionMeta, next *SessionMeta, messages []providers.Message) (publicHistoryChange, error) {
	if next.HistoryRecoveryRequired {
		return publicHistoryChange{}, nil
	}
	var entries []PublicHistoryEntry
	if original.PublicHistoryVersion == 0 {
		old, err := s.readCommittedHistory(key, original, 0)
		if err != nil {
			return publicHistoryChange{}, err
		}
		entries = publicHistoryEntries(old, original)
		next.PublicHistoryComplete = original.HistoryPrefix == 0
		next.PublicHistoryVersion = PublicHistoryVersion
		next.PublicHistoryGeneration = newHistoryID()
	} else {
		var err error
		entries, err = s.readPublicEntries(key, original)
		if err != nil {
			return publicHistoryChange{}, err
		}
	}
	if next.HistoryGeneration != original.HistoryGeneration {
		active, err := s.readCommittedHistory(key, original, original.Skip)
		if err != nil {
			return publicHistoryChange{}, err
		}
		activePublic := publicHistoryEntries(active, original)
		boundary := len(entries)
		if len(activePublic) > 0 {
			boundary = -1
			for index, entry := range entries {
				if entry.ID == activePublic[0].ID {
					boundary = index
					break
				}
			}
			if boundary < 0 {
				return publicHistoryChange{}, ErrPublicHistoryCoverage
			}
		}
		// A content edit replaces the active suffix. Compaction retains the public
		// prefix; neither operation may reattach a supplement by an old position.
		entries = append(entries[:boundary:boundary], publicHistoryEntries(messages, *next)...)
		next.PublicHistoryGeneration = newHistoryID()
	}
	return s.replacePublicEntries(key, entries, next)
}

func (s *JSONLStore) replacePublicEntries(key string, entries []PublicHistoryEntry, next *SessionMeta) (publicHistoryChange, error) {
	data, err := encodePublicEntries(entries)
	if err != nil {
		return publicHistoryChange{}, err
	}
	old, err := os.ReadFile(s.publicHistoryPath(key))
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return publicHistoryChange{}, err
	}
	next.PublicHistoryDigest, err = publicEntriesDigest(emptyHistoryDigest, entries)
	if err != nil {
		return publicHistoryChange{}, err
	}
	next.PublicHistoryBytes = int64(len(data))
	next.PublicHistoryCount = int64(len(entries))
	next.PublicHistoryRevision++
	return publicHistoryChange{Data: data, Recovery: &publicHistoryRecovery{Data: old, Existed: existed}}, nil
}

func (s *JSONLStore) writePublicChange(key string, change publicHistoryChange) error {
	if change.Recovery == nil {
		return nil
	}
	path := s.publicHistoryPath(key)
	if change.Recovery.Offset == nil {
		return s.writeSessionFile(path, change.Data, 0o600)
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(*change.Recovery.Offset); err != nil {
		return err
	}
	if _, err := file.Seek(*change.Recovery.Offset, io.SeekStart); err != nil {
		return err
	}
	n, err := file.Write(change.Data)
	if err != nil {
		return err
	}
	if n != len(change.Data) {
		return io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Close()
}

func (s *JSONLStore) restorePublicHistory(key string, recovery *publicHistoryRecovery) error {
	if recovery == nil {
		return nil
	}
	path := s.publicHistoryPath(key)
	if recovery.Offset != nil {
		file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := file.Truncate(*recovery.Offset); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		return file.Close()
	}
	if !recovery.Existed {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return s.writeSessionFile(path, recovery.Data, 0o600)
}

func (s *JSONLStore) commitPublicChange(key string, next SessionMeta, change publicHistoryChange) error {
	old, err := s.readMeta(key)
	if err != nil {
		return err
	}
	if old.HistoryBytes == nil {
		return ErrPublicHistoryCoverage
	}
	offset := *old.HistoryBytes
	if err := s.beginHistoryWrite(key, historyWriteRecovery{Meta: old, Next: next, AppendOffset: &offset, Public: change.Recovery}); err != nil {
		return err
	}
	err = s.writePublicChange(key, change)
	if err == nil {
		err = s.writeMeta(key, next)
	}
	if err == nil {
		err = s.finishHistoryWrite(key)
	}
	if err != nil {
		committed, recoveryErr := s.recoverExpectedHistoryCommit(key, next)
		if committed {
			return nil
		}
		return errors.Join(err, recoveryErr)
	}
	return nil
}

func samePublicEntry(a, b PublicHistoryEntry) bool {
	if b.AnchorID == "" && !b.Partial && b.Order == (HistoryOrderProof{}) {
		a.Order = HistoryOrderProof{}
	}
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func mergePublicImport(existing []PublicHistoryEntry, request PublicHistoryImport, meta SessionMeta) ([]PublicHistoryEntry, bool, error) {
	byID := make(map[string]PublicHistoryEntry, len(existing))
	byImport := make(map[string]PublicHistoryEntry, len(existing))
	for _, entry := range existing {
		byID[entry.ID] = entry
		if entry.ImportID != "" {
			byImport[entry.ImportID] = entry
		}
	}
	pending := make([]PublicHistoryEntry, 0, len(request.Entries))
	for _, entry := range request.Entries {
		if previous, ok := byImport[entry.ImportID]; ok {
			if !samePublicEntry(previous, entry) {
				return nil, false, publicImportConflict()
			}
			continue
		}
		if _, ok := byID[entry.ID]; ok {
			return nil, false, publicImportConflict()
		}
		if entry.AnchorID != "" {
			anchor, ok := byID[entry.AnchorID]
			if !ok || anchor.Order != entry.AnchorOrder {
				return nil, false, publicImportConflict()
			}
		}
		byID[entry.ID] = entry
		byImport[entry.ImportID] = entry
		pending = append(pending, entry)
	}
	if len(pending) == 0 {
		return existing, false, nil
	}
	if request.Generation != meta.PublicHistoryGeneration || request.Revision != meta.PublicHistoryRevision {
		return nil, false, ErrPublicHistoryCoverage
	}
	if (request.BeforeID == "") == (request.AfterID == "") {
		return nil, false, ErrPublicHistoryCoverage
	}
	position := -1
	for index, entry := range existing {
		if entry.ID == request.BeforeID {
			position = index
		}
		if entry.ID == request.AfterID {
			position = index + 1
		}
	}
	if position < 0 {
		return nil, false, ErrPublicHistoryCoverage
	}
	result := make([]PublicHistoryEntry, 0, len(existing)+len(pending))
	result = append(result, existing[:position]...)
	result = append(result, pending...)
	result = append(result, existing[position:]...)
	return result, true, nil
}
