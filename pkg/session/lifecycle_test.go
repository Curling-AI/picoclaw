package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

func TestLifecycle_DailyReset(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(dir)

	key := "daily-test"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "old message")

	// Manually set the session's Updated to yesterday
	sm.mu.Lock()
	sm.sessions[key].Updated = time.Now().Add(-25 * time.Hour)
	sm.mu.Unlock()

	hour := 4
	policy := &config.ResetPolicyConfig{
		DailyResetHour: &hour,
		Timezone:       "UTC",
	}
	lm := NewLifecycleManager(sm, policy)

	shouldReset, reason := lm.ShouldReset(key)
	if !shouldReset {
		t.Fatal("expected ShouldReset to return true for daily reset")
	}
	if reason != "daily reset" {
		t.Errorf("reason = %q, want 'daily reset'", reason)
	}
}

func TestLifecycle_DailyReset_NotYet(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(dir)

	key := "daily-not-yet"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "recent message")
	// Updated is now (just created), so reset should not trigger

	hour := 4
	policy := &config.ResetPolicyConfig{
		DailyResetHour: &hour,
		Timezone:       "UTC",
	}
	lm := NewLifecycleManager(sm, policy)

	shouldReset, _ := lm.ShouldReset(key)
	if shouldReset {
		t.Fatal("expected ShouldReset to return false (updated recently)")
	}
}

func TestLifecycle_IdleExpiry(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(dir)

	key := "idle-test"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "hello")

	// Set updated to 2 hours ago
	sm.mu.Lock()
	sm.sessions[key].Updated = time.Now().Add(-2 * time.Hour)
	sm.mu.Unlock()

	policy := &config.ResetPolicyConfig{
		IdleExpiryMins: 60, // 1 hour
	}
	lm := NewLifecycleManager(sm, policy)

	shouldReset, reason := lm.ShouldReset(key)
	if !shouldReset {
		t.Fatal("expected ShouldReset to return true for idle expiry")
	}
	if reason != "idle expiry" {
		t.Errorf("reason = %q, want 'idle expiry'", reason)
	}
}

func TestLifecycle_IdleExpiry_NotYet(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(dir)

	key := "idle-not-yet"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "hello")
	// Updated is now, idle expiry should not trigger

	policy := &config.ResetPolicyConfig{
		IdleExpiryMins: 60,
	}
	lm := NewLifecycleManager(sm, policy)

	shouldReset, _ := lm.ShouldReset(key)
	if shouldReset {
		t.Fatal("expected ShouldReset to return false (active session)")
	}
}

func TestLifecycle_NilPolicy(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(dir)

	key := "no-policy"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "hello")

	lm := NewLifecycleManager(sm, nil)

	shouldReset, _ := lm.ShouldReset(key)
	if shouldReset {
		t.Fatal("expected ShouldReset to return false with nil policy")
	}
}

func TestLifecycle_Reset_ClearsSession(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(dir)

	key := "reset-clear"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "hello")
	sm.SetSummary(key, "summary")

	lm := NewLifecycleManager(sm, nil)
	if err := lm.Reset(key, "manual"); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	history := sm.GetHistory(key)
	if len(history) != 0 {
		t.Errorf("expected 0 messages after reset, got %d", len(history))
	}
	summary := sm.GetSummary(key)
	if summary != "" {
		t.Errorf("expected empty summary after reset, got %q", summary)
	}
}

func TestLifecycle_Reset_WritesMetaEvent(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(dir)

	key := "reset-meta"
	sm.GetOrCreate(key)
	sm.AddMessage(key, "user", "hello")

	lm := NewLifecycleManager(sm, nil)
	lm.Reset(key, "test reason")

	// Check JSONL for meta event
	jsonlPath := filepath.Join(dir, "reset-meta.jsonl")
	tr := NewTranscriptReader(jsonlPath)
	entries, err := tr.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Should have: 1 message + 1 meta
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 entries, got %d", len(entries))
	}
	lastEntry := entries[len(entries)-1]
	if lastEntry.Kind != "meta" {
		t.Errorf("last entry kind = %q, want 'meta'", lastEntry.Kind)
	}
	if lastEntry.Meta["action"] != "reset" {
		t.Errorf("meta action = %v, want 'reset'", lastEntry.Meta["action"])
	}
	if lastEntry.Meta["reason"] != "test reason" {
		t.Errorf("meta reason = %v, want 'test reason'", lastEntry.Meta["reason"])
	}
}

func TestLifecycle_NonexistentSession(t *testing.T) {
	dir := t.TempDir()
	sm := NewSessionManager(dir)

	lm := NewLifecycleManager(sm, &config.ResetPolicyConfig{IdleExpiryMins: 1})

	shouldReset, _ := lm.ShouldReset("nonexistent")
	if shouldReset {
		t.Fatal("should not reset nonexistent session")
	}
}
