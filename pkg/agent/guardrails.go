package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// LoopSignal indicates what action the caller should take after recording a tool call.
type LoopSignal int

const (
	LoopSignalNone    LoopSignal = iota // No issue detected
	LoopSignalWarning                   // Possible loop, log a warning
	LoopSignalBlock                     // Likely loop, inject a nudge message
	LoopSignalAbort                     // Circuit breaker tripped, abort run
)

// VerbosityLevel controls how much detail is included in error/timeout summaries.
type VerbosityLevel string

const (
	VerbosityOff  VerbosityLevel = "off"
	VerbosityOn   VerbosityLevel = "on"
	VerbosityFull VerbosityLevel = "full"
)

// pollToolNames lists tools whose output should be tracked for no-progress detection.
var pollToolNames = map[string]bool{
	"exec":       true,
	"read_file":  true,
	"list_dir":   true,
	"web_fetch":  true,
	"web_search": true,
}

// loopHistoryEntry records a single tool invocation for the rolling window.
type loopHistoryEntry struct {
	Tool       string
	ArgsKey    string
	OutputHash string
}

// LoopDetector tracks tool call patterns within a single agent run to detect
// repetitive loops that waste tokens without making progress.
type LoopDetector struct {
	cfg               config.LoopDetectionConfig
	history           []loopHistoryEntry
	patternCounts     map[string]int // key: "tool:argsKey"
	pollOutputCounts  map[string]int // key: "tool:argsKey:outputHash" — same-output repetitions
	lastTool          string
	prevLastTool      string
	pingPongCounts    map[string]int // key: "toolA->toolB"
	detectedLoopCount int
}

// NewLoopDetector creates a LoopDetector from config. Returns nil if disabled.
func NewLoopDetector(cfg config.LoopDetectionConfig) *LoopDetector {
	if !cfg.Enabled {
		return nil
	}
	return &LoopDetector{
		cfg:              cfg,
		history:          make([]loopHistoryEntry, 0, cfg.HistorySize),
		patternCounts:    make(map[string]int),
		pollOutputCounts: make(map[string]int),
		pingPongCounts:   make(map[string]int),
	}
}

// Record processes a tool call and returns a signal indicating the severity of any detected loop.
func (ld *LoopDetector) Record(toolName string, args map[string]any, output string) LoopSignal {
	if ld == nil {
		return LoopSignalNone
	}

	argsKey := normalizeArgs(args)
	outputHash := hashOutput(output)

	entry := loopHistoryEntry{
		Tool:       toolName,
		ArgsKey:    argsKey,
		OutputHash: outputHash,
	}

	// Maintain rolling window
	ld.history = append(ld.history, entry)
	if len(ld.history) > ld.cfg.HistorySize {
		ld.history = ld.history[1:]
	}

	worst := LoopSignalNone

	// 1. Generic repeat detector
	if ld.cfg.Detectors.GenericRepeat {
		if sig := ld.detectGenericRepeat(toolName, argsKey); sig > worst {
			worst = sig
		}
	}

	// 2. Known poll no-progress detector
	if ld.cfg.Detectors.KnownPollNoProgress {
		if sig := ld.detectPollNoProgress(toolName, argsKey, outputHash); sig > worst {
			worst = sig
		}
	}

	// 3. Ping-pong detector
	if ld.cfg.Detectors.PingPong {
		if sig := ld.detectPingPong(toolName); sig > worst {
			worst = sig
		}
	}

	// Update tool tracking for ping-pong (after detection so current call doesn't affect it)
	ld.prevLastTool = ld.lastTool
	ld.lastTool = toolName

	return worst
}

// detectGenericRepeat checks if the same (tool, args) tuple has been called too many times.
func (ld *LoopDetector) detectGenericRepeat(toolName, argsKey string) LoopSignal {
	key := toolName + ":" + argsKey
	ld.patternCounts[key]++
	count := ld.patternCounts[key]

	if count >= ld.cfg.GlobalCircuitBreakerThreshold {
		ld.detectedLoopCount++
		logger.WarnCF("guardrails", "Circuit breaker: generic repeat", map[string]any{
			"tool": toolName, "count": count,
		})
		return LoopSignalAbort
	}
	if count >= ld.cfg.CriticalThreshold {
		ld.detectedLoopCount++
		logger.WarnCF("guardrails", "Critical: generic repeat", map[string]any{
			"tool": toolName, "count": count,
		})
		return LoopSignalBlock
	}
	if count >= ld.cfg.WarningThreshold {
		logger.WarnCF("guardrails", "Warning: generic repeat", map[string]any{
			"tool": toolName, "count": count,
		})
		return LoopSignalWarning
	}
	return LoopSignalNone
}

