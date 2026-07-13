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
