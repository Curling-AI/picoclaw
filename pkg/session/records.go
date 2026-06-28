package session

import (
	"os"
	"path/filepath"
	"time"
)

// SessionRecord summarizes a stored session for listing, without exposing its
// full message history. Upstream's Session already tracks Key/Created/Updated
// and the message slice, so this is a thin projection over the existing data
// (the fork's heavier RecordStore/transcript index is not needed).
type SessionRecord struct {
	SessionKey   string
	MessageCount int
	Created      time.Time
	Updated      time.Time
}

// ListSessionRecords returns metadata for every loaded session. NewSessionManager
// loads all persisted sessions at startup, so this covers persisted sessions too.
func (sm *SessionManager) ListSessionRecords() []SessionRecord {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	records := make([]SessionRecord, 0, len(sm.sessions))
	for _, s := range sm.sessions {
		records = append(records, SessionRecord{
			SessionKey:   s.Key,
			MessageCount: len(s.Messages),
			Created:      s.Created,
			Updated:      s.Updated,
		})
	}
	return records
}

// DeleteSession removes a session from memory and deletes its persisted file.
// Returns nil if the session does not exist.
func (sm *SessionManager) DeleteSession(key string) error {
	sm.mu.Lock()
	delete(sm.sessions, key)
	storage := sm.storage
	sm.mu.Unlock()

	if storage == "" {
		return nil
	}
	path := filepath.Join(storage, sanitizeFilename(key)+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
