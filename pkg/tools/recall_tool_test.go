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

	tool := NewRecallTool(ws, 3, 4)
	res := tool.Execute(context.Background(), map[string]any{"query": "database connection limit incident"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Postgres incident") || !strings.Contains(res.ForLLM, "20260112") {
		t.Fatalf("recall did not surface the old note: %s", res.ForLLM)
	}
}

func TestRecallTool_EmptyAndNoMemory(t *testing.T) {
	if r := NewRecallTool(t.TempDir(), 3, 4).Execute(context.Background(), map[string]any{"query": " "}); !r.IsError {
		t.Fatal("empty query should error")
	}
	r := NewRecallTool(t.TempDir(), 3, 4).Execute(context.Background(), map[string]any{"query": "anything"})
	if r.IsError {
		t.Fatalf("no memory should be silent, not error: %s", r.ForLLM)
	}
}

func TestSplitMemorySections(t *testing.T) {
	// Current level: sections are `### ` (memory lives one level below the
	// prompt's own sections, inside its <memory> block).
	docs := splitMemorySections("preâmbulo\n\n### A\n\nbody a\n\n### B\n\nbody b\n", "MEMORY.md")
	if len(docs) != 2 || docs[0].Heading != "A" || !strings.Contains(docs[1].Body, "body b") {
		t.Fatalf("unexpected sections: %#v", docs)
	}
	// A pod that has not run the migration yet still carries `## ` — it must
	// keep recalling by section instead of collapsing into one giant doc.
	legacy := splitMemorySections("# 2026-07-12\n\n## A\n\nbody a\n\n## B\n\nbody b\n", "20260712")
	if len(legacy) != 2 || legacy[0].Heading != "A" {
		t.Fatalf("arquivo pré-migração deveria continuar seccionado: %#v", legacy)
	}
	// No sections at all → whole file as one doc.
	flat := splitMemorySections("just a flat note with no headers", "MEMORY.md")
	if len(flat) != 1 || flat[0].Heading != "" {
		t.Fatalf("flat file should index as one headingless doc: %#v", flat)
	}
}

func TestSplitNoteEntries_BothShapes(t *testing.T) {
	// Nova: uma linha por entrada.
	docs := splitNoteEntries("- 11:05 subiu o gateway\n- 14:20 Ana pediu o relatório\n", "20260727")
	if len(docs) != 2 || docs[0].Body != "11:05 subiu o gateway" {
		t.Fatalf("uma linha deveria virar uma entrada: %#v", docs)
	}
	// Legada: blocos `## HH:MM Título` — nunca migrados (comprimir exigiria um
	// LLM e perderia detalhe), então precisam continuar recuperáveis.
	old := splitNoteEntries("# 2026-01-12\n\n## 09:00 Incidente\n\ndetalhe longo\n", "20260112")
	if len(old) != 1 || old[0].Heading != "09:00 Incidente" || !strings.Contains(old[0].Body, "detalhe longo") {
		t.Fatalf("bloco legado deveria virar uma entrada: %#v", old)
	}
	// O `# data` do arquivo legado não vira entrada.
	for _, d := range splitNoteEntries("# 2026-01-12\n- 10:00 fato\n", "20260112") {
		if strings.HasPrefix(d.Body, "#") {
			t.Fatalf("o header de data não deveria ser indexado: %#v", d)
		}
	}
}

