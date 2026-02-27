package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

// DenyPatternInfo describes a deny pattern and whether it is a built-in default.
type DenyPatternInfo struct {
	Pattern   string
	IsDefault bool
}

// DeniedCommandsOptions controls pagination for GetDeniedCommands.
type DeniedCommandsOptions struct {
	Limit  int
	Offset int
}

// DeniedCommandEntry records a single denied command in the JSONL log.
type DeniedCommandEntry struct {
	Timestamp      string `json:"timestamp"`
	Command        string `json:"command"`
	Reason         string `json:"reason"`
	MatchedPattern string `json:"matched_pattern,omitempty"`
	WorkingDir     string `json:"working_dir,omitempty"`
}

type ExecTool struct {
	workingDir          string
	allowedDirs         []string
	timeout             time.Duration
	defaultDenyPatterns []*regexp.Regexp // immutable built-in defaults
	customDenyPatterns  []*regexp.Regexp // user-added deny patterns
	allowPatterns       []*regexp.Regexp
	denyEnabled         bool
	restrictToWorkspace bool
}

var defaultDenyPatterns = []*regexp.Regexp{
	regexp.MustCompile(`\bdel\s+/[fq]\b`),
	regexp.MustCompile(`\brmdir\s+/s\b`),
	regexp.MustCompile(`\b(format|mkfs|diskpart)\b\s`), // Match disk wiping commands (must be followed by space/args)
	regexp.MustCompile(`\bdd\s+if=`),
	regexp.MustCompile(`>\s*/dev/sd[a-z]\b`), // Block writes to disk devices (but allow /dev/null)
	regexp.MustCompile(`\b(shutdown|reboot|poweroff)\b`),
	regexp.MustCompile(`:\(\)\s*\{.*\};\s*:`),
	regexp.MustCompile(`\$\([^)]+\)`),
	regexp.MustCompile(`\$\{[^}]+\}`),
	regexp.MustCompile("`[^`]+`"),
	regexp.MustCompile(`\|\s*sh\b`),
	regexp.MustCompile(`\|\s*bash\b`),
	regexp.MustCompile(`>\s*/dev/null\s*>&?\s*\d?`),
	regexp.MustCompile(`<<\s*EOF`),
	regexp.MustCompile(`\$\(\s*cat\s+`),
	regexp.MustCompile(`\$\(\s*curl\s+`),
	regexp.MustCompile(`\$\(\s*wget\s+`),
	regexp.MustCompile(`\$\(\s*which\s+`),
	regexp.MustCompile(`\bchmod\s+[0-7]{3,4}\b`),
	regexp.MustCompile(`\bchown\b`),
	regexp.MustCompile(`\bpkill\b`),
	regexp.MustCompile(`\bkillall\b`),
	regexp.MustCompile(`\bkill\s+-[9]\b`),
	regexp.MustCompile(`\bcurl\b.*\|\s*(sh|bash)`),
	regexp.MustCompile(`\bwget\b.*\|\s*(sh|bash)`),
	regexp.MustCompile(`\bnpm\s+install\s+-g\b`),
	regexp.MustCompile(`\bpip\s+install\s+--user\b`),
	regexp.MustCompile(`\bapt\s+(install|remove|purge)\b`),
	regexp.MustCompile(`\byum\s+(install|remove)\b`),
	regexp.MustCompile(`\bdnf\s+(install|remove)\b`),
	regexp.MustCompile(`\bdocker\s+run\b`),
	regexp.MustCompile(`\bdocker\s+exec\b`),
	regexp.MustCompile(`\bgit\s+.*force\b`),
	regexp.MustCompile(`\bssh\b.*@`),
	regexp.MustCompile(`\beval\b`),
	regexp.MustCompile(`\bsource\s+.*\.sh\b`),
}

func NewExecTool(workingDir string, restrict bool) *ExecTool {
	return NewExecToolWithConfig(workingDir, restrict, nil)
}

func NewExecToolWithDirs(workingDir string, allowedDirs []string, restrict bool, cfg *config.Config) *ExecTool {
	tool := NewExecToolWithConfig(workingDir, restrict, cfg)
	tool.allowedDirs = allowedDirs
	return tool
}

