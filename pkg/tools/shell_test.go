package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

// TestShellTool_Success verifies successful command execution
func TestShellTool_Success(t *testing.T) {
	tool := NewExecTool("", false)

	ctx := context.Background()
	args := map[string]any{
		"command": "echo 'hello world'",
	}

	result := tool.Execute(ctx, args)

	// Success should not be an error
	if result.IsError {
		t.Errorf("Expected success, got IsError=true: %s", result.ForLLM)
	}

	// ForUser should contain command output
	if !strings.Contains(result.ForUser, "hello world") {
		t.Errorf("Expected ForUser to contain 'hello world', got: %s", result.ForUser)
	}

	// ForLLM should contain full output
	if !strings.Contains(result.ForLLM, "hello world") {
		t.Errorf("Expected ForLLM to contain 'hello world', got: %s", result.ForLLM)
	}
}

// TestShellTool_Failure verifies failed command execution
func TestShellTool_Failure(t *testing.T) {
	tool := NewExecTool("", false)

	ctx := context.Background()
	args := map[string]any{
		"command": "ls /nonexistent_directory_12345",
	}

	result := tool.Execute(ctx, args)

	// Failure should be marked as error
	if !result.IsError {
		t.Errorf("Expected error for failed command, got IsError=false")
	}

	// ForUser should contain error information
	if result.ForUser == "" {
		t.Errorf("Expected ForUser to contain error info, got empty string")
	}

	// ForLLM should contain exit code or error
	if !strings.Contains(result.ForLLM, "Exit code") && result.ForUser == "" {
		t.Errorf("Expected ForLLM to contain exit code or error, got: %s", result.ForLLM)
	}
}

// TestShellTool_Timeout verifies command timeout handling
func TestShellTool_Timeout(t *testing.T) {
	tool := NewExecTool("", false)
	tool.SetTimeout(100 * time.Millisecond)

	ctx := context.Background()
	args := map[string]any{
		"command": "sleep 10",
	}

	result := tool.Execute(ctx, args)

	// Timeout should be marked as error
	if !result.IsError {
		t.Errorf("Expected error for timeout, got IsError=false")
	}

	// Should mention timeout
	if !strings.Contains(result.ForLLM, "timed out") && !strings.Contains(result.ForUser, "timed out") {
		t.Errorf("Expected timeout message, got ForLLM: %s, ForUser: %s", result.ForLLM, result.ForUser)
	}
}

// TestShellTool_WorkingDir verifies custom working directory
func TestShellTool_WorkingDir(t *testing.T) {
	// Create temp directory
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	os.WriteFile(testFile, []byte("test content"), 0o644)

	tool := NewExecTool("", false)

	ctx := context.Background()
	args := map[string]any{
		"command":     "cat test.txt",
		"working_dir": tmpDir,
	}

	result := tool.Execute(ctx, args)

	if result.IsError {
		t.Errorf("Expected success in custom working dir, got error: %s", result.ForLLM)
	}

	if !strings.Contains(result.ForUser, "test content") {
		t.Errorf("Expected output from custom dir, got: %s", result.ForUser)
	}
}

// TestShellTool_DangerousCommand verifies safety guard blocks dangerous commands
func TestShellTool_DangerousCommand(t *testing.T) {
	tool := NewExecTool("", false)

	ctx := context.Background()
	args := map[string]any{
		"command": "mkfs /dev/sda",
	}

	result := tool.Execute(ctx, args)

	// Dangerous command should be blocked
	if !result.IsError {
		t.Errorf("Expected dangerous command to be blocked (IsError=true)")
	}

	if !strings.Contains(result.ForLLM, "blocked") && !strings.Contains(result.ForUser, "blocked") {
		t.Errorf("Expected 'blocked' message, got ForLLM: %s, ForUser: %s", result.ForLLM, result.ForUser)
	}
}

// TestShellTool_MissingCommand verifies error handling for missing command
func TestShellTool_MissingCommand(t *testing.T) {
	tool := NewExecTool("", false)

	ctx := context.Background()
	args := map[string]any{}

	result := tool.Execute(ctx, args)

	// Should return error result
	if !result.IsError {
		t.Errorf("Expected error when command is missing")
	}
}

