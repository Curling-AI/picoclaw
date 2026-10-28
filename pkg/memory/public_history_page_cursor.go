package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

var ErrPublicHistoryCursorMalformed = errors.New("public history cursor format invalid")

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
	previous, err := decodePublicHistoryCursor(request.Cursor)
	if err != nil {
		return cursor, err
	}
	cursor = previous
	if cursor.Key != publicHistoryKeyDigest(request.Key) || cursor.Generation != meta.PublicHistoryGeneration ||
		cursor.Revision > meta.PublicHistoryRevision ||
		cursor.Count > meta.PublicHistoryCount || cursor.Bytes < 0 || cursor.Bytes > meta.PublicHistoryBytes ||
		cursor.Position > cursor.Count {
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

// ValidatePublicHistoryPageCursor validates only the issued versioned shape.
// Source generation/frontier compatibility is checked separately under its lock.
func ValidatePublicHistoryPageCursor(encoded string) error {
	_, err := decodePublicHistoryCursor(encoded)
	return err
}

func decodePublicHistoryCursor(encoded string) (publicHistoryCursor, error) {
	var cursor publicHistoryCursor
	if len(encoded) == 0 || len(encoded) > publicHistoryCursorBytes {
		return cursor, ErrPublicHistoryCursorMalformed
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || json.Unmarshal(data, &cursor) != nil {
		return cursor, ErrPublicHistoryCursorMalformed
	}
	canonical, err := json.Marshal(cursor)
	// Issued opaque tokens have one canonical, complete representation. This
	// rejects null, omitted/unknown/duplicate fields and object-shape ambiguity.
	if err != nil || !bytes.Equal(data, canonical) {
		return cursor, ErrPublicHistoryCursorMalformed
	}
	key, keyErr := base64.RawURLEncoding.DecodeString(cursor.Key)
	digest, digestErr := hex.DecodeString(cursor.Digest)
	if cursor.Version != 1 || keyErr != nil || len(key) != sha256.Size || uuid.Validate(cursor.Generation) != nil ||
		cursor.Revision == 0 || cursor.Revision > uint64(maxHistoryPosition) || cursor.Count <= 0 || cursor.Count > maxHistoryPosition ||
		cursor.Bytes <= 0 || cursor.Bytes > maxHistoryPosition || digestErr != nil || len(digest) != sha256.Size ||
		cursor.Position < 0 || cursor.Position > cursor.Count ||
		(cursor.Direction != publicPageBefore && cursor.Direction != publicPageAfter) ||
		(cursor.Direction == publicPageBefore && cursor.Position == 0) ||
		(cursor.Direction == publicPageAfter && cursor.Position == cursor.Count) {
		return cursor, ErrPublicHistoryCursorMalformed
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
