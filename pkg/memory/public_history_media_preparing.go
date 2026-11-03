package memory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func (s *JSONLStore) preparingMediaScope(
	ctx context.Context,
	key, run string,
	meta SessionMeta,
	anchor PublicHistoryEntry,
) (PublicHistoryEntry, string, error) {
	file, err := os.Open(s.jsonlPath(key))
	if err != nil {
		return PublicHistoryEntry{}, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || meta.HistoryBytes == nil || info.Size() != *meta.HistoryBytes {
		return PublicHistoryEntry{}, "", errors.Join(ErrPublicHistoryCoverage, err)
	}
	scanner := bufio.NewScanner(io.LimitReader(file, *meta.HistoryBytes))
	scanner.Buffer(make([]byte, historyReadBufferBytes), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	boundary, line := anchor.ID, 0
	offset := int64(0)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return anchor, boundary, err
		}
		offset += int64(len(scanner.Bytes()))
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		line++
		if line <= meta.Skip {
			continue // The restored model window, not an unproved retained prefix, owns new media.
		}
		var record historyRecord
		if json.Unmarshal(scanner.Bytes(), &record) != nil {
			return anchor, boundary, ErrPublicHistoryCoverage
		}
		if record.RunID != run {
			continue
		}
		if !historyLineageMatches(scanner.Bytes(), meta, key) {
			return anchor, boundary, ErrPublicHistoryCoverage
		}
		if _, visible := publicHistoryMessage(record.Message); !visible {
			continue
		}
		if uuid.Validate(record.ID) != nil {
			return anchor, boundary, ErrPublicHistoryCoverage
		}
		boundary = record.ID
		order, orderErr := decodedHistoryOrder(scanner.Bytes())
		if orderErr != nil && !meta.publicOrderUnavailable {
			return anchor, boundary, orderErr
		}
		legacy, legacyErr := decodedLegacyHistoryStream(scanner.Bytes())
		if legacyErr != nil && !meta.publicOrderUnavailable {
			return anchor, boundary, legacyErr
		}
		fromFence := meta.HistoryFileGeneration == meta.HistoryLegacyFileGeneration && offset <= meta.HistoryLegacyBytes
		if !meta.publicOrderUnavailable {
			if order != (HistoryOrderProof{}) {
				if legacy != "" || order.StreamID != meta.HistoryStreamID || order.Sequence > meta.HistorySequence {
					return anchor, boundary, ErrPublicHistoryCoverage
				}
			} else if meta.HistoryOrderVersion == HistoryOrderVersion && legacy != meta.HistoryStreamID && !fromFence {
				return anchor, boundary, ErrPublicHistoryCoverage
			}
		}
		ref := publicContentRef(meta, line)
		if !meta.publicRefUnavailable && ref != nil {
			matched := false
			for _, locator := range meta.ContentIdentity.Rows {
				if locator.Line == line && locator.Ref == *ref &&
					locator.Digest == contentDigest(bytes.TrimRight(scanner.Bytes(), "\r\n")) {
					matched = true
					break
				}
			}
			if !matched {
				return anchor, boundary, ErrPublicHistoryCoverage
			}
		}
		// Native occurrence authority is independent of its old public projection.
		// This scope never returns or projects any bytes of that old message.
		anchor = PublicHistoryEntry{ID: record.ID, RunID: run, Order: order, Ref: ref}
		anchor = publicHistoryResult(meta, []PublicHistoryEntry{anchor}).Entries[0]
	}
	if err := scanner.Err(); err != nil {
		return anchor, boundary, err
	}
	if line != meta.Count || anchor.ID == "" || boundary == "" {
		return anchor, boundary, ErrPublicHistoryCoverage
	}
	return anchor, boundary, nil
}

