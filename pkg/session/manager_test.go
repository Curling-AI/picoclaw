package session

import (
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
		{"agent:main:telegram:group:-1003822706455/12", "agent_main_telegram_group_-1003822706455_12"},
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

	// The file on disk should use sanitized name.
	expectedFile := filepath.Join(tmpDir, "telegram_123456.json")
	if _, err := os.Stat(expectedFile); os.IsNotExist(err) {
		t.Fatalf("expected session file %s to exist", expectedFile)
	}

	// Load into a fresh manager and verify the session round-trips.
	sm2 := NewSessionManager(tmpDir)
	history := sm2.GetHistory(key)
	if len(history) != 1 {
		t.Fatalf("expected 1 message after reload, got %d", len(history))
	}
	if history[0].Content != "hello" {
		t.Errorf("expected message content %q, got %q", "hello", history[0].Content)
	}
}

func TestSave_RejectsPathTraversal(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)

	// Invalid names that must still be rejected.
	badKeys := []string{"", ".", ".."}
	for _, key := range badKeys {
		sm.GetOrCreate(key)
		if err := sm.Save(key); err == nil {
			t.Errorf("Save(%q) should have failed but didn't", key)
		}
	}

	// Keys containing path separators are sanitized (no subdirs created).
	sm.GetOrCreate("foo/bar")
	if err := sm.Save("foo/bar"); err != nil {
		t.Fatalf("Save(\"foo/bar\") after sanitize should succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, "foo_bar.json")); os.IsNotExist(err) {
		t.Errorf("expected foo_bar.json in storage (sanitized from foo/bar)")
	}
}

func TestLoadSessions_NormalizesMissingCreatedAt(t *testing.T) {
	tmpDir := t.TempDir()
	sessionPath := filepath.Join(tmpDir, "telegram_legacy.json")
	legacy := `{
  "key": "telegram:legacy",
  "messages": [
    {
      "role": "user",
      "content": "hello"
    }
  ],
  "created": "2026-01-01T00:00:00Z",
  "updated": "2026-01-01T00:00:00Z"
}`

	if err := os.WriteFile(sessionPath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sm := NewSessionManager(tmpDir)
	history := sm.GetHistory("telegram:legacy")
	if len(history) != 1 {
		t.Fatalf("history = %d, want 1", len(history))
	}
	if history[0].CreatedAt == nil || history[0].CreatedAt.IsZero() {
		t.Fatalf("history[0].CreatedAt = %v, want non-zero timestamp", history[0].CreatedAt)
	}
}

func TestLoadSessions_SessionWrittenBeforeCallTraceStillLoads(t *testing.T) {
	tmpDir := t.TempDir()
	sessionPath := filepath.Join(tmpDir, "telegram_no_trace.json")
	stored := `{
  "key": "telegram:no-trace",
  "messages": [
    {
      "role": "user",
      "content": "hello"
    },
    {
      "role": "assistant",
      "content": "hi there",
      "model_name": "old-model"
    }
  ],
  "created": "2026-01-01T00:00:00Z",
  "updated": "2026-01-01T00:00:00Z"
}`

	if err := os.WriteFile(sessionPath, []byte(stored), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sm := NewSessionManager(tmpDir)
	history := sm.GetHistory("telegram:no-trace")
	if len(history) != 2 {
		t.Fatalf("history = %d, want 2", len(history))
	}
	for i, msg := range history {
		if msg.LLMCall != nil {
			t.Errorf("history[%d].LLMCall = %+v, want nil for a message stored without one", i, msg.LLMCall)
		}
	}
	if history[1].Content != "hi there" || history[1].ModelName != "old-model" {
		t.Errorf("assistant message lost fields: %+v", history[1])
	}
}

func TestSaveAndLoad_KeepsTheCallTraceOfAnAssistantMessage(t *testing.T) {
	tmpDir := t.TempDir()
	sm := NewSessionManager(tmpDir)
	sm.AddFullMessage("web:trace", providers.Message{
		Role:      "assistant",
		Content:   "answer",
		ModelName: "some-model",
		LLMCall: &providers.LLMCall{
			RequestID:         "pc-sent",
			ProviderRequestID: "pc-echoed",
			UpstreamID:        "chatcmpl-1",
			ResolvedProvider:  "acme-inference",
			FinishReason:      "stop",
		},
	})
	if err := sm.Save("web:trace"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded := NewSessionManager(tmpDir)
	history := reloaded.GetHistory("web:trace")
	if len(history) != 1 {
		t.Fatalf("history = %d, want 1", len(history))
	}
	call := history[0].LLMCall
	if call == nil {
		t.Fatal("reloaded assistant message carries no call trace")
	}
	if call.RequestID != "pc-sent" || call.ProviderRequestID != "pc-echoed" {
		t.Errorf("trace ids = %+v, want the persisted ones", call)
	}
	if call.UpstreamID != "chatcmpl-1" || call.ResolvedProvider != "acme-inference" {
		t.Errorf("trace = %+v, want the persisted completion id and provider", call)
	}
	if call.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q", call.FinishReason, "stop")
	}
}