// detectPollNoProgress checks if a poll-like tool keeps returning the same output.
func (ld *LoopDetector) detectPollNoProgress(toolName, argsKey, outputHash string) LoopSignal {
	if !pollToolNames[toolName] {
		return LoopSignalNone
	}

	sameKey := toolName + ":" + argsKey + ":" + outputHash
	diffKey := toolName + ":" + argsKey + ":*"

	// Check if output changed compared to last same-tool call
	prevHash := ""
	for i := len(ld.history) - 2; i >= 0; i-- {
		if ld.history[i].Tool == toolName && ld.history[i].ArgsKey == argsKey {
			prevHash = ld.history[i].OutputHash
			break
		}
	}

	if prevHash != "" && prevHash != outputHash {
		// Output changed — reset the same-output counter
		delete(ld.pollOutputCounts, sameKey)
		ld.pollOutputCounts[diffKey] = 0
		return LoopSignalNone
	}

	ld.pollOutputCounts[sameKey]++
	count := ld.pollOutputCounts[sameKey]

	if count >= ld.cfg.GlobalCircuitBreakerThreshold {
		ld.detectedLoopCount++
		return LoopSignalAbort
	}
	if count >= ld.cfg.CriticalThreshold {
		ld.detectedLoopCount++
		return LoopSignalBlock
	}
	if count >= ld.cfg.WarningThreshold {
		return LoopSignalWarning
	}
	return LoopSignalNone
}

// detectPingPong checks for A→B→A→B alternation patterns.
func (ld *LoopDetector) detectPingPong(toolName string) LoopSignal {
	if ld.lastTool == "" || ld.prevLastTool == "" {
		return LoopSignalNone
	}

	// Detect A→B→A pattern (current = A, last = B, prevLast = A)
	if toolName == ld.prevLastTool && toolName != ld.lastTool {
		key := ld.prevLastTool + "->" + ld.lastTool
		ld.pingPongCounts[key]++
		count := ld.pingPongCounts[key]

		if count >= ld.cfg.GlobalCircuitBreakerThreshold/2 {
			ld.detectedLoopCount++
			return LoopSignalAbort
		}
		if count >= ld.cfg.CriticalThreshold/2 {
			ld.detectedLoopCount++
			return LoopSignalBlock
		}
		if count >= ld.cfg.WarningThreshold/2 {
			return LoopSignalWarning
		}
	}

	return LoopSignalNone
}

// normalizeArgs produces a deterministic string representation of tool arguments.
func normalizeArgs(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ordered := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		ordered = append(ordered, k, args[k])
	}

	data, err := json.Marshal(ordered)
	if err != nil {
		return fmt.Sprintf("%v", args)
	}
	return string(data)
}

// hashOutput returns a short hash of tool output for comparison.
func hashOutput(output string) string {
	h := sha256.Sum256([]byte(output))
	return fmt.Sprintf("%x", h[:8])
}

// buildTimeoutSummary constructs a user-facing message when a run times out.
func buildTimeoutSummary(log []toolCallEntry, activeToolName string, elapsed time.Duration, verbosity VerbosityLevel) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("I had to stop after %.0f seconds because the time limit was reached. ", elapsed.Seconds()))

	if len(log) == 0 {
		sb.WriteString("No tool calls were completed before the timeout.")
		return sb.String()
	}

	succeeded := 0
	failed := 0
	for _, e := range log {
		if e.IsError {
			failed++
		} else {
			succeeded++
		}
	}

	sb.WriteString(fmt.Sprintf("I completed %d tool calls (%d succeeded, %d failed) before timing out.", len(log), succeeded, failed))

	if activeToolName != "" {
		sb.WriteString(fmt.Sprintf(" The tool '%s' was running when the timeout hit.", activeToolName))
	}

	if verbosity == VerbosityOn || verbosity == VerbosityFull {
		// Show recent errors
		var recentErrors []toolCallEntry
		for i := len(log) - 1; i >= 0 && len(recentErrors) < 3; i-- {
			if log[i].IsError {
				recentErrors = append(recentErrors, log[i])
			}
		}
		if len(recentErrors) > 0 {
			sb.WriteString("\n\nRecent errors:")
			for _, e := range recentErrors {
				sb.WriteString(fmt.Sprintf("\n- %s: %s", e.Name, e.Content))
			}
		}
	}

	if verbosity == VerbosityFull {
		sb.WriteString("\n\nFull tool call log:")
		for i, e := range log {
			status := "ok"
			if e.IsError {
				status = "ERROR"
			}
			sb.WriteString(fmt.Sprintf("\n  %d. %s [%s]: %s", i+1, e.Name, status, e.Content))
		}
	}

	sb.WriteString("\n\nHow would you like me to proceed?")
	return sb.String()
}