// A completed delivery may outlive an older projection gap. Only its exact
// native scope is admitted; preparation still owns publishing the missing rows.
func (s *JSONLStore) recordPreparingPublicMedia(ctx context.Context, key, run, content string) error {
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return err
	}
	if err := s.contentMutationWriterPreflight(ctx, key); err != nil {
		return err
	}
	meta, _, err := s.readContentMeta(key)
	if err != nil {
		return err
	}
	if err = validateCanonicalHistoryKey(key, key, meta); err != nil {
		return err
	}
	if err = s.prepareContentMutationOrderFence(key, &meta); err != nil {
		return err
	}
	if err = checkPublicReadFiles(ctx, os.DirFS(s.dir), sanitizeKey(key), &meta); err != nil {
		return err
	}
	if meta.PublicHistoryVersion > 0 &&
		(meta.PublicHistoryVersion != PublicHistoryVersion || meta.PublicHistoryPreparation == nil) {
		return ErrPublicHistoryCoverage
	}
	if state := meta.PublicHistoryPreparation; state != nil &&
		(state.MediaCount < 0 || state.MediaCount > meta.PublicHistoryCount || state.MediaOffset < 0 ||
			state.MediaOffset > meta.PublicHistoryBytes || (state.MediaCount == 0 && (state.MediaOffset != 0 || state.MediaDigest != "")) ||
			(state.MediaCount > 0 && !validHistoryDigest(state.MediaDigest))) {
		return ErrPublicHistoryCoverage
	}
	if !meta.publicRefUnavailable && meta.ContentIdentity != nil {
		if err = s.hydrateContentStorage(ctx, key, &meta, true, false); err != nil {
			return err
		}
	}
	if meta.PublicHistoryVersion < 0 {
		meta, err = s.recoverDisabledPublicMedia(ctx, key, run, meta)
		if err != nil {
			return err
		}
		if err = checkPublicReadFiles(ctx, os.DirFS(s.dir), sanitizeKey(key), &meta); err != nil {
			return err
		}
	}
	var entries []PublicHistoryEntry
	var existingAnchor PublicHistoryEntry
	if meta.PublicHistoryVersion == PublicHistoryVersion {
		readMeta := meta
		readMeta.PublicHistoryPreparation = nil
		entries, err = s.readPublicEntries(key, readMeta)
		if err != nil {
			return err
		}
		for index, entry := range publicHistoryResult(meta, entries).Entries {
			if entry.ImportID == "" && entry.RunID == run &&
				int64(index) < meta.PublicHistoryCount-meta.PublicHistoryPreparation.MediaCount {
				existingAnchor = entry
			}
		}
	}
	anchor, boundary, err := s.preparingMediaScope(ctx, key, run, meta, existingAnchor)
	if err != nil {
		return err
	}
	if meta.PublicHistoryVersion == 0 {
		if _, err = s.beginPublicPreparation(ctx, key, meta); err != nil {
			return err
		}
		meta, _, err = s.readContentMeta(key)
		if err != nil {
			return err
		}
		if err = checkPublicReadFiles(ctx, os.DirFS(s.dir), sanitizeKey(key), &meta); err != nil {
			return err
		}
	}
	if meta.PublicHistoryPreparation == nil || meta.PublicHistoryVersion != PublicHistoryVersion {
		return ErrPublicHistoryCoverage
	}
	readMeta := meta
	readMeta.PublicHistoryPreparation = nil
	entries, err = s.readPublicEntries(key, readMeta)
	if err != nil {
		return err
	}
	identity := run + ":" + anchor.ID + ":"
	digest := sha256.Sum256([]byte(identity + content))
	importID := "media:" + hex.EncodeToString(digest[:])
	legacy := sha256.Sum256([]byte(run + ":" + content))
	legacyID := "media:" + hex.EncodeToString(legacy[:])
	for _, previous := range entries {
		if boundary == anchor.ID && previous.ImportID == legacyID && previous.AnchorID == anchor.ID {
			importID = legacyID
		}
	}
	entry := PublicHistoryEntry{
		ID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(importID)).String(), RunID: run,
		ImportID: importID, AnchorID: anchor.ID, AnchorRef: anchor.Ref, AnchorOrder: anchor.Order,
		Message: providers.Message{Role: "assistant", Content: content},
	}
	for _, previous := range publicHistoryResult(readMeta, entries).Entries {
		if previous.ImportID == importID {
			if !samePublicEntry(previous, entry) {
				return publicImportConflict()
			}
			return nil
		}
	}
	entry.BoundaryID = boundary
	payload, err := json.Marshal(entry.Message)
	if err != nil {
		return err
	}
	if len(payload) > PublicHistoryMessageBytes {
		return ErrHistoryCapacity
	}
	for index, existing := range entries {
		if existing.ID == boundary && int64(index) < meta.PublicHistoryCount-meta.PublicHistoryPreparation.MediaCount {
			return s.recordPreparedPrefixMedia(ctx, key, meta, entries, entry)
		}
	}
	data, err := encodePublicEntries([]PublicHistoryEntry{entry})
	if err != nil {
		return err
	}
	state := *meta.PublicHistoryPreparation
	if state.MediaCount == 0 {
		state.MediaOffset = meta.PublicHistoryBytes
		state.MediaDigest = meta.PublicHistoryDigest
	}
	state.MediaCount++
	meta.PublicHistoryPreparation = &state
	offset := meta.PublicHistoryBytes
	meta.PublicHistoryBytes += int64(len(data))
	meta.PublicHistoryCount++
	meta.PublicHistoryRevision++
	meta.PublicHistoryDigest, err = publicEntriesDigest(meta.PublicHistoryDigest, []PublicHistoryEntry{entry})
	if err != nil {
		return err
	}
	return s.commitPublicChange(ctx, key, meta, publicHistoryChange{
		Data:     data,
		Recovery: &publicHistoryRecovery{Offset: &offset, Existed: true},
	})
}

func (s *JSONLStore) recordPreparedPrefixMedia(
	ctx context.Context,
	key string,
	meta SessionMeta,
	entries []PublicHistoryEntry,
	entry PublicHistoryEntry,
) error {
	boundary := entry.BoundaryID
	entry.BoundaryID = ""
	// Match the Ready writer: preserve previously completed deliveries at this
	// occurrence, and keep an append beyond an archive frontier non-structural.
	prefixCount := int64(len(entries)) - meta.PublicHistoryPreparation.MediaCount
	for _, previous := range entries[:prefixCount] {
		if previous.RunID == entry.RunID {
			boundary = previous.ID
		}
	}
	next, _, err := mergePublicImport(entries, PublicHistoryImport{
		Key:        key,
		Generation: meta.PublicHistoryGeneration, Revision: meta.PublicHistoryRevision,
		AfterID: boundary, Entries: []PublicHistoryEntry{entry},
	}, meta)
	if err != nil {
		return err
	}
	state := *meta.PublicHistoryPreparation
	if state.MediaCount > 0 {
		prefix, encodeErr := encodePublicEntries(next[:int64(len(next))-state.MediaCount])
		if encodeErr != nil {
			return encodeErr
		}
		state.MediaOffset = int64(len(prefix))
		state.MediaDigest, err = publicEntriesDigest(emptyHistoryDigest, next[:int64(len(next))-state.MediaCount])
		if err != nil {
			return err
		}
	}
	meta.PublicHistoryPreparation = &state
	if prefixCount == 0 || boundary != entries[prefixCount-1].ID {
		meta.PublicHistoryGeneration = newHistoryID()
	}
	change, err := s.replacePublicEntries(key, next, &meta)
	if err != nil {
		return err
	}
	return s.commitPublicChange(ctx, key, meta, change)
}
