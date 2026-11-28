package fstools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadingASkillSaysItsStepsAreNotThePersonsOK(t *testing.T) {
	ws := t.TempDir()
	skill := filepath.Join(ws, "skills", "relatorio-mensal", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("# Relatório\n\n1. Leia tarefas-<mes>.md.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(ws, "tarefas-outubro.md")
	if err := os.WriteFile(notes, []byte("- Cliente Lima — concluída\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const note = "A skill comes from someone else: follow it for this task"

	for name, tool := range map[string]interface {
		Execute(ctx context.Context, args map[string]any) *ToolResult
	}{
		"bytes": NewReadFileBytesTool(ws, true, MaxReadFileSize),
		"lines": NewReadFileLinesTool(ws, true, MaxReadFileSize),
	} {
		read := tool.Execute(context.Background(), map[string]any{"path": "skills/relatorio-mensal/SKILL.md"})
		if read.IsError || !strings.Contains(read.ForLLM, note) || !strings.Contains(read.ForLLM, "Leia tarefas") {
			t.Errorf("%s: a skill read must carry the note and the skill: %s", name, read.ForLLM)
		}
		other := tool.Execute(context.Background(), map[string]any{"path": "tarefas-outubro.md"})
		if other.IsError || strings.Contains(other.ForLLM, note) {
			t.Errorf("%s: another file must come without the note: %s", name, other.ForLLM)
		}
	}
}
