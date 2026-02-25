package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"simple", "simple"},
		{"telegram:123456", "telegram_123456"},
		{"discord:987654321", "discord_987654321"},
		{"slack:C01234", "slack_C01234"},
		{"no-colons-here", "no-colons-here"},
		{"multiple:colons:here", "multiple_colons_here"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeFilename(tt.input)
			if got != tt.expected {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestSave_WithColonInKey(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)

	// Create a session with a key containing colon (typical channel session key).
	key := "telegram:123456"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "hello")

	// Save should succeed even though the key contains ':'
	if err := sm.Save(key); err != nil {
		t.Fatalf("Save(%q) failed: %v", key, err)
	}

	// The JSONL file on disk should exist with sanitized name.
	expectedFile := filepath.Join(tmpDir, "telegram_123456.jsonl")
	if _, err := os.Stat(expectedFile); os.IsNotExist(err) {
		t.Fatalf("expected JSONL file %s to exist", expectedFile)
	}
}

func TestSave_RejectsPathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)

	badKeys := []string{"", ".", "..", "foo/bar", "foo\\bar"}
	for _, key := range badKeys {
		sm.GetOrCreate(key)
		if err := sm.Save(key); err == nil {
			t.Errorf("Save(%q) should have failed but didn't", key)
		}
	}
}

func TestJSON_to_JSONL_Migration(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a legacy .json session file
	legacySession := Session{
		Key: "telegram:999",
		Messages: []providers.Message{
			{Role: "user", Content: "old message"},
			{Role: "assistant", Content: "old reply"},
		},
		Summary: "previous summary",
	}
	data, err := json.MarshalIndent(legacySession, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	jsonPath := filepath.Join(tmpDir, "telegram_999.json")
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		t.Fatalf("write json: %v", err)
	}

	// Boot a new SessionManager - should auto-migrate
	sm := NewSessionManager(tmpDir)

	// Verify .jsonl was created
	jsonlPath := filepath.Join(tmpDir, "telegram_999.jsonl")
	if _, err := os.Stat(jsonlPath); os.IsNotExist(err) {
		t.Fatalf("expected JSONL file to be created by migration")
	}

	// Verify .json was renamed to .json.migrated
	migratedPath := jsonPath + ".migrated"
	if _, err := os.Stat(migratedPath); os.IsNotExist(err) {
		t.Fatalf("expected .json.migrated file to exist")
	}

	// Verify in-memory session is correct
	history := sm.GetHistory("telegram:999")
	if len(history) != 2 {
		t.Fatalf("expected 2 messages after migration, got %d", len(history))
	}
	if history[0].Content != "old message" {
		t.Errorf("message 0: %q", history[0].Content)
	}
	if history[1].Content != "old reply" {
		t.Errorf("message 1: %q", history[1].Content)
	}

	summary := sm.GetSummary("telegram:999")
	if summary != "previous summary" {
		t.Errorf("summary: %q, want %q", summary, "previous summary")
	}
}

func TestAddFullMessage_WritesJSONL(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)

	key := "test-session"
	sm.GetOrCreate(key)

	msg := providers.Message{
		Role:    "user",
		Content: "hello JSONL",
	}
	sm.AddFullMessage(key, msg)

	// Verify in-memory
	history := sm.GetHistory(key)
	if len(history) != 1 || history[0].Content != "hello JSONL" {
		t.Errorf("in-memory: %v", history)
	}

	// Verify JSONL file
	jsonlPath := filepath.Join(tmpDir, "test-session.jsonl")
	tr := NewTranscriptReader(jsonlPath)
	entries, err := tr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 JSONL entry, got %d", len(entries))
	}
	if entries[0].Kind != "message" || entries[0].Message.Content != "hello JSONL" {
		t.Errorf("JSONL entry: %+v", entries[0])
	}
}

func TestSetSummary_WritesJSONL(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)

	key := "summary-test"
	sm.GetOrCreate(key)
	sm.SetSummary(key, "test summary")

	jsonlPath := filepath.Join(tmpDir, "summary-test.jsonl")
	tr := NewTranscriptReader(jsonlPath)
	entries, err := tr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 JSONL entry, got %d", len(entries))
	}
	if entries[0].Kind != "summary" || entries[0].Summary != "test summary" {
		t.Errorf("JSONL entry: %+v", entries[0])
	}
}

func TestTruncateHistory_WritesMeta(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)

	key := "truncate-test"
	sm.GetOrCreate(key)
	for i := 0; i < 10; i++ {
		sm.AddMessage(key, "user", "msg")
	}

	sm.TruncateHistory(key, 3)

	history := sm.GetHistory(key)
	if len(history) != 3 {
		t.Fatalf("expected 3 messages after truncate, got %d", len(history))
	}

	// Verify meta entry was written
	jsonlPath := filepath.Join(tmpDir, "truncate-test.jsonl")
	tr := NewTranscriptReader(jsonlPath)
	entries, err := tr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// 10 message entries + 1 meta entry
	if len(entries) != 11 {
		t.Fatalf("expected 11 JSONL entries, got %d", len(entries))
	}
	lastEntry := entries[len(entries)-1]
	if lastEntry.Kind != "meta" {
		t.Errorf("last entry kind: %s, want meta", lastEntry.Kind)
	}
	if lastEntry.Meta["action"] != "truncate" {
		t.Errorf("meta action: %v", lastEntry.Meta["action"])
	}
}

func TestBackwardCompat_JSONLPreferredOverJSON(t *testing.T) {
	tmpDir := t.TempDir()

	// Create both a .json and .jsonl file for the same base name
	base := "both_test"

	// JSON file with one message
	jsonSession := Session{
		Key:      "both:test",
		Messages: []providers.Message{{Role: "user", Content: "from json"}},
	}
	jsonData, _ := json.MarshalIndent(jsonSession, "", "  ")
	os.WriteFile(filepath.Join(tmpDir, base+".json"), jsonData, 0o644)

	// JSONL file with a different message
	tw := NewTranscriptWriter(filepath.Join(tmpDir, base+".jsonl"))
	msg := providers.Message{Role: "user", Content: "from jsonl"}
	tw.Append(TranscriptEntry{Kind: "message", Message: &msg})

	// Boot session manager
	sm := NewSessionManager(tmpDir)

	// Should prefer JSONL content
	history := sm.GetHistory(base)
	if len(history) != 1 {
		t.Fatalf("expected 1 message, got %d", len(history))
	}
	if history[0].Content != "from jsonl" {
		t.Errorf("content = %q, want 'from jsonl' (JSONL should be preferred)", history[0].Content)
	}
}

func TestClearSession(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)

	key := "clear-me"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "hello")
	sm.SetSummary(key, "some summary")

	sm.ClearSession(key)

	history := sm.GetHistory(key)
	if len(history) != 0 {
		t.Errorf("expected 0 messages after clear, got %d", len(history))
	}
	summary := sm.GetSummary(key)
	if summary != "" {
		t.Errorf("expected empty summary after clear, got %q", summary)
	}
}
