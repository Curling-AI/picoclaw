package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

type Session struct {
	Key      string              `json:"key"`
	Messages []providers.Message `json:"messages"`
	Summary  string              `json:"summary,omitempty"`
	Created  time.Time           `json:"created"`
	Updated  time.Time           `json:"updated"`
}

type SessionManager struct {
	sessions    map[string]*Session
	mu          sync.RWMutex
	storage     string
	transcripts map[string]*TranscriptWriter
	records     *RecordStore
}

func NewSessionManager(storage string) *SessionManager {
	sm := &SessionManager{
		sessions:    make(map[string]*Session),
		storage:     storage,
		transcripts: make(map[string]*TranscriptWriter),
	}

	if storage != "" {
		os.MkdirAll(storage, 0o755)
		sm.records = NewRecordStore(storage)
		sm.loadSessions()
	}

	return sm
}

// GetRecordStore returns the record store for external access.
func (sm *SessionManager) GetRecordStore() *RecordStore {
	return sm.records
}

// GetTranscriptWriter returns the transcript writer for a session key,
// creating one if it doesn't exist.
func (sm *SessionManager) GetTranscriptWriter(key string) *TranscriptWriter {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if tw, ok := sm.transcripts[key]; ok {
		return tw
	}
	tw := sm.ensureTranscriptWriter(key)
	return tw
}

// ensureTranscriptWriter returns or creates a TranscriptWriter for the key.
// Caller must hold sm.mu.
func (sm *SessionManager) ensureTranscriptWriter(key string) *TranscriptWriter {
	if tw, ok := sm.transcripts[key]; ok {
		return tw
	}
	if sm.storage == "" {
		return nil
	}
	filename := sanitizeFilename(key)
	path := filepath.Join(sm.storage, filename+".jsonl")
	tw := NewTranscriptWriter(path)
	sm.transcripts[key] = tw
	return tw
}

func (sm *SessionManager) GetOrCreate(key string) *Session {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if ok {
		return session
	}

	session = &Session{
		Key:      key,
		Messages: []providers.Message{},
		Created:  time.Now(),
		Updated:  time.Now(),
	}
	sm.sessions[key] = session

	// Ensure record exists
	if sm.records != nil {
		sm.records.GetOrCreate(key, "")
	}

	return session
}

func (sm *SessionManager) AddMessage(sessionKey, role, content string) {
	sm.AddFullMessage(sessionKey, providers.Message{
		Role:    role,
		Content: content,
	})
}

// AddFullMessage adds a complete message with tool calls and tool call ID to the session.
// This is used to save the full conversation flow including tool calls and tool results.
func (sm *SessionManager) AddFullMessage(sessionKey string, msg providers.Message) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[sessionKey]
	if !ok {
		session = &Session{
			Key:      sessionKey,
			Messages: []providers.Message{},
			Created:  time.Now(),
		}
		sm.sessions[sessionKey] = session
	}

	session.Messages = append(session.Messages, msg)
	session.Updated = time.Now()

	// Append to JSONL transcript
	if tw := sm.ensureTranscriptWriter(sessionKey); tw != nil {
		tw.Append(TranscriptEntry{Kind: "message", Message: &msg})
	}

	// Update record store message count
	if sm.records != nil {
		sm.records.GetOrCreate(sessionKey, "")
		sm.records.Update(sessionKey, func(r *SessionRecord) {
			r.MessageCount = len(session.Messages)
		})
	}
}

func (sm *SessionManager) GetHistory(key string) []providers.Message {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[key]
	if !ok {
		return []providers.Message{}
	}

	history := make([]providers.Message, len(session.Messages))
	copy(history, session.Messages)
	return history
}

func (sm *SessionManager) GetSummary(key string) string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[key]
	if !ok {
		return ""
	}
	return session.Summary
}

