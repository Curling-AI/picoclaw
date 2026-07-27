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

// The two kinds of memory this tool searches, kept as SEPARATE corpora.
//
// They have wildly different shapes: a durable section is prose with runbooks,
// an episodic note is one line. The BM25 engine runs with b=0.9 — much stronger
// length normalization than the 0.75 default — so in a single ranking the short
// lines systematically outrank the long sections, and trivia drowns the durable
// fact. Scoring each kind against its own corpus and returning a fixed quota of
// each keeps both reachable, and labels them so the agent never has to guess
// which it got.
const (
	recallKindDurable  = "durable"
	recallKindEpisodic = "episodic"
)

// RecallTool indexes the workspace's memory files and returns the entries most
// relevant to a query.
type RecallTool struct {
	workspace   string
	durableMax  int
	episodicMax int
}

// NewRecallTool builds the tool over a workspace. Non-positive quotas default
// to 3 durable + 4 episodic. A scoped query gets the sum for its own kind.
func NewRecallTool(workspace string, durableMax, episodicMax int) *RecallTool {
	if durableMax <= 0 {
		durableMax = 3
	}
	if episodicMax <= 0 {
		episodicMax = 4
	}
	return &RecallTool{workspace: workspace, durableMax: durableMax, episodicMax: episodicMax}
}

func (t *RecallTool) Name() string { return RecallToolName }

func (t *RecallTool) Description() string {
	return "Search your own memory. It has two kinds and this searches BOTH by default, returning each " +
		"labeled — you do not have to choose:\n" +
		"- durable: your long-term memory (MEMORY.md), organized in topic sections. Facts that stay true.\n" +
		"- episodic: the daily notes — one line per event, the record of what actually happened on a " +
		"given day (decisions, what you did, what the user said).\n" +
		"The prompt shows your durable memory in full but only the last few days of notes, so use this " +
		"for anything older or beyond that window. Query by topic (\"the postgres incident\") OR by date " +
		"(\"2026-07-12\", \"20260712\") to see what happened then. Narrow with 'scope' only when you are " +
		"sure which kind you want."
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
				"type": "string",
				"description": "What to recall: a topic/description of the fact or context, or a date " +
					"(YYYY-MM-DD or YYYYMMDD) to retrieve what happened that day.",
			},
			"scope": map[string]any{
				"type": "string",
				"enum": []string{"all", recallKindDurable, recallKindEpisodic},
				"description": "Which memory to search. Default 'all' (both, each labeled). " +
					"'durable' = long-term topic sections; 'episodic' = the daily notes.",
			},
		},
		"required": []string{"query"},
	}
}

// recallDoc is one indexed memory entry: a durable section, or one episodic
// note entry.
type recallDoc struct {
	Source  string // e.g. "MEMORY.md" or "20260712" (the note's date)
	Heading string
	Body    string
}

type recallResult struct {
	Kind    string `json:"kind"`
	Source  string `json:"source"`
	Date    string `json:"date,omitempty"`
	Heading string `json:"heading,omitempty"`
	Snippet string `json:"snippet"`
}

// recallEnvelope keeps the two kinds apart in the answer, so the agent reads
// "these are durable facts, those are things that happened" without inferring
// it from the source string.
type recallEnvelope struct {
	Durable  []recallResult `json:"durable,omitempty"`
	Episodic []recallResult `json:"episodic,omitempty"`
}

func (t *RecallTool) Execute(_ context.Context, args map[string]any) *ToolResult {
	query, ok := args["query"].(string)
	if !ok || strings.TrimSpace(query) == "" {
		return ErrorResult("Missing or invalid 'query' argument. Must be a non-empty string.")
	}
	scope, _ := args["scope"].(string)
	scope = strings.ToLower(strings.TrimSpace(scope))
	switch scope {
	case "", "all", recallKindDurable, recallKindEpisodic:
	default:
		return ErrorResult(fmt.Sprintf("Unknown scope %q — use 'all' (default), %q or %q.",
			scope, recallKindDurable, recallKindEpisodic))
	}

	durable, episodic := t.collectCorpora()
	if len(durable) == 0 && len(episodic) == 0 {
		return SilentResult("No memory to search yet.")
	}

	// A scoped query spends the whole budget on the kind it asked for.
	total := t.durableMax + t.episodicMax
	durableMax, episodicMax := t.durableMax, t.episodicMax
	switch scope {
	case recallKindDurable:
		durableMax, episodicMax = total, 0
	case recallKindEpisodic:
		durableMax, episodicMax = 0, total
	}

	env := recallEnvelope{
		Durable:  t.search(durable, query, durableMax, recallKindDurable),
		Episodic: t.search(episodic, query, episodicMax, recallKindEpisodic),
	}
	n := len(env.Durable) + len(env.Episodic)
	if n == 0 {
		return SilentResult("No memory entries found matching the query.")
	}
	logger.InfoCF("memory", "recall completed", map[string]any{
		"query": query, "scope": scope,
		"durable": len(env.Durable), "episodic": len(env.Episodic),
	})

	body, err := json.Marshal(env)
	if err != nil {
		return ErrorResult("Failed to format recall results: " + err.Error())
	}
	return SilentResult(fmt.Sprintf("Recalled %d memory entrie(s):\n%s", n, string(body)))
}

