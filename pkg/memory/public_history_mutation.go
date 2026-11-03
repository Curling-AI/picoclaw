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
	Index   *publicIndexRecovery `json:"index,omitempty"`
	Offset  *int64               `json:"offset,omitempty"`
	Data    []byte               `json:"data,omitempty"`
	Existed bool                 `json:"existed,omitempty"`
}

type publicHistoryChange struct {
	Index    *publicIndexChange
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
	projectionUnavailable := next.publicProjectionUnavailable
	next.publicProjectionUnavailable = false
	if original.PublicHistoryPreparation != nil || original.PublicHistoryVersion < 0 ||
		next.PublicHistoryVersion < 0 {
		return publicHistoryChange{}, nil
	}
	if projectionUnavailable {
		if next.HistoryBytes == nil {
			return publicHistoryChange{}, ErrPublicHistoryCoverage
		}
		state := &PublicHistoryPreparation{
			FileGeneration: next.HistoryFileGeneration,
			Generation:     next.HistoryGeneration,
			Ceiling:        *next.HistoryBytes,
			Count:          int64(next.Count),
			Offset:         sourceOffset,
			Line:           int64(original.Count),
			Prefix:         next.HistoryPrefix,
		}
		if original.PublicHistoryVersion == 0 {
			if _, err := os.Stat(s.publicHistoryPath(key)); err == nil || !errors.Is(err, os.ErrNotExist) {
				return publicHistoryChange{}, ErrPublicHistoryCoverage
			}
			state.Offset, state.Line = 0, 0
			next.PublicHistoryVersion = PublicHistoryVersion
			next.PublicHistoryGeneration = newHistoryID()
			next.PublicHistoryDigest = emptyHistoryDigest
			next.PublicHistoryPreparation = state
			return s.replacePublicEntries(key, nil, next)
		}
		next.PublicHistoryPreparation = state
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
	return s.preparePublicIndex(
		key,
		original,
		*next,
		publicHistoryChange{Data: data, Recovery: &publicHistoryRecovery{Offset: &offset, Existed: true}},
	)
}

func (s *JSONLStore) preparePublicRewrite(
	key string,
	original SessionMeta,
	next *SessionMeta,
	messages []providers.Message,
) (publicHistoryChange, error) {
	collecting := next.publicHistoryGC
	next.publicHistoryGC = false
	if next.publicHistoryClear {
		next.publicHistoryClear = false
		next.PublicHistoryVersion = PublicHistoryVersion
		next.PublicHistoryPreparation = nil
		next.PublicHistoryGeneration = newHistoryID()
		next.PublicHistoryComplete = true
		return s.replacePublicEntries(key, nil, next)
	}
	if original.PublicHistoryVersion < 0 || next.PublicHistoryVersion < 0 {
		return publicHistoryChange{}, ErrPublicHistoryCoverage
	}
	if next.publicProjectionUnavailable {
		return publicHistoryChange{}, &publicArtifactVerificationError{cause: ErrPublicHistoryCoverage}
	}
	if original.PublicHistoryVersion == 0 {
		_, statErr := os.Stat(s.publicHistoryPath(key))
		if statErr == nil {
			return publicHistoryChange{}, ErrPublicHistoryCoverage
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return publicHistoryChange{}, statErr
		}
	}
	var entries []PublicHistoryEntry
	if original.PublicHistoryVersion == 0 || original.PublicHistoryPreparation != nil {
		// Structural rewrites already read the complete source. Materialize its
		// display in the same OLD/NEW journal before retiring any physical row.
		old, sourceLines, filtered, oldErr := s.readPublicSourceLines(key, original, 0, !collecting)
		if oldErr != nil {
			if errors.Is(oldErr, ErrPublicHistoryCoverage) {
				return publicHistoryChange{}, &publicArtifactVerificationError{cause: oldErr}
			}
			return publicHistoryChange{}, oldErr
		}
		for index, message := range old {
			projected, projectionErr := s.projectLegacyPublic(message)
			if projectionErr != nil {
				return publicHistoryChange{}, &publicArtifactVerificationError{cause: projectionErr}
			}
			if projector := s.historyPublicProjector(); projector != nil {
				if verifyErr := projector.Verify(context.Background(), projected); verifyErr != nil {
					return publicHistoryChange{}, &publicArtifactVerificationError{cause: verifyErr}
				}
			}
			// Keep the raw receipt body separate from its immutable public projection.
			old[index].HistoryPublicMessage = &projected
		}
		if original.ContentIdentity != nil && !original.ContentIdentity.Unavailable {
			if err := s.hydrateContentStorage(context.Background(), key, &original, true, false); err != nil {
				return publicHistoryChange{}, err
			}
		}
		entries = publicHistoryEntries(old, original)
		for index := range entries {
			entries[index].Ref = publicContentRef(original, sourceLines[entries[index].ID])
		}
		if original.PublicHistoryVersion == PublicHistoryVersion {
			readMeta := original
			readMeta.PublicHistoryPreparation = nil
			previous, readErr := s.readPublicEntries(key, readMeta)
			if readErr != nil {
				return publicHistoryChange{}, readErr
			}
			// Previously committed supplements and compacted source entries have
			// no remaining source row. Preserve their exact identifiers/bytes.
			seen := make(map[string]bool, len(previous))
			for _, entry := range previous {
				seen[entry.ID] = true
			}
			confirmed := previous
			for _, entry := range entries {
				if !seen[entry.ID] {
					confirmed = append(confirmed, entry)
				}
			}
			entries = confirmed
		}
		next.PublicHistoryPreparation = nil
		next.PublicHistoryComplete = !filtered && (original.PublicHistoryComplete || original.HistoryPrefix == 0)
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
	return s.rewritePublicDisplay(key, original, next, messages, entries)
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
	original := SessionMeta{}
	if existed {
		var metaErr error
		original, metaErr = s.readMeta(key)
		if metaErr != nil {
			return publicHistoryChange{}, metaErr
		}
	}
	if existed && bytes.Equal(old, data) && original.PublicHistoryVersion == PublicHistoryVersion &&
		original.PublicHistoryDigest == next.PublicHistoryDigest {
		// An unchanged public view keeps its index and cursors even when model history compacted.
		next.PublicHistoryGeneration = original.PublicHistoryGeneration
		next.PublicHistoryRevision = original.PublicHistoryRevision
		return publicHistoryChange{}, nil
	}
	if existed && bytes.HasPrefix(data, old) && original.PublicHistoryGeneration == next.PublicHistoryGeneration {
		offset := int64(len(old))
		return s.preparePublicIndex(key, original, *next, publicHistoryChange{
			Data: data[len(old):], Recovery: &publicHistoryRecovery{Offset: &offset, Existed: true},
		})
	}
	return s.preparePublicIndex(key, SessionMeta{}, *next,
		publicHistoryChange{Data: data, Recovery: &publicHistoryRecovery{Data: old, Existed: existed}})
}

func (s *JSONLStore) writePublicChange(key string, change publicHistoryChange) (resultErr error) {
	if change.Recovery == nil {
		return nil
	}
	path := s.publicHistoryPath(key)
	if change.Recovery.Offset == nil {
		if err := s.writeSessionFile(path, change.Data, 0o600); err != nil {
			return err
		}
		return s.writePublicIndex(key, change.Index)
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
	return s.writePublicIndex(key, change.Index)
}

func (s *JSONLStore) restorePublicHistory(key string, recovery *publicHistoryRecovery) (resultErr error) {
	if recovery == nil {
		return nil
	}
	if err := s.restorePublicIndex(key, recovery.Index); err != nil {
		return err
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
	indexed := change
	if indexed.Index == nil {
		indexed, oldErr9 = s.preparePublicIndex(key, old, next, change)
		if oldErr9 != nil {
			return oldErr9
		}
	}
	if operationErr10 := s.beginHistoryWrite(
		ctx, key,
		historyWriteRecovery{Meta: old, Next: next, PublicOnly: true, Public: indexed.Recovery},
	); operationErr10 != nil {
		return operationErr10
	}
	oldErr9 = s.writePublicChange(key, indexed)
	if oldErr9 == nil {
		oldErr9 = s.writeMeta(key, next)
	}
	if oldErr9 == nil && old.PublicHistoryNonLocalImports != next.PublicHistoryNonLocalImports {
		oldErr9 = s.ensureContentDomain(key, old, next)
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
	if b.AnchorRef == nil {
		a.AnchorRef = nil
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
	for _, entry := range publicHistoryResult(meta, existing).Entries {
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

// This is context restoration, not GC. Unknown coverage/checkpoints remain
// unavailable, while a verified existing display can be edited without reading
// or retiring its raw historical prefix. Mixed raw lineage never grants ACK/GC.
func (s *JSONLStore) preparePublicRetainedRewrite(
	key string,
	original SessionMeta,
	next *SessionMeta,
	messages []providers.Message,
) (publicHistoryChange, error) {
	if original.PublicHistoryVersion != PublicHistoryVersion || original.PublicHistoryPreparation != nil {
		next.publicProjectionUnavailable = false
		next.PublicHistoryComplete = false
		return publicHistoryChange{}, nil
	}
	entries, err := s.readPublicEntries(key, original)
	if err != nil {
		if !publicProofUnavailable(err) {
			return publicHistoryChange{}, err
		}
		next.PublicHistoryVersion = -1
		next.PublicHistoryComplete = false
		next.publicProjectionUnavailable = false
		return publicHistoryChange{}, nil
	}
	if next.publicProjectionUnavailable {
		// The old display bytes remain owned. A missing immutable projection cannot
		// be replaced with mutable input or be promoted to complete coverage.
		next.PublicHistoryVersion = -1
		next.PublicHistoryComplete = false
		next.publicProjectionUnavailable = false
		return publicHistoryChange{}, nil
	}
	return s.rewritePublicDisplay(key, original, next, messages, entries)
}
