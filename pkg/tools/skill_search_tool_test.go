package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/skills"
)

type stubSkillLister struct{ items []skills.SkillInfo }

func (s stubSkillLister) ListSkills() []skills.SkillInfo { return s.items }

func TestSkillSearchTool_RanksByQuery(t *testing.T) {
	loader := stubSkillLister{items: []skills.SkillInfo{
		{Name: "pdf-extract", Description: "Extract text and tables from PDFs", Path: "skills/pdf-extract/SKILL.md"},
		{Name: "deck-builder", Description: "Build slide decks", Path: "skills/deck-builder/SKILL.md"},
		{Name: "csv-report", Description: "Summarize CSV data into a report", Path: "skills/csv-report/SKILL.md"},
		{Name: "disabled-one", Description: "should never appear", Path: "x", Disabled: true},
	}}
	tool := NewSkillSearchTool(loader, 2)

	res := tool.Execute(context.Background(), map[string]any{"query": "read a pdf document"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "pdf-extract") {
		t.Fatalf("expected pdf-extract in results: %s", res.ForLLM)
	}
	if strings.Contains(res.ForLLM, "disabled-one") {
		t.Fatalf("disabled skill leaked into results: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "skills/pdf-extract/SKILL.md") {
		t.Fatalf("expected the skill location in results: %s", res.ForLLM)
	}
}

func TestSkillSearchTool_EmptyQueryAndNoSkills(t *testing.T) {
	empty := NewSkillSearchTool(stubSkillLister{}, 5)
	if r := empty.Execute(context.Background(), map[string]any{"query": " "}); !r.IsError {
		t.Fatal("empty query should error")
	}
	if r := empty.Execute(context.Background(), map[string]any{"query": "anything"}); r.IsError {
		t.Fatalf("no skills should be silent, not error: %s", r.ForLLM)
	}
}