func NewExecToolWithConfig(workingDir string, restrict bool, cfg *config.Config) *ExecTool {
	tool := &ExecTool{
		workingDir:          workingDir,
		allowedDirs:         []string{workingDir},
		timeout:             60 * time.Second,
		denyEnabled:         true,
		restrictToWorkspace: restrict,
	}

	if cfg != nil {
		execConfig := cfg.Tools.Exec
		tool.denyEnabled = execConfig.EnableDenyPatterns

		if tool.denyEnabled {
			// Always include built-in defaults
			tool.defaultDenyPatterns = make([]*regexp.Regexp, len(defaultDenyPatterns))
			copy(tool.defaultDenyPatterns, defaultDenyPatterns)

			// Custom deny patterns are additive
			for _, pattern := range execConfig.CustomDenyPatterns {
				re, err := regexp.Compile(pattern)
				if err != nil {
					fmt.Printf("Invalid custom deny pattern %q: %v\n", pattern, err)
					continue
				}
				tool.customDenyPatterns = append(tool.customDenyPatterns, re)
			}
		} else {
			fmt.Println("Warning: deny patterns are disabled. All commands will be allowed.")
		}

		// Compile custom allow patterns from config
		for _, pattern := range execConfig.CustomAllowPatterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				fmt.Printf("Invalid custom allow pattern %q: %v\n", pattern, err)
				continue
			}
			tool.allowPatterns = append(tool.allowPatterns, re)
		}
	} else {
		// No config: use defaults with deny enabled
		tool.defaultDenyPatterns = make([]*regexp.Regexp, len(defaultDenyPatterns))
		copy(tool.defaultDenyPatterns, defaultDenyPatterns)
	}

	return tool
}

func (t *ExecTool) Name() string {
	return "exec"
}

func (t *ExecTool) Description() string {
	return "Execute a shell command and return its output. Use with caution."
}

func (t *ExecTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The shell command to execute",
			},
			"working_dir": map[string]any{
				"type":        "string",
				"description": "Optional working directory for the command",
			},
		},
		"required": []string{"command"},
	}
}

