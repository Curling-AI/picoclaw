package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type historyRecord struct {
	providers.Message
	ID             string `json:"history_id"`
	RunID          string `json:"history_run_id,omitempty"`
	Generation     string `json:"history_generation"`
	FileGeneration string `json:"history_file_generation"`
}

func newHistoryID() string { return uuid.NewString() }

func ensureHistoryGeneration(meta *SessionMeta, key string) {
	if meta.HistoryGeneration == "" {
		meta.HistoryGeneration = legacyHistoryGeneration(key)
	}
	if meta.HistoryFileGeneration == "" {
		meta.HistoryFileGeneration = meta.HistoryGeneration
	}
}

func resetHistoryGeneration(meta *SessionMeta) {
	meta.HistoryEpoch++
	meta.HistoryFileEpoch++
	meta.HistoryGeneration = newHistoryID()
	meta.HistoryFileGeneration = meta.HistoryGeneration
}

func legacyHistoryGeneration(key string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("legacy-history:"+key)).String()
}

func marshalHistoryMessage(message providers.Message, meta SessionMeta) ([]byte, error) {
	id := message.HistoryID
	if uuid.Validate(id) != nil {
		id = newHistoryID()
	}
	return json.Marshal(
		historyRecord{
			Message:        message,
			ID:             id,
			RunID:          message.HistoryRunID,
			Generation:     meta.HistoryGeneration,
			FileGeneration: meta.HistoryFileGeneration,
		},
	)
}

func decodedHistoryRunID(data []byte) string {
	var record struct {
		RunID string `json:"history_run_id"`
	}
	if json.Unmarshal(data, &record) == nil && uuid.Validate(record.RunID) == nil {
		return record.RunID
	}
	return ""
}

// Metadata and the atomic JSONL replacement are separate writes. A crash
// between them must retire capture rather than describe the old file as new.
func historyLineageMatches(data []byte, meta SessionMeta, key string) bool {
	var record struct {
		Generation     string `json:"history_generation"`
		FileGeneration string `json:"history_file_generation"`
	}
	if json.Unmarshal(data, &record) != nil {
		return false
	}
	if record.Generation == "" && record.FileGeneration == "" {
		return meta.HistoryEpoch == 0 && meta.HistoryFileEpoch == 0 &&
			meta.HistoryFileGeneration == legacyHistoryGeneration(key)
	}
	return record.Generation == meta.HistoryGeneration && record.FileGeneration == meta.HistoryFileGeneration
}

// The legacy identity is also assigned by GetHistory before any rewrite. A
// compaction therefore persists exactly the IDs an earlier page already saw.
func decodedHistoryID(data []byte, filename string, position int64) string {
	var record struct {
		ID string `json:"history_id"`
	}
	if json.Unmarshal(data, &record) == nil && uuid.Validate(record.ID) == nil {
		return record.ID
	}
	digest := sha256.Sum256(bytes.TrimRight(data, "\r\n"))
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("%s:%d:%x", filename, position, digest))).String()
}
