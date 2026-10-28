package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

const (
	PublicHistoryRecentMessages = 20
	PublicHistoryPageMessages   = 50
	publicHistoryEnvelopeBytes  = 64 << 10
)

// MessageBytes measures the consumer's actual public projection. Private order
// evidence and the bounded operation envelope have a separate byte budget.
type PublicHistoryPageRequest struct {
	Key          string
	Cursor       string
	MessageBytes func(PublicHistoryEntry) (int, error)
}

type PublicHistoryReadStats struct {
	DataBytesRead  int64 `json:"dataBytesRead"`
	IndexBytesRead int64 `json:"indexBytesRead"`
	LookaheadBytes int64 `json:"lookaheadBytes"`
	MessageBytes   int64 `json:"messageBytes"`
	RowsRead       int   `json:"rowsRead"`
}

type PublicHistoryPage struct {
	Version     int                    `json:"version"`
	Generation  string                 `json:"generation"`
	Revision    uint64                 `json:"revision"`
	Start       int64                  `json:"start"`
	End         int64                  `json:"end"`
	Ceiling     int64                  `json:"ceiling"`
	OlderCursor string                 `json:"olderCursor,omitempty"`
	NewerCursor string                 `json:"newerCursor,omitempty"`
	Entries     []PublicHistoryEntry   `json:"entries"`
	Stats       PublicHistoryReadStats `json:"-"`
}

