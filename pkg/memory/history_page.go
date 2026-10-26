package memory

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
)

const (
	HistoryPageMessages    = 50
	HistoryMessageBytes    = 256 << 10
	HistoryPageBytes       = 1 << 20
	historyEnvelopeBytes   = 16 << 10
	historyReadBufferBytes = 32 << 10
	maxHistoryPosition     = int64(1<<53 - 1)
)

var (
	ErrHistoryCursor      = errors.New("history source cursor invalid or retired")
	ErrHistoryCapacity    = errors.New("history source exceeds operation budget")
	ErrHistoryUnsupported = errors.New("history source does not support bounded pages")
)

type HistoryPageRequest struct {
	Key     string
	Cursor  string
	Refresh bool
}

type HistoryEntry struct {
	ID       string            `json:"id"`
	Position int64             `json:"position"`
	Message  providers.Message `json:"message"`
}

type HistoryPage struct {
	ParentGeneration    string         `json:"parentGeneration"`
	RetainedUntil       int64          `json:"retainedUntil"`
	MutationRunID       string         `json:"mutationRunId,omitempty"`
	TotalPosition       int64          `json:"totalPosition"`
	Summary             string         `json:"summary"`
	Epoch               int64          `json:"epoch"`
	FileEpoch           int64          `json:"fileEpoch"`
	ActiveStartPosition int64          `json:"activeStartPosition"`
	Generation          string         `json:"generation"`
	FileGeneration      string         `json:"fileGeneration"`
	StartPosition       int64          `json:"startPosition"`
	EndPosition         int64          `json:"endPosition"`
	SourcePrefix        int64          `json:"sourcePrefix"`
	Complete            bool           `json:"complete"`
	ResumeCursor        string         `json:"resumeCursor"`
	Entries             []HistoryEntry `json:"entries"`
	LinesRead           int            `json:"linesRead"`
	BytesRead           int            `json:"bytesRead"`
}

type historyPageCursor struct {
	Key            [32]byte `json:"k"`
	Generation     string   `json:"g"`
	FileGeneration string   `json:"f"`
	Byte           int64    `json:"b"`
	Line           int64    `json:"l"`
	Ceiling        int64    `json:"c"`
	Total          int64    `json:"t"`
}