// TestShellTool_StderrCapture verifies stderr is captured and included
func TestShellTool_StderrCapture(t *testing.T) {
	tool := NewExecTool("", false)

	ctx := context.Background()
	args := map[string]any{
		"command": "sh -c 'echo stdout; echo stderr >&2'",
	}

	result := tool.Execute(ctx, args)

	// Both stdout and stderr should be in output
	if !strings.Contains(result.ForLLM, "stdout") {
		t.Errorf("Expected stdout in output, got: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "stderr") {
		t.Errorf("Expected stderr in output, got: %s", result.ForLLM)
	}
}

// TestShellTool_OutputTruncation verifies long output is truncated
func TestShellTool_OutputTruncation(t *testing.T) {
	tool := NewExecTool("", false)

	ctx := context.Background()
	// Generate long output (>10000 chars)
	args := map[string]any{
		"command": "python3 -c \"print('x' * 20000)\" || echo " + strings.Repeat("x", 20000),
	}

	result := tool.Execute(ctx, args)

	// Should have truncation message or be truncated
	if len(result.ForLLM) > 15000 {
		t.Errorf("Expected output to be truncated, got length: %d", len(result.ForLLM))
	}
}

// TestShellTool_WorkingDir_OutsideWorkspace verifies that working_dir cannot escape the workspace directly
func TestShellTool_WorkingDir_OutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outsideDir := filepath.Join(root, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("failed to create workspace: %v", err)
	}
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatalf("failed to create outside dir: %v", err)
	}

	tool := NewExecTool(workspace, true)
	result := tool.Execute(context.Background(), map[string]any{
		"command":     "pwd",
		"working_dir": outsideDir,
	})

	if !result.IsError {
		t.Fatalf("expected working_dir outside workspace to be blocked, got output: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "blocked") {
		t.Errorf("expected 'blocked' in error, got: %s", result.ForLLM)
	}
}

// TestShellTool_WorkingDir_SymlinkEscape verifies that a symlink inside the workspace
// pointing outside cannot be used as working_dir to escape the sandbox.
func TestShellTool_WorkingDir_SymlinkEscape(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	secretDir := filepath.Join(root, "secret")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("failed to create workspace: %v", err)
	}
	if err := os.MkdirAll(secretDir, 0o755); err != nil {
		t.Fatalf("failed to create secret dir: %v", err)
	}
	os.WriteFile(filepath.Join(secretDir, "secret.txt"), []byte("top secret"), 0o644)

	// symlink lives inside the workspace but resolves to secretDir outside it
	link := filepath.Join(workspace, "escape")
	if err := os.Symlink(secretDir, link); err != nil {
		t.Skipf("symlinks not supported in this environment: %v", err)
	}

	tool := NewExecTool(workspace, true)
	result := tool.Execute(context.Background(), map[string]any{
		"command":     "cat secret.txt",
		"working_dir": link,
	})

	if !result.IsError {
		t.Fatalf("expected symlink working_dir escape to be blocked, got output: %s", result.ForLLM)
	}
	if !strings.Contains(result.ForLLM, "blocked") {
		t.Errorf("expected 'blocked' in error, got: %s", result.ForLLM)
	}
}

// TestShellTool_RestrictToWorkspace verifies workspace restriction
func TestShellTool_RestrictToWorkspace(t *testing.T) {
	tmpDir := t.TempDir()
	tool := NewExecTool(tmpDir, false)
	tool.SetRestrictToWorkspace(true)

	ctx := context.Background()
	args := map[string]any{
		"command": "cat ../../etc/passwd",
	}

	result := tool.Execute(ctx, args)

	// Path traversal should be blocked
	if !result.IsError {
		t.Errorf("Expected path traversal to be blocked with restrictToWorkspace=true")
	}

	if !strings.Contains(result.ForLLM, "blocked") && !strings.Contains(result.ForUser, "blocked") {
		t.Errorf(
			"Expected 'blocked' message for path traversal, got ForLLM: %s, ForUser: %s",
			result.ForLLM,
			result.ForUser,
		)
	}
}

