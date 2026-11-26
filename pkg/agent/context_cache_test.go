package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/skills"
)

// setupWorkspace creates a temporary workspace with standard directories and optional files.
// Returns the tmpDir path; caller should defer os.RemoveAll(tmpDir).
func setupWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "picoclaw-test-*")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(tmpDir, "memory"), 0o755)
	os.MkdirAll(filepath.Join(tmpDir, "skills"), 0o755)
	for name, content := range files {
		dir := filepath.Dir(filepath.Join(tmpDir, name))
		os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(tmpDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tmpDir
}

// TestSingleSystemMessage verifies that BuildMessages always produces exactly one
// system message regardless of summary/history variations.
// Fix: multiple system messages break Anthropic (top-level system param) and
// Codex (only reads last system message as instructions).
func TestSingleSystemMessage(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nTest agent.",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	tests := []struct {
		name    string
		history []providers.Message
		summary string
		message string
	}{
		{
			name:    "no summary, no history",
			summary: "",
			message: "hello",
		},
		{
			name:    "with summary",
			summary: "Previous conversation discussed X",
			message: "hello",
		},
		{
			name: "with history and summary",
			history: []providers.Message{
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
			},
			summary: strings.Repeat("Long summary text. ", 50),
			message: "new message",
		},
		{
			name: "system message in history is filtered",
			history: []providers.Message{
				{Role: "system", Content: "stale system prompt from previous session"},
				{Role: "user", Content: "hi"},
				{Role: "assistant", Content: "hello"},
			},
			summary: "",
			message: "new message",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := cb.BuildMessages(tt.history, tt.summary, tt.message, nil, "test", "chat1", "", "")

			systemCount := 0
			for _, m := range msgs {
				if m.Role == "system" {
					systemCount++
				}
			}
			if systemCount != 1 {
				t.Errorf("expected exactly 1 system message, got %d", systemCount)
			}
			if msgs[0].Role != "system" {
				t.Errorf("first message should be system, got %s", msgs[0].Role)
			}
			if msgs[len(msgs)-1].Role != "user" {
				t.Errorf("last message should be user, got %s", msgs[len(msgs)-1].Role)
			}

			// System message must contain identity (static) and the date (dynamic,
			// day granularity — the precise time rides in the turn tail to keep
			// the cache prefix stable).
			sys := msgs[0].Content
			if !strings.Contains(sys, "picoclaw") {
				t.Error("system message missing identity")
			}
			if !strings.Contains(sys, "Current Date") {
				t.Error("system message missing dynamic date context")
			}

			// Summary handling
			if tt.summary != "" {
				if !strings.Contains(sys, "CONTEXT_SUMMARY:") {
					t.Error("summary present but CONTEXT_SUMMARY prefix missing")
				}
				if !strings.Contains(sys, tt.summary[:20]) {
					t.Error("summary content not found in system message")
				}
			} else {
				if strings.Contains(sys, "CONTEXT_SUMMARY:") {
					t.Error("CONTEXT_SUMMARY should not appear without summary")
				}
			}
		})
	}
}

func TestBuildMessages_CurrentSenderDynamicContext(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"IDENTITY.md": "# Identity\nTest agent.",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	tests := []struct {
		name              string
		senderID          string
		senderDisplayName string
		wantLine          string
		wantSection       bool
	}{
		{
			name:              "both id and display name",
			senderID:          "feishu:ou_xxx",
			senderDisplayName: "Zhang San",
			wantLine:          "Current sender: Zhang San (ID: feishu:ou_xxx)",
			wantSection:       true,
		},
		{
			name:              "display name only",
			senderDisplayName: "Alice",
			wantLine:          "Current sender: Alice",
			wantSection:       true,
		},
		{
			name:        "id only",
			senderID:    "discord:123",
			wantLine:    "Current sender: discord:123",
			wantSection: true,
		},
		{
			name:        "no sender info",
			wantSection: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs := cb.BuildMessages(nil, "", "hello", nil, "discord", "chat1", tt.senderID, tt.senderDisplayName)
			sys := msgs[0].Content

			if tt.wantSection {
				if !strings.Contains(sys, "## Current Sender") {
					t.Fatalf("system prompt missing Current Sender section:\n%s", sys)
				}
				if !strings.Contains(sys, tt.wantLine) {
					t.Fatalf("system prompt missing sender line %q:\n%s", tt.wantLine, sys)
				}
				return
			}

			if strings.Contains(sys, "## Current Sender") {
				t.Fatalf("system prompt should omit Current Sender section:\n%s", sys)
			}
		})
	}
}

