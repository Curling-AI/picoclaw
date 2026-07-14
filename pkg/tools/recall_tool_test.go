package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRecallFile(t *testing.T, ws, rel, content string) {
	t.Helper()
	p := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRecallTool_FindsOldNoteBeyondWindow(t *testing.T) {
	ws := t.TempDir()
	writeRecallFile(t, ws, "memory/MEMORY.md", "# Memory\n\n## Preferences\n\nUser prefers dark mode.\n")
	// An old daily note (months ago) — outside the 3-day prompt window.
	writeRecallFile(t, ws, "memory/202601/20260112.md",
		"# 2026-01-12\n\n## Postgres incident\n\nThe checkout DB hit connection limits; "+
			"fix was raising max_connections.\n")
	writeRecallFile(t, ws, "memory/202607/20260712.md",
		"# 2026-07-12\n\n## Deploy\n\nShipped the caching work.\n")

	tool := NewRecallTool(ws, 3)
	res := tool.Execute(context.Background(), map[string]any{"query": "database connection limit incident"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Postgres incident") || !strings.Contains(res.ForLLM, "20260112") {
		t.Fatalf("recall did not surface the old note: %s", res.ForLLM)
	}
}

func TestRecallTool_EmptyAndNoMemory(t *testing.T) {
	if r := NewRecallTool(t.TempDir(), 5).Execute(context.Background(), map[string]any{"query": " "}); !r.IsError {
		t.Fatal("empty query should error")
	}
	r := NewRecallTool(t.TempDir(), 5).Execute(context.Background(), map[string]any{"query": "anything"})
	if r.IsError {
		t.Fatalf("no memory should be silent, not error: %s", r.ForLLM)
	}
}

func TestSplitMemorySections(t *testing.T) {
	docs := splitMemorySections("# 2026-07-12\n\n## A\n\nbody a\n\n## B\n\nbody b\n", "20260712")
	if len(docs) != 2 || docs[0].Heading != "A" || !strings.Contains(docs[1].Body, "body b") {
		t.Fatalf("unexpected sections: %#v", docs)
	}
	// No ## sections → whole file as one doc.
	flat := splitMemorySections("just a flat note with no headers", "MEMORY.md")
	if len(flat) != 1 || flat[0].Heading != "" {
		t.Fatalf("flat file should index as one headingless doc: %#v", flat)
	}
}

func TestRecallTool_SearchByDate(t *testing.T) {
	ws := t.TempDir()
	writeRecallFile(t, ws, "memory/202601/20260112.md",
		"# 2026-01-12\n\n## Postgres incident\n\nConnection limits raised.\n")
	writeRecallFile(t, ws, "memory/202607/20260712.md",
		"# 2026-07-12\n\n## Deploy\n\nShipped the caching work.\n")

	tool := NewRecallTool(ws, 3)
	// Hyphenated date query hits the note from that day.
	res := tool.Execute(context.Background(), map[string]any{"query": "2026-01-12"})
	if res.IsError || !strings.Contains(res.ForLLM, "Postgres incident") {
		t.Fatalf("date query 2026-01-12 should surface that day's note: %s", res.ForLLM)
	}
	// Compact form works too.
	res2 := tool.Execute(context.Background(), map[string]any{"query": "20260712"})
	if res2.IsError || !strings.Contains(res2.ForLLM, "Deploy") {
		t.Fatalf("date query 20260712 should surface that day's note: %s", res2.ForLLM)
	}
}

func TestHyphenatedDate(t *testing.T) {
	if got := hyphenatedDate("20260712"); got != "2026-07-12" {
		t.Fatalf("hyphenatedDate = %q, want 2026-07-12", got)
	}
	if got := hyphenatedDate("MEMORY.md"); got != "" {
		t.Fatalf("non-date source should yield empty, got %q", got)
	}
}
