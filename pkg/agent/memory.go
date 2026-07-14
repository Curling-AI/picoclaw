// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/fileutil"
)

// MemoryStore manages persistent memory for the agent.
// - Long-term memory: memory/MEMORY.md
// - Daily notes: memory/YYYYMM/YYYYMMDD.md
type MemoryStore struct {
	workspace  string
	memoryDir  string
	memoryFile string
}

// NewMemoryStore creates a new MemoryStore with the given workspace path.
// It ensures the memory directory exists.
func NewMemoryStore(workspace string) *MemoryStore {
	memoryDir := filepath.Join(workspace, "memory")
	memoryFile := filepath.Join(memoryDir, "MEMORY.md")

	// Ensure memory directory exists
	os.MkdirAll(memoryDir, 0o755)

	return &MemoryStore{
		workspace:  workspace,
		memoryDir:  memoryDir,
		memoryFile: memoryFile,
	}
}

// getTodayFile returns the path to today's daily note file (memory/YYYYMM/YYYYMMDD.md).
func (ms *MemoryStore) getTodayFile() string {
	today := time.Now().Format("20060102") // YYYYMMDD
	monthDir := today[:6]                  // YYYYMM
	filePath := filepath.Join(ms.memoryDir, monthDir, today+".md")
	return filePath
}

// ReadLongTerm reads the long-term memory (MEMORY.md).
// Returns empty string if the file doesn't exist.
func (ms *MemoryStore) ReadLongTerm() string {
	if data, err := os.ReadFile(ms.memoryFile); err == nil {
		return string(data)
	}
	return ""
}

// WriteLongTerm writes content to the long-term memory file (MEMORY.md).
func (ms *MemoryStore) WriteLongTerm(content string) error {
	// Use unified atomic write utility with explicit sync for flash storage reliability.
	// Using 0o600 (owner read/write only) for secure default permissions.
	return fileutil.WriteFileAtomic(ms.memoryFile, []byte(content), 0o600)
}

// ReadToday reads today's daily note.
// Returns empty string if the file doesn't exist.
func (ms *MemoryStore) ReadToday() string {
	todayFile := ms.getTodayFile()
	if data, err := os.ReadFile(todayFile); err == nil {
		return string(data)
	}
	return ""
}

// AppendToday appends content to today's daily note.
// If the file doesn't exist, it creates a new file with a date header.
func (ms *MemoryStore) AppendToday(content string) error {
	todayFile := ms.getTodayFile()

	// Ensure month directory exists
	monthDir := filepath.Dir(todayFile)
	if err := os.MkdirAll(monthDir, 0o755); err != nil {
		return err
	}

	var existingContent string
	if data, err := os.ReadFile(todayFile); err == nil {
		existingContent = string(data)
	}

	var newContent string
	if existingContent == "" {
		// Add header for new day
		header := fmt.Sprintf("# %s\n\n", time.Now().Format("2006-01-02"))
		newContent = header + content
	} else {
		// Append to existing content
		newContent = existingContent + "\n" + content
	}

	// Use unified atomic write utility with explicit sync for flash storage reliability.
	return fileutil.WriteFileAtomic(todayFile, []byte(newContent), 0o600)
}

// dailyNotesBudget caps how many bytes of recent daily notes are injected
// into the prompt. Notes are unbounded user/agent content; without a cap a
// noisy writer (e.g. a 5-minute cron logging every run) inflates EVERY prompt
// for 3 days straight — cost compounds. 16KB ≈ 4k tokens keeps genuine notes
// intact (production p99 across agents is well under this) while bounding the
// blast radius of pollution.
const dailyNotesBudget = 16 * 1024

// GetRecentDailyNotes returns daily notes from the last N days, newest first,
// joined with "---" separators and capped at dailyNotesBudget bytes. When the
// budget is hit, older content is dropped (a day may be partially included,
// keeping its most recent entries — files are append-only so the tail is the
// newest) and a truncation marker is appended.
func (ms *MemoryStore) GetRecentDailyNotes(days int) string {
	var parts []string
	budget := dailyNotesBudget
	truncated := false

	for i := range days {
		if budget <= 0 {
			truncated = true
			break
		}
		date := time.Now().AddDate(0, 0, -i)
		dateStr := date.Format("20060102") // YYYYMMDD
		monthDir := dateStr[:6]            // YYYYMM
		filePath := filepath.Join(ms.memoryDir, monthDir, dateStr+".md")

		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}
		if len(data) > budget {
			// Keep the newest tail of the day, cutting at an entry boundary
			// ("## " header) when one exists inside the kept window.
			tail := string(data[len(data)-budget:])
			if idx := strings.Index(tail, "\n## "); idx >= 0 {
				tail = tail[idx+1:]
			}
			parts = append(parts, "[notas mais antigas deste dia truncadas]\n"+tail)
			budget = 0
			truncated = true
		} else {
			parts = append(parts, string(data))
			budget -= len(data)
		}
	}

	out := strings.Join(parts, "\n\n---\n\n")
	if truncated {
		out += "\n\n[notas mais antigas omitidas — janela de notas limitada a 16KB]"
	}
	return out
}

// GetMemoryContext returns formatted memory context for the agent prompt:
// long-term memory plus the last recentDays of daily notes. recentDays <= 0
// injects no daily notes — the recall tool then supplies them on demand, which
// keeps the prompt lean.
func (ms *MemoryStore) GetMemoryContext(recentDays int) string {
	longTerm := ms.ReadLongTerm()
	recentNotes := ""
	if recentDays > 0 {
		recentNotes = ms.GetRecentDailyNotes(recentDays)
	}

	if longTerm == "" && recentNotes == "" {
		return ""
	}

	var sb strings.Builder

	if longTerm != "" {
		sb.WriteString("## Long-term Memory\n\n")
		sb.WriteString(longTerm)
	}

	if recentNotes != "" {
		if longTerm != "" {
			sb.WriteString("\n\n---\n\n")
		}
		sb.WriteString("## Recent Daily Notes\n\n")
		sb.WriteString(recentNotes)
	}

	return sb.String()
}
