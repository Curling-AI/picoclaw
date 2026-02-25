package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SessionRecord holds metadata about a session (stored in the index file).
type SessionRecord struct {
	SessionKey     string    `json:"session_key"`
	AgentID        string    `json:"agent_id"`
	Created        time.Time `json:"created"`
	Updated        time.Time `json:"updated"`
	MessageCount   int       `json:"message_count"`
	HasSummary     bool      `json:"has_summary"`
	TranscriptFile string    `json:"transcript_file"`
}

// RecordStore manages session records as a JSON index file.
type RecordStore struct {
	path    string // path to the index JSON file
	records map[string]*SessionRecord
	mu      sync.RWMutex
}

// NewRecordStore creates or loads a record store from the given directory.
func NewRecordStore(dir string) *RecordStore {
	rs := &RecordStore{
		path:    filepath.Join(dir, "sessions_index.json"),
		records: make(map[string]*SessionRecord),
	}
	rs.load()
	return rs
}

// GetOrCreate returns an existing record or creates a new one.
func (rs *RecordStore) GetOrCreate(key, agentID string) *SessionRecord {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rec, ok := rs.records[key]; ok {
		return rec
	}

	rec := &SessionRecord{
		SessionKey: key,
		AgentID:    agentID,
		Created:    time.Now(),
		Updated:    time.Now(),
	}
	rs.records[key] = rec
	return rec
}

// Update applies a mutation function to the record for the given key.
func (rs *RecordStore) Update(key string, fn func(r *SessionRecord)) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	rec, ok := rs.records[key]
	if !ok {
		return
	}
	fn(rec)
	rec.Updated = time.Now()
}

// Get returns the record for the given key, or nil if not found.
func (rs *RecordStore) Get(key string) *SessionRecord {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	rec, ok := rs.records[key]
	if !ok {
		return nil
	}
	// Return a copy
	cp := *rec
	return &cp
}

// List returns a copy of all records.
func (rs *RecordStore) List() []*SessionRecord {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	result := make([]*SessionRecord, 0, len(rs.records))
	for _, rec := range rs.records {
		cp := *rec
		result = append(result, &cp)
	}
	return result
}

// Save persists the records to disk using write-to-tmp-then-rename.
func (rs *RecordStore) Save() error {
	rs.mu.RLock()
	data, err := json.MarshalIndent(rs.records, "", "  ")
	rs.mu.RUnlock()
	if err != nil {
		return err
	}

	tmpPath := rs.path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, rs.path)
}

// Delete removes a record from the store.
func (rs *RecordStore) Delete(key string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	delete(rs.records, key)
}

func (rs *RecordStore) load() {
	data, err := os.ReadFile(rs.path)
	if err != nil {
		return
	}
	var records map[string]*SessionRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return
	}
	rs.records = records
}
