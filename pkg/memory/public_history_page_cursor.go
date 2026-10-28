package memory

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"github.com/google/uuid"
)

const (
	publicHistoryCursorBytes = 512
	publicPageBefore         = "before"
	publicPageAfter          = "after"
)

type publicHistoryCursor struct {
	Version    uint8  `json:"v"`
	Key        string `json:"k"`
	Generation string `json:"g"`
	Revision   uint64 `json:"r"`
	Count      int64  `json:"n"`
	Bytes      int64  `json:"b"`
	Digest     string `json:"d"`
	Position   int64  `json:"p"`
	Direction  string `json:"o"`
}

func publicHistoryKeyDigest(key string) string {
	digest := sha256.Sum256([]byte(key))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func readPublicHistoryCursor(request PublicHistoryPageRequest, meta SessionMeta, index *publicIndexReader) (publicHistoryCursor, error) {
	cursor := publicHistoryCursor{Version: 1, Key: publicHistoryKeyDigest(request.Key),
		Generation: meta.PublicHistoryGeneration, Revision: meta.PublicHistoryRevision,
		Count: meta.PublicHistoryCount, Bytes: meta.PublicHistoryBytes, Digest: meta.PublicHistoryDigest,
		Position: meta.PublicHistoryCount, Direction: publicPageBefore}
	if request.Cursor == "" {
		return cursor, nil
	}
	if len(request.Cursor) > publicHistoryCursorBytes {
		return cursor, ErrHistoryCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(request.Cursor)
	if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 ||
		cursor.Key != publicHistoryKeyDigest(request.Key) || cursor.Generation != meta.PublicHistoryGeneration ||
		cursor.Revision == 0 || cursor.Revision > meta.PublicHistoryRevision || cursor.Count < 0 ||
		cursor.Count > meta.PublicHistoryCount || cursor.Bytes < 0 || cursor.Bytes > meta.PublicHistoryBytes ||
		cursor.Position < 0 || cursor.Position > cursor.Count ||
		(cursor.Direction != publicPageBefore && cursor.Direction != publicPageAfter) ||
		(cursor.Direction == publicPageBefore && cursor.Position == 0) ||
		(cursor.Direction == publicPageAfter && cursor.Position == cursor.Count) {
		return cursor, ErrHistoryCursor
	}
	if cursor.Count == 0 {
		if cursor.Bytes != 0 || cursor.Digest != emptyHistoryDigest {
			return cursor, ErrHistoryCursor
		}
		return cursor, nil
	}
	last, err := index.record(cursor.Count - 1)
	if err != nil || last.Offset+last.Length != cursor.Bytes || publicIndexRecordDigest(last) != cursor.Digest {
		return cursor, ErrHistoryCursor
	}
	return cursor, nil
}

func encodePublicHistoryCursor(cursor publicHistoryCursor, position int64, direction string) (string, error) {
	cursor.Position, cursor.Direction = position, direction
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	if len(encoded) > publicHistoryCursorBytes {
		return "", ErrHistoryCursor
	}
	return encoded, nil
}

func publicIndexRecordDigest(record publicIndexRecord) string {
	return hex.EncodeToString(record.Chain[:])
}

func validPublicPageEntry(entry PublicHistoryEntry) bool {
	if uuid.Validate(entry.ID) != nil || (entry.RunID != "" && uuid.Validate(entry.RunID) != nil) ||
		(entry.Order != (HistoryOrderProof{}) && !entry.Order.Valid()) {
		return false
	}
	_, public := publicHistoryMessage(entry.Message)
	return public
}
