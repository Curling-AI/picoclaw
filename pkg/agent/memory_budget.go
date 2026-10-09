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
// arquivo (o mais antigo e consolidado) e um marcador que lista, pelo byte e
// pela linha onde começa, o que ficou de fora — as primeiras e as últimas
// partes, porque uma seção nova entra no fim. O agente lê o que precisar com
// read_file.

// Orçamentos em tokens ESTIMADOS (tokenizer.EstimateMessageTokens, 2,5
// caracteres por token), marcador incluído. O estimador é conservador para
// português: medido no kind, ~0,8 token real por token estimado.
//
// Somados (12k + 2×4k + 6k estimados) às notas (16 KB) e ao resto do prompt
// (~11–15k reais), o pior caso fica perto de 40k tokens reais — abaixo da
// faixa de 50k onde estava o custo. O MEMORY.md tinha p90 de 19 KB (jul/2026); 12k estimados são
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

// memoryFit é o resultado do recorte. Linhas e offsets são os do arquivo em
// disco.
type memoryFit struct {
	text        string
	capped      bool
	totalTokens int
	shownTokens int
	totalLines  int
	shownLines  int  // última linha mostrada inteira
	partialLine bool // a linha seguinte aparece só no começo
	// hidden são TODAS as partes que ficaram de fora, em ordem; o marcador lista
	// só as primeiras e as últimas.
	hidden []omittedEntry
	// hiddenSections são as seções que o agente não vê inteiras: a cortada no
	// meio e as escondidas, com o texto exato. É o que uma reescrita do arquivo
	// inteiro feita a partir do prompt apagaria ou trocaria.
	hiddenSections []HiddenMemorySection
	markerTokens   int
}

// filePos é um ponto do arquivo em disco: linha (1-based, o start_line do
// read_file por linhas) e byte (o offset do read_file por bytes, o do Maestro).
type filePos struct{ line, offset int }

// trimmedLines é o conteúdo aparado partido em linhas, com a posição de cada
// linha no arquivo em disco: o prompt mostra o aparado, mas o agente lê o
// arquivo, então o marcador precisa das coordenadas dele.
type trimmedLines struct {
	raw     string
	content string
	lines   []string
	starts  []int // byte de cada linha dentro de content; starts[len(lines)] = len(content)+1
	origin  filePos
	code    []bool   // a linha é de bloco de código cercado (as cercas incluídas)
	open    []string // a cerca aberta depois da linha, "" fora de bloco
}

func splitTrimmed(raw string) trimmedLines {
	lead := len(raw) - len(strings.TrimLeftFunc(raw, unicode.IsSpace))
	content := strings.TrimSpace(raw)
	lines := strings.Split(content, "\n")
	starts := make([]int, len(lines)+1)
	for i, ln := range lines {
		starts[i+1] = starts[i] + len(ln) + 1
	}
	code, open := markCode(lines)
	return trimmedLines{
		raw:     raw,
		content: content,
		lines:   lines,
		starts:  starts,
		origin:  filePos{line: strings.Count(raw[:lead], "\n"), offset: lead},
		code:    code,
		open:    open,
	}
}

// lastLine é a linha do arquivo em disco onde o conteúdo aparado termina.
func lastLine(raw, content string) int {
	if content == "" {
		return 0
	}
	lead := len(raw) - len(strings.TrimLeftFunc(raw, unicode.IsSpace))
	return strings.Count(raw[:lead], "\n") + strings.Count(content, "\n") + 1
}

// span é o trecho do arquivo em disco das linhas [from, to) do conteúdo
// aparado, sem o \n final. Nas pontas do arquivo volta o que o TrimSpace tirou
// da própria linha: a indentação da primeira e o espaço no fim da última
// (quebra de linha forçada em markdown).
func (t trimmedLines) span(from, to int) string {
	lead := t.origin.offset
	start, end := lead+t.starts[from], lead+t.starts[to]-1
	if from == 0 {
		start = strings.LastIndexByte(t.raw[:lead], '\n') + 1
	}
	if to == len(t.lines) {
		if nl := strings.IndexByte(t.raw[end:], '\n'); nl >= 0 {
			end += nl
		} else {
			end = len(t.raw)
		}
	}
	return t.raw[start:end]
}

