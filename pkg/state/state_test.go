package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicSave(t *testing.T) {
	// Create temp workspace
	tmpDir, err := os.MkdirTemp("", "state-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sm := NewManager(tmpDir)

	// Test SetLastChannel
	err = sm.SetLastChannel("test-channel")
	if err != nil {
		t.Fatalf("SetLastChannel failed: %v", err)
	}

	// Verify the channel was saved
	lastChannel := sm.GetLastChannel()
	if lastChannel != "test-channel" {
		t.Errorf("Expected channel 'test-channel', got '%s'", lastChannel)
	}

	// Verify timestamp was updated
	if sm.GetTimestamp().IsZero() {
		t.Error("Expected timestamp to be updated")
	}

	// Verify state file exists
	stateFile := filepath.Join(tmpDir, "state", "state.json")
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		t.Error("Expected state file to exist")
	}

	// Create a new manager to verify persistence
	sm2 := NewManager(tmpDir)
	if sm2.GetLastChannel() != "test-channel" {
		t.Errorf("Expected persistent channel 'test-channel', got '%s'", sm2.GetLastChannel())
	}
}

func TestSetLastChatID(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "state-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sm := NewManager(tmpDir)

	// Test SetLastChatID
	err = sm.SetLastChatID("test-chat-id")
	if err != nil {
		t.Fatalf("SetLastChatID failed: %v", err)
	}

	// Verify the chat ID was saved
	lastChatID := sm.GetLastChatID()
	if lastChatID != "test-chat-id" {
		t.Errorf("Expected chat ID 'test-chat-id', got '%s'", lastChatID)
	}

	// Verify timestamp was updated
	if sm.GetTimestamp().IsZero() {
		t.Error("Expected timestamp to be updated")
	}

	// Create a new manager to verify persistence
	sm2 := NewManager(tmpDir)
	if sm2.GetLastChatID() != "test-chat-id" {
		t.Errorf("Expected persistent chat ID 'test-chat-id', got '%s'", sm2.GetLastChatID())
	}
}

func TestOverwriteState(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "state-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sm := NewManager(tmpDir)

	// Write initial state
	err = sm.SetLastChannel("initial-channel")
	if err != nil {
		t.Fatalf("SetLastChannel failed: %v", err)
	}

	// Verify initial state
	if sm.GetLastChannel() != "initial-channel" {
		t.Errorf("Expected channel 'initial-channel', got '%s'", sm.GetLastChannel())
	}

	// Overwrite with new state
	err = sm.SetLastChannel("new-channel")
	if err != nil {
		t.Fatalf("SetLastChannel failed: %v", err)
	}

	// Verify the new state was saved
	if sm.GetLastChannel() != "new-channel" {
		t.Errorf("Expected channel 'new-channel', got '%s'", sm.GetLastChannel())
	}

	// Verify no temp files left behind
	stateDir := filepath.Join(tmpDir, "state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("Failed to read state dir: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".tmp" {
			t.Errorf("Unexpected temp file found: %s", entry.Name())
		}
	}
}

func TestConcurrentAccess(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "state-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sm := NewManager(tmpDir)

	// Test concurrent writes
	done := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			channel := fmt.Sprintf("channel-%d", idx)
			sm.SetLastChannel(channel)
			done <- true
		}(i)
	}

	// Wait for all goroutines to complete
	for i := 0; i < 10; i++ {
		<-done
	}

	// Verify the final state is consistent
	lastChannel := sm.GetLastChannel()
	if lastChannel == "" {
		t.Error("Expected non-empty channel after concurrent writes")
	}

	// Verify state file is valid JSON
	stateFile := filepath.Join(tmpDir, "state", "state.json")
	data, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("Failed to read state file: %v", err)
	}

	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Errorf("State file contains invalid JSON: %v", err)
	}
}

func TestNewManager_ExistingState(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "state-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create initial state
	sm1 := NewManager(tmpDir)
	sm1.SetLastChannel("existing-channel")
	sm1.SetLastChatID("existing-chat-id")

	// Create new manager with same workspace
	sm2 := NewManager(tmpDir)

	// Verify state was loaded
	if sm2.GetLastChannel() != "existing-channel" {
		t.Errorf("Expected channel 'existing-channel', got '%s'", sm2.GetLastChannel())
	}

	if sm2.GetLastChatID() != "existing-chat-id" {
		t.Errorf("Expected chat ID 'existing-chat-id', got '%s'", sm2.GetLastChatID())
	}
}

func TestNewManager_EmptyWorkspace(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "state-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	sm := NewManager(tmpDir)

	// Verify default state
	if sm.GetLastChannel() != "" {
		t.Errorf("Expected empty channel, got '%s'", sm.GetLastChannel())
	}

	if sm.GetLastChatID() != "" {
		t.Errorf("Expected empty chat ID, got '%s'", sm.GetLastChatID())
	}

	if !sm.GetTimestamp().IsZero() {
		t.Error("Expected zero timestamp for new state")
	}
}
