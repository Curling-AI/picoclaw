package memory

import (
	"context"
	"os"
	"strings"
)

// InitializeNewHistoryContentIdentity only activates a source with no durable
// artifacts. Existing content, lost checkpoints and explicit preparation stay separate.
func (s *JSONLStore) InitializeNewHistoryContentIdentity(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return err
	}
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), sanitizeKey(key)+".") {
			return nil
		}
	}
	image, err := s.readContentImage(ctx, key)
	if err != nil {
		return err
	}
	if image.Exists || image.Count != 0 || image.Meta.Summary != "" || image.Meta.HistoryRecoveryRequired {
		return ErrContentIdentityUnavailable
	}
	_, err = s.initializeContentIdentityLocked(ctx, key, image.Observation, nativePublicArchivePolicy)
	return err
}

var _ HistoryContentInitializer = (*JSONLStore)(nil)
