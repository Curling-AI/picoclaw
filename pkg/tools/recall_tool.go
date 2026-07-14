package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// RecallToolName searches the assistant's own memory (MEMORY.md + all daily
// notes) with BM25. The system prompt only injects MEMORY.md + the last 3 days
// of notes (capped at 16KB); this tool lifts that ceiling on demand, letting
// the agent retrieve relevant facts from any past day without inflating every
// prompt.
const RecallToolName = "recall"

// recallMaxSnippet bounds each returned entry so a match never dumps a whole
// note into the context.
const recallMaxSnippet = 800

// RecallTool indexes the workspace's memory files and returns the entries most
// relevant to a query.
type RecallTool struct {
	workspace  string
	maxResults int
}

// NewRecallTool builds the tool over a workspace. maxResults <= 0 defaults to 5.
func NewRecallTool(workspace string, maxResults int) *RecallTool {
	if maxResults <= 0 {
		maxResults = 5
	}
	return &RecallTool{workspace: workspace, maxResults: maxResults}
}

func (t *RecallTool) Name() string { return RecallToolName }

func (t *RecallTool) Description() string {
	return "Search your long-term memory and daily notes for relevant past facts. The prompt only " +
		"carries the last few days of notes, so use this to recall anything older or beyond that window. " +
		"Returns the most relevant memory entries with their source file and date."
}

func (t *RecallTool) PromptMetadata() PromptMetadata {
	return PromptMetadata{
		Layer:  ToolPromptLayerCapability,
		Slot:   ToolPromptSlotTooling,
		Source: ToolPromptSourceDiscovery,
	}
}

func (t *RecallTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Natural-language description of the fact or context you want to recall.",
			},
		},
		"required": []string{"query"},
	}
}

// recallDoc is one indexed memory entry (a `## ` section of a memory file).
type recallDoc struct {
	Source  string // e.g. "MEMORY.md" or "20260712" (the note's date)
	Heading string
	Body    string
}

type recallResult struct {
	Source  string `json:"source"`
	Heading string `json:"heading,omitempty"`
	Snippet string `json:"snippet"`
}

func (t *RecallTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return ErrorResult("Missing or invalid 'query' argument. Must be a non-empty string.")
	}

	docs := t.collectDocs()
	if len(docs) == 0 {
		return SilentResult("No memory to search yet.")
	}

	engine := utils.NewBM25Engine(docs, func(d recallDoc) string {
		return d.Heading + " " + d.Body
	})
	ranked := engine.Search(query, t.maxResults)
	if len(ranked) == 0 {
		return SilentResult("No memory entries found matching the query.")
	}

	results := make([]recallResult, len(ranked))
	for i, r := range ranked {
		results[i] = recallResult{
			Source:  r.Document.Source,
			Heading: r.Document.Heading,
			Snippet: truncateRunes(r.Document.Body, recallMaxSnippet),
		}
	}
	logger.InfoCF("memory", "recall completed", map[string]any{"query": query, "results": len(results)})

	body, err := json.Marshal(results)
	if err != nil {
		return ErrorResult("Failed to format recall results: " + err.Error())
	}
	return SilentResult(fmt.Sprintf("Recalled %d memory entrie(s):\n%s", len(results), string(body)))
}

// collectDocs reads MEMORY.md and every daily note, splitting each into `## `
// sections so a query matches a specific fact, not a whole file.
func (t *RecallTool) collectDocs() []recallDoc {
	memoryDir := filepath.Join(t.workspace, "memory")
	var files []struct{ path, source string }

	if p := filepath.Join(memoryDir, "MEMORY.md"); fileExists(p) {
		files = append(files, struct{ path, source string }{p, "MEMORY.md"})
	}
	// Daily notes live under memory/YYYYMM/YYYYMMDD.md.
	monthDirs, _ := os.ReadDir(memoryDir)
	for _, md := range monthDirs {
		if !md.IsDir() {
			continue
		}
		dayFiles, _ := os.ReadDir(filepath.Join(memoryDir, md.Name()))
		for _, df := range dayFiles {
			if df.IsDir() || !strings.HasSuffix(df.Name(), ".md") {
				continue
			}
			files = append(files, struct{ path, source string }{
				filepath.Join(memoryDir, md.Name(), df.Name()),
				strings.TrimSuffix(df.Name(), ".md"),
			})
		}
	}
	// Newest notes first so ties favor recent memory.
	sort.Slice(files, func(i, j int) bool { return files[i].source > files[j].source })

	var docs []recallDoc
	for _, f := range files {
		content, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		docs = append(docs, splitMemorySections(string(content), f.source)...)
	}
	return docs
}

// splitMemorySections splits a markdown memory file into one doc per `## `
// heading. Content before the first `## ` (e.g. the `# date` title) is dropped.
func splitMemorySections(content, source string) []recallDoc {
	lines := strings.Split(content, "\n")
	var docs []recallDoc
	var heading string
	var body []string
	flush := func() {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		if heading != "" || text != "" {
			docs = append(docs, recallDoc{Source: source, Heading: heading, Body: text})
		}
		heading, body = "", nil
	}
	started := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "## ") {
			if started {
				flush()
			}
			started = true
			heading = strings.TrimSpace(strings.TrimPrefix(ln, "## "))
			continue
		}
		if started {
			body = append(body, ln)
		}
	}
	if started {
		flush()
	}
	// Files with no `## ` sections (e.g. a flat MEMORY.md) index as one doc.
	if len(docs) == 0 {
		if text := strings.TrimSpace(content); text != "" {
			docs = append(docs, recallDoc{Source: source, Body: text})
		}
	}
	return docs
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
