package agent

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
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

// memoryFit é o resultado do recorte, com os números para o log.
type memoryFit struct {
	text        string
	capped      bool
	totalTokens int
	shownTokens int
	totalLines  int
	shownLines  int
	omitted     int
}

// fitMemoryToPromptBudget devolve o conteúdo como deve entrar no prompt:
// inteiro quando cabe no orçamento, recortado com marcador quando não.
func fitMemoryToPromptBudget(content string, file memoryPromptFile) string {
	return fitMemory(content, file).text
}

func fitMemory(content string, file memoryPromptFile) memoryFit {
	total := estimateTextTokens(content)
	if total <= file.budget {
		return memoryFit{text: content, totalTokens: total}
	}

	lines := strings.Split(content, "\n")
	bodyBudget := max(0, file.budget-markerReserveTokens(file))
	shown := shownLineCount(content, lines, bodyBudget)

	partialFirstLine := shown == 0
	body := strings.Join(lines[:shown], "\n")
	shownLines := lines[:shown]
	if partialFirstLine {
		body = runePrefixWithin(lines[0], bodyBudget)
		shownLines = lines[:1]
	}
	if _, fenced := scanShown(shownLines); fenced {
		body += "\n```"
	}

	entries, more := omittedEntries(lines, shown, partialFirstLine)
	fit := memoryFit{
		capped:      true,
		totalTokens: total,
		shownTokens: estimateTextTokens(body),
		totalLines:  len(lines),
		shownLines:  shown,
		omitted:     len(entries) + more,
	}
	fit.text = body + "\n\n" + omissionMarker(file, fit, entries, more)
	return fit
}

// shownLineCount é o maior k tal que as k primeiras linhas cabem em budget.
// O prefixo de k linhas é um prefixo do próprio content, então a busca não
// aloca, e o estimador é monotônico no tamanho do prefixo.
func shownLineCount(content string, lines []string, budget int) int {
	ends := make([]int, len(lines)+1)
	for i, ln := range lines {
		ends[i+1] = ends[i] + len(ln) + 1
	}
	prefix := func(k int) string {
		if k == 0 {
			return ""
		}
		return content[:ends[k]-1]
	}
	return sort.Search(len(lines)+1, func(k int) bool {
		return k > 0 && estimateTextTokens(prefix(k)) > budget
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
	line int // 1-based, como o start_line do read_file
	text string
}

// omittedEntries lista o que ficou de fora a partir da linha shown+1: primeiro
// a continuação da seção cortada no meio, se houver, depois cada título fora de
// bloco de código. Devolve no máximo maxOmittedHeadingsListed entradas e
// quantas títulos sobraram além delas.
func omittedEntries(lines []string, shown int, partialFirstLine bool) ([]omittedEntry, int) {
	var entries []omittedEntry
	more := 0
	add := func(e omittedEntry) {
		if len(entries) < maxOmittedHeadingsListed {
			entries = append(entries, e)
			return
		}
		more++
	}

	current, fenced := scanShown(lines[:shown])
	start := shown
	if partialFirstLine {
		add(omittedEntry{line: 1, text: continuation("")})
		if isFenceLine(lines[0]) {
			fenced = !fenced
		}
		start = 1
	} else if next := firstNonBlank(lines, shown); next < len(lines) {
		if _, ok := headingText(lines[next]); !ok || fenced {
			add(omittedEntry{line: next + 1, text: continuation(current)})
		}
	}

	for i := start; i < len(lines); i++ {
		if isFenceLine(lines[i]) {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if h, ok := headingText(lines[i]); ok {
			add(omittedEntry{line: i + 1, text: truncateHeading(h)})
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
	shownWhat := fmt.Sprintf("lines 1–%d of %d", fit.shownLines, fit.totalLines)
	if fit.shownLines == 0 {
		shownWhat = fmt.Sprintf("the start of line 1 of %d", fit.totalLines)
	}
	fmt.Fprintf(&sb, "[%s is over its prompt budget: this shows %s (~%d of ~%d estimated tokens). "+
		"The file is intact on disk.", file.path, shownWhat, fit.shownTokens, fit.totalTokens)
	how := fmt.Sprintf("read it with read_file (path %q, start_line)", file.path)
	if file.recall {
		how += " or search it with recall"
	}
	fmt.Fprintf(&sb, " Not shown, by the line where it starts — %s:\n", how)
	for _, e := range entries {
		fmt.Fprintf(&sb, "- line %d: %s\n", e.line, e.text)
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
	worst := make([]omittedEntry, maxOmittedHeadingsListed)
	heading := "### " + strings.Repeat("w", maxOmittedHeadingRunes) + "…"
	for i := range worst {
		worst[i] = omittedEntry{line: 9_999_999, text: "(continues " + heading + ")"}
	}
	fit := memoryFit{shownTokens: 9_999_999, totalTokens: 9_999_999, totalLines: 9_999_999, shownLines: 9_999_999}
	// "\n```" do fechamento de cerca e o "\n\n" antes do marcador.
	return estimateTextTokens(omissionMarker(file, fit, worst, 9_999_999)+"\n```\n\n") + 1
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