// at é a posição no arquivo do byte `b` da linha `i` do conteúdo aparado.
func (t trimmedLines) at(i, b int) filePos {
	return filePos{line: t.origin.line + i + 1, offset: t.origin.offset + t.starts[i] + b}
}

// memoryCut é o trecho mostrado: as `shown` primeiras linhas inteiras e, quando
// a linha seguinte sozinha nunca caberia, os `partial` primeiros bytes dela. O
// trecho é sempre o prefixo content[:end].
type memoryCut struct {
	shown, partial, end int
}

func fitMemory(raw string, file memoryPromptFile) memoryFit {
	content := strings.TrimSpace(raw)
	total := estimateTextTokens(content)
	if total <= file.budget {
		n := lastLine(raw, content)
		return memoryFit{text: content, totalTokens: total, totalLines: n, shownLines: n}
	}

	// Duas passadas, ambas função só do conteúdo. A primeira reserva o PIOR
	// marcador possível, então sempre cabe. A segunda reserva o marcador real
	// do primeiro corte, que costuma ter metade do tamanho, e só vale se couber:
	// num overlay de 4k, a reserva do pior caso sozinha leva um terço do teto.
	tl := splitTrimmed(raw)
	fit := fitBody(tl, file, total, file.budget-markerReserveTokens(file))
	tighter := renderCut(tl, file, total, cutWithin(tl, file.budget-fit.markerTokens-fenceCloseTokens))
	if estimateTextTokens(tighter.text) <= file.budget {
		fit = tighter
	}
	return fit
}

// fitBody corta o corpo para caber em bodyBudget. Uma cerca de código mais
// longa do que a reserva cobre fecha com o comprimento real: o excesso sai do
// corpo e corta de novo. Com o corpo já vazio não há mais o que tirar, e o
// resultado fica acima do teto; só acontece se o marcador sozinho passar do
// teto (um slug de loop com milhares de caracteres).
func fitBody(tl trimmedLines, file memoryPromptFile, total, bodyBudget int) memoryFit {
	fit := renderCut(tl, file, total, cutWithin(tl, bodyBudget))
	for over := estimateTextTokens(fit.text) - file.budget; over > 0 && bodyBudget > 0; {
		bodyBudget -= over
		fit = renderCut(tl, file, total, cutWithin(tl, bodyBudget))
		over = estimateTextTokens(fit.text) - file.budget
	}
	return fit
}

// fenceCloseTokens cobre o "\n" + cerca (até 10 caracteres) que fecha um bloco de
// código aberto no corte e o "\n\n" antes do marcador. Cerca mais longa é
// tratada em fitBody.
const fenceCloseTokens = (1 + 10 + 2 + 4) * 2 / 5

// cutWithin acha o maior trecho que cabe em budget: linhas inteiras, por busca
// binária sobre prefixos (o estimador é monotônico no tamanho do prefixo).
func cutWithin(tl trimmedLines, budget int) memoryCut {
	budget = max(0, budget)
	shown := sort.Search(len(tl.lines)+1, func(k int) bool {
		return k > 0 && estimateTextTokens(tl.content[:tl.starts[k]-1]) > budget
	}) - 1
	cut := memoryCut{shown: shown, end: max(0, tl.starts[shown]-1)}
	if shown == len(tl.lines) || estimateTextTokens(tl.lines[shown]) <= budget {
		return cut
	}
	// A linha seguinte não caberia nem sozinha (um JSON colado na memória):
	// mostra o começo dela, cortado numa fronteira de runa, em vez de esconder
	// tudo dali em diante.
	// A busca é direto sobre o byte, sem montar a lista de runas: uma linha de
	// MB viraria centenas de MB alocados a cada montagem do prompt.
	line := tl.lines[shown]
	n := sort.Search(len(line)+1, func(b int) bool {
		return estimateTextTokens(tl.content[:tl.starts[shown]+b]) > budget
	}) - 1
	for n > 0 && n < len(line) && !utf8.RuneStart(line[n]) {
		n--
	}
	if n > 0 {
		cut.partial = n
		cut.end = tl.starts[shown] + n
	}
	return cut
}