// TestShellTool_DeniedCommandLogging verifies deny-pattern denial is logged with the matched regex
func TestShellTool_DeniedCommandLogging(t *testing.T) {
	workspace := t.TempDir()
	tool := NewExecTool(workspace, false)

	result := tool.Execute(context.Background(), map[string]any{
		"command": "mkfs /dev/sda",
	})
	if !result.IsError {
		t.Fatalf("expected command to be blocked")
	}

	logFile := filepath.Join(workspace, "state", "denied_commands.jsonl")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}

	var entry DeniedCommandEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("failed to parse log entry: %v", err)
	}

	if entry.Command != "mkfs /dev/sda" {
		t.Errorf("expected command 'mkfs /dev/sda', got %q", entry.Command)
	}
	if !strings.Contains(entry.Reason, "dangerous pattern") {
		t.Errorf("expected reason to mention 'dangerous pattern', got %q", entry.Reason)
	}
	if entry.MatchedPattern == "" {
		t.Errorf("expected matched_pattern to be set")
	}
	if entry.Timestamp == "" {
		t.Errorf("expected timestamp to be set")
	}
}

// TestShellTool_DeniedCommandLogging_WorkspaceRestriction verifies workspace restriction denial is logged
func TestShellTool_DeniedCommandLogging_WorkspaceRestriction(t *testing.T) {
	workspace := t.TempDir()
	tool := NewExecTool(workspace, true)

	result := tool.Execute(context.Background(), map[string]any{
		"command": "cat ../../etc/passwd",
	})
	if !result.IsError {
		t.Fatalf("expected command to be blocked")
	}

	logFile := filepath.Join(workspace, "state", "denied_commands.jsonl")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}

	var entry DeniedCommandEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("failed to parse log entry: %v", err)
	}

	if entry.Command != "cat ../../etc/passwd" {
		t.Errorf("expected command 'cat ../../etc/passwd', got %q", entry.Command)
	}
	if !strings.Contains(entry.Reason, "blocked") {
		t.Errorf("expected reason to contain 'blocked', got %q", entry.Reason)
	}
	if entry.MatchedPattern != "" {
		t.Errorf("expected empty matched_pattern for workspace restriction, got %q", entry.MatchedPattern)
	}
}

// TestShellTool_DeniedCommandLogging_NoWorkspace verifies no panic/crash when workingDir is empty
func TestShellTool_DeniedCommandLogging_NoWorkspace(t *testing.T) {
	tool := NewExecTool("", false)

	result := tool.Execute(context.Background(), map[string]any{
		"command": "mkfs /dev/sda",
	})
	if !result.IsError {
		t.Fatalf("expected command to be blocked")
	}

	// Should not panic and no log file should be created anywhere.
	// The test passes if we reach this point without a panic.
}

// TestIsExecutableInSystemPath_Found verifies that an executable in a PATH directory is recognized.
func TestIsExecutableInSystemPath_Found(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "myexe")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if !isExecutableInSystemPath(exe) {
		t.Error("expected executable in PATH to return true")
	}
}

// TestIsExecutableInSystemPath_NotInPath verifies that an executable NOT in PATH returns false.
func TestIsExecutableInSystemPath_NotInPath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "myexe")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/some/other/dir")

	if isExecutableInSystemPath(exe) {
		t.Error("expected executable not in PATH to return false")
	}
}

// TestIsExecutableInSystemPath_NotExecutable verifies that a non-executable file in a PATH dir returns false (Unix only).
func TestIsExecutableInSystemPath_NotExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("execute-permission check not applicable on Windows")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "noexec")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if isExecutableInSystemPath(file) {
		t.Error("expected non-executable file to return false")
	}
}

// TestIsExecutableInSystemPath_FileDoesNotExist verifies that a nonexistent file returns false.
func TestIsExecutableInSystemPath_FileDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	if isExecutableInSystemPath(filepath.Join(dir, "nonexistent")) {
		t.Error("expected nonexistent file to return false")
	}
}

// TestIsExecutableInSystemPath_EmptyPATH verifies that an empty PATH returns false.
func TestIsExecutableInSystemPath_EmptyPATH(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "myexe")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	if isExecutableInSystemPath(exe) {
		t.Error("expected empty PATH to return false")
	}
}