func (sm *SessionManager) SetSummary(key string, summary string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if ok {
		session.Summary = summary
		session.Updated = time.Now()
	}

	// Append summary entry to transcript
	if tw := sm.ensureTranscriptWriter(key); tw != nil {
		tw.Append(TranscriptEntry{Kind: "summary", Summary: summary})
	}

	// Update record store
	if sm.records != nil {
		sm.records.Update(key, func(r *SessionRecord) {
			r.HasSummary = summary != ""
		})
	}
}

func (sm *SessionManager) TruncateHistory(key string, keepLast int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if !ok {
		return
	}

	if keepLast <= 0 {
		session.Messages = []providers.Message{}
		session.Updated = time.Now()
		return
	}

	if len(session.Messages) <= keepLast {
		return
	}

	droppedCount := len(session.Messages) - keepLast
	session.Messages = session.Messages[len(session.Messages)-keepLast:]
	session.Updated = time.Now()

	// Append meta entry recording truncation
	if tw := sm.ensureTranscriptWriter(key); tw != nil {
		tw.Append(TranscriptEntry{
			Kind: "meta",
			Meta: map[string]any{
				"action":  "truncate",
				"kept":    keepLast,
				"dropped": droppedCount,
			},
		})
	}
}

// sanitizeFilename converts a session key into a cross-platform safe filename.
// Session keys use "channel:chatID" (e.g. "telegram:123456") but ':' is the
// volume separator on Windows, so filepath.Base would misinterpret the key.
// We replace it with '_'. The original key is preserved inside the JSON file,
// so loadSessions still maps back to the right in-memory key.
func sanitizeFilename(key string) string {
	return strings.ReplaceAll(key, ":", "_")
}