// TestMtimeAutoInvalidation verifies that the cache detects source file changes
// via mtime without requiring explicit InvalidateCache().
// Fix: original implementation had no auto-invalidation — edits to bootstrap files,
// memory, or skills were invisible until process restart.
func TestMtimeAutoInvalidation(t *testing.T) {
	tests := []struct {
		name       string
		file       string // relative path inside workspace
		contentV1  string
		contentV2  string
		checkField string // substring to verify in rebuilt prompt
	}{
		{
			name:       "bootstrap file change",
			file:       "AGENT.md",
			contentV1:  "# Original Agent",
			contentV2:  "# Updated Agent",
			checkField: "Updated Agent",
		},
		{
			name:       "memory file change",
			file:       "memory/MEMORY.md",
			contentV1:  "# Memory\nUser likes Go.",
			contentV2:  "# Memory\nUser likes Rust.",
			checkField: "User likes Rust",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := setupWorkspace(t, map[string]string{tt.file: tt.contentV1})
			defer os.RemoveAll(tmpDir)

			cb := NewContextBuilder(tmpDir)

			sp1 := cb.BuildSystemPromptWithCache()

			// Overwrite file and set future mtime to ensure detection.
			// Use 2s offset for filesystem mtime resolution safety (some FS
			// have 1s or coarser granularity, especially in CI containers).
			fullPath := filepath.Join(tmpDir, tt.file)
			os.WriteFile(fullPath, []byte(tt.contentV2), 0o644)
			future := time.Now().Add(2 * time.Second)
			os.Chtimes(fullPath, future, future)

			// Verify sourceFilesChangedLocked detects the mtime change
			cb.systemPromptMutex.RLock()
			changed := cb.sourceFilesChangedLocked()
			cb.systemPromptMutex.RUnlock()
			if !changed {
				t.Fatalf("sourceFilesChangedLocked() should detect %s change", tt.file)
			}

			// Should auto-rebuild without explicit InvalidateCache()
			sp2 := cb.BuildSystemPromptWithCache()
			if sp1 == sp2 {
				t.Errorf("cache not rebuilt after %s change", tt.file)
			}
			if !strings.Contains(sp2, tt.checkField) {
				t.Errorf("rebuilt prompt missing expected content %q", tt.checkField)
			}
		})
	}

	// Skills directory mtime change
	t.Run("skills dir change", func(t *testing.T) {
		tmpDir := setupWorkspace(t, nil)
		defer os.RemoveAll(tmpDir)

		cb := NewContextBuilder(tmpDir)
		_ = cb.BuildSystemPromptWithCache() // populate cache

		// Touch skills directory (simulate new skill installed)
		skillsDir := filepath.Join(tmpDir, "skills")
		future := time.Now().Add(2 * time.Second)
		os.Chtimes(skillsDir, future, future)

		// Verify sourceFilesChangedLocked detects it (cache is rebuilt)
		// We confirm by checking internal state: a second call should rebuild.
		cb.systemPromptMutex.RLock()
		changed := cb.sourceFilesChangedLocked()
		cb.systemPromptMutex.RUnlock()
		if !changed {
			t.Error("sourceFilesChangedLocked() should detect skills dir mtime change")
		}
	})
}