// search ranks one corpus on its own. Scoring the two together would let BM25's
// length normalization (b=0.9) float the one-line notes above every section.
func (t *RecallTool) search(docs []recallDoc, query string, limit int, kind string) []recallResult {
	if limit <= 0 || len(docs) == 0 {
		return nil
	}
	// Index the entry's date (both 20260712 and 2026-07-12 forms) alongside its
	// text so date queries match — the BM25 identifier tokenizer splits the
	// hyphenated form into 2026/07/12 parts, so either query form hits.
	engine := utils.NewBM25Engine(docs, func(d recallDoc) string {
		return d.Source + " " + hyphenatedDate(d.Source) + " " + d.Heading + " " + d.Body
	})
	ranked := engine.Search(query, limit)
	out := make([]recallResult, len(ranked))
	for i, r := range ranked {
		out[i] = recallResult{
			Kind:    kind,
			Source:  r.Document.Source,
			Date:    hyphenatedDate(r.Document.Source),
			Heading: r.Document.Heading,
			Snippet: truncateRunes(r.Document.Body, recallMaxSnippet),
		}
	}
	return out
}

// collectCorpora reads the two memory stores into their own doc lists.
//
// USER.md and SOUL.md are deliberately absent: both go into EVERY prompt in
// full, so indexing them would only return what the agent is already reading.
func (t *RecallTool) collectCorpora() (durable, episodic []recallDoc) {
	memoryDir := filepath.Join(t.workspace, "memory")

	if p := filepath.Join(memoryDir, "MEMORY.md"); fileExists(p) {
		if content, err := os.ReadFile(p); err == nil {
			durable = splitMemorySections(string(content), "MEMORY.md")
		}
	}

	// Daily notes live under memory/YYYYMM/YYYYMMDD.md.
	var notes []struct{ path, source string }
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
			notes = append(notes, struct{ path, source string }{
				filepath.Join(memoryDir, md.Name(), df.Name()),
				strings.TrimSuffix(df.Name(), ".md"),
			})
		}
	}
	// Newest notes first so ties favor recent memory.
	sort.Slice(notes, func(i, j int) bool { return notes[i].source > notes[j].source })
	for _, f := range notes {
		content, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		episodic = append(episodic, splitNoteEntries(string(content), f.source)...)
	}
	return durable, episodic
}

// durableHeadings are the section levels a MEMORY.md may use. `### ` is the
// current one (memory sits inside a <memory> block in the prompt, one level
// below the prompt's own sections); `## ` is what files carried before the
// migration, and a pod that has not migrated yet must still recall properly.
var durableHeadings = []string{"### ", "## "}

// splitMemorySections splits MEMORY.md into one doc per section, so a query
// matches a specific topic instead of the whole file. Content before the first
// heading (the preamble) is dropped.
func splitMemorySections(content, source string) []recallDoc {
	for _, prefix := range durableHeadings {
		if docs := splitOnHeading(content, source, prefix); len(docs) > 0 {
			return docs
		}
	}
	// No headings at all (a flat file): index it whole rather than lose it.
	if text := strings.TrimSpace(content); text != "" {
		return []recallDoc{{Source: source, Body: text}}
	}
	return nil
}

func splitOnHeading(content, source, prefix string) []recallDoc {
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
	for _, ln := range strings.Split(content, "\n") {
		if strings.HasPrefix(ln, prefix) {
			if started {
				flush()
			}
			started = true
			heading = strings.TrimSpace(strings.TrimPrefix(ln, prefix))
			continue
		}
		if started {
			body = append(body, ln)
		}
	}
	if started {
		flush()
	}
	return docs
}

// splitNoteEntries turns one daily note into one doc per entry. It reads both
// shapes on purpose: an entry is ONE LINE now, but notes written before that
// are `## HH:MM Title` blocks and were never migrated (compressing them would
// need an LLM and would lose detail), so both stay retrievable forever.
func splitNoteEntries(content, source string) []recallDoc {
	if strings.Contains(content, "\n## ") || strings.HasPrefix(content, "## ") {
		return splitOnHeading(content, source, "## ")
	}
	var docs []recallDoc
	for _, ln := range strings.Split(content, "\n") {
		text := strings.TrimSpace(ln)
		// The `# YYYY-MM-DD` title old notes opened with is noise: the source
		// already carries the date.
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		docs = append(docs, recallDoc{Source: source, Body: strings.TrimPrefix(text, "- ")})
	}
	return docs
}

// hyphenatedDate turns an 8-digit YYYYMMDD source into "YYYY-MM-DD" so date
// queries in either form match. Non-date sources (e.g. "MEMORY.md") yield "".
func hyphenatedDate(source string) string {
	if len(source) != 8 {
		return ""
	}
	for _, r := range source {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return source[0:4] + "-" + source[4:6] + "-" + source[6:8]
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func truncateRunes(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit]) + "…"
}
