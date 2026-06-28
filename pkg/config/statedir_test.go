package config

import (
	"path/filepath"
	"testing"
)

func TestStateDirPath(t *testing.T) {
	// Explicit state_dir is honored (with ~ expansion).
	c := &Config{}
	c.Agents.Defaults.StateDir = "/var/lib/picoclaw/state"
	if got := c.StateDirPath(); got != "/var/lib/picoclaw/state" {
		t.Errorf("explicit StateDir: got %q", got)
	}

	// Empty falls back to a non-empty default under ~/.picoclaw/state.
	c2 := &Config{}
	got := c2.StateDirPath()
	if got == "" {
		t.Fatal("default StateDirPath is empty")
	}
	if filepath.Base(got) != "state" {
		t.Errorf("default should end in /state, got %q", got)
	}
}