// TestExplicitInvalidateCache verifies that InvalidateCache() forces a rebuild
// even when source files haven't changed (useful for tests and reload commands).
func TestExplicitInvalidateCache(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Test Agent",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	sp1 := cb.BuildSystemPromptWithCache()
	cb.InvalidateCache()
	sp2 := cb.BuildSystemPromptWithCache()

	if sp1 != sp2 {
		t.Error("prompt should be identical after invalidate+rebuild when files unchanged")
	}

	// Verify cachedAt was reset
	cb.InvalidateCache()
	cb.systemPromptMutex.RLock()
	if !cb.cachedAt.IsZero() {
		t.Error("cachedAt should be zero after InvalidateCache()")
	}
	cb.systemPromptMutex.RUnlock()
}

// TestCacheStability verifies that the static prompt is stable across repeated calls
// when no files change (regression test for issue #607).
func TestCacheStability(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md": "# Agent\nContent",
		"SOUL.md":  "# Soul\nContent",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	results := make([]string, 5)
	for i := range results {
		results[i] = cb.BuildSystemPromptWithCache()
	}
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Errorf("cached prompt changed between call 0 and %d", i)
		}
	}

	// Static prompt must NOT contain per-request data
	if strings.Contains(results[0], "Current Time") {
		t.Error("static cached prompt should not contain time (added dynamically)")
	}
}

// TestNewFileCreationInvalidatesCache verifies that creating a source file that
// did not exist when the cache was built triggers a cache rebuild.
// This catches the "from nothing to something" edge case that the old
// modifiedSince (return false on stat error) would miss.
func TestNewFileCreationInvalidatesCache(t *testing.T) {
	tests := []struct {
		name       string
		file       string // relative path inside workspace
		content    string
		checkField string // substring to verify in rebuilt prompt
	}{
		{
			name:       "new bootstrap file",
			file:       "SOUL.md",
			content:    "# Soul\nBe kind and helpful.",
			checkField: "Be kind and helpful",
		},
		{
			name:       "new memory file",
			file:       "memory/MEMORY.md",
			content:    "# Memory\nUser prefers dark mode.",
			checkField: "User prefers dark mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start with an empty workspace (no bootstrap/memory files)
			tmpDir := setupWorkspace(t, nil)
			defer os.RemoveAll(tmpDir)

			cb := NewContextBuilder(tmpDir)

			// Populate cache — file does not exist yet
			sp1 := cb.BuildSystemPromptWithCache()
			if strings.Contains(sp1, tt.checkField) {
				t.Fatalf("prompt should not contain %q before file is created", tt.checkField)
			}

			// Create the file after cache was built
			fullPath := filepath.Join(tmpDir, tt.file)
			os.MkdirAll(filepath.Dir(fullPath), 0o755)
			if err := os.WriteFile(fullPath, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			// Set future mtime to guarantee detection
			future := time.Now().Add(2 * time.Second)
			os.Chtimes(fullPath, future, future)

			// Cache should auto-invalidate because file went from absent -> present
			sp2 := cb.BuildSystemPromptWithCache()
			if !strings.Contains(sp2, tt.checkField) {
				t.Errorf("cache not invalidated on new file creation: expected %q in prompt", tt.checkField)
			}
		})
	}
}

