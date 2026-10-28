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

func (s *JSONLStore) preparePublicAppend(
	key string,
	original SessionMeta,
	next *SessionMeta,
	messages []providers.Message,
	sourceOffset int64,
) (publicHistoryChange, error) {
	if next.HistoryRecoveryRequired || original.PublicHistoryPreparation != nil || original.PublicHistoryVersion < 0 ||
		next.PublicHistoryVersion < 0 {
		return publicHistoryChange{}, nil
	}
	newEntries := publicHistoryEntries(messages, *next)
	if original.PublicHistoryVersion == 0 {
		if _, statErr := os.Stat(s.publicHistoryPath(key)); statErr == nil {
			next.PublicHistoryVersion = -1
			return publicHistoryChange{}, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return publicHistoryChange{}, statErr
		}
		next.PublicHistoryComplete = original.HistoryPrefix == 0 && original.Count == 0
		next.PublicHistoryVersion = PublicHistoryVersion
		next.PublicHistoryGeneration = newHistoryID()
		if original.Count > 0 {
			ensureHistoryGeneration(&original, key)
			next.PublicHistoryPreparation = &PublicHistoryPreparation{
				FileGeneration: original.HistoryFileGeneration,
				Generation:     original.HistoryGeneration,
				Ceiling:        sourceOffset,
				Count:          int64(original.Count),
				Prefix:         original.HistoryPrefix,
			}
			return s.replacePublicEntries(key, nil, next)
		}
		return s.replacePublicEntries(key, newEntries, next)
	}
	if original.PublicHistoryVersion != PublicHistoryVersion {
		return publicHistoryChange{}, ErrPublicHistoryCoverage
	}
	data, dataErr := encodePublicEntries(newEntries)
	if dataErr != nil {
		return publicHistoryChange{}, dataErr
	}
	next.PublicHistoryDigest, dataErr = publicEntriesDigest(original.PublicHistoryDigest, newEntries)
	if dataErr != nil {
		return publicHistoryChange{}, dataErr
	}
	offset := original.PublicHistoryBytes
	next.PublicHistoryBytes = offset + int64(len(data))
	next.PublicHistoryCount = original.PublicHistoryCount + int64(len(newEntries))
	next.PublicHistoryRevision++
	return publicHistoryChange{Data: data, Recovery: &publicHistoryRecovery{Offset: &offset, Existed: true}}, nil
}

