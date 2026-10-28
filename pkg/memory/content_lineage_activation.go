package memory

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
)

type contentActivationMetadata struct {
	Key      string          `json:"key"`
	Aliases  []string        `json:"aliases"`
	Identity json.RawMessage `json:"content_identity"`
}

func (s *JSONLStore) readContentActivationMetadata(key string) (contentActivationMetadata, bool, error) {
	data, err := os.ReadFile(s.metaPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return contentActivationMetadata{}, false, nil
	}
	if err != nil {
		return contentActivationMetadata{}, false, err
	}
	var meta contentActivationMetadata
	if err = json.Unmarshal(data, &meta); err != nil {
		return contentActivationMetadata{}, false, err
	}
	return meta, true, nil
}

func (s *JSONLStore) HistoryContentIdentityActive(ctx context.Context, key string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.checkContentJournalSettled(key); err != nil {
		return false, err
	}
	meta, present, err := s.readContentActivationMetadata(key)
	if err != nil {
		return false, err
	}
	if present && meta.Key == key && len(meta.Identity) == 0 {
		// The legacy caller already resolved this source. Its own exact-key
		// checkpoint, without any identity/witness/journal, proves only that
		// this physical source never opted in. Do not require an unrelated
		// metadata directory scan to retain that existing legacy behavior.
		// Qualified reads/activation and typed append still use the separate
		// strict ownership preflight below; this negative probe proves no alias.
		if _, statErr := os.Stat(s.contentDomainPath(key)); errors.Is(statErr, os.ErrNotExist) {
			return false, ctx.Err()
		} else if statErr != nil {
			return false, statErr
		}
	}
	return s.contentIdentityActiveLocked(ctx, key)
}

// The caller holds this physical source's lock. Writers use the same metadata
// ownership preflight without importing the stricter frame/count checks.
func (s *JSONLStore) contentIdentityActiveLocked(ctx context.Context, key string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if strings.TrimSpace(key) == "" || strings.TrimSpace(key) != key {
		return false, ErrHistoryOwnershipUnavailable
	}
	meta, present, err := s.readContentActivationMetadata(key)
	if err != nil {
		return false, err
	}
	if present && meta.Key != "" && meta.Key != key {
		return false, ErrHistoryOwnershipUnavailable
	}
	// Preserve the established canonical sk_v1 shortcut. Other requests must
	// not label an alias of a known source as an absent/uninitialized source.
	if !shouldShortCircuitSessionResolve(key) || !present || meta.Key != key {
		entries, readErr := os.ReadDir(s.dir)
		if readErr != nil {
			return false, readErr
		}
		for _, entry := range entries {
			if err = ctx.Err(); err != nil {
				return false, err
			}
			if !strings.HasSuffix(entry.Name(), ".meta.json") {
				continue
			}
			physical := strings.TrimSuffix(entry.Name(), ".meta.json")
			owner, _, readErr := s.readContentActivationMetadata(physical)
			if readErr != nil {
				return false, readErr
			}
			if physical == sanitizeKey(key) && owner.Key != "" && owner.Key != key {
				return false, ErrHistoryOwnershipUnavailable
			}
			for _, alias := range owner.Aliases {
				if alias == key && owner.Key != key {
					return false, ErrHistoryOwnershipUnavailable
				}
			}
		}
	}
	_, err = os.Stat(s.contentDomainPath(key))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	// Even null or malformed identity/witness evidence remains an activation
	// marker. The checked frame reader then fails closed rather than silently
	// enabling the legacy destructive path for a once-initialized source.
	return len(meta.Identity) != 0 || err == nil, nil
}

var _ HistoryContentActivation = (*JSONLStore)(nil)
