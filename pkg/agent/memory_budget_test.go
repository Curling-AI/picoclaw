package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

func estimatedTokens(s string) int {
	return tokenizer.EstimateMessageTokens(providers.Message{Content: s})
}

// bigMemory monta um MEMORY.md legado com `n` seções `### `, cada uma com ~1 KB
// de corpo — o formato que o agente grava, só que grande demais.
func bigMemory(n int) string {
	var sb strings.Builder
	for i := range n {
		fmt.Fprintf(&sb, "### Tópico %03d\n", i)
		for j := range 8 {
			fmt.Fprintf(
				&sb,
				"- **Fato %d**: valor do tópico %03d com acentuação e texto de enchimento suficiente\n",
				j,
				i,
			)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func longTermBlock(t *testing.T, memoryContext string) string {
	t.Helper()
	const open = `<memory scope="long-term">` + "\n"
	start := strings.Index(memoryContext, open)
	if start < 0 {
		t.Fatalf("sem bloco long-term: %q", memoryContext)
	}
	rest := memoryContext[start+len(open):]
	end := strings.Index(rest, "\n</memory>")
	if end < 0 {
		t.Fatalf("bloco long-term sem fechamento: %q", rest)
	}
	return rest[:end]
}

// Abaixo do teto nada muda: o bloco de memória de quem tem memória pequena (a
// maioria) é byte a byte o de antes.
func TestFitMemoryToPromptBudget_UnderBudgetIsUntouched(t *testing.T) {
	content := strings.TrimSpace(bigMemory(3))
	if got := fitMemoryToPromptBudget(content, assistantLongTermFile); got != content {
		t.Fatalf("conteúdo abaixo do teto foi alterado:\n%s", got)
	}

	ms := NewMemoryStore(t.TempDir())
	if err := ms.WriteLongTerm(content); err != nil {
		t.Fatal(err)
	}
	if got := longTermBlock(t, ms.GetMemoryContext(0)); got != content {
		t.Fatalf("bloco long-term difere do arquivo abaixo do teto:\n%s", got)
	}
}

// Caminho legado: assistente que já passou do teto. O prompt fica dentro do
// orçamento, o começo do arquivo continua visível, o que ficou de fora aparece
// pelo título com a linha onde começa, e o arquivo em disco não muda.
func TestGetMemoryContext_LegacyMemoryAboveBudget(t *testing.T) {
	ws := t.TempDir()
	ms := NewMemoryStore(ws)
	content := bigMemory(400)
	if estimatedTokens(content) < 5*assistantLongTermFile.budget {
		t.Fatalf("fixture pequena demais para o teste: %d tokens", estimatedTokens(content))
	}
	if err := ms.WriteLongTerm(content); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, "memory", "MEMORY.md")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	block := longTermBlock(t, ms.GetMemoryContext(0))

	if got := estimatedTokens(block); got > assistantLongTermFile.budget {
		t.Fatalf("bloco long-term com %d tokens, teto %d", got, assistantLongTermFile.budget)
	}
	if !strings.HasPrefix(block, "### Tópico 000\n") {
		t.Fatalf("o começo do arquivo deveria continuar no prompt:\n%.300s", block)
	}
	for _, want := range []string{"intact on disk", "memory/MEMORY.md", "offset", "start_line", "recall"} {
		if !strings.Contains(block, want) {
			t.Fatalf("marcador sem %q:\n%s", want, block[max(0, len(block)-2000):])
		}
	}
	// A lista de omitidas tem teto: o último título não precisa estar nela, mas
	// a contagem do que sobrou sim.
	if !strings.Contains(block, "more") {
		t.Fatalf("lista de omitidas deveria dizer quantas ficaram de fora:\n%s", block[max(0, len(block)-2000):])
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != content || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("montar o prompt não pode reescrever a memória em disco")
	}
}

// omittedParts lê as entradas "- offset B, line N: texto" do marcador.
func omittedParts(t *testing.T, out string) []omittedEntry {
	t.Helper()
	var parts []omittedEntry
	for _, ln := range strings.Split(out, "\n") {
		var e omittedEntry
		if _, err := fmt.Sscanf(ln, "- offset %d, line %d:", &e.offset, &e.line); err != nil {
			continue
		}
		e.text = strings.TrimSpace(ln[strings.Index(ln, ":")+1:])
		parts = append(parts, e)
	}
	return parts
}

// assertPartsPointIntoFile confere cada entrada contra o arquivo em disco: o
// título está na linha N e começa no byte B. É o que o agente passa ao
// read_file (offset no modo de bytes, o do Maestro; start_line no de linhas).
func assertPartsPointIntoFile(t *testing.T, raw string, parts []omittedEntry) {
	t.Helper()
	lines := strings.Split(raw, "\n")
	for _, e := range parts {
		if strings.HasPrefix(e.text, "(continues") {
			continue
		}
		if e.line < 1 || e.line > len(lines) || lines[e.line-1] != e.text {
			t.Fatalf("linha %d deveria ser %q", e.line, e.text)
		}
		if e.offset < 0 || !strings.HasPrefix(raw[e.offset:], e.text+"\n") {
			t.Fatalf("offset %d deveria começar em %q", e.offset, e.text)
		}
	}
}

func TestFitMemoryToPromptBudget_OmittedPartsPointToOffsetAndLine(t *testing.T) {
	content := bigMemory(400)
	out := fitMemoryToPromptBudget(content, assistantLongTermFile)

	parts := omittedParts(t, out)
	if len(parts) < 2 {
		t.Fatalf("seções omitidas não listadas:\n%s", out[max(0, len(out)-2000):])
	}
	if len(parts) > maxOmittedHeadingsListed {
		t.Fatalf("listou %d partes, teto %d", len(parts), maxOmittedHeadingsListed)
	}
	assertPartsPointIntoFile(t, content, parts)
}

// Arquivo que começa com linhas em branco: o prompt mostra o conteúdo aparado,
// mas linhas e offsets do marcador são os do arquivo em disco.
func TestGetMemoryContext_OffsetsAreRelativeToTheFileOnDisk(t *testing.T) {
	ms := NewMemoryStore(t.TempDir())
	raw := "\n\n  \n" + bigMemory(400)
	if err := ms.WriteLongTerm(raw); err != nil {
		t.Fatal(err)
	}
	block := longTermBlock(t, ms.GetMemoryContext(0))

	if !strings.HasPrefix(block, "### Tópico 000\n") {
		t.Fatalf("o prompt deveria mostrar o conteúdo aparado:\n%.200s", block)
	}
	parts := omittedParts(t, block)
	if len(parts) == 0 {
		t.Fatal("sem partes omitidas")
	}
	assertPartsPointIntoFile(t, raw, parts)
}

// O recorte é função pura do conteúdo: turnos seguidos e um pod novo montam o
// mesmo system prompt, então o prefixo cacheado no provedor não muda enquanto a
// memória não muda.
func TestBuildSystemPrompt_CappedMemoryIsStableAcrossTurnsAndPods(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	ws := t.TempDir()
	if err := NewMemoryStore(ws).WriteLongTerm(bigMemory(400)); err != nil {
		t.Fatal(err)
	}

	cb := NewContextBuilder(ws).WithRecentNotesDays(0)
	first := cb.BuildSystemPromptWithCache()
	cb.InvalidateCache()
	second := cb.BuildSystemPromptWithCache()
	restarted := NewContextBuilder(ws).WithRecentNotesDays(0).BuildSystemPromptWithCache()

	if first != second || first != restarted {
		t.Fatal("system prompt com memória recortada mudou sem a memória mudar")
	}
	if !strings.Contains(first, "intact on disk") {
		t.Fatal("system prompt deveria trazer a memória recortada")
	}
}

// Arquivo sem nenhum título (texto corrido): o corte cai numa fronteira de
// linha, nunca no meio de uma.
func TestFitMemoryToPromptBudget_FlatFileCutsAtLineBoundary(t *testing.T) {
	var sb strings.Builder
	for i := range 5000 {
		fmt.Fprintf(&sb, "linha %05d de uma memória sem seções\n", i)
	}
	content := strings.TrimSpace(sb.String())
	out := fitMemoryToPromptBudget(content, assistantLongTermFile)

	if estimatedTokens(out) > assistantLongTermFile.budget {
		t.Fatalf("saída com %d tokens, teto %d", estimatedTokens(out), assistantLongTermFile.budget)
	}
	body := out[:strings.Index(out, "\n\n[")]
	if !strings.HasPrefix(content, body+"\n") {
		t.Fatal("o corpo mostrado deveria ser um prefixo de linhas inteiras do arquivo")
	}
}

// Uma linha só, maior que o teto (um JSON colado na memória): corta na
// fronteira de runa, sem quebrar UTF-8.
func TestFitMemoryToPromptBudget_SingleHugeLine(t *testing.T) {
	content := strings.Repeat("ação ", 60000)
	out := fitMemoryToPromptBudget(content, assistantLongTermFile)

	if estimatedTokens(out) > assistantLongTermFile.budget {
		t.Fatalf("saída com %d tokens, teto %d", estimatedTokens(out), assistantLongTermFile.budget)
	}
	if !utf8.ValidString(out) {
		t.Fatal("corte quebrou UTF-8")
	}
	if !strings.HasPrefix(out, "ação ação") || !strings.Contains(out, "intact on disk") {
		t.Fatalf("esperava o começo da linha e o marcador:\n%.200s", out)
	}
	parts := omittedParts(t, out)
	body := out[:strings.Index(out, "\n\n[")]
	if len(parts) != 1 || parts[0].offset != len(body) || parts[0].line != 1 {
		t.Fatalf("a continuação deveria apontar o byte %d da linha 1: %+v", len(body), parts)
	}
}

// Corte dentro de um bloco de código: fecha a cerca antes do marcador, senão o
// marcador vira parte do código aos olhos do modelo.
func TestFitMemoryToPromptBudget_ClosesOpenFence(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("### Runbook\n```sql\n")
	for i := range 5000 {
		fmt.Fprintf(&sb, "select %d from tabela_grande where coluna = 'valor';\n", i)
	}
	sb.WriteString("```\n")
	out := fitMemoryToPromptBudget(strings.TrimSpace(sb.String()), assistantLongTermFile)

	marker := strings.Index(out, "\n\n[")
	if marker < 0 {
		t.Fatalf("sem marcador:\n%.300s", out)
	}
	if strings.Count(out[:marker], "```")%2 != 0 {
		t.Fatal("bloco de código ficou aberto antes do marcador")
	}
	if strings.Count(out[marker:], "```") != 0 {
		t.Fatal("marcador não deveria abrir cerca")
	}
	if !strings.Contains(out, "(continues ### Runbook)") {
		t.Fatalf("a seção cortada no meio deveria aparecer como continuação:\n%s", out[marker:])
	}
}

// Overlays aprendidos (memory/USER.md, memory/SOUL.md) têm o próprio teto. O
// USER.md base, que vem da persona configurada, não é recortado.
func TestLoadBootstrapFiles_LearnedOverlayAboveBudget(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "USER.md"), []byte("# User\nbase user prefs"), 0o644); err != nil {
		t.Fatal(err)
	}
	overlay := bigMemory(200)
	overlayPath := filepath.Join(ws, "memory", "USER.md")
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	out := NewContextBuilder(ws).LoadBootstrapFiles()

	const header = "## USER.md (learned)\n\n"
	start := strings.Index(out, header)
	if start < 0 {
		t.Fatalf("sem seção do overlay:\n%.300s", out)
	}
	section := out[start+len(header):]
	if got := estimatedTokens(section); got > learnedOverlayFile("USER.md").budget {
		t.Fatalf("overlay com %d tokens, teto %d", got, learnedOverlayFile("USER.md").budget)
	}
	if !strings.Contains(section, "memory/USER.md") || !strings.Contains(section, "intact on disk") {
		t.Fatalf("marcador do overlay deveria apontar memory/USER.md:\n%s", section[max(0, len(section)-1500):])
	}
	if strings.Contains(section, "recall") {
		t.Fatal("o recall não indexa os overlays; o marcador não pode mandar o agente para lá")
	}
	if !strings.Contains(out, "base user prefs") {
		t.Fatal("USER.md base deveria continuar inteiro")
	}
	data, err := os.ReadFile(overlayPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != overlay {
		t.Fatal("overlay em disco foi alterado")
	}
}

// Memória de loop acima do teto: mesmo recorte, apontando o arquivo do loop.
func TestLoopPromptPart_LoopMemoryAboveBudget(t *testing.T) {
	root := filepath.Join(t.TempDir(), "loops", "vendas")
	if err := os.MkdirAll(filepath.Join(root, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(loopMemoryFile(root), []byte(bigMemory(200)), 0o600); err != nil {
		t.Fatal(err)
	}

	part := loopPromptPart(LoopScope{ID: "id", Slug: "vendas", Root: root})
	if part == nil {
		t.Fatal("loopPromptPart = nil")
	}
	const open = `<memory scope="loop:vendas">` + "\n"
	start := strings.Index(part.Content, open)
	end := strings.Index(part.Content, "\n</memory>")
	if start < 0 || end < start {
		t.Fatalf("sem bloco de memória do loop:\n%.300s", part.Content)
	}
	block := part.Content[start+len(open) : end]
	if got := estimatedTokens(block); got > loopLongTermFile("vendas").budget {
		t.Fatalf("memória do loop com %d tokens, teto %d", got, loopLongTermFile("vendas").budget)
	}
	if !strings.Contains(block, "loops/vendas/memory/MEMORY.md") {
		t.Fatalf("marcador deveria apontar o arquivo do loop:\n%s", block[max(0, len(block)-1500):])
	}
}

// Arquivo legado com `## ` (antes da migração para `### `): as seções omitidas
// são reconhecidas do mesmo jeito.
func TestFitMemoryToPromptBudget_LegacyLevelTwoHeadings(t *testing.T) {
	content := strings.ReplaceAll(strings.TrimSpace(bigMemory(400)), "### ", "## ")
	out := fitMemoryToPromptBudget(content, assistantLongTermFile)
	if !strings.Contains(out, ": ## Tópico") {
		t.Fatalf("seções `## ` omitidas deveriam ser listadas:\n%s", out[max(0, len(out)-1500):])
	}
}

// A consulta exportada (usada pela ferramenta de memória do control plane para
// avisar o agente depois de uma escrita) devolve o mesmo recorte do prompt.
func TestMemoryPromptUsageFor_MatchesThePromptCut(t *testing.T) {
	big := bigMemory(400)
	cases := []struct {
		relPath string
		file    memoryPromptFile
	}{
		{"memory/MEMORY.md", assistantLongTermFile},
		{"memory/USER.md", learnedOverlayFile("USER.md")},
		{"memory/SOUL.md", learnedOverlayFile("SOUL.md")},
		{"loops/vendas/memory/MEMORY.md", loopLongTermFile("vendas")},
	}
	for _, tc := range cases {
		usage, ok := MemoryPromptUsageFor(tc.relPath, big)
		if !ok {
			t.Fatalf("%s: caminho de memória não reconhecido", tc.relPath)
		}
		fit := fitMemory(strings.TrimSpace(big), tc.file)
		if !usage.Capped || usage.Budget != tc.file.budget || usage.ShownLines != fit.shownLines ||
			usage.TotalLines != fit.totalLines || usage.TotalTokens != fit.totalTokens {
			t.Fatalf("%s: usage %+v difere do recorte do prompt %+v", tc.relPath, usage, fit)
		}

		small, _ := MemoryPromptUsageFor(tc.relPath, "### Curta\n- um fato")
		if small.Capped {
			t.Fatalf("%s: memória pequena não pode aparecer como recortada", tc.relPath)
		}
	}

	for _, other := range []string{"MEMORY.md", "memory/notes.md", "loops//memory/MEMORY.md", "loops/a/b/memory/MEMORY.md"} {
		if _, ok := MemoryPromptUsageFor(other, big); ok {
			t.Fatalf("%q não é um arquivo de memória do prompt", other)
		}
	}
}
