package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

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
	if original.PublicHistoryVersion == 0 {
		_, statErr := os.Stat(s.publicHistoryPath(key))
		if statErr == nil {
			next.PublicHistoryVersion = -1
			return publicHistoryChange{}, nil
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return publicHistoryChange{}, statErr
		}
	}
	var entries []PublicHistoryEntry
	if original.PublicHistoryVersion == 0 {
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
	if original.ContentIdentity != nil {
		return s.rewritePublicContent(key, original, next, messages, entries)
	}
	if next.HistoryGeneration == original.HistoryGeneration {
		return s.replacePublicEntries(key, entries, next)
	}
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
	}
	if boundary < 0 {
		return publicHistoryChange{}, ErrPublicHistoryCoverage
	}
	entries = append(entries[:boundary:boundary], publicHistoryEntries(messages, *next)...)
	next.PublicHistoryGeneration = newHistoryID()

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

func (s *JSONLStore) writePublicChange(key string, change publicHistoryChange) (resultErr error) {
	if change.Recovery == nil {
		return nil
	}
	path := s.publicHistoryPath(key)
	if change.Recovery.Offset == nil {
		return s.writeSessionFile(path, change.Data, 0o600)
	}
	file, fileErr := os.OpenFile(filepath.Clean(path), os.O_WRONLY, 0o600)
	if fileErr != nil {
		return fileErr
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
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
	return nil
}

func (s *JSONLStore) restorePublicHistory(key string, recovery *publicHistoryRecovery) (resultErr error) {
	if recovery == nil {
		return nil
	}
	path := s.publicHistoryPath(key)
	if recovery.Offset != nil {
		file, fileErr5 := os.OpenFile(filepath.Clean(path), os.O_WRONLY, 0o600)
		if fileErr5 != nil {
			return fileErr5
		}
		defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
		if operationErr6 := file.Truncate(*recovery.Offset); operationErr6 != nil {
			return operationErr6
		}
		if operationErr7 := file.Sync(); operationErr7 != nil {
			return operationErr7
		}
		return nil
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

func (s *JSONLStore) commitPublicChange(
	ctx context.Context,
	key string,
	next SessionMeta,
	change publicHistoryChange,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	old, _, oldErr9 := s.readContentMeta(key)
	if oldErr9 != nil {
		return oldErr9
	}
	if operationErr10 := s.beginHistoryWrite(
		ctx, key,
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
		committed, recoveryErr := s.recoverExpectedHistoryContentCommit(ctx, key, next)
		if committed {
			return nil
		}
		return errors.Join(oldErr9, recoveryErr)
	}
	return nil
}

func samePublicEntry(a, b PublicHistoryEntry) bool {
	a.BoundaryID = ""
	if b.Ref == nil {
		a.Ref = nil
	}
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
		previous, imported := byImport[entry.ImportID]
		if imported && !samePublicEntry(previous, entry) {
			return nil, false, publicImportConflict()
		}
		if imported {
			continue
		}
		if _, ok := byID[entry.ID]; ok {
			return nil, false, publicImportConflict()
		}
		anchor, anchored := byID[entry.AnchorID]
		if entry.AnchorID != "" &&
			(!anchored || anchor.Order != entry.AnchorOrder || !samePublicRef(anchor.Ref, entry.AnchorRef)) {
			return nil, false, publicImportConflict()
		}
		entry.BoundaryID = request.BeforeID
		if entry.BoundaryID == "" {
			entry.BoundaryID = request.AfterID
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