// ReadHistoryPage seeks directly to the previous byte checkpoint. Importing a
// long file never rescans an earlier prefix; retries seek to their own batch.
// It shares the writer's lock, so metadata and the committed JSONL agree.
func (s *JSONLStore) ReadHistoryPage(ctx context.Context, request HistoryPageRequest) (HistoryPage, error) {
	lock := s.sessionLock(request.Key)
	lock.Lock()
	defer lock.Unlock()
	if request.Key == "" || ctx.Err() != nil {
		return HistoryPage{}, ErrHistoryCursor
	}
	meta, err := s.readMeta(request.Key)
	if err != nil {
		return HistoryPage{}, err
	}
	ensureHistoryGeneration(&meta, request.Key)
	file, err := os.Open(s.jsonlPath(request.Key))
	if errors.Is(err, os.ErrNotExist) && meta.Count == 0 && meta.HistoryFileEpoch == 0 {
		file, err = os.Open(os.DevNull)
	}
	if err != nil {
		return HistoryPage{}, fmt.Errorf("open bounded history source: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return HistoryPage{}, fmt.Errorf("stat bounded history source: %w", err)
	}
	cursor, err := readPageCursor(request, meta, info.Size())
	if err != nil {
		return HistoryPage{}, err
	}
	if _, err := file.Seek(cursor.Byte, io.SeekStart); err != nil {
		return HistoryPage{}, fmt.Errorf("seek bounded history source: %w", err)
	}
	return readSourcePage(ctx, file, sourcePageState{meta: meta, cursor: cursor, key: request.Key})
}

func readPageCursor(request HistoryPageRequest, meta SessionMeta, size int64) (historyPageCursor, error) {
	c := historyPageCursor{
		Key:            sha256.Sum256([]byte(request.Key)),
		Generation:     meta.HistoryGeneration,
		FileGeneration: meta.HistoryFileGeneration,
		Ceiling:        size,
		Total:          meta.HistoryPrefix + int64(meta.Count),
	}
	if request.Cursor != "" {
		if len(request.Cursor) > 1024 {
			return c, ErrHistoryCursor
		}
		data, err := base64.RawURLEncoding.DecodeString(request.Cursor)
		var previous historyPageCursor
		if err != nil || json.Unmarshal(data, &previous) != nil || previous.Key != c.Key ||
			previous.Generation != c.Generation ||
			previous.FileGeneration != c.FileGeneration {
			return c, ErrHistoryCursor
		}
		c = previous
		if request.Refresh {
			c.Ceiling = size
			c.Total = meta.HistoryPrefix + int64(meta.Count)
		}
	}
	if c.Byte < 0 || c.Byte > c.Ceiling || c.Ceiling > size || c.Line < 0 ||
		c.Line > maxHistoryPosition-meta.HistoryPrefix ||
		meta.HistoryPrefix < 0 ||
		c.Total < meta.HistoryPrefix+c.Line ||
		c.Total > meta.HistoryPrefix+int64(meta.Count) {
		return c, ErrHistoryCursor
	}
	return c, nil
}

type historyReadCounter struct {
	reader io.Reader
	bytes  int
}

func (r *historyReadCounter) Read(data []byte) (int, error) {
	n, err := r.reader.Read(data)
	r.bytes += n
	return n, err
}

type sourcePageState struct {
	meta   SessionMeta
	cursor historyPageCursor
	key    string
}

func readSourcePage(ctx context.Context, file *os.File, state sourcePageState) (HistoryPage, error) {
	meta, cursor := state.meta, state.cursor
	summary, summaryErr := json.Marshal(meta.Summary)
	if summaryErr != nil || len(summary) > HistoryMessageBytes {
		return HistoryPage{}, ErrHistoryCapacity
	}
	page := HistoryPage{
		TotalPosition:       cursor.Total,
		Epoch:               meta.HistoryEpoch,
		FileEpoch:           meta.HistoryFileEpoch,
		ActiveStartPosition: meta.HistoryPrefix + int64(meta.Skip),
		Generation:          meta.HistoryGeneration,
		FileGeneration:      meta.HistoryFileGeneration,
		SourcePrefix:        meta.HistoryPrefix,
		StartPosition:       meta.HistoryPrefix + cursor.Line,
		Entries:             []HistoryEntry{},
	}
	page.Summary = meta.Summary
	page.ParentGeneration, page.RetainedUntil, page.MutationRunID = meta.HistoryParentGeneration, meta.HistoryRetainedUntil, meta.HistoryMutationRunID
	counter := &historyReadCounter{reader: io.LimitReader(file, cursor.Ceiling-cursor.Byte)}
	reader := bufio.NewReaderSize(counter, historyReadBufferBytes)
	payloadBytes := historyEnvelopeBytes + len(summary)
	for cursor.Byte < cursor.Ceiling && page.LinesRead < HistoryPageMessages {
		if err := ctx.Err(); err != nil {
			return HistoryPage{}, err
		}
		line, err := readHistoryLine(reader)
		page.LinesRead++
		if errors.Is(err, ErrHistoryCapacity) || errors.Is(err, bufio.ErrBufferFull) ||
			len(line) > HistoryMessageBytes {
			if len(page.Entries) > 0 {
				break
			}
			return HistoryPage{}, ErrHistoryCapacity
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return HistoryPage{}, ErrHistoryCapacity
			}
			return HistoryPage{}, fmt.Errorf("read bounded history line: %w", err)
		}
		if len(line) == 1 && line[0] == '\n' {
			cursor.Byte++
			continue
		}
		var message providers.Message
		if json.Unmarshal(line, &message) != nil || messageutil.IsTransientAssistantThoughtMessage(message) {
			// Match GetHistory's legacy filtering while retaining physical cursor positions.
			cursor.Byte += int64(len(line))
			cursor.Line++
			continue
		}
		if !historyLineageMatches(line, meta, state.key) {
			return HistoryPage{}, ErrHistoryCursor
		}
		message.HistoryID = decodedHistoryID(line, filepath.Base(file.Name()), cursor.Line)
		message.HistoryRunID = decodedHistoryRunID(line)
		entry := HistoryEntry{ID: message.HistoryID, Position: meta.HistoryPrefix + cursor.Line, Message: message}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return HistoryPage{}, fmt.Errorf("encode bounded history entry: %w", err)
		}
		if payloadBytes+len(encoded) > HistoryPageBytes {
			break
		}
		page.Entries = append(page.Entries, entry)
		payloadBytes += len(encoded)
		cursor.Byte += int64(len(line))
		cursor.Line++
	}
	page.EndPosition = meta.HistoryPrefix + cursor.Line
	page.BytesRead = counter.bytes
	page.Complete = cursor.Byte == cursor.Ceiling
	if page.EndPosition > cursor.Total || (page.Complete && page.EndPosition != cursor.Total) {
		return HistoryPage{}, ErrHistoryCursor
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return HistoryPage{}, fmt.Errorf("encode bounded history checkpoint: %w", err)
	}
	page.ResumeCursor = base64.RawURLEncoding.EncodeToString(data)
	return page, nil
}

func readHistoryLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > HistoryMessageBytes {
			return nil, ErrHistoryCapacity
		}
		line = append(line, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}