func (sm *SessionManager) Save(key string) error {
	if sm.storage == "" {
		return nil
	}

	filename := sanitizeFilename(key)

	// filepath.IsLocal rejects empty names, "..", absolute paths, and
	// OS-reserved device names (NUL, COM1 ... on Windows).
	// The extra checks reject "." and any directory separators so that
	// the session file is always written directly inside sm.storage.
	if filename == "." || !filepath.IsLocal(filename) || strings.ContainsAny(filename, `/\`) {
		return os.ErrInvalid
	}

	// JSONL transcript is append-only (already persisted on each AddFullMessage).
	// Save only updates the record store index.
	if sm.records != nil {
		return sm.records.Save()
	}
	return nil
}

func (sm *SessionManager) loadSessions() error {
	files, err := os.ReadDir(sm.storage)
	if err != nil {
		return err
	}

	// Build set of JSONL files for priority check
	jsonlFiles := make(map[string]bool)
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		if filepath.Ext(file.Name()) == ".jsonl" {
			base := strings.TrimSuffix(file.Name(), ".jsonl")
			jsonlFiles[base] = true
		}
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		ext := filepath.Ext(file.Name())
		base := strings.TrimSuffix(file.Name(), ext)

		switch ext {
		case ".jsonl":
			sm.loadJSONL(file.Name())

		case ".json":
			// Skip if already migrated or if the index file
			if strings.HasSuffix(file.Name(), ".migrated") {
				continue
			}
			if file.Name() == "sessions_index.json" {
				continue
			}
			// If a .jsonl already exists for this base, skip the .json
			if jsonlFiles[base] {
				continue
			}
			// Auto-migrate: read JSON, write JSONL, rename .json -> .json.migrated
			sm.migrateJSON(file.Name())
		}
	}

	return nil
}

// loadJSONL loads a JSONL transcript file into in-memory session.
func (sm *SessionManager) loadJSONL(filename string) {
	path := filepath.Join(sm.storage, filename)
	tr := NewTranscriptReader(path)
	messages, summary, err := tr.ReadMessages()
	if err != nil {
		return
	}

	entries, _ := tr.ReadAll()
	var maxSeq int64
	for _, e := range entries {
		if e.Seq > maxSeq {
			maxSeq = e.Seq
		}
	}

	// Derive session key from filename (reverse sanitizeFilename is lossy,
	// so we also check entries for the original key from message metadata).
	// For JSONL files we use the sanitized name as-is since the key is embedded in records.
	base := strings.TrimSuffix(filename, ".jsonl")
	// Attempt to find the real key from the record store
	sessionKey := sm.findSessionKeyForBase(base)
	if sessionKey == "" {
		// Fallback: use base as key (works for simple keys)
		sessionKey = base
	}

	session := &Session{
		Key:      sessionKey,
		Messages: messages,
		Summary:  summary,
		Created:  time.Now(),
		Updated:  time.Now(),
	}
	if len(entries) > 0 {
		session.Created = entries[0].Timestamp
		session.Updated = entries[len(entries)-1].Timestamp
	}

	sm.sessions[sessionKey] = session

	// Set up transcript writer with correct sequence
	tw := NewTranscriptWriter(path)
	tw.SetSeq(maxSeq)
	sm.transcripts[sessionKey] = tw

	// Update record store
	if sm.records != nil {
		rec := sm.records.GetOrCreate(sessionKey, "")
		rec.MessageCount = len(messages)
		rec.HasSummary = summary != ""
		rec.TranscriptFile = filename
		if len(entries) > 0 {
			rec.Created = entries[0].Timestamp
			rec.Updated = entries[len(entries)-1].Timestamp
		}
	}
}

// migrateJSON reads a legacy .json session file and converts it to JSONL format.
func (sm *SessionManager) migrateJSON(filename string) {
	jsonPath := filepath.Join(sm.storage, filename)
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return
	}

	// Write each message as a JSONL entry
	base := strings.TrimSuffix(filename, ".json")
	jsonlPath := filepath.Join(sm.storage, base+".jsonl")
	tw := NewTranscriptWriter(jsonlPath)

	for _, msg := range session.Messages {
		msgCopy := msg
		tw.Append(TranscriptEntry{
			Kind:      "message",
			Message:   &msgCopy,
			Timestamp: session.Updated,
		})
	}

	if session.Summary != "" {
		tw.Append(TranscriptEntry{
			Kind:      "summary",
			Summary:   session.Summary,
			Timestamp: session.Updated,
		})
	}

	// Store in-memory
	sm.sessions[session.Key] = &session
	sm.transcripts[session.Key] = tw

	// Update record store
	if sm.records != nil {
		rec := sm.records.GetOrCreate(session.Key, "")
		rec.MessageCount = len(session.Messages)
		rec.HasSummary = session.Summary != ""
		rec.TranscriptFile = base + ".jsonl"
		rec.Created = session.Created
		rec.Updated = session.Updated
	}

	// Rename .json -> .json.migrated
	os.Rename(jsonPath, jsonPath+".migrated")
}

// findSessionKeyForBase looks up the record store for a session whose transcript
// file matches the given base name.
func (sm *SessionManager) findSessionKeyForBase(base string) string {
	if sm.records == nil {
		return ""
	}
	expectedFile := base + ".jsonl"
	for _, rec := range sm.records.List() {
		if rec.TranscriptFile == expectedFile {
			return rec.SessionKey
		}
	}
	return ""
}

// SetHistory updates the messages of a session.
func (sm *SessionManager) SetHistory(key string, history []providers.Message) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if ok {
		// Create a deep copy to strictly isolate internal state
		// from the caller's slice.
		msgs := make([]providers.Message, len(history))
		copy(msgs, history)
		session.Messages = msgs
		session.Updated = time.Now()
	}
}

// ClearSession resets a session's in-memory messages and summary.
func (sm *SessionManager) ClearSession(key string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, ok := sm.sessions[key]
	if !ok {
		return
	}
	session.Messages = []providers.Message{}
	session.Summary = ""
	session.Updated = time.Now()
}

// GetSession returns the session object for the given key, or nil if not found.
func (sm *SessionManager) GetSession(key string) *Session {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	session, ok := sm.sessions[key]
	if !ok {
		return nil
	}
	return session
}