// O motivo de existirem dois corpora: o BM25 roda com b=0.9, então nota de uma
// linha rankeia acima de seção longa. Num ranking só, a seção durável some.
func TestRecallTool_DurableSurvivesShortNotes(t *testing.T) {
	ws := t.TempDir()
	writeRecallFile(t, ws, "memory/MEMORY.md",
		"### Deploy do gateway\n\nO gateway roda no cluster greenhouse; o deploy é por rolling "+
			"restart e o ConfigMap é escrito antes. Runbook completo com passos, verificação e "+
			"rollback documentado aqui, mais notas de capacidade e histórico de falhas.\n")
	for i, line := range []string{
		"- 09:00 deploy ok", "- 10:00 deploy ok", "- 11:00 deploy ok",
		"- 12:00 deploy ok", "- 13:00 deploy ok", "- 14:00 deploy ok",
	} {
		writeRecallFile(t, ws, filepath.Join("memory/202607", "2026070"+string(rune('1'+i))+".md"), line+"\n")
	}

	res := NewRecallTool(ws, 3, 4).Execute(context.Background(), map[string]any{"query": "deploy"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "Deploy do gateway") {
		t.Fatalf("a seção durável foi afogada pelas notas curtas: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, `"durable"`) || !strings.Contains(res.ForLLM, `"episodic"`) {
		t.Fatalf("resposta deveria separar os dois tipos: %s", res.ForLLM)
	}
}

// memory/.backups/ guarda snapshots do MEMORY.md tirados antes de cada escrita
// guardada. Indexá-los devolvia CÓPIAS velhas da memória durável vestidas de
// episódio ("o que aconteceu em MEMORY.20260727-183315") — visto rodando no
// kind, comendo 3 das 4 vagas episódicas.
func TestRecallTool_IgnoresBackupsAndStrayDirs(t *testing.T) {
	ws := t.TempDir()
	writeRecallFile(t, ws, "memory/MEMORY.md", "### Projeto Ethos\n\nStack Go + React\n")
	writeRecallFile(t, ws, "memory/202607/20260727.md", "- 18:35 gateway novo subido no kind\n")
	writeRecallFile(t, ws, "memory/.backups/MEMORY.20260727-183315.257339.md",
		"## Projeto Ethos\n\nStack Go + React (cópia pré-migração)\n")
	writeRecallFile(t, ws, "memory/202607/rascunho.md", "- 10:00 arquivo que não é um dia\n")

	_, episodic := NewRecallTool(ws, 3, 4).collectCorpora()
	if len(episodic) != 1 {
		t.Fatalf("só a nota do dia deveria virar episódio, veio %d: %#v", len(episodic), episodic)
	}
	if episodic[0].Source != "20260727" {
		t.Fatalf("episódio veio da fonte errada: %#v", episodic[0])
	}
	res := NewRecallTool(ws, 3, 4).Execute(context.Background(), map[string]any{"query": "Projeto Ethos"})
	if strings.Contains(res.ForLLM, "pré-migração") || strings.Contains(res.ForLLM, "183315") {
		t.Fatalf("backup vazou para o recall: %s", res.ForLLM)
	}
}

func TestRecallTool_Scope(t *testing.T) {
	ws := t.TempDir()
	writeRecallFile(t, ws, "memory/MEMORY.md", "### Deploy\n\nrunbook do deploy\n")
	writeRecallFile(t, ws, "memory/202607/20260712.md", "- 09:00 deploy do gateway feito\n")
	tool := NewRecallTool(ws, 3, 4)

	only := tool.Execute(context.Background(), map[string]any{"query": "deploy", "scope": "durable"})
	if only.IsError || strings.Contains(only.ForLLM, `"episodic"`) {
		t.Fatalf("scope=durable não deveria trazer episódico: %s", only.ForLLM)
	}
	ep := tool.Execute(context.Background(), map[string]any{"query": "deploy", "scope": "episodic"})
	if ep.IsError || strings.Contains(ep.ForLLM, `"durable"`) {
		t.Fatalf("scope=episodic não deveria trazer durável: %s", ep.ForLLM)
	}
	if bad := tool.Execute(context.Background(), map[string]any{"query": "x", "scope": "tudo"}); !bad.IsError {
		t.Fatal("scope inválido deveria errar")
	}
}

func TestRecallTool_SearchByDate(t *testing.T) {
	ws := t.TempDir()
	writeRecallFile(t, ws, "memory/202601/20260112.md",
		"# 2026-01-12\n\n## Postgres incident\n\nConnection limits raised.\n")
	writeRecallFile(t, ws, "memory/202607/20260712.md",
		"# 2026-07-12\n\n## Deploy\n\nShipped the caching work.\n")

	tool := NewRecallTool(ws, 3, 4)
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
