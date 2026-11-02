package memory

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

const (
	PublicHistoryRecentMessages = 20
	PublicHistoryPageMessages   = 50
	publicHistoryEnvelopeBytes  = 64 << 10
)

// PublicHistoryPageRequest measures messages separately from private evidence and the bounded envelope.
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

func (s *JSONLStore) ReadPublicHistoryPage(
	ctx context.Context,
	request PublicHistoryPageRequest,
) (PublicHistoryPage, error) {
	if operationErr0 := ctx.Err(); operationErr0 != nil {
		return PublicHistoryPage{}, operationErr0
	}
	if request.Cursor != "" {
		if operationErr1 := ValidatePublicHistoryPageCursor(request.Cursor); operationErr1 != nil {
			return PublicHistoryPage{}, operationErr1
		}
	}
	if strings.TrimSpace(request.Key) == "" || strings.TrimSpace(request.Key) != request.Key {
		return PublicHistoryPage{}, ErrHistoryCursor
	}
	lock := s.sessionLock(request.Key)
	lock.Lock()
	defer lock.Unlock()
	return ReadPublicHistoryPageFS(ctx, os.DirFS(s.dir), request)
}

func readPublicPage(
	ctx context.Context,
	request PublicHistoryPageRequest,
	cursor publicHistoryCursor,
	source io.ReaderAt,
	index *publicIndexReader,
) (PublicHistoryPage, error) {
	limit := PublicHistoryPageMessages
	if request.Cursor == "" {
		limit = PublicHistoryRecentMessages
	}
	page := PublicHistoryPage{
		Version: PublicHistoryVersion, Generation: cursor.Generation, Revision: cursor.Revision,
		Start: cursor.Position, End: cursor.Position, Ceiling: cursor.Count, Entries: []PublicHistoryEntry{},
	}
	position := cursor.Position
	for len(page.Entries) < limit {
		row := position
		if cursor.Direction == publicPageBefore {
			row--
		}
		if row < 0 || row >= cursor.Count {
			break
		}
		if operationErr2 := ctx.Err(); operationErr2 != nil {
			return PublicHistoryPage{}, operationErr2
		}
		record, recordErr := index.record(row)
		if recordErr != nil {
			return PublicHistoryPage{}, recordErr
		}
		if record.Length > PublicHistoryOperationBytes-page.Stats.DataBytesRead {
			break
		}
		entry, readBytes, operationErr3 := readPublicPageEntry(source, index, row, cursor.Bytes)
		page.Stats.DataBytesRead += readBytes
		page.Stats.RowsRead++
		if operationErr3 != nil {
			return PublicHistoryPage{}, operationErr3
		}
		size, operationErr3 := publicPageMessageBytes(entry, request.MessageBytes)
		if operationErr3 != nil || size > PublicHistoryMessageBytes {
			return PublicHistoryPage{}, errors.Join(ErrHistoryCapacity, operationErr3)
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

func finishPublicPage(
	page PublicHistoryPage,
	cursor publicHistoryCursor,
	index *publicIndexReader,
) (PublicHistoryPage, error) {
	if cursor.Direction == publicPageBefore {
		for left, right := 0, len(page.Entries)-1; left < right; left, right = left+1, right-1 {
			page.Entries[left], page.Entries[right] = page.Entries[right], page.Entries[left]
		}
	}
	if page.Start > 0 {
		var operationErr4 error
		page.OlderCursor, operationErr4 = encodePublicHistoryCursor(cursor, page.Start, publicPageBefore)
		if operationErr4 != nil {
			return PublicHistoryPage{}, operationErr4
		}
	}
	if page.End < cursor.Count {
		var operationErr5 error
		page.NewerCursor, operationErr5 = encodePublicHistoryCursor(cursor, page.End, publicPageAfter)
		if operationErr5 != nil {
			return PublicHistoryPage{}, operationErr5
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
	data, operationErr6 := json.Marshal(entry.Message)
	return len(data), operationErr6
}

func readPublicPageEntry(
	source io.ReaderAt,
	index *publicIndexReader,
	position, ceilingBytes int64,
) (PublicHistoryEntry, int64, error) {
	record, operationErr7 := index.record(position)
	if operationErr7 != nil {
		return PublicHistoryEntry{}, 0, operationErr7
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
		previous, previousErr := index.record(position - 1)
		if previousErr != nil {
			return PublicHistoryEntry{}, 0, previousErr
		}
		previousDigest, previousEnd = publicIndexRecordDigest(previous), previous.Offset+previous.Length
	}
	if previousEnd != record.Offset {
		return PublicHistoryEntry{}, 0, ErrPublicHistoryCoverage
	}
	data := make([]byte, record.Length)
	n, operationErr7 := source.ReadAt(data, record.Offset)
	if operationErr7 != nil {
		return PublicHistoryEntry{}, int64(n), errors.Join(ErrPublicHistoryCoverage, operationErr7)
	}
	if data[len(data)-1] != '\n' {
		return PublicHistoryEntry{}, int64(n), ErrPublicHistoryCoverage
	}
	line := data[:len(data)-1]
	if publicIndexRecordHash(record, line) != record.Hash ||
		appendPublicDigest(previousDigest, line) != hex.EncodeToString(record.Chain[:]) {
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
