package session

import (
	"os"
	"path/filepath"
	"time"

	"github.com/sipeed/picoclaw/pkg/memory/sessionlock"
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
	Scope        *SessionScope
	Aliases      []string
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
	sm.mu.RLock()
	storage := sm.storage
	sm.mu.RUnlock()

	if storage == "" {
		sm.forget(key)
		return nil
	}
	name := sanitizeFilename(key)
	// No MarkRemove: the .json is not the JSONL store's file, so the lock file
	// stays for the store's own writers.
	lock, err := sessionlock.Acquire(storage, name, sessionManagerLockWait)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	// The removal goes only after a Check, as Save's rename does.
	if err := lock.Check(); err != nil {
		return err
	}

	path := filepath.Join(storage, name+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	// The session leaves memory only with its file, still under the lock: a
	// refused delete keeps it, or the next Save would write a new, empty
	// session over the history the file still holds.
	sm.forget(key)
	return nil
}

func (sm *SessionManager) forget(key string) {
	sm.mu.Lock()
	delete(sm.sessions, key)
	sm.mu.Unlock()
}