func (s *JSONLStore) preparePublicRewrite(
	key string,
	original SessionMeta,
	next *SessionMeta,
	messages []providers.Message,
) (publicHistoryChange, error) {
	if next.HistoryRecoveryRequired || original.PublicHistoryPreparation != nil || original.PublicHistoryVersion < 0 ||
		next.PublicHistoryVersion < 0 {
		return publicHistoryChange{}, nil
	}
	var entries []PublicHistoryEntry
	if original.PublicHistoryVersion == 0 {
		if _, statErr := os.Stat(s.publicHistoryPath(key)); statErr == nil {
			next.PublicHistoryVersion = -1
			return publicHistoryChange{}, nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return publicHistoryChange{}, statErr
		}
		old, oldErr := s.readCommittedHistory(key, original, 0)
		if oldErr != nil {
			return publicHistoryChange{}, oldErr
		}
		for index, message := range old {
			projected, projectionErr := s.projectLegacyPublic(message)
			hasProjection := projectionErr == nil
			if !hasProjection {
				next.PublicHistoryVersion = -1
				return publicHistoryChange{}, nil
			}
			projected.HistoryID, projected.HistoryRunID = message.HistoryID, message.HistoryRunID
			projected = messageWithHistoryOrder(projected, messageHistoryOrder(message))
			old[index] = projected
		}
		entries = publicHistoryEntries(old, original)
		next.PublicHistoryComplete = original.HistoryPrefix == 0
		next.PublicHistoryVersion = PublicHistoryVersion
		next.PublicHistoryGeneration = newHistoryID()
	} else {
		var operationErr error
		entries, operationErr = s.readPublicEntries(key, original)
		if operationErr != nil {
			return publicHistoryChange{}, operationErr
		}
	}
	if next.HistoryGeneration != original.HistoryGeneration {
		active, activeErr := s.readCommittedHistory(key, original, original.Skip)
		if activeErr != nil {
			return publicHistoryChange{}, activeErr
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

func (s *JSONLStore) replacePublicEntries(
	key string,
	entries []PublicHistoryEntry,
	next *SessionMeta,
) (publicHistoryChange, error) {
	data, dataErr1 := encodePublicEntries(entries)
	if dataErr1 != nil {
		return publicHistoryChange{}, dataErr1
	}
	old, dataErr1 := os.ReadFile(s.publicHistoryPath(key))
	existed := dataErr1 == nil
	if dataErr1 != nil && !errors.Is(dataErr1, os.ErrNotExist) {
		return publicHistoryChange{}, dataErr1
	}
	next.PublicHistoryDigest, dataErr1 = publicEntriesDigest(emptyHistoryDigest, entries)
	if dataErr1 != nil {
		return publicHistoryChange{}, dataErr1
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
	file, fileErr := os.OpenFile(path, os.O_WRONLY, 0o600)
	if fileErr != nil {
		return fileErr
	}
	defer file.Close()
	if operationErr2 := file.Truncate(*change.Recovery.Offset); operationErr2 != nil {
		return operationErr2
	}
	if _, operationErr3 := file.Seek(*change.Recovery.Offset, io.SeekStart); operationErr3 != nil {
		return operationErr3
	}
	n, fileErr := file.Write(change.Data)
	if fileErr != nil {
		return fileErr
	}
	if n != len(change.Data) {
		return io.ErrShortWrite
	}
	if operationErr4 := file.Sync(); operationErr4 != nil {
		return operationErr4
	}
	return file.Close()
}

func (s *JSONLStore) restorePublicHistory(key string, recovery *publicHistoryRecovery) error {
	if recovery == nil {
		return nil
	}
	path := s.publicHistoryPath(key)
	if recovery.Offset != nil {
		file, fileErr5 := os.OpenFile(path, os.O_WRONLY, 0o600)
		if fileErr5 != nil {
			return fileErr5
		}
		defer file.Close()
		if operationErr6 := file.Truncate(*recovery.Offset); operationErr6 != nil {
			return operationErr6
		}
		if operationErr7 := file.Sync(); operationErr7 != nil {
			return operationErr7
		}
		return file.Close()
	}
	if !recovery.Existed {
		operationErr8 := os.Remove(path)
		if errors.Is(operationErr8, os.ErrNotExist) {
			return nil
		}
		return operationErr8
	}
	return s.writeSessionFile(path, recovery.Data, 0o600)
}

func (s *JSONLStore) commitPublicChange(key string, next SessionMeta, change publicHistoryChange) error {
	old, oldErr9 := s.readMeta(key)
	if oldErr9 != nil {
		return oldErr9
	}
	if operationErr10 := s.beginHistoryWrite(
		key,
		historyWriteRecovery{Meta: old, Next: next, PublicOnly: true, Public: change.Recovery},
	); operationErr10 != nil {
		return operationErr10
	}
	oldErr9 = s.writePublicChange(key, change)
	if oldErr9 == nil {
		oldErr9 = s.writeMeta(key, next)
	}
	if oldErr9 == nil {
		oldErr9 = s.finishHistoryWrite(key)
	}
	if oldErr9 != nil {
		committed, recoveryErr := s.recoverExpectedHistoryCommit(key, next)
		if committed {
			return nil
		}
		return errors.Join(oldErr9, recoveryErr)
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

func mergePublicImport(
	existing []PublicHistoryEntry,
	request PublicHistoryImport,
	meta SessionMeta,
) ([]PublicHistoryEntry, bool, error) {
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
	if len(existing) == 0 && meta.PublicHistoryComplete && request.BeforeID == "" && request.AfterID == "" {
		return pending, true, nil
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