func renderCut(tl trimmedLines, file memoryPromptFile, total int, cut memoryCut) memoryFit {
	shown := cut.shown
	if cut.partial > 0 {
		shown++
	}
	current, currentIdx, fence := tl.scanShown(shown)
	body := tl.content[:cut.end]
	if fence != "" {
		body += "\n" + fence
	}

	hidden, continues := hiddenParts(tl, cut, current)
	fit := memoryFit{
		capped:      true,
		totalTokens: total,
		shownTokens: estimateTextTokens(body),
		totalLines:  tl.origin.line + len(tl.lines),
		shownLines:  tl.origin.line + cut.shown,
		partialLine: cut.partial > 0,
		hidden:      hidden,
	}
	fit.hiddenSections = hiddenSections(tl, hidden, continues, current, currentIdx)
	marker := omissionMarker(file, fit)
	fit.markerTokens = estimateTextTokens(marker)
	fit.text = body + "\n\n" + marker
	return fit
}

// HiddenMemorySection é uma seção que o prompt não mostra inteira. Text é o
// trecho exato do arquivo, do título até antes do próximo (ou até o fim da
// última linha com texto); Heading é "" para o texto antes da primeira seção.
type HiddenMemorySection struct {
	Heading string
	Text    string
}

// hiddenSections recorta, do arquivo, cada seção que o agente não vê inteira:
// a cortada no meio (desde o título dela, que aparece) e as escondidas. São
// substrings do arquivo, sem cópia.
func hiddenSections(
	tl trimmedLines,
	hidden []omittedEntry,
	continues bool,
	current string,
	currentIdx int,
) []HiddenMemorySection {
	type span struct {
		heading string
		from    int
	}
	var spans []span
	if continues {
		spans = append(spans, span{heading: current, from: max(currentIdx, 0)})
	}
	for _, e := range hidden {
		if !e.continues {
			spans = append(spans, span{heading: e.heading, from: e.idx})
		}
	}
	out := make([]HiddenMemorySection, len(spans))
	for k, sp := range spans {
		to := len(tl.lines)
		if k+1 < len(spans) {
			to = spans[k+1].from
		}
		out[k] = HiddenMemorySection{Heading: sp.heading, Text: tl.span(sp.from, to)}
	}
	return out
}

// fenceOf devolve a cerca que a linha abriria (a sequência de três ou mais ` ou
// ~ do começo), ou "". Como no CommonMark, numa cerca de crases o resto da
// linha não tem crase: "```select 1``` inline" é código inline, não cerca.
func fenceOf(line string) string {
	t := strings.TrimSpace(line)
	if len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return ""
	}
	n := len(t) - len(strings.TrimLeft(t, t[:1]))
	if n < 3 || (t[0] == '`' && strings.IndexByte(t[n:], '`') >= 0) {
		return ""
	}
	return t[:n]
}

// closingFence devolve o caractere e o comprimento de uma linha que só tem
// cerca, ou n = 0. Como no CommonMark, a cerca de fechamento não tem texto
// depois.
func closingFence(line string) (c byte, n int) {
	f := fenceOf(line)
	if f == "" || strings.TrimSpace(line) != f {
		return 0, 0
	}
	return f[0], len(f)
}

func fenceKind(c byte) int {
	if c == '~' {
		return 1
	}
	return 0
}