// TestSkillFileContentChange verifies that modifying a skill file's content
// (not just the directory structure) invalidates the cache.
// This is the scenario where directory mtime alone is insufficient — on most
// filesystems, editing a file inside a directory does NOT update the parent
// directory's mtime.
func TestSkillFileContentChange(t *testing.T) {
	skillMD := `---
name: test-skill
description: "A test skill"
---
# Test Skill v1
Original content.`

	tmpDir := setupWorkspace(t, map[string]string{
		"skills/test-skill/SKILL.md": skillMD,
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	// Populate cache
	sp1 := cb.BuildSystemPromptWithCache()
	_ = sp1 // cache is warm

	// Modify the skill file content (without touching the skills/ directory)
	updatedSkillMD := `---
name: test-skill
description: "An updated test skill"
---
# Test Skill v2
Updated content.`

	skillPath := filepath.Join(tmpDir, "skills", "test-skill", "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(updatedSkillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	// Set future mtime on the skill file only (NOT the directory)
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(skillPath, future, future)

	if cb.BuildSystemPromptWithCache() != sp1 {
		t.Fatal("building the turn's prompt must not walk the skill tree")
	}
	cb.RefreshSystemPromptCache()

	// Verify cache is actually rebuilt with new content
	sp2 := cb.BuildSystemPromptWithCache()
	if sp1 == sp2 && strings.Contains(sp1, "test-skill") {
		// If the skill appeared in the prompt and the prompt didn't change,
		// the cache was not invalidated.
		t.Error("cache should be invalidated when skill file content changes")
	}
}

// TestGlobalSkillFileContentChange verifies that modifying a global skill
// (~/.picoclaw/skills) invalidates the cached system prompt.
func TestGlobalSkillFileContentChange(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	globalSkillPath := filepath.Join(tmpHome, ".picoclaw", "skills", "global-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(globalSkillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	v1 := `---
name: global-skill
description: global-v1
---
# Global Skill v1`
	if err := os.WriteFile(globalSkillPath, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder(tmpDir)
	sp1 := cb.BuildSystemPromptWithCache()
	if !strings.Contains(sp1, "global-v1") {
		t.Fatal("expected initial prompt to contain global skill description")
	}

	v2 := `---
name: global-skill
description: global-v2
---
# Global Skill v2`
	if err := os.WriteFile(globalSkillPath, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(globalSkillPath, future, future); err != nil {
		t.Fatalf("failed to update mtime for %s: %v", globalSkillPath, err)
	}

	if cb.BuildSystemPromptWithCache() != sp1 {
		t.Fatal("building the turn's prompt must not walk the skill tree")
	}
	cb.RefreshSystemPromptCache()

	sp2 := cb.BuildSystemPromptWithCache()
	if !strings.Contains(sp2, "global-v2") {
		t.Error("rebuilt prompt should contain updated global skill description")
	}
	if sp1 == sp2 {
		t.Error("cache should be invalidated when global skill file content changes")
	}
}

// TestBuiltinSkillFileContentChange verifies that modifying a builtin skill
// invalidates the cached system prompt.
func TestBuiltinSkillFileContentChange(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	builtinRoot := t.TempDir()
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", builtinRoot)

	builtinSkillPath := filepath.Join(builtinRoot, "builtin-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(builtinSkillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	v1 := `---
name: builtin-skill
description: builtin-v1
---
# Builtin Skill v1`
	if err := os.WriteFile(builtinSkillPath, []byte(v1), 0o644); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder(tmpDir)
	sp1 := cb.BuildSystemPromptWithCache()
	if !strings.Contains(sp1, "builtin-v1") {
		t.Fatal("expected initial prompt to contain builtin skill description")
	}

	v2 := `---
name: builtin-skill
description: builtin-v2
---
# Builtin Skill v2`
	if err := os.WriteFile(builtinSkillPath, []byte(v2), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(builtinSkillPath, future, future); err != nil {
		t.Fatalf("failed to update mtime for %s: %v", builtinSkillPath, err)
	}

	if cb.BuildSystemPromptWithCache() != sp1 {
		t.Fatal("building the turn's prompt must not walk the skill tree")
	}
	cb.RefreshSystemPromptCache()

	sp2 := cb.BuildSystemPromptWithCache()
	if !strings.Contains(sp2, "builtin-v2") {
		t.Error("rebuilt prompt should contain updated builtin skill description")
	}
	if sp1 == sp2 {
		t.Error("cache should be invalidated when builtin skill file content changes")
	}
}

// TestSkillFileDeletionInvalidatesCache verifies that deleting a nested skill
// file invalidates the cached system prompt.
func TestSkillFileDeletionInvalidatesCache(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"skills/delete-me/SKILL.md": `---
name: delete-me
description: delete-me-v1
---
# Delete Me`,
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)
	sp1 := cb.BuildSystemPromptWithCache()
	if !strings.Contains(sp1, "delete-me-v1") {
		t.Fatal("expected initial prompt to contain skill description")
	}

	skillPath := filepath.Join(tmpDir, "skills", "delete-me", "SKILL.md")
	if err := os.Remove(skillPath); err != nil {
		t.Fatal(err)
	}

	if cb.BuildSystemPromptWithCache() != sp1 {
		t.Fatal("building the turn's prompt must not walk the skill tree")
	}
	cb.RefreshSystemPromptCache()

	sp2 := cb.BuildSystemPromptWithCache()
	if strings.Contains(sp2, "delete-me-v1") {
		t.Error("rebuilt prompt should not contain deleted skill description")
	}
	if sp1 == sp2 {
		t.Error("cache should be invalidated when skill file is deleted")
	}
}

// TestConcurrentBuildSystemPromptWithCache verifies that multiple goroutines
// can safely call BuildSystemPromptWithCache concurrently without producing
// empty results, panics, or data races.
// Run with: go test -race ./pkg/agent/ -run TestConcurrentBuildSystemPromptWithCache
func TestConcurrentBuildSystemPromptWithCache(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"AGENT.md":             "# Agent\nConcurrency test agent.",
		"SOUL.md":              "# Soul\nBe helpful.",
		"memory/MEMORY.md":     "# Memory\nUser prefers Go.",
		"skills/demo/SKILL.md": "---\nname: demo\ndescription: \"demo skill\"\n---\n# Demo",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup
	errs := make(chan string, goroutines*iterations)

	for g := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range iterations {
				result := cb.BuildSystemPromptWithCache()
				if result == "" {
					errs <- "empty prompt returned"
					return
				}
				if !strings.Contains(result, "picoclaw") {
					errs <- "prompt missing identity"
					return
				}

				// Also exercise BuildMessages concurrently
				msgs := cb.BuildMessages(nil, "", "hello", nil, "test", "chat", "", "")
				if len(msgs) < 2 {
					errs <- "BuildMessages returned fewer than 2 messages"
					return
				}
				if msgs[0].Role != "system" {
					errs <- "first message not system"
					return
				}

				// Occasionally invalidate to exercise the write path
				if i%10 == 0 {
					cb.InvalidateCache()
				}
				if i%10 == 5 {
					cb.RefreshSystemPromptCache()
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)

	for errMsg := range errs {
		t.Errorf("concurrent access error: %s", errMsg)
	}
}

// BenchmarkBuildMessagesWithCache measures caching performance.

// TestEmptyWorkspaceBaselineDetectsNewFiles verifies that when the cache is
// built on an empty workspace (no tracked files exist), creating a file
// afterwards still triggers cache invalidation. This validates the
// time.Unix(1, 0) fallback for maxMtime: any real file's mtime is after epoch,
// so fileChangedSince correctly detects the absent -> present transition AND
// the mtime comparison succeeds even without artificially inflated Chtimes.
func TestEmptyWorkspaceBaselineDetectsNewFiles(t *testing.T) {
	// Empty workspace: no bootstrap files, no memory, no skills content.
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)

	// Build cache — all tracked files are absent, maxMtime falls back to epoch.
	sp1 := cb.BuildSystemPromptWithCache()

	// Create a bootstrap file with natural mtime (no Chtimes manipulation).
	// The file's mtime should be the current wall-clock time, which is
	// strictly after time.Unix(1, 0).
	soulPath := filepath.Join(tmpDir, "SOUL.md")
	if err := os.WriteFile(soulPath, []byte("# Soul\nNewly created."), 0o644); err != nil {
		t.Fatal(err)
	}

	// Cache should detect the new file via existedAtCache (absent -> present).
	cb.systemPromptMutex.RLock()
	changed := cb.sourceFilesChangedLocked()
	cb.systemPromptMutex.RUnlock()
	if !changed {
		t.Fatal("sourceFilesChangedLocked should detect newly created file on empty workspace")
	}

	sp2 := cb.BuildSystemPromptWithCache()
	if !strings.Contains(sp2, "Newly created") {
		t.Error("rebuilt prompt should contain new file content")
	}
	if sp1 == sp2 {
		t.Error("cache should have been invalidated after file creation")
	}
}

func TestBuildMessages_IncludesMediaOnlyCurrentMessage(t *testing.T) {
	tmpDir := setupWorkspace(t, nil)
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)
	msgs := cb.BuildMessages(
		nil,
		"",
		"",
		[]string{"data:image/png;base64,abc123"},
		"pico",
		"chat-1",
		"",
		"",
	)

	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}

	userMsg := msgs[1]
	if userMsg.Role != "user" {
		t.Fatalf("userMsg.Role = %q, want %q", userMsg.Role, "user")
	}
	if userMsg.Content != "" {
		t.Fatalf("userMsg.Content = %q, want empty string", userMsg.Content)
	}
	if len(userMsg.Media) != 1 || userMsg.Media[0] != "data:image/png;base64,abc123" {
		t.Fatalf("userMsg.Media = %#v, want image payload", userMsg.Media)
	}
}

// BenchmarkBuildMessagesWithCache measures caching performance.
func BenchmarkBuildMessagesWithCache(b *testing.B) {
	tmpDir, _ := os.MkdirTemp("", "picoclaw-bench-*")
	defer os.RemoveAll(tmpDir)

	os.MkdirAll(filepath.Join(tmpDir, "memory"), 0o755)
	os.MkdirAll(filepath.Join(tmpDir, "skills"), 0o755)
	for _, name := range []string{"AGENT.md", "SOUL.md"} {
		os.WriteFile(filepath.Join(tmpDir, name), []byte(strings.Repeat("Content.\n", 10)), 0o644)
	}

	cb := NewContextBuilder(tmpDir)
	history := []providers.Message{
		{Role: "user", Content: "previous message"},
		{Role: "assistant", Content: "previous response"},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = cb.BuildMessages(history, "summary", "new message", nil, "cli", "test", "", "")
	}
}

// A refresh requested while a walk runs must not be dropped: the walk may have
// already passed the file the requester changed.
func TestRefreshRequestedDuringWalkRunsAgain(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"skills/late/SKILL.md": "---\nname: late\ndescription: late-v1\n---\n# Late",
	})
	defer os.RemoveAll(tmpDir)

	cb := NewContextBuilder(tmpDir)
	sp1 := cb.BuildSystemPromptWithCache()
	if !strings.Contains(sp1, "late-v1") {
		t.Fatal("expected initial prompt to contain the skill description")
	}

	skillPath := filepath.Join(tmpDir, "skills", "late", "SKILL.md")
	cb.afterSkillScan = func() {
		cb.afterSkillScan = nil
		// Another turn edits the skill after this walk checked it, then asks
		// for its own refresh while the walk still runs.
		v2 := "---\nname: late\ndescription: late-v2\n---\n# Late"
		if err := os.WriteFile(skillPath, []byte(v2), 0o644); err != nil {
			t.Fatal(err)
		}
		future := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(skillPath, future, future); err != nil {
			t.Fatal(err)
		}
		cb.RefreshSystemPromptCache()
		// The walk holds no lock: a turn reads the previous prompt meanwhile.
		if cb.BuildSystemPromptWithCache() != sp1 {
			t.Error("a turn during the walk should read the previous prompt")
		}
	}
	cb.RefreshSystemPromptCache()

	if !strings.Contains(cb.BuildSystemPromptWithCache(), "late-v2") {
		t.Error("the edit made during the walk should be in the prompt once the refresh returns")
	}
}

// A skill added next to a future-dated file must still be seen on the turn's
// path: each root is compared with its own mtime, not the newest file's.
func TestSkillAddedNextToFutureDatedFile(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"skills/old/SKILL.md": "---\nname: old\ndescription: old-skill\n---\n# Old",
	})
	defer os.RemoveAll(tmpDir)

	oldPath := filepath.Join(tmpDir, "skills", "old", "SKILL.md")
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(oldPath, future, future); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder(tmpDir)
	if sp := cb.BuildSystemPromptWithCache(); !strings.Contains(sp, "old-skill") {
		t.Fatal("expected initial prompt to contain the old skill")
	}

	newPath := filepath.Join(tmpDir, "skills", "new", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("---\nname: new\ndescription: new-skill\n---\n# New"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(cb.BuildSystemPromptWithCache(), "new-skill") {
		t.Error("a skill added to the root should be in the next turn's prompt")
	}
}

// savedCatalogWorkspace builds the prompt and saves the catalog, as a process
// does when it stops, so the next process finds the skill catalog saved.
func savedCatalogWorkspace(t *testing.T) (string, string) {
	t.Helper()
	t.Setenv("PICOCLAW_HOME", t.TempDir())
	tmpDir := setupWorkspace(t, map[string]string{
		"skills/kept/SKILL.md": "---\nname: kept\ndescription: kept-v1\n---\n# Kept",
		"memory/MEMORY.md":     "memory-v1",
	})
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	// Written well before: a skill added later moves the root's mtime even on a
	// coarse filesystem clock.
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(tmpDir, "skills"), past, past); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder(tmpDir)
	prompt := cb.BuildSystemPromptWithCache()
	cb.saveUnsavedCatalog()
	entries, err := os.ReadDir(filepath.Join(tmpDir, "state"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "skill_catalog.json" {
		t.Fatalf("saving should leave only the skill catalog in state/: %v %v", entries, err)
	}
	return tmpDir, prompt
}

func TestRestartBuildsFromSavedSkillCatalog(t *testing.T) {
	tmpDir, before := savedCatalogWorkspace(t)
	if NewContextBuilder(tmpDir).BuildSystemPromptWithCache() != before {
		t.Fatal("a restart with nothing changed should build the same prompt")
	}

	// An edit inside a skill moves no root: the restart's first turn keeps the
	// saved catalog, and the refresh after that turn reads the edit.
	skillPath := filepath.Join(tmpDir, "skills", "kept", "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: kept\ndescription: kept-v2\n---\n# Kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(skillPath, future, future); err != nil {
		t.Fatal(err)
	}
	cb := NewContextBuilder(tmpDir)
	if !strings.Contains(cb.BuildSystemPromptWithCache(), "kept-v1") {
		t.Fatal("the first turn after a restart should not read the skill trees")
	}
	cb.RefreshSystemPromptCache()
	if !strings.Contains(cb.BuildSystemPromptWithCache(), "kept-v2") {
		t.Error("the refresh after the first turn should read the edit inside the skill")
	}
	cb.saveUnsavedCatalog()
	if !strings.Contains(NewContextBuilder(tmpDir).BuildSystemPromptWithCache(), "kept-v2") {
		t.Error("stopping should save the refreshed catalog for the next restart")
	}
}

func TestRestartSeesEditsMadeWhileStopped(t *testing.T) {
	cases := map[string]struct {
		path, content, want string
	}{
		"memory":    {"memory/MEMORY.md", "memory-v2", "memory-v2"},
		"bootstrap": {"AGENTS.md", "# Agent\nbootstrap-v2", "bootstrap-v2"},
		"new skill": {
			"skills/added/SKILL.md",
			"---\nname: added\ndescription: added-skill\n---\n# Added",
			"added-skill",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tmpDir, _ := savedCatalogWorkspace(t)
			path := filepath.Join(tmpDir, tc.path)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(NewContextBuilder(tmpDir).BuildSystemPromptWithCache(), tc.want) {
				t.Errorf("the first turn after a restart should see the %s edit", name)
			}
		})
	}
}

func TestRestartIgnoresBadSavedCatalog(t *testing.T) {
	cases := map[string]func(data []byte, catalog skillCatalog) []byte{
		"truncated": func(data []byte, _ skillCatalog) []byte { return data[:len(data)/2] },
		"older version": func(_ []byte, catalog skillCatalog) []byte {
			catalog.Version--
			data, _ := json.Marshal(catalog)
			return data
		},
		"incomplete": func(_ []byte, catalog skillCatalog) []byte {
			catalog.SkillFiles = nil
			data, _ := json.Marshal(catalog)
			return data
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			tmpDir, _ := savedCatalogWorkspace(t)
			catalogPath := filepath.Join(tmpDir, "state", "skill_catalog.json")
			data, err := os.ReadFile(catalogPath)
			if err != nil {
				t.Fatal(err)
			}
			var catalog skillCatalog
			if err := json.Unmarshal(data, &catalog); err != nil {
				t.Fatal(err)
			}
			// A catalog used despite being bad would show this skill.
			catalog.Skills = []skills.SkillInfo{{Name: "ghost", Description: "ghost-skill", Source: "workspace"}}
			data, _ = json.Marshal(catalog)
			if err := os.WriteFile(catalogPath, corrupt(data, catalog), 0o600); err != nil {
				t.Fatal(err)
			}

			sp := NewContextBuilder(tmpDir).BuildSystemPromptWithCache()
			if strings.Contains(sp, "ghost-skill") || !strings.Contains(sp, "kept-v1") {
				t.Error("a bad saved catalog should be ignored and the skills read")
			}
		})
	}
}

// The catalog is saved when the loop stops, not during a turn: no turn waits
// on the write, and a stopped pod leaves it to the next one.
func TestStopSavesSkillCatalog(t *testing.T) {
	al, _ := newScriptedLoop(t, providers.LLMResponse{Content: "done"})
	if _, err := al.ProcessDirect(context.Background(), "hello", "skill-catalog-session"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	catalogPath := filepath.Join(al.GetConfig().WorkspacePath(), "state", "skill_catalog.json")
	if _, err := os.Stat(catalogPath); !os.IsNotExist(err) {
		t.Fatalf("a turn should not write the skill catalog: %v", err)
	}
	al.Stop()
	if _, err := os.Stat(catalogPath); err != nil {
		t.Errorf("stopping the loop should save the skill catalog: %v", err)
	}
}

// On the pods' network volume a skill root's mtime, as the client reports it,
// keeps its old value after a skill is added or removed, while the root's
// listing changes at once: the next turn's prompt must follow the listing.
func TestSkillAddedOrRemovedWhileTheRootMtimeStays(t *testing.T) {
	tmpDir := setupWorkspace(t, map[string]string{
		"skills/old/SKILL.md": "---\nname: old\ndescription: old-skill\n---\n# Old",
	})
	defer os.RemoveAll(tmpDir)
	root := filepath.Join(tmpDir, "skills")
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	keepRootMtime := func() {
		if err := os.Chtimes(root, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
	}

	cb := NewContextBuilder(tmpDir)
	if sp := cb.BuildSystemPromptWithCache(); !strings.Contains(sp, "old-skill") || strings.Contains(sp, "new-skill") {
		t.Fatal("expected the initial prompt to list only the old skill")
	}

	newPath := filepath.Join(root, "new", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("---\nname: new\ndescription: new-skill\n---\n# New"), 0o644); err != nil {
		t.Fatal(err)
	}
	keepRootMtime()
	if !strings.Contains(cb.BuildSystemPromptWithCache(), "new-skill") {
		t.Error("a skill added while the root's mtime stayed should be in the next turn's prompt")
	}

	if err := os.RemoveAll(filepath.Dir(newPath)); err != nil {
		t.Fatal(err)
	}
	keepRootMtime()
	if strings.Contains(cb.BuildSystemPromptWithCache(), "new-skill") {
		t.Error("a skill removed while the root's mtime stayed should be out of the next turn's prompt")
	}
}
