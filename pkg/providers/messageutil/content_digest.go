package messageutil

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

const sourceContentDigestPrefix = "source:v1:"

func ContentDigest(createdAt, content string) string {
	if at, err := time.Parse(time.RFC3339Nano, createdAt); err == nil {
		createdAt = at.UTC().Format(time.RFC3339Nano)
	}
	digest := sha256.Sum256([]byte(createdAt + "\x00" + content))
	return hex.EncodeToString(digest[:])
}

func SourceContentDigest(createdAt, content string) string {
	return sourceContentDigestPrefix + ContentDigest(createdAt, content)
}

func ValidSourceContentDigest(value string) bool {
	if len(value) != len(sourceContentDigestPrefix)+sha256.Size*2 ||
		!strings.HasPrefix(value, sourceContentDigestPrefix) {
		return false
	}
	encoded := strings.TrimPrefix(value, sourceContentDigestPrefix)
	digest, err := hex.DecodeString(encoded)
	return err == nil && len(digest) == sha256.Size && hex.EncodeToString(digest) == encoded
}
