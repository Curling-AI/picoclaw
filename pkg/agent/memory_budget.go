package agent

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// Teto da memória no prompt. (fork)
//
// MEMORY.md, os overlays aprendidos (memory/USER.md, memory/SOUL.md) e a
// memória do loop entravam INTEIROS em toda chamada. São escritos pelo agente,
// sem limite, e o custo é por chamada: uma cron de minuto em minuto paga a
// memória inteira em cada execução. Medido em prod (out/2026, usage_events):
// 133 assistentes com piso de cron (1ª chamada de sessão nova) acima de 50k
// tokens; a mediana desse piso foi de 29k para 61k em três semanas, subindo
// aos poucos — memória crescendo, não conector novo. O trecho do piso acima de
// 50k custou 8% de todo o gasto da semana. Sem memória grande o piso fica em
// ~15k; as outras partes do system prompt já tinham teto.
//
// O teto é aplicado na MONTAGEM do prompt, nunca no arquivo: nada é apagado em
// disco. O corte é uma função pura do conteúdo, para o prefixo cacheado no
// provedor não mudar entre turnos enquanto a memória não muda. Fica o começo do
// arquivo (o mais antigo e consolidado; o que é novo entra no fim) e um
// marcador que lista, pela linha onde começa, cada seção que ficou de fora —
// o agente lê o que precisar com read_file e é avisado de que deve consolidar.

// Orçamentos em tokens ESTIMADOS (tokenizer.EstimateMessageTokens, 2,5
// caracteres por token), marcador incluído. O estimador é conservador para
// português: o número real em tokens do modelo fica abaixo.
//
// Somados (12k + 2×4k + 6k) às notas (16 KB) e ao resto do prompt (~15k reais),
// o pior caso fica perto de 37k tokens reais — abaixo da faixa de 50k onde
// estava o custo. O MEMORY.md tinha p90 de 19 KB (jul/2026); 12k estimados são
// 30k caracteres, então o teto só alcança a cauda.
const (
	longTermPromptBudget       = 12_000
	learnedOverlayPromptBudget = 4_000
	loopMemoryPromptBudget     = 6_000
)

// Teto da lista de seções omitidas no marcador: com milhares de seções, a
// própria lista estouraria o orçamento.
const (
	maxOmittedHeadingsListed = 20
	maxOmittedHeadingRunes   = 80
)

// memoryPromptFile descreve um arquivo de memória que entra no prompt.
type memoryPromptFile struct {
	// path relativo ao workspace, com "/": é o que o agente passa ao read_file.
	path string
	// budget em tokens estimados, marcador incluído.
	budget int
	// recall indica que o recall indexa este arquivo (só o MEMORY.md do
	// assistente); o marcador só manda o agente para lá quando é verdade.
	recall bool
}

var assistantLongTermFile = memoryPromptFile{
	path:   "memory/MEMORY.md",
	budget: longTermPromptBudget,
	recall: true,
}

// learnedOverlayFile é o overlay gravável memory/<name> (USER.md ou SOUL.md).
func learnedOverlayFile(name string) memoryPromptFile {
	return memoryPromptFile{path: path.Join("memory", name), budget: learnedOverlayPromptBudget}
}

// loopLongTermFile é a memória do loop, <workspace>/loops/<slug>/memory/MEMORY.md.
func loopLongTermFile(slug string) memoryPromptFile {
	return memoryPromptFile{
		path:   path.Join(loopsDirName, slug, "memory", "MEMORY.md"),
		budget: loopMemoryPromptBudget,
	}
}

func estimateTextTokens(s string) int {
	return tokenizer.EstimateMessageTokens(providers.Message{Content: s})
}

// memoryFit é o resultado do recorte, com os números para o log. Linhas são
// as do arquivo em disco.
type memoryFit struct {
	text        string
	capped      bool
	totalTokens int
	shownTokens int
	totalLines  int
	shownLines  int // última linha mostrada inteira
	partialLine bool
	omitted     int
}

