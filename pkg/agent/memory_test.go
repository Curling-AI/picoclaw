package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeDailyNote(t *testing.T, ms *MemoryStore, daysAgo int, content string) {
	t.Helper()
	date := time.Now().AddDate(0, 0, -daysAgo)
	dateStr := date.Format("20060102")
	dir := filepath.Join(ms.memoryDir, dateStr[:6])
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dateStr+".md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestGetRecentDailyNotesUnderBudgetIsUntouched(t *testing.T) {
	ms := NewMemoryStore(t.TempDir())
	writeDailyNote(t, ms, 0, "# hoje\n\n## 10:00\nnota de hoje")
	writeDailyNote(t, ms, 1, "# ontem\n\n## 09:00\nnota de ontem")

	out := ms.GetRecentDailyNotes(3)
	if !strings.Contains(out, "nota de hoje") || !strings.Contains(out, "nota de ontem") {
		t.Fatalf("notas pequenas deveriam entrar inteiras: %q", out)
	}
	if strings.Contains(out, "truncad") || strings.Contains(out, "omitidas") {
		t.Fatalf("sem truncamento abaixo do budget: %q", out)
	}
	// Mais recente primeiro.
	if strings.Index(out, "nota de hoje") > strings.Index(out, "nota de ontem") {
		t.Fatalf("hoje deveria vir antes de ontem: %q", out)
	}
}

func TestGetRecentDailyNotesCapsPollutedDay(t *testing.T) {
	ms := NewMemoryStore(t.TempDir())
	// Dia de hoje poluído (estilo cron 5/5min): maior que o budget inteiro.
	var sb strings.Builder
	sb.WriteString("# 2026-07-13\n\n## 00:01 — nota antiga importante\nprimeira do dia\n\n")
	for range 800 {
		sb.WriteString("## 12:00 — Monitor Executado\n- Resultado: nada novo\n\n")
	}
	sb.WriteString("## 23:59 — ultima-entrada-do-dia\nfato recente\n")
	writeDailyNote(t, ms, 0, sb.String())
	writeDailyNote(t, ms, 1, "# ontem\n\n## 09:00 — nota-de-ontem\nconteúdo")

	out := ms.GetRecentDailyNotes(3)
	if len(out) > dailyNotesBudget+256 {
		t.Fatalf("saída estourou o budget: %d bytes", len(out))
	}
	// O rabo (mais recente) do dia poluído sobrevive; o começo cai.
	if !strings.Contains(out, "ultima-entrada-do-dia") {
		t.Fatalf("a entrada mais recente do dia deveria sobreviver")
	}
	if strings.Contains(out, "nota antiga importante") {
		t.Fatalf("o início do dia estourado deveria ser cortado")
	}
	// Dias mais antigos ficam de fora quando o budget acabou, com marcador.
	if strings.Contains(out, "nota-de-ontem") {
		t.Fatalf("ontem não cabe no budget consumido por hoje")
	}
	if !strings.Contains(out, "[notas mais antigas omitidas") {
		t.Fatalf("faltou o marcador de truncamento: %q", out[:200])
	}
	// Corte em fronteira de entrada (começa num header ##).
	tailStart := strings.Index(out, "truncadas]\n") + len("truncadas]\n")
	if !strings.HasPrefix(out[tailStart:], "## ") {
		t.Fatalf("o corte deveria cair numa fronteira de entrada: %q", out[tailStart:tailStart+40])
	}
}

func TestGetMemoryContext_RecentDaysWindow(t *testing.T) {
	ms := NewMemoryStore(t.TempDir())
	if err := ms.WriteLongTerm("durable fact about the user"); err != nil {
		t.Fatalf("WriteLongTerm: %v", err)
	}
	if err := ms.AppendToday("something that happened today"); err != nil {
		t.Fatalf("AppendToday: %v", err)
	}

	// days > 0: long-term + recent notes both injected.
	withNotes := ms.GetMemoryContext(3)
	if !strings.Contains(withNotes, "durable fact") || !strings.Contains(withNotes, "something that happened today") {
		t.Fatalf("days=3 should include long-term and recent notes: %q", withNotes)
	}

	// days == 0: only long-term; daily notes deferred to the recall tool.
	leanCtx := ms.GetMemoryContext(0)
	if !strings.Contains(leanCtx, "durable fact") {
		t.Fatalf("days=0 should still include long-term memory: %q", leanCtx)
	}
	if strings.Contains(leanCtx, "something that happened today") || strings.Contains(leanCtx, "Recent Daily Notes") {
		t.Fatalf("days=0 must NOT inject daily notes: %q", leanCtx)
	}
}

func TestWithRecentNotesDays_GatesPromptInjection(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	ws := t.TempDir()
	NewMemoryStore(ws).AppendToday("yesterday we shipped the caching work")

	lean := NewContextBuilder(ws).WithRecentNotesDays(0)
	sys := lean.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hi"})[0].Content
	if strings.Contains(sys, "yesterday we shipped") || strings.Contains(sys, "Recent Daily Notes") {
		t.Fatalf("recentNotesDays=0 should keep daily notes out of the prompt: %q", sys)
	}
	// With notes deferred, the prompt must nudge the agent toward the recall tool.
	if !strings.Contains(sys, "recall tool") {
		t.Fatalf("recentNotesDays=0 should nudge the agent about the recall tool: %q", sys)
	}

	full := NewContextBuilder(ws) // default 3
	sysFull := full.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hi"})[0].Content
	if !strings.Contains(sysFull, "yesterday we shipped") {
		t.Fatalf("default should inject recent daily notes: %q", sysFull)
	}
}