// buildLoopAbortSummary constructs a user-facing message when the loop circuit breaker fires.
func buildLoopAbortSummary(log []toolCallEntry, pattern string) string {
	var sb strings.Builder

	sb.WriteString("I detected a repetitive loop and stopped to avoid wasting resources. ")

	if pattern != "" {
		sb.WriteString(fmt.Sprintf("Pattern: %s. ", pattern))
	}

	if len(log) > 0 {
		succeeded := 0
		failed := 0
		for _, e := range log {
			if e.IsError {
				failed++
			} else {
				succeeded++
			}
		}
		sb.WriteString(fmt.Sprintf("I made %d tool calls (%d succeeded, %d failed) before stopping.", len(log), succeeded, failed))

		// Show the last few unique tools called
		seen := map[string]bool{}
		var lastTools []string
		for i := len(log) - 1; i >= 0 && len(lastTools) < 3; i-- {
			if !seen[log[i].Name] {
				seen[log[i].Name] = true
				lastTools = append(lastTools, log[i].Name)
			}
		}
		if len(lastTools) > 0 {
			sb.WriteString(fmt.Sprintf(" Last tools used: %s.", strings.Join(lastTools, ", ")))
		}
	}

	sb.WriteString("\n\nPlease try rephrasing your request or breaking it into smaller steps.")
	return sb.String()
}

// buildFallbackErrorReply produces a concise error message from the last failing tool call.
func buildFallbackErrorReply(lastErr toolCallEntry) string {
	return fmt.Sprintf("The %s tool failed: %s\n\nPlease check the input and try again.", lastErr.Name, lastErr.Content)
}

// stopPatterns lists keywords that indicate the user wants to abort the current run.
var stopPatterns = []string{
	"stop", "cancel", "abort", "halt",
	"don't do this", "dont do this",
	"stop that", "never mind", "nevermind",
}

// isStopMessage returns true if the message content matches a user stop intent.
func isStopMessage(content string) bool {
	normalized := strings.ToLower(strings.TrimSpace(content))
	if normalized == "" {
		return false
	}
	for _, pattern := range stopPatterns {
		if normalized == pattern || strings.HasPrefix(normalized, pattern+" ") {
			return true
		}
	}
	return false
}

// buildStopSummary constructs a user-facing message when a run is stopped by the user.
func buildStopSummary(log []toolCallEntry) string {
	var sb strings.Builder
	sb.WriteString("Stopped by user request. ")
	if len(log) > 0 {
		succeeded := 0
		failed := 0
		for _, e := range log {
			if e.IsError {
				failed++
			} else {
				succeeded++
			}
		}
		sb.WriteString(fmt.Sprintf("Completed %d tool calls (%d succeeded, %d failed) before stopping.",
			len(log), succeeded, failed))
	}
	sb.WriteString("\n\nWhat would you like me to do instead?")
	return sb.String()
}

// shouldSuppressToolErrorForUser returns true if errors from non-mutating tools
// should be hidden from the user when suppress_tool_errors is enabled.
func shouldSuppressToolErrorForUser(toolName string, suppress bool) bool {
	if !suppress {
		return false
	}
	// Only suppress errors from read-only/non-mutating tools
	nonMutating := map[string]bool{
		"read_file":  true,
		"list_dir":   true,
		"web_search": true,
		"web_fetch":  true,
	}
	return nonMutating[toolName]
}
