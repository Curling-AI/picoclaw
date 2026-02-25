package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/session"
)

func TestCompactSession_WritesSummaryToTranscript(t *testing.T) {
	dir := t.TempDir()
	sm := session.NewSessionManager(dir)
	sessionKey := "compact-test"
	sm.GetOrCreate(sessionKey)

	// Fill with messages
	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			sm.AddMessage(sessionKey, "user", "hello")
		} else {
			sm.AddMessage(sessionKey, "assistant", "world")
		}
	}

	agent := &AgentInstance{
		ID:               "test",
		Model:            "test-model",
		Provider:         &mockProvider{response: "Summary: user greeted multiple times"},
		Sessions:         sm,
		KeepLastMessages: 6,
		ContextWindow:    131072,
	}

	al := &AgentLoop{}
	err := al.CompactSession(context.Background(), agent, sessionKey)
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}

	// Verify in-memory state
	history := sm.GetHistory(sessionKey)
	if len(history) != 6 {
		t.Errorf("expected 6 messages after compaction, got %d", len(history))
	}

	summary := sm.GetSummary(sessionKey)
	if summary == "" {
		t.Error("expected non-empty summary after compaction")
	}

	// Verify transcript has summary and meta entries
	jsonlPath := filepath.Join(dir, "compact-test.jsonl")
	tr := session.NewTranscriptReader(jsonlPath)
	entries, err := tr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	hasSummaryEntry := false
	hasCompactionMeta := false
	for _, e := range entries {
		if e.Kind == "summary" && e.Summary != "" {
			hasSummaryEntry = true
		}
		if e.Kind == "meta" {
			if action, ok := e.Meta["action"]; ok && action == "compaction" {
				hasCompactionMeta = true
			}
		}
	}

	if !hasSummaryEntry {
		t.Error("expected summary entry in transcript")
	}
	if !hasCompactionMeta {
		t.Error("expected compaction meta entry in transcript")
	}
}

func TestCompactSession_TooFewMessages(t *testing.T) {
	dir := t.TempDir()
	sm := session.NewSessionManager(dir)
	sessionKey := "few-msgs"
	sm.GetOrCreate(sessionKey)

	sm.AddMessage(sessionKey, "user", "hi")
	sm.AddMessage(sessionKey, "assistant", "hello")

	agent := &AgentInstance{
		ID:               "test",
		Model:            "test-model",
		Provider:         &mockProvider{response: "should not be called"},
		Sessions:         sm,
		KeepLastMessages: 6,
		ContextWindow:    131072,
	}

	al := &AgentLoop{}
	err := al.CompactSession(context.Background(), agent, sessionKey)
	if err != nil {
		t.Fatalf("CompactSession: %v", err)
	}

	// Should not have compacted (only 2 messages, keepLast=6)
	history := sm.GetHistory(sessionKey)
	if len(history) != 2 {
		t.Errorf("should not compact, got %d messages", len(history))
	}
}

func TestCompactSession_Idempotent(t *testing.T) {
	dir := t.TempDir()
	sm := session.NewSessionManager(dir)
	sessionKey := "idempotent-test"
	sm.GetOrCreate(sessionKey)

	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			sm.AddMessage(sessionKey, "user", "msg")
		} else {
			sm.AddMessage(sessionKey, "assistant", "reply")
		}
	}

	agent := &AgentInstance{
		ID:               "test",
		Model:            "test-model",
		Provider:         &mockProvider{response: "Summary of conversation"},
		Sessions:         sm,
		KeepLastMessages: 6,
		ContextWindow:    131072,
	}

	al := &AgentLoop{}

	// Compact once
	al.CompactSession(context.Background(), agent, sessionKey)
	history1 := sm.GetHistory(sessionKey)

	// Compact again (should be no-op since only 6 messages remain)
	al.CompactSession(context.Background(), agent, sessionKey)
	history2 := sm.GetHistory(sessionKey)

	if len(history1) != len(history2) {
		t.Errorf("compaction not idempotent: %d vs %d messages", len(history1), len(history2))
	}
}