// fitMemoryToPromptBudget devolve o conteúdo como deve entrar no prompt:
// aparado e inteiro quando cabe no orçamento, recortado com marcador quando não.
func fitMemoryToPromptBudget(raw string, file memoryPromptFile) string {
	return fitMemory(raw, file).text
}

// filePos é um ponto do arquivo em disco: linha (1-based, o start_line do
// read_file por linhas) e byte (o offset do read_file por bytes, o do Maestro).
type filePos struct{ line, offset int }

// trimmedLines é o conteúdo aparado partido em linhas, com a posição de cada
// linha no arquivo em disco: o prompt mostra o aparado, mas o agente lê o
// arquivo, então o marcador precisa das coordenadas dele.
type trimmedLines struct {
	content string
	lines   []string
	starts  []int // byte de cada linha dentro de content; starts[len(lines)] = len(content)+1
	origin  filePos
}

func splitTrimmed(raw string) trimmedLines {
	lead := len(raw) - len(strings.TrimLeftFunc(raw, unicode.IsSpace))
	content := strings.TrimSpace(raw)
	lines := strings.Split(content, "\n")
	starts := make([]int, len(lines)+1)
	for i, ln := range lines {
		starts[i+1] = starts[i] + len(ln) + 1
	}
	return trimmedLines{
		content: content,
		lines:   lines,
		starts:  starts,
		origin:  filePos{line: strings.Count(raw[:lead], "\n"), offset: lead},
	}
}

// at é a posição no arquivo do byte `b` da linha `i` do conteúdo aparado.
func (t trimmedLines) at(i, b int) filePos {
	return filePos{line: t.origin.line + i + 1, offset: t.origin.offset + t.starts[i] + b}
}

func fitMemory(raw string, file memoryPromptFile) memoryFit {
	content := strings.TrimSpace(raw)
	total := estimateTextTokens(content)
	if total <= file.budget {
		return memoryFit{text: content, totalTokens: total}
	}

	tl := splitTrimmed(raw)
	bodyBudget := max(0, file.budget-markerReserveTokens(file))
	shown := shownLineCount(tl, bodyBudget)

	body := tl.content[:max(0, tl.starts[shown]-1)]
	shownLines := tl.lines[:shown]
	partial := ""
	if shown == 0 {
		partial = runePrefixWithin(tl.lines[0], bodyBudget)
		body = partial
		shownLines = tl.lines[:1]
	}
	if _, fenced := scanShown(shownLines); fenced {
		body += "\n```"
	}

	entries, more := omittedEntries(tl, shown, len(partial))
	fit := memoryFit{
		capped:      true,
		totalTokens: total,
		shownTokens: estimateTextTokens(body),
		totalLines:  tl.origin.line + len(tl.lines),
		shownLines:  tl.origin.line + shown,
		partialLine: shown == 0,
		omitted:     len(entries) + more,
	}
	fit.text = body + "\n\n" + omissionMarker(file, fit, entries, more)
	return fit
}

// shownLineCount é o maior k tal que as k primeiras linhas cabem em budget.
// O prefixo de k linhas é um prefixo do próprio conteúdo, então a busca não
// aloca, e o estimador é monotônico no tamanho do prefixo.
func shownLineCount(tl trimmedLines, budget int) int {
	return sort.Search(len(tl.lines)+1, func(k int) bool {
		return k > 0 && estimateTextTokens(tl.content[:tl.starts[k]-1]) > budget
	}) - 1
}

// runePrefixWithin corta uma linha maior que o orçamento numa fronteira de runa.
func runePrefixWithin(line string, budget int) string {
	runes := []rune(line)
	n := sort.Search(len(runes)+1, func(k int) bool {
		return k > 0 && estimateTextTokens(string(runes[:k])) > budget
	}) - 1
	return string(runes[:max(0, n)])
}

func isFenceLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "```")
}

