package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"time"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

const emptyHistoryDigest = "0000000000000000000000000000000000000000000000000000000000000000"

func appendHistoryDigest(previous string, message providers.Message) (string, error) {
	data, err := json.Marshal(message)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(append([]byte(previous+":"), data...))), nil
}

func historyDigest(messages []providers.Message) (string, error) {
	digest := emptyHistoryDigest
	for _, message := range messages {
		var err error
		digest, err = appendHistoryDigest(digest, message)
		if err != nil {
			return "", err
		}
	}
	return digest, nil
}

func prepareHistoryMessage(original providers.Message, at time.Time, runID string) providers.Message {
	message := original
	if message.CreatedAt == nil {
		message.CreatedAt = &at
	}
	if uuid.Validate(message.HistoryID) != nil {
		message.HistoryID = newHistoryID()
		message.HistoryRunID = runID
	}
	return message
}

// Only fallback replacements inspect the old active window. Normal append
// checkpoints prove their prefix from metadata and write just the new suffix.
func (s *JSONLStore) replaceHistory(
	ctx context.Context,
	key string,
	input []providers.Message,
	original SessionMeta,
) error {
	meta := original
	old, err := readMessages(s.jsonlPath(key), meta.Skip)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	common := 0
	prepared := append([]providers.Message(nil), input...)
	for index, message := range prepared {
		if index < len(old) && common == index && sameHistoryCore(old[index], message) &&
			(message.HistoryID == "" || message.HistoryID == old[index].HistoryID) {
			message.HistoryID, message.HistoryRunID = old[index].HistoryID, old[index].HistoryRunID
			if message.CreatedAt == nil {
				message.CreatedAt = old[index].CreatedAt
			}
			common++
		} else if index < len(old) && message.HistoryID == old[index].HistoryID &&
			!sameHistoryCore(old[index], message) {
			message.HistoryRunID = providers.HistoryRun(ctx)
		}
		prepared[index] = prepareHistoryMessage(message, meta.UpdatedAt, providers.HistoryRun(ctx))
	}
	oldDigest, err := historyDigest(old)
	if err != nil {
		return err
	}
	meta.HistoryDigest = oldDigest
	if len(old) > 0 {
		meta.HistoryTip = old[len(old)-1].HistoryID
	}
	if common == len(old) && len(prepared) >= len(old) {
		if appended, appendErr := s.appendHistorySuffix(key, prepared, meta); appended || appendErr != nil {
			return appendErr
		}
	}
	ensureHistoryGeneration(&meta, key)
	meta.HistoryPrefix += int64(meta.Skip)
	if common != len(old) || len(prepared) < len(old) {
		meta.HistoryParentGeneration = meta.HistoryGeneration
		meta.HistoryRetainedUntil = meta.HistoryPrefix + int64(common)
		meta.HistoryMutationRunID = providers.HistoryRun(ctx)
		resetHistoryGeneration(&meta)
	} else {
		meta.HistoryFileGeneration = newHistoryID()
		meta.HistoryFileEpoch++
	}
	meta.Skip, meta.Count = 0, len(prepared)
	meta.HistoryDigest, err = historyDigest(prepared)
	if err != nil {
		return err
	}
	meta.HistoryTip = ""
	if len(prepared) > 0 {
		meta.HistoryTip = prepared[len(prepared)-1].HistoryID
	}
	if err := s.writeMeta(key, meta); err != nil {
		return err
	}
	return s.rewriteJSONL(key, prepared, meta)
}

func sameHistoryCore(a, b providers.Message) bool {
	// Audit, cache and model metadata can change after a turn; they retire a
	// physical cursor without declaring that a user edited the transcript.
	return a.Role == b.Role && a.Content == b.Content && a.ToolCallID == b.ToolCallID &&
		reflect.DeepEqual(a.ToolCalls, b.ToolCalls) &&
		reflect.DeepEqual(a.Media, b.Media) &&
		reflect.DeepEqual(a.Attachments, b.Attachments)
}

func (s *JSONLStore) appendHistorySuffix(key string, messages []providers.Message, original SessionMeta) (bool, error) {
	meta := original
	activeCount := meta.Count - meta.Skip
	if !validHistoryDigest(meta.HistoryDigest) || activeCount < 0 || len(messages) < activeCount {
		return false, nil
	}
	digest, digestErr := historyDigest(messages[:activeCount])
	if digestErr != nil {
		return false, digestErr
	}
	if digest != meta.HistoryDigest || !s.historyTipMatches(key, meta) {
		return false, nil
	}
	if len(messages) == activeCount {
		if activeCount == 0 {
			file, err := os.OpenFile(s.jsonlPath(key), os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return false, err
			}
			if err := file.Sync(); err != nil {
				file.Close()
				return false, err
			}
			if err := file.Close(); err != nil {
				return false, err
			}
		}
		return true, s.writeMeta(key, meta)
	}
	var data bytes.Buffer
	for _, message := range messages[activeCount:] {
		line, err := marshalHistoryMessage(message, meta)
		if err != nil {
			return false, err
		}
		data.Write(line)
		data.WriteByte('\n')
		digest, digestErr = appendHistoryDigest(digest, message)
		if digestErr != nil {
			return false, digestErr
		}
	}
	file, err := os.OpenFile(s.jsonlPath(key), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := file.Write(data.Bytes()); err != nil {
		file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	meta.Count += len(messages) - activeCount
	meta.HistoryDigest = digest
	meta.HistoryTip = messages[len(messages)-1].HistoryID
	return true, s.writeMeta(key, meta)
}

func (s *JSONLStore) historyTipMatches(key string, meta SessionMeta) bool {
	tip, count := meta.HistoryTip, meta.Count
	file, err := os.Open(s.jsonlPath(key))
	if err != nil {
		return os.IsNotExist(err) && count == 0
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	if info.Size() == 0 {
		return count == 0
	}
	length := min(info.Size(), int64(HistoryMessageBytes))
	data := make([]byte, length)
	if _, err := file.ReadAt(data, info.Size()-length); err != nil && err != io.EOF {
		return false
	}
	line := bytes.TrimRight(data, "\r\n")
	last := bytes.LastIndexByte(line, '\n')
	if last < 0 && length < info.Size() {
		return false
	}
	line = line[last+1:]
	var record struct {
		ID string `json:"history_id"`
	}
	return json.Unmarshal(line, &record) == nil && record.ID == tip && historyLineageMatches(line, meta, key)
}

func validHistoryDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