func (s *JSONLStore) ReadPublicHistoryPage(ctx context.Context, request PublicHistoryPageRequest) (_ PublicHistoryPage, resultErr error) {
	if err := ctx.Err(); err != nil {
		return PublicHistoryPage{}, err
	}
	if request.Cursor != "" {
		if err := ValidatePublicHistoryPageCursor(request.Cursor); err != nil {
			return PublicHistoryPage{}, err
		}
	}
	canonical := strings.TrimSpace(request.Key)
	if canonical == "" || canonical != request.Key {
		return PublicHistoryPage{}, ErrHistoryCursor
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return PublicHistoryPage{}, err
	}
	meta, err := s.readMeta(canonical)
	if err != nil {
		return PublicHistoryPage{}, err
	}
	if err := validateCanonicalHistoryKey(request.Key, canonical, meta); err != nil {
		return PublicHistoryPage{}, err
	}
	if err := s.probeHistoryRecoveryMarker(canonical, &meta); err != nil {
		return PublicHistoryPage{}, err
	}
	if err := s.probeHistoryOrderWitness(canonical, &meta); err != nil {
		return PublicHistoryPage{}, err
	}
	fresh, err := s.readRecoveryMeta(canonical)
	if err != nil || !historyCheckpointMatches(meta, fresh) {
		return PublicHistoryPage{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	if meta.HistoryRecoveryRequired || meta.PublicHistoryVersion != PublicHistoryVersion || !meta.PublicHistoryComplete {
		return PublicHistoryPage{}, ErrPublicHistoryCoverage
	}
	if _, err := encodePublicIndexHeader(publicIndexMeta(meta)); err != nil {
		return PublicHistoryPage{}, err
	}
	index, err := s.openPublicIndex(canonical, meta)
	if errors.Is(err, os.ErrNotExist) {
		if request.Cursor != "" {
			return PublicHistoryPage{}, ErrPublicHistoryCoverage
		}
		return PublicHistoryPage{}, s.preparePublicIndexLocked(ctx, canonical, meta)
	}
	if err != nil {
		return PublicHistoryPage{}, err
	}
	defer closePublicFile(index.file, &resultErr)
	source, err := os.Open(s.publicHistoryPath(canonical))
	if err != nil {
		return PublicHistoryPage{}, err
	}
	defer closePublicFile(source, &resultErr)
	info, err := source.Stat()
	if err != nil || info.Size() != meta.PublicHistoryBytes {
		return PublicHistoryPage{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	cursor, err := readPublicHistoryCursor(request, meta, index)
	if err != nil {
		return PublicHistoryPage{}, err
	}
	return readPublicPage(ctx, request, cursor, source, index)
}

func readPublicPage(ctx context.Context, request PublicHistoryPageRequest, cursor publicHistoryCursor, source *os.File, index *publicIndexReader) (PublicHistoryPage, error) {
	limit := PublicHistoryPageMessages
	if request.Cursor == "" {
		limit = PublicHistoryRecentMessages
	}
	page := PublicHistoryPage{Version: PublicHistoryVersion, Generation: cursor.Generation, Revision: cursor.Revision,
		Start: cursor.Position, End: cursor.Position, Ceiling: cursor.Count, Entries: []PublicHistoryEntry{}}
	position := cursor.Position
	for len(page.Entries) < limit {
		row := position
		if cursor.Direction == publicPageBefore {
			row--
		}
		if row < 0 || row >= cursor.Count {
			break
		}
		if err := ctx.Err(); err != nil {
			return PublicHistoryPage{}, err
		}
		entry, readBytes, err := readPublicPageEntry(source, index, row, cursor.Bytes)
		page.Stats.DataBytesRead += readBytes
		page.Stats.RowsRead++
		if err != nil {
			return PublicHistoryPage{}, err
		}
		size, err := publicPageMessageBytes(entry, request.MessageBytes)
		if err != nil || size > PublicHistoryMessageBytes {
			return PublicHistoryPage{}, errors.Join(ErrHistoryCapacity, err)
		}
		if page.Stats.MessageBytes+int64(size)+publicHistoryEnvelopeBytes > PublicHistoryOperationBytes {
			page.Stats.LookaheadBytes += readBytes
			break
		}
		page.Entries = append(page.Entries, entry)
		page.Stats.MessageBytes += int64(size)
		if cursor.Direction == publicPageBefore {
			position--
			page.Start = position
		} else {
			position++
			page.End = position
		}
	}
	return finishPublicPage(page, cursor, index)
}

func finishPublicPage(page PublicHistoryPage, cursor publicHistoryCursor, index *publicIndexReader) (PublicHistoryPage, error) {
	if cursor.Direction == publicPageBefore {
		for left, right := 0, len(page.Entries)-1; left < right; left, right = left+1, right-1 {
			page.Entries[left], page.Entries[right] = page.Entries[right], page.Entries[left]
		}
	}
	if page.Start > 0 {
		var err error
		page.OlderCursor, err = encodePublicHistoryCursor(cursor, page.Start, publicPageBefore)
		if err != nil {
			return PublicHistoryPage{}, err
		}
	}
	if page.End < cursor.Count {
		var err error
		page.NewerCursor, err = encodePublicHistoryCursor(cursor, page.End, publicPageAfter)
		if err != nil {
			return PublicHistoryPage{}, err
		}
	}
	if len(page.Entries) == 0 && ((cursor.Direction == publicPageBefore && cursor.Position > 0) ||
		(cursor.Direction == publicPageAfter && cursor.Position < cursor.Count)) {
		return PublicHistoryPage{}, ErrHistoryCapacity
	}
	page.Stats.IndexBytesRead = index.bytes
	return page, nil
}

func publicPageMessageBytes(entry PublicHistoryEntry, measure func(PublicHistoryEntry) (int, error)) (int, error) {
	if measure != nil {
		return measure(entry)
	}
	data, err := json.Marshal(entry.Message)
	return len(data), err
}

func readPublicPageEntry(source *os.File, index *publicIndexReader, position, ceilingBytes int64) (PublicHistoryEntry, int64, error) {
	record, err := index.record(position)
	if err != nil {
		return PublicHistoryEntry{}, 0, err
	}
	if record.Length > PublicHistoryOperationBytes {
		return PublicHistoryEntry{}, 0, ErrHistoryCapacity
	}
	if record.Offset+record.Length > ceilingBytes {
		return PublicHistoryEntry{}, 0, ErrPublicHistoryCoverage
	}
	previousDigest := emptyHistoryDigest
	previousEnd := int64(0)
	if position > 0 {
		previous, err := index.record(position - 1)
		if err != nil {
			return PublicHistoryEntry{}, 0, err
		}
		previousDigest, previousEnd = publicIndexRecordDigest(previous), previous.Offset+previous.Length
	}
	if previousEnd != record.Offset {
		return PublicHistoryEntry{}, 0, ErrPublicHistoryCoverage
	}
	data := make([]byte, record.Length)
	n, err := source.ReadAt(data, record.Offset)
	if err != nil {
		return PublicHistoryEntry{}, int64(n), errors.Join(ErrPublicHistoryCoverage, err)
	}
	if data[len(data)-1] != '\n' {
		return PublicHistoryEntry{}, int64(n), ErrPublicHistoryCoverage
	}
	line := data[:len(data)-1]
	if sha256.Sum256(line) != record.Hash || appendPublicDigest(previousDigest, line) != hex.EncodeToString(record.Chain[:]) {
		return PublicHistoryEntry{}, int64(n), ErrPublicHistoryCoverage
	}
	var entry PublicHistoryEntry
	if json.Unmarshal(line, &entry) != nil || !validPublicPageEntry(entry) {
		return PublicHistoryEntry{}, int64(n), ErrPublicHistoryCoverage
	}
	if !bytes.Equal(bytes.TrimSpace(line), line) {
		return PublicHistoryEntry{}, int64(n), ErrPublicHistoryCoverage
	}
	return entry, int64(n), nil
}