// headingText reconhece os níveis que a memória usa como seção: `### ` (o
// atual), `## ` (antes da migração) e `# `.
func headingText(line string) (string, bool) {
	for _, prefix := range []string{"# ", "## ", "### "} {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line), true
		}
	}
	return "", false
}

type omittedEntry struct {
	filePos
	text string
}

// omittedEntries lista o que ficou de fora depois das `shown` primeiras linhas
// (ou dos `partialBytes` primeiros bytes da linha 1, quando nem ela coube):
// primeiro a continuação da seção cortada no meio, se houver, depois cada
// título fora de bloco de código. Devolve no máximo maxOmittedHeadingsListed
// entradas e quantos títulos sobraram além delas.
func omittedEntries(tl trimmedLines, shown, partialBytes int) ([]omittedEntry, int) {
	var entries []omittedEntry
	more := 0
	add := func(e omittedEntry) {
		if len(entries) < maxOmittedHeadingsListed {
			entries = append(entries, e)
			return
		}
		more++
	}

	current, fenced := scanShown(tl.lines[:shown])
	start := shown
	if shown == 0 {
		add(omittedEntry{filePos: tl.at(0, partialBytes), text: continuation("")})
		if isFenceLine(tl.lines[0]) {
			fenced = !fenced
		}
		start = 1
	} else if next := firstNonBlank(tl.lines, shown); next < len(tl.lines) {
		if _, ok := headingText(tl.lines[next]); !ok || fenced {
			add(omittedEntry{filePos: tl.at(next, 0), text: continuation(current)})
		}
	}

	for i := start; i < len(tl.lines); i++ {
		if isFenceLine(tl.lines[i]) {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if h, ok := headingText(tl.lines[i]); ok {
			add(omittedEntry{filePos: tl.at(i, 0), text: truncateHeading(h)})
		}
	}
	return entries, more
}

// scanShown devolve o último título fora de bloco de código no trecho mostrado
// e se o trecho termina dentro de um bloco aberto.
func scanShown(lines []string) (heading string, fenced bool) {
	for _, ln := range lines {
		if isFenceLine(ln) {
			fenced = !fenced
			continue
		}
		if h, ok := headingText(ln); ok && !fenced {
			heading = h
		}
	}
	return heading, fenced
}

func firstNonBlank(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return len(lines)
}

func continuation(heading string) string {
	if heading == "" {
		return "(continues)"
	}
	return "(continues " + truncateHeading(heading) + ")"
}

func truncateHeading(h string) string {
	if utf8.RuneCountInString(h) <= maxOmittedHeadingRunes {
		return h
	}
	return string([]rune(h)[:maxOmittedHeadingRunes]) + "…"
}

func omissionMarker(file memoryPromptFile, fit memoryFit, entries []omittedEntry, more int) string {
	var sb strings.Builder
	shownWhat := fmt.Sprintf("it up to line %d of %d", fit.shownLines, fit.totalLines)
	if fit.partialLine {
		shownWhat = fmt.Sprintf("only the start of line %d of %d", fit.shownLines+1, fit.totalLines)
	}
	fmt.Fprintf(&sb, "[%s is over its prompt budget: this shows %s (~%d of ~%d estimated tokens). "+
		"The file is intact on disk.", file.path, shownWhat, fit.shownTokens, fit.totalTokens)
	how := fmt.Sprintf("read it with read_file on %q from that offset (or start_line, if your "+
		"read_file reads by line)", file.path)
	if file.recall {
		how += ", or search it with recall"
	}
	fmt.Fprintf(&sb, " Not shown, by where each part starts — %s:\n", how)
	for _, e := range entries {
		fmt.Fprintf(&sb, "- offset %d, line %d: %s\n", e.offset, e.line, e.text)
	}
	if more > 0 {
		fmt.Fprintf(&sb, "- … and %d more sections\n", more)
	}
	sb.WriteString("Consolidate this file — merge duplicates, drop what is stale — so what matters fits.]")
	return sb.String()
}

// markerReserveTokens é o tamanho do PIOR marcador possível para o arquivo:
// todas as entradas com título no teto de runas e números grandes. Reservar o
// pior caso garante o orçamento sem depender de onde o corte cai.
func markerReserveTokens(file memoryPromptFile) int {
	const big = 9_999_999_999
	worst := make([]omittedEntry, maxOmittedHeadingsListed)
	heading := "### " + strings.Repeat("w", maxOmittedHeadingRunes) + "…"
	for i := range worst {
		worst[i] = omittedEntry{filePos: filePos{line: big, offset: big}, text: "(continues " + heading + ")"}
	}
	fit := memoryFit{shownTokens: big, totalTokens: big, totalLines: big, shownLines: big}
	// "\n```" do fechamento de cerca e o "\n\n" antes do marcador.
	return estimateTextTokens(omissionMarker(file, fit, worst, big)+"\n```\n\n") + 1
}

// MemoryPromptUsage diz quanto de um arquivo de memória entra no prompt.
type MemoryPromptUsage struct {
	Capped      bool
	Budget      int // tokens estimados, marcador incluído
	TotalTokens int // tokens estimados do arquivo inteiro
	ShownLines  int
	TotalLines  int
}

// MemoryPromptUsageFor roda, sobre o conteúdo de um arquivo de memória, o mesmo
// recorte da montagem do prompt. É o que a ferramenta de memória usa para
// avisar o agente, depois de uma escrita, de que o arquivo passou do teto.
// relPath é relativo ao workspace: memory/MEMORY.md, memory/USER.md,
// memory/SOUL.md ou loops/<slug>/memory/MEMORY.md; ok é false para outro.
func MemoryPromptUsageFor(relPath, content string) (MemoryPromptUsage, bool) {
	file, ok := memoryPromptFileFor(relPath)
	if !ok {
		return MemoryPromptUsage{}, false
	}
	fit := fitMemory(content, file)
	return MemoryPromptUsage{
		Capped:      fit.capped,
		Budget:      file.budget,
		TotalTokens: fit.totalTokens,
		ShownLines:  fit.shownLines,
		TotalLines:  fit.totalLines,
	}, true
}

func memoryPromptFileFor(relPath string) (memoryPromptFile, bool) {
	clean := path.Clean(filepath.ToSlash(relPath))
	switch clean {
	case assistantLongTermFile.path:
		return assistantLongTermFile, true
	case "memory/USER.md", "memory/SOUL.md":
		return learnedOverlayFile(path.Base(clean)), true
	}
	parts := strings.Split(clean, "/")
	if len(parts) == 4 && parts[0] == loopsDirName && parts[1] != "" &&
		parts[2] == "memory" && parts[3] == "MEMORY.md" {
		return loopLongTermFile(parts[1]), true
	}
	return memoryPromptFile{}, false
}

// capMemoryForPrompt é o lado imperativo: recorta e registra o recorte uma vez
// por arquivo e tamanho. Partes de prompt fora do cache (perfis de turno com
// ferramentas restritas, loops) são remontadas a cada turno; sem a
// deduplicação seria uma linha de log por chamada de cron.
func capMemoryForPrompt(content string, file memoryPromptFile, absPath string) string {
	fit := fitMemory(content, file)
	if fit.capped {
		logMemoryCap(absPath, file, fit)
	}
	return fit.text
}

var memoryCapLogged sync.Map // absPath -> totalTokens já registrado

func logMemoryCap(absPath string, file memoryPromptFile, fit memoryFit) {
	if last, ok := memoryCapLogged.Load(absPath); ok && last.(int) == fit.totalTokens {
		return
	}
	memoryCapLogged.Store(absPath, fit.totalTokens)
	logger.InfoCF("memory", "Memory capped for the prompt", map[string]any{
		"path":         file.path,
		"budget":       file.budget,
		"total_tokens": fit.totalTokens,
		"shown_tokens": fit.shownTokens,
		"total_lines":  fit.totalLines,
		"shown_lines":  fit.shownLines,
		"omitted":      fit.omitted,
	})
}