func (t *ExecTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	command, ok := args["command"].(string)
	if !ok {
		return ErrorResult("command is required")
	}

	cwd := t.workingDir
	if wd, ok := args["working_dir"].(string); ok && wd != "" {
		if t.restrictToWorkspace && t.workingDir != "" {
			resolvedWD, err := validatePath(wd, t.allowedDirs, true)
			if err != nil {
				reason := "Command blocked by safety guard (" + err.Error() + ")"
				t.logDeniedCommand(command, reason, "", wd)
				return ErrorResult(reason)
			}
			cwd = resolvedWD
		} else {
			cwd = wd
		}
	}

	if cwd == "" {
		wd, err := os.Getwd()
		if err == nil {
			cwd = wd
		}
	}

	if guardError, matchedPattern := t.guardCommand(command, cwd); guardError != "" {
		t.logDeniedCommand(command, guardError, matchedPattern, cwd)
		return ErrorResult(guardError)
	}

	// timeout == 0 means no timeout
	var cmdCtx context.Context
	var cancel context.CancelFunc
	if t.timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, t.timeout)
	} else {
		cmdCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(cmdCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	} else {
		cmd = exec.CommandContext(cmdCtx, "sh", "-c", command)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}

	prepareCommandForTermination(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return ErrorResult(fmt.Sprintf("failed to start command: %v", err))
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var err error
	select {
	case err = <-done:
	case <-cmdCtx.Done():
		_ = terminateProcessTree(cmd)
		select {
		case err = <-done:
		case <-time.After(2 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			err = <-done
		}
	}

	output := stdout.String()
	if stderr.Len() > 0 {
		output += "\nSTDERR:\n" + stderr.String()
	}

	if err != nil {
		if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
			msg := fmt.Sprintf("Command timed out after %v", t.timeout)
			return &ToolResult{
				ForLLM:  msg,
				ForUser: msg,
				IsError: true,
			}
		}
		output += fmt.Sprintf("\nExit code: %v", err)
	}

	if output == "" {
		output = "(no output)"
	}

	maxLen := 10000
	if len(output) > maxLen {
		output = output[:maxLen] + fmt.Sprintf("\n... (truncated, %d more chars)", len(output)-maxLen)
	}

	if err != nil {
		return &ToolResult{
			ForLLM:  output,
			ForUser: output,
			IsError: true,
		}
	}

	return &ToolResult{
		ForLLM:  output,
		ForUser: output,
		IsError: false,
	}
}

func (t *ExecTool) guardCommand(command, cwd string) (string, string) {
	cmd := strings.TrimSpace(command)
	lower := strings.ToLower(cmd)

	// Check allowlist first: if an allowlist is configured and the command
	// matches, skip deny-pattern checks entirely (allowlist has priority).
	if len(t.allowPatterns) > 0 {
		allowed := false
		for _, pattern := range t.allowPatterns {
			if pattern.MatchString(lower) {
				allowed = true
				break
			}
		}
		if allowed {
			// Explicitly allowlisted — bypass deny patterns
			goto postDeny
		}
		// Allowlist is configured but command didn't match — block
		return "Command blocked by safety guard (not in allowlist)", "(allowlist mode)"
	}

	if t.denyEnabled {
		for _, pattern := range t.defaultDenyPatterns {
			if pattern.MatchString(lower) {
				return "Command blocked by safety guard (dangerous pattern detected)", pattern.String()
			}
		}
		for _, pattern := range t.customDenyPatterns {
			if pattern.MatchString(lower) {
				return "Command blocked by safety guard (dangerous pattern detected)", pattern.String()
			}
		}
	}

postDeny:

	if t.restrictToWorkspace {
		if strings.Contains(cmd, "..\\") || strings.Contains(cmd, "../") {
			return "Command blocked by safety guard (path traversal detected)", ""
		}

		cwdPath, err := filepath.Abs(cwd)
		if err != nil {
			return "", ""
		}

		pathPattern := regexp.MustCompile(`(?:^|[\s=><|;(])(/[^\s\"']+)|([A-Za-z]:\\[^\\\"']+)`)
		submatches := pathPattern.FindAllStringSubmatch(cmd, -1)

		var matches []string
		for _, sm := range submatches {
			if sm[1] != "" {
				matches = append(matches, sm[1])
			} else if sm[2] != "" {
				matches = append(matches, sm[2])
			}
		}

		for _, raw := range matches {
			p, err := filepath.Abs(raw)
			if err != nil {
				continue
			}

			rel, err := filepath.Rel(cwdPath, p)
			if err != nil {
				continue
			}

			if strings.HasPrefix(rel, "..") {
				if isWithinAnyAllowedDir(p, t.allowedDirs) {
					continue
				}
				if isExecutableInSystemPath(p) {
					continue
				}
				if !looksLikeFilesystemPath(raw) {
					continue
				}
				return "Command blocked by safety guard (path outside working dir)", ""
			}
		}
	}

	return "", ""
}

// looksLikeFilesystemPath heuristically checks whether a string that starts with /
// is likely intended as a filesystem path rather than a URL path, API route, etc.
// Returns false for strings that look like API paths (e.g. /repos/org/name/...).
func looksLikeFilesystemPath(raw string) bool {
	// If the path or its parent exists on disk, it's definitely a filesystem path
	if _, err := os.Stat(raw); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Dir(raw)); err == nil {
		return true
	}
	// Common filesystem root prefixes always count as filesystem paths
	fsRoots := []string{"/home/", "/tmp/", "/etc/", "/var/", "/usr/", "/opt/", "/root/", "/dev/", "/proc/", "/sys/", "/mnt/", "/media/", "/srv/", "/run/", "/boot/"}
	for _, root := range fsRoots {
		if strings.HasPrefix(raw, root) {
			return true
		}
	}
	// Otherwise assume it's an API path / URL path argument
	return false
}

// isExecutableInSystemPath checks whether absPath is an executable file
// located in one of the directories listed in $PATH.
func isExecutableInSystemPath(absPath string) bool {
	dir := filepath.Clean(filepath.Dir(absPath))

	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		return false
	}

	inPath := false
	for _, d := range strings.Split(pathEnv, string(os.PathListSeparator)) {
		if filepath.Clean(d) == dir {
			inPath = true
			break
		}
	}
	if !inPath {
		return false
	}

	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() {
		return false
	}

	if runtime.GOOS != "windows" {
		return info.Mode()&0111 != 0
	}
	return true
}

func (t *ExecTool) SetTimeout(timeout time.Duration) {
	t.timeout = timeout
}

func (t *ExecTool) SetRestrictToWorkspace(restrict bool) {
	t.restrictToWorkspace = restrict
}

