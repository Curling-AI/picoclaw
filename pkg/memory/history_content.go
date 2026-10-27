package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// ReadHistoryContent reads ordinary content without claiming an order receipt.
// Canonical ownership and I/O errors remain explicit even when proof is lost.
func (s *JSONLStore) ReadHistoryContent(ctx context.Context, key string) ([]providers.Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	canonical, err := s.canonicalHistoryOrderKey(key)
	if err != nil {
		return nil, err
	}
	lock := s.sessionLock(canonical)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	meta, err := s.readMeta(canonical)
	if err != nil {
		return nil, err
	}
	if err := validateCanonicalHistoryKey(key, canonical, meta); err != nil {
		return nil, err
	}
	file, err := os.Open(s.jsonlPath(canonical))
	if errors.Is(err, os.ErrNotExist) && emptyHistoryContent(meta) {
		return []providers.Message{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory: open content: %w", err)
	}
	defer file.Close()
	messages, err := decodeHistoryMessages(ctx, file, meta.Skip, true)
	if err != nil {
		return nil, err
	}
	for index := range messages {
		messages[index].HistoryID = ""
		messages[index].HistoryRunID = ""
		messages[index].HistoryOrderVersion = 0
		messages[index].HistoryStreamID = ""
		messages[index].HistorySequence = 0
	}
	return messages, nil
}

func emptyHistoryContent(meta SessionMeta) bool {
	if meta.Count != 0 || meta.Skip != 0 || meta.HistoryRecoveryRequired {
		return false
	}
	if meta.HistoryBytes != nil {
		return *meta.HistoryBytes == 0
	}
	// A known stream needs an explicit empty byte checkpoint; a missing count
	// alone cannot establish that its previously durable content was cleared.
	return meta.HistoryOrderVersion == 0 && meta.HistoryOrderWitnessVersion == 0 &&
		meta.HistorySequence == 0 && meta.HistoryStreamID == ""
}