// TestShellTool_RestrictToWorkspace_AllowsPathExecutable verifies that guardCommand allows
// an absolute path to an executable that resides in a $PATH directory.
func TestShellTool_RestrictToWorkspace_AllowsPathExecutable(t *testing.T) {
	workspace := t.TempDir()
	binDir := t.TempDir()

	exe := filepath.Join(binDir, "mytool")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	tool := NewExecTool(workspace, true)
	guardErr, _ := tool.guardCommand(exe+" --version", workspace)
	if guardErr != "" {
		t.Errorf("expected PATH executable to be allowed, got: %s", guardErr)
	}
}

// TestShellTool_RestrictToWorkspace_BlocksNonPathAbsolutePath verifies that an absolute path
// outside the workspace that is NOT in $PATH is still blocked.
func TestShellTool_RestrictToWorkspace_BlocksNonPathAbsolutePath(t *testing.T) {
	workspace := t.TempDir()
	otherDir := t.TempDir()

	file := filepath.Join(otherDir, "secrets.txt")
	if err := os.WriteFile(file, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/usr/bin:/bin")

	tool := NewExecTool(workspace, true)
	guardErr, _ := tool.guardCommand("cat "+file, workspace)
	if guardErr == "" {
		t.Error("expected non-PATH outside-workspace path to be blocked")
	}
}

// TestShellTool_RestrictToWorkspace_WorkspacePathStillAllowed is a regression test
// ensuring paths within the workspace continue to be allowed.
func TestShellTool_RestrictToWorkspace_WorkspacePathStillAllowed(t *testing.T) {
	workspace := t.TempDir()
	file := filepath.Join(workspace, "myfile.txt")
	if err := os.WriteFile(file, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewExecTool(workspace, true)
	guardErr, _ := tool.guardCommand("cat "+file, workspace)
	if guardErr != "" {
		t.Errorf("expected workspace path to be allowed, got: %s", guardErr)
	}
}

// TestShellTool_RestrictToWorkspace_OrgRepoNotBlocked verifies that org/repo
// arguments (e.g. gh repo clone org/repo) are not incorrectly flagged as
// filesystem paths outside the working directory.
func TestShellTool_RestrictToWorkspace_OrgRepoNotBlocked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}

	workspace := t.TempDir()
	binDir := t.TempDir()
	gh := filepath.Join(binDir, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\necho ok"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	tool := NewExecTool(workspace, true)
	guardErr, _ := tool.guardCommand(gh+" repo clone org/repo", workspace)
	if guardErr != "" {
		t.Errorf("expected org/repo to be allowed, got: %s", guardErr)
	}
}

// TestShellTool_RestrictToWorkspace_RedirectOutsideBlocked verifies that
// absolute paths after shell operators (e.g. > /tmp/outside) are still caught.
func TestShellTool_RestrictToWorkspace_RedirectOutsideBlocked(t *testing.T) {
	workspace := t.TempDir()
	tool := NewExecTool(workspace, true)
	guardErr, _ := tool.guardCommand("echo foo > /tmp/outside", workspace)
	if guardErr == "" {
		t.Error("expected redirect to /tmp/outside to be blocked")
	}
}

// TestExecTool_AddDenyPattern verifies adding a custom deny pattern.
func TestExecTool_AddDenyPattern(t *testing.T) {
	tool := NewExecTool("", false)
	if err := tool.AddDenyPattern(`\bmy_dangerous_cmd\b`); err != nil {
		t.Fatalf("AddDenyPattern failed: %v", err)
	}

	patterns := tool.ListDenyPatterns()
	found := false
	for _, p := range patterns {
		if p.Pattern == `\bmy_dangerous_cmd\b` {
			if p.IsDefault {
				t.Error("expected custom pattern to have IsDefault=false")
			}
			found = true
		}
	}
	if !found {
		t.Error("expected custom deny pattern to appear in ListDenyPatterns")
	}

	// Verify the pattern actually blocks commands
	guardErr, _ := tool.guardCommand("my_dangerous_cmd --flag", "")
	if guardErr == "" {
		t.Error("expected custom deny pattern to block the command")
	}
}

// TestExecTool_AddDenyPattern_InvalidRegex verifies that an invalid regex returns an error.
func TestExecTool_AddDenyPattern_InvalidRegex(t *testing.T) {
	tool := NewExecTool("", false)
	err := tool.AddDenyPattern(`[invalid`)
	if err == nil {
		t.Error("expected error for invalid regex pattern")
	}
}

// TestExecTool_RemoveDenyPattern verifies removing custom patterns works and defaults are immutable.
func TestExecTool_RemoveDenyPattern(t *testing.T) {
	tool := NewExecTool("", false)

	// Add then remove a custom pattern
	if err := tool.AddDenyPattern(`\bcustom_bad\b`); err != nil {
		t.Fatal(err)
	}
	if !tool.RemoveDenyPattern(`\bcustom_bad\b`) {
		t.Error("expected RemoveDenyPattern to return true for custom pattern")
	}

	// Verify it's gone
	for _, p := range tool.ListDenyPatterns() {
		if p.Pattern == `\bcustom_bad\b` {
			t.Error("expected custom pattern to be removed")
		}
	}

	// Try to remove a default pattern — should return false
	defaults := tool.ListDenyPatterns()
	if len(defaults) == 0 {
		t.Fatal("expected at least one default pattern")
	}
	if tool.RemoveDenyPattern(defaults[0].Pattern) {
		t.Error("expected RemoveDenyPattern to return false for default pattern")
	}
}

// TestExecTool_AddRemoveAllowPattern verifies add, list, and remove for allow patterns.
func TestExecTool_AddRemoveAllowPattern(t *testing.T) {
	tool := NewExecTool("", false)

	if err := tool.AddAllowPattern(`^echo\b`); err != nil {
		t.Fatal(err)
	}
	if err := tool.AddAllowPattern(`^ls\b`); err != nil {
		t.Fatal(err)
	}

	patterns := tool.ListAllowPatterns()
	if len(patterns) != 2 {
		t.Fatalf("expected 2 allow patterns, got %d", len(patterns))
	}

	if !tool.RemoveAllowPattern(`^echo\b`) {
		t.Error("expected RemoveAllowPattern to return true")
	}

	patterns = tool.ListAllowPatterns()
	if len(patterns) != 1 {
		t.Fatalf("expected 1 allow pattern after removal, got %d", len(patterns))
	}
	if patterns[0] != `^ls\b` {
		t.Errorf("expected remaining pattern to be '^ls\\b', got %q", patterns[0])
	}

	// Remove nonexistent
	if tool.RemoveAllowPattern(`^nonexistent$`) {
		t.Error("expected RemoveAllowPattern to return false for nonexistent pattern")
	}
}

// TestExecTool_CustomDenyExtendsDefaults verifies that config custom deny patterns
// are additive to the built-in defaults (not replacing them).
func TestExecTool_CustomDenyExtendsDefaults(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Exec.EnableDenyPatterns = true
	cfg.Tools.Exec.CustomDenyPatterns = []string{`\bmy_extra_deny\b`}

	tool := NewExecToolWithConfig("", false, cfg)

	patterns := tool.ListDenyPatterns()
	hasDefault := false
	hasCustom := false
	for _, p := range patterns {
		if p.IsDefault {
			hasDefault = true
		}
		if p.Pattern == `\bmy_extra_deny\b` && !p.IsDefault {
			hasCustom = true
		}
	}
	if !hasDefault {
		t.Error("expected default deny patterns to be present")
	}
	if !hasCustom {
		t.Error("expected custom deny pattern to be present alongside defaults")
	}

	// Verify built-in pattern still blocks
	guardErr, _ := tool.guardCommand("mkfs /dev/sda", "")
	if guardErr == "" {
		t.Error("expected built-in deny pattern to still block 'mkfs'")
	}

	// Verify custom pattern also blocks
	guardErr, _ = tool.guardCommand("my_extra_deny", "")
	if guardErr == "" {
		t.Error("expected custom deny pattern to block 'my_extra_deny'")
	}
}

// TestExecTool_GetDeniedCommands verifies that a blocked command appears in the log.
func TestExecTool_GetDeniedCommands(t *testing.T) {
	workspace := t.TempDir()
	tool := NewExecTool(workspace, false)

	// Trigger a denied command
	tool.Execute(context.Background(), map[string]any{"command": "mkfs /dev/sda"})

	entries, err := tool.GetDeniedCommands(nil)
	if err != nil {
		t.Fatalf("GetDeniedCommands error: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one denied command entry")
	}
	if entries[0].Command != "mkfs /dev/sda" {
		t.Errorf("expected command 'mkfs /dev/sda', got %q", entries[0].Command)
	}
}

// TestExecTool_GetDeniedCommands_WithLimit verifies limit/offset pagination.
func TestExecTool_GetDeniedCommands_WithLimit(t *testing.T) {
	workspace := t.TempDir()
	tool := NewExecTool(workspace, false)

	// Trigger multiple denied commands
	tool.Execute(context.Background(), map[string]any{"command": "mkfs /dev/sda"})
	tool.Execute(context.Background(), map[string]any{"command": "mkfs /dev/sdb"})
	tool.Execute(context.Background(), map[string]any{"command": "mkfs /dev/sdc"})

	// Limit to 2
	entries, err := tool.GetDeniedCommands(&DeniedCommandsOptions{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries with limit=2, got %d", len(entries))
	}

	// Offset 1, limit 1
	entries, err = tool.GetDeniedCommands(&DeniedCommandsOptions{Offset: 1, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry with offset=1,limit=1, got %d", len(entries))
	}
	if entries[0].Command != "mkfs /dev/sdb" {
		t.Errorf("expected second command 'mkfs /dev/sdb', got %q", entries[0].Command)
	}

	// Offset beyond entries
	entries, err = tool.GetDeniedCommands(&DeniedCommandsOptions{Offset: 100})
	if err != nil {
		t.Fatal(err)
	}
	if entries != nil {
		t.Errorf("expected nil for offset beyond entries, got %d entries", len(entries))
	}
}

// TestExecTool_GetDeniedCommands_NoWorkspace verifies empty workspace returns nil.
func TestExecTool_GetDeniedCommands_NoWorkspace(t *testing.T) {
	tool := NewExecTool("", false)
	entries, err := tool.GetDeniedCommands(nil)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil entries for empty workspace, got %d", len(entries))
	}
}

// TestExecTool_DenyPatternsEnabled verifies toggling deny patterns on/off.
func TestExecTool_DenyPatternsEnabled(t *testing.T) {
	tool := NewExecTool("", false)

	if !tool.DenyPatternsEnabled() {
		t.Error("expected deny patterns to be enabled by default")
	}

	// Command should be blocked with deny enabled
	guardErr, _ := tool.guardCommand("mkfs /dev/sda", "")
	if guardErr == "" {
		t.Error("expected command to be blocked when deny is enabled")
	}

	// Disable deny patterns
	tool.SetDenyPatternsEnabled(false)
	if tool.DenyPatternsEnabled() {
		t.Error("expected deny patterns to be disabled")
	}

	// Command should be allowed now
	guardErr, _ = tool.guardCommand("mkfs /dev/sda", "")
	if guardErr != "" {
		t.Errorf("expected command to pass when deny is disabled, got: %s", guardErr)
	}

	// Re-enable
	tool.SetDenyPatternsEnabled(true)
	guardErr, _ = tool.guardCommand("mkfs /dev/sda", "")
	if guardErr == "" {
		t.Error("expected command to be blocked again after re-enabling")
	}
}

// TestExecTool_SudoNotBlockedByDefault verifies sudo is no longer in the default deny list.
func TestExecTool_SudoNotBlockedByDefault(t *testing.T) {
	tool := NewExecTool("", false)

	guardErr, _ := tool.guardCommand("sudo ls", "")
	if guardErr != "" {
		t.Errorf("expected sudo to be allowed by default, got: %s", guardErr)
	}
}

// TestExecTool_AllowlistBypassesDenylist verifies that an allowlisted command
// is not blocked by deny patterns (allowlist takes precedence).
func TestExecTool_AllowlistBypassesDenylist(t *testing.T) {
	tool := NewExecTool("", false)

	// "apt install" is blocked by a default deny pattern
	guardErr, _ := tool.guardCommand("apt install vim", "")
	if guardErr == "" {
		t.Fatal("expected 'apt install' to be blocked by default deny pattern")
	}

	// Now add an allow pattern that matches "apt install"
	if err := tool.SetAllowPatterns([]string{`\bapt\s+install\b`}); err != nil {
		t.Fatal(err)
	}

	// The same command should now be allowed because allowlist has priority
	guardErr, _ = tool.guardCommand("apt install vim", "")
	if guardErr != "" {
		t.Errorf("expected allowlisted command to bypass deny patterns, got: %s", guardErr)
	}

	// A command not in the allowlist should still be blocked (allowlist mode)
	guardErr, _ = tool.guardCommand("echo hello", "")
	if guardErr == "" {
		t.Error("expected command not in allowlist to be blocked in allowlist mode")
	}
}

// TestShellTool_GuardCommand_AllowsHomePaths verifies that guardCommand allows
// absolute paths within the home (second allowed) directory.
func TestShellTool_GuardCommand_AllowsHomePaths(t *testing.T) {
	workspace := t.TempDir()
	homeDir := t.TempDir()
	homeFile := filepath.Join(homeDir, "config.txt")
	os.WriteFile(homeFile, []byte("data"), 0o644)

	tool := NewExecToolWithDirs(workspace, []string{workspace, homeDir}, true, nil)
	guardErr, _ := tool.guardCommand("cat "+homeFile, workspace)
	if guardErr != "" {
		t.Errorf("expected home dir path to be allowed, got: %s", guardErr)
	}
}

// TestShellTool_GuardCommand_BlocksOutsideBothDirs verifies that guardCommand blocks
// absolute paths outside both workspace and home directories.
func TestShellTool_GuardCommand_BlocksOutsideBothDirs(t *testing.T) {
	workspace := t.TempDir()
	homeDir := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	os.WriteFile(outsideFile, []byte("secret"), 0o644)

	tool := NewExecToolWithDirs(workspace, []string{workspace, homeDir}, true, nil)
	guardErr, _ := tool.guardCommand("cat "+outsideFile, workspace)
	if guardErr == "" {
		t.Error("expected path outside both dirs to be blocked")
	}
}

// TestShellTool_GuardCommand_AllowsAPIPath verifies that API-style paths
// (e.g. /repos/org/name/...) are not incorrectly blocked as filesystem paths.
func TestShellTool_GuardCommand_AllowsAPIPath(t *testing.T) {
	workspace := t.TempDir()
	binDir := t.TempDir()
	gh := filepath.Join(binDir, "gh")
	os.WriteFile(gh, []byte("#!/bin/sh\necho ok"), 0o755)
	t.Setenv("PATH", binDir)

	tool := NewExecTool(workspace, true)
	guardErr, _ := tool.guardCommand(
		gh+` api -H "Accept: application/vnd.github+json" /repos/my-org/my-repo/dependabot/alerts -f state=open`,
		workspace,
	)
	if guardErr != "" {
		t.Errorf("expected API path to be allowed, got: %s", guardErr)
	}
}

// TestExecTool_AllowPatternsFromConfig verifies allow patterns loaded from config.
func TestExecTool_AllowPatternsFromConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Tools.Exec.EnableDenyPatterns = true
	cfg.Tools.Exec.CustomAllowPatterns = []string{`^echo\b`, `^ls\b`}

	tool := NewExecToolWithConfig("", false, cfg)

	patterns := tool.ListAllowPatterns()
	if len(patterns) != 2 {
		t.Fatalf("expected 2 allow patterns from config, got %d", len(patterns))
	}
	if patterns[0] != `^echo\b` {
		t.Errorf("expected first pattern '^echo\\b', got %q", patterns[0])
	}
	if patterns[1] != `^ls\b` {
		t.Errorf("expected second pattern '^ls\\b', got %q", patterns[1])
	}
}