func (t *ExecTool) SetAllowPatterns(patterns []string) error {
	t.allowPatterns = make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("invalid allow pattern %q: %w", p, err)
		}
		t.allowPatterns = append(t.allowPatterns, re)
	}
	return nil
}

// AddDenyPattern compiles and appends a custom deny regex pattern.
func (t *ExecTool) AddDenyPattern(pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid deny pattern %q: %w", pattern, err)
	}
	t.customDenyPatterns = append(t.customDenyPatterns, re)
	return nil
}

// RemoveDenyPattern removes a custom deny pattern by its string representation.
// Returns true if the pattern was found and removed. Built-in default patterns
// cannot be removed.
func (t *ExecTool) RemoveDenyPattern(pattern string) bool {
	for i, re := range t.customDenyPatterns {
		if re.String() == pattern {
			t.customDenyPatterns = append(t.customDenyPatterns[:i], t.customDenyPatterns[i+1:]...)
			return true
		}
	}
	return false
}

// ListDenyPatterns returns all deny patterns with a flag indicating whether
// each is a built-in default or a custom pattern.
func (t *ExecTool) ListDenyPatterns() []DenyPatternInfo {
	result := make([]DenyPatternInfo, 0, len(t.defaultDenyPatterns)+len(t.customDenyPatterns))
	for _, re := range t.defaultDenyPatterns {
		result = append(result, DenyPatternInfo{Pattern: re.String(), IsDefault: true})
	}
	for _, re := range t.customDenyPatterns {
		result = append(result, DenyPatternInfo{Pattern: re.String(), IsDefault: false})
	}
	return result
}

// AddAllowPattern compiles and appends an allow regex pattern.
func (t *ExecTool) AddAllowPattern(pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid allow pattern %q: %w", pattern, err)
	}
	t.allowPatterns = append(t.allowPatterns, re)
	return nil
}

// RemoveAllowPattern removes an allow pattern by its string representation.
// Returns true if the pattern was found and removed.
func (t *ExecTool) RemoveAllowPattern(pattern string) bool {
	for i, re := range t.allowPatterns {
		if re.String() == pattern {
			t.allowPatterns = append(t.allowPatterns[:i], t.allowPatterns[i+1:]...)
			return true
		}
	}
	return false
}

// ListAllowPatterns returns all allow pattern strings.
func (t *ExecTool) ListAllowPatterns() []string {
	result := make([]string, 0, len(t.allowPatterns))
	for _, re := range t.allowPatterns {
		result = append(result, re.String())
	}
	return result
}

// DenyPatternsEnabled returns whether deny pattern checking is active.
func (t *ExecTool) DenyPatternsEnabled() bool {
	return t.denyEnabled
}

// SetDenyPatternsEnabled toggles deny pattern checking at runtime.
func (t *ExecTool) SetDenyPatternsEnabled(enabled bool) {
	t.denyEnabled = enabled
}

// GetDeniedCommands reads the denied commands JSONL log and returns entries
// with optional limit/offset pagination. Returns nil if no workspace is set
// or the log file does not exist.
func (t *ExecTool) GetDeniedCommands(opts *DeniedCommandsOptions) ([]DeniedCommandEntry, error) {
	if t.workingDir == "" {
		return nil, nil
	}

	logPath := filepath.Join(t.workingDir, "state", "denied_commands.jsonl")
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var entries []DeniedCommandEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var entry DeniedCommandEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if opts != nil {
		if opts.Offset > 0 {
			if opts.Offset >= len(entries) {
				return nil, nil
			}
			entries = entries[opts.Offset:]
		}
		if opts.Limit > 0 && opts.Limit < len(entries) {
			entries = entries[:opts.Limit]
		}
	}

	return entries, nil
}

func (t *ExecTool) logDeniedCommand(command, reason, matchedPattern, workDir string) {
	if t.workingDir == "" {
		return
	}

	entry := DeniedCommandEntry{
		Timestamp:      time.Now().UTC().Format(time.RFC3339),
		Command:        command,
		Reason:         reason,
		MatchedPattern: matchedPattern,
		WorkingDir:     workDir,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	data = append(data, '\n')

	stateDir := filepath.Join(t.workingDir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return
	}

	f, err := os.OpenFile(filepath.Join(stateDir, "denied_commands.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	_, _ = f.Write(data)
}