// markCode marca as linhas de bloco de código cercado e a cerca aberta depois
// de cada uma. Fecha com o mesmo caractere e pelo menos o mesmo comprimento da
// abertura. Uma cerca que nunca fecha não abre bloco: no CommonMark ela iria até
// o fim do arquivo, mas numa memória é uma escrita cortada no meio, e engoliria
// da lista todas as seções gravadas depois dela.
func markCode(lines []string) (code []bool, open []string) {
	// longest[i] é a maior cerca de fechamento de cada caractere da linha i em
	// diante: diz, sem varrer de novo, se uma abertura vai fechar.
	longest := make([][2]int, len(lines)+1)
	for i := len(lines) - 1; i >= 0; i-- {
		longest[i] = longest[i+1]
		if c, n := closingFence(lines[i]); n > 0 {
			longest[i][fenceKind(c)] = max(longest[i][fenceKind(c)], n)
		}
	}
	code = make([]bool, len(lines))
	open = make([]string, len(lines))
	fence := ""
	for i, ln := range lines {
		if fence != "" {
			code[i] = true
			if c, n := closingFence(ln); c == fence[0] && n >= len(fence) {
				fence = ""
			}
		} else if f := fenceOf(ln); f != "" && longest[i+1][fenceKind(f[0])] >= len(f) {
			fence, code[i] = f, true
		}
		open[i] = fence
	}
	return code, open
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

// scanShown devolve o último título fora de bloco de código nas n primeiras
// linhas (e o índice da linha dele, -1 sem título) e a cerca que ficou aberta
// no fim delas ("" se nenhuma).
func (t trimmedLines) scanShown(n int) (heading string, idx int, fence string) {
	if n > 0 {
		fence = t.open[n-1]
	}
	for i := n - 1; i >= 0; i-- {
		if h, ok := headingText(t.lines[i]); ok && !t.code[i] {
			return h, i, fence
		}
	}
	return "", -1, fence
}

type omittedEntry struct {
	filePos
	idx       int // linha em trimmedLines.lines
	heading   string
	continues bool // resto da seção cortada no meio, não um título
}

// hiddenParts lista tudo o que ficou de fora depois do corte: primeiro a
// continuação da seção cortada no meio, se houver, depois cada título fora de
// bloco de código. Devolve também se houve continuação.
func hiddenParts(tl trimmedLines, cut memoryCut, current string) ([]omittedEntry, bool) {
	var parts []omittedEntry
	start := cut.shown
	continues := false
	if cut.partial > 0 {
		parts = append(parts, omittedEntry{filePos: tl.at(cut.shown, cut.partial), heading: current, continues: true})
		continues = true
		start = cut.shown + 1
	} else if next := firstNonBlank(tl.lines, cut.shown); next < len(tl.lines) {
		if _, ok := headingText(tl.lines[next]); !ok || tl.code[next] {
			parts = append(parts, omittedEntry{filePos: tl.at(next, 0), heading: current, continues: true})
			continues = true
		}
	}
	for i := start; i < len(tl.lines); i++ {
		if tl.code[i] {
			continue
		}
		if h, ok := headingText(tl.lines[i]); ok {
			parts = append(parts, omittedEntry{filePos: tl.at(i, 0), idx: i, heading: h})
		}
	}
	return parts, continues
}

func firstNonBlank(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return len(lines)
}

func (e omittedEntry) label() string {
	h := e.heading
	if utf8.RuneCountInString(h) > maxOmittedHeadingRunes {
		h = string([]rune(h)[:maxOmittedHeadingRunes]) + "…"
	}
	if !e.continues {
		return h
	}
	if h == "" {
		return "(continues)"
	}
	return "(continues " + h + ")"
}

// omissionMarker só descreve, de propósito. Uma ordem de enxugar iria em
// TODO prompt do assistente, inclusive de cron de minuto em minuto, e
// empurraria o agente a reescrever o arquivo a partir do que vê — justamente o
// que apagaria o que não vê. O pedido de consolidar fica no retorno da escrita.
func omissionMarker(file memoryPromptFile, fit memoryFit) string {
	var sb strings.Builder
	shownWhat := fmt.Sprintf("it up to line %d of %d", fit.shownLines, fit.totalLines)
	if fit.partialLine {
		shownWhat = fmt.Sprintf("it up to the start of line %d of %d", fit.shownLines+1, fit.totalLines)
	}
	fmt.Fprintf(&sb, "[%s is over its prompt budget: this shows %s (~%d of ~%d estimated tokens). "+
		"The file is intact on disk.", file.path, shownWhat, fit.shownTokens, fit.totalTokens)
	how := fmt.Sprintf("read it with read_file on %q from that offset (or start_line, if your "+
		"read_file reads by line)", file.path)
	if file.recall {
		how += ", or search it with recall"
	}
	fmt.Fprintf(&sb, " Not shown, by where each part starts — %s:\n", how)

	// As primeiras e as ÚLTIMAS: uma seção nova entra no fim do arquivo, e sem
	// as últimas ela sumiria dentro do "… e mais N".
	parts := fit.hidden
	head, tail := parts, []omittedEntry(nil)
	if len(parts) > maxOmittedHeadingsListed {
		half := maxOmittedHeadingsListed / 2
		head, tail = parts[:half], parts[len(parts)-half:]
	}
	for _, e := range head {
		fmt.Fprintf(&sb, "- offset %d, line %d: %s\n", e.offset, e.line, e.label())
	}
	if tail != nil {
		skipped := len(parts) - len(head) - len(tail)
		fmt.Fprintf(&sb, "- … %d more sections between offset %d and offset %d\n",
			skipped, parts[len(head)].offset, tail[0].offset)
		for _, e := range tail {
			fmt.Fprintf(&sb, "- offset %d, line %d: %s\n", e.offset, e.line, e.label())
		}
	}
	sb.WriteString("Never rewrite this whole file from this partial view: what is not shown would be " +
		"lost. Change one section at a time.]")
	return sb.String()
}

// markerReserveTokens é o tamanho do PIOR marcador possível para o arquivo:
// todas as entradas com título no teto de runas, a linha do meio e números
// grandes. Reservar o pior caso garante o orçamento sem depender do corte.
func markerReserveTokens(file memoryPromptFile) int {
	const big = 9_999_999_999
	worst := make([]omittedEntry, maxOmittedHeadingsListed+1)
	heading := "### " + strings.Repeat("w", maxOmittedHeadingRunes)
	for i := range worst {
		worst[i] = omittedEntry{filePos: filePos{line: big, offset: big}, heading: heading, continues: true}
	}
	fit := memoryFit{
		shownTokens: big,
		totalTokens: big,
		totalLines:  big,
		shownLines:  big,
		partialLine: true,
		hidden:      worst,
	}
	return estimateTextTokens(omissionMarker(file, fit)) + fenceCloseTokens
}

// MemoryPromptUsage diz quanto de um arquivo de memória entra no prompt.
type MemoryPromptUsage struct {
	Capped      bool
	Budget      int // tokens estimados, marcador incluído
	TotalTokens int // tokens estimados do arquivo inteiro
	ShownLines  int // última linha mostrada inteira; igual a TotalLines abaixo do teto
	TotalLines  int
	PartialLine bool // a linha ShownLines+1 aparece só no começo
	// HiddenSections são as seções que o prompt não mostra inteiras, com o
	// texto exato. Uma reescrita do arquivo inteiro feita a partir do prompt não
	// tem como repeti-las; a ferramenta de memória recusa a que não as traga
	// iguais.
	HiddenSections []HiddenMemorySection
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
		Capped:         fit.capped,
		Budget:         file.budget,
		TotalTokens:    fit.totalTokens,
		ShownLines:     fit.shownLines,
		TotalLines:     fit.totalLines,
		PartialLine:    fit.partialLine,
		HiddenSections: fit.hiddenSections,
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
		"omitted":      len(fit.hidden),
	})
}
