package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	runtimeevents "github.com/sipeed/picoclaw/pkg/events"
)

func newLoopTestAgent(workspace string) *AgentInstance {
	return &AgentInstance{ID: "main", Workspace: workspace}
}

func newLoopTestLoop(t *testing.T) *AgentLoop {
	t.Helper()
	return NewAgentLoop(&config.Config{}, bus.NewMessageBus(), &simpleMockProvider{response: "ok"})
}

// A validação existe porque o Root vira caminho de ESCRITA: é para lá que o cold
// path do evolution grava skills. Um Root fora de loops/ faria um loop escrever
// nas skills globais do assistente.
func TestResolveLoop_RejeitaRootForaDoDiretorioDeLoops(t *testing.T) {
	workspace := t.TempDir()
	agent := newLoopTestAgent(workspace)

	cases := map[string]string{
		"irmão do workspace":     filepath.Join(filepath.Dir(workspace), "outro"),
		"skills globais":         filepath.Join(workspace, "skills"),
		"a própria raiz de loop": filepath.Join(workspace, "loops"),
		"traversal com ..":       filepath.Join(workspace, "loops", "..", "skills"),
		"prefixo parecido":       workspace + "-loops/vendas",
		"absoluto alheio":        "/etc",
	}

	for name, root := range cases {
		t.Run(name, func(t *testing.T) {
			al := newLoopTestLoop(t)
			al.SetLoopResolver(func(string) LoopScope {
				return LoopScope{ID: "id", Slug: "vendas", Root: root}
			})
			if got := al.resolveLoop(agent, "agent:web-abc"); got.Active() {
				t.Fatalf("resolveLoop(%q).Root = %q, want inativo", root, got.Root)
			}
		})
	}
}

func TestResolveLoop_AceitaRootValidoENormaliza(t *testing.T) {
	workspace := t.TempDir()
	agent := newLoopTestAgent(workspace)
	al := newLoopTestLoop(t)

	// Passa sujo de propósito: o control-plane monta esse caminho por
	// concatenação e o Clean é parte do contrato.
	al.SetLoopResolver(func(string) LoopScope {
		return LoopScope{ID: "id", Slug: "vendas", Root: workspace + "/loops/./vendas/"}
	})

	got := al.resolveLoop(agent, "agent:web-abc")
	want := filepath.Join(workspace, "loops", "vendas")
	if got.Root != want {
		t.Fatalf("Root = %q, want %q", got.Root, want)
	}
	if got.Slug != "vendas" {
		t.Fatalf("Slug = %q, want vendas", got.Slug)
	}
}

// Um teto absurdo em loop.json não pode virar turno infinito de madrugada — o
// cap continua sendo o detector de laço preso.
func TestResolveLoop_LimitaOTetoDeIteracoes(t *testing.T) {
	workspace := t.TempDir()
	agent := newLoopTestAgent(workspace)
	root := filepath.Join(workspace, "loops", "vendas")

	resolveWith := func(n int) LoopScope {
		al := newLoopTestLoop(t)
		al.SetLoopResolver(func(string) LoopScope {
			return LoopScope{ID: "id", Slug: "vendas", Root: root, MaxToolIterations: n}
		})
		return al.resolveLoop(agent, "agent:web-abc")
	}

	if got := resolveWith(100_000).MaxToolIterations; got != maxLoopToolIterations {
		t.Fatalf("MaxToolIterations = %d, want %d", got, maxLoopToolIterations)
	}
	if got := resolveWith(-5).MaxToolIterations; got != 0 {
		t.Fatalf("negativo: MaxToolIterations = %d, want 0", got)
	}
	if got := resolveWith(400).MaxToolIterations; got != 400 {
		t.Fatalf("valor válido: MaxToolIterations = %d, want 400", got)
	}
}

// Sem resolver instalado, o comportamento é o de antes da feature: escopo global.
func TestResolveLoop_SemResolverOuSessionKeyEhInativo(t *testing.T) {
	workspace := t.TempDir()
	agent := newLoopTestAgent(workspace)

	al := newLoopTestLoop(t)
	if got := al.resolveLoop(agent, "agent:web-abc"); got.Active() {
		t.Fatalf("sem resolver: Active() = true, want false")
	}

	al.SetLoopResolver(func(string) LoopScope {
		return LoopScope{ID: "id", Slug: "vendas", Root: filepath.Join(workspace, "loops", "vendas")}
	})
	if got := al.resolveLoop(agent, "   "); got.Active() {
		t.Fatalf("session key vazia: Active() = true, want false")
	}
}

func TestLoopPromptPart_SemLoopNaoEmiteNada(t *testing.T) {
	if part := loopPromptPart(LoopScope{}); part != nil {
		t.Fatalf("loopPromptPart(zero) = %+v, want nil", part)
	}
	if parts := promptOverlaysForOptions(processOptions{}); len(parts) != 0 {
		t.Fatalf("promptOverlaysForOptions(zero) = %d partes, want 0", len(parts))
	}
}

func TestLoopPromptPart_TrazInstrucoesEMemoria(t *testing.T) {
	root := filepath.Join(t.TempDir(), "loops", "vendas")
	if err := os.MkdirAll(filepath.Join(root, "memory"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(loopInstructionsFile(root), []byte("Fale sempre em número."), 0o600); err != nil {
		t.Fatalf("WriteFile LOOP.md: %v", err)
	}
	if err := os.WriteFile(loopMemoryFile(root), []byte("### Baseline\nR$ 100k em junho."), 0o600); err != nil {
		t.Fatalf("WriteFile MEMORY.md: %v", err)
	}

	part := loopPromptPart(LoopScope{ID: "id", Slug: "vendas", Root: root})
	if part == nil {
		t.Fatal("loopPromptPart = nil, want parte")
	}
	for _, want := range []string{
		"# Loop: vendas",
		"Fale sempre em número.",
		`<memory scope="loop:vendas">`,
		"R$ 100k em junho.",
		"</memory>",
	} {
		if !strings.Contains(part.Content, want) {
			t.Fatalf("conteúdo não contém %q:\n%s", want, part.Content)
		}
	}

	// A parte precisa passar pela validação do registry, senão é descartada
	// silenciosamente em BuildMessagesFromPrompt (só vira WARN no log).
	if err := NewPromptRegistry().ValidatePart(*part); err != nil {
		t.Fatalf("ValidatePart: %v", err)
	}
}

// Loop recém-criado não tem arquivo nenhum — isso é estado normal, não erro, e o
// bloco ainda precisa existir para o modelo saber em que loop está.
func TestLoopPromptPart_SemArquivosAindaIdentificaOLoop(t *testing.T) {
	part := loopPromptPart(LoopScope{ID: "id", Slug: "vendas", Root: filepath.Join(t.TempDir(), "loops", "vendas")})
	if part == nil {
		t.Fatal("loopPromptPart = nil, want parte")
	}
	if !strings.Contains(part.Content, "# Loop: vendas") {
		t.Fatalf("conteúdo = %q, want cabeçalho do loop", part.Content)
	}
	if strings.Contains(part.Content, "<memory") {
		t.Fatalf("emitiu bloco de memória vazio:\n%s", part.Content)
	}
}

// O overlay do loop vem ANTES do override de subturn: os dois moram no mesmo
// slot, e a instrução mais específica do turno tem de falar por último.
func TestPromptOverlays_LoopEDepoisSubTurn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "loops", "vendas")
	opts := processOptions{
		Loop:                 LoopScope{ID: "id", Slug: "vendas", Root: root},
		SystemPromptOverride: "Você é um sub-agente.",
	}

	parts := promptOverlaysForOptions(opts)
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}
	if parts[0].Source.ID != PromptSourceLoop {
		t.Fatalf("parts[0].Source = %q, want %q", parts[0].Source.ID, PromptSourceLoop)
	}
	if parts[1].Source.ID != PromptSourceSubTurnProfile {
		t.Fatalf("parts[1].Source = %q, want %q", parts[1].Source.ID, PromptSourceSubTurnProfile)
	}
}

func TestTurnStateMaxIterations_OverridePorTurno(t *testing.T) {
	agent := &AgentInstance{MaxIterations: 100}

	semOverride := &turnState{agent: agent}
	if got := semOverride.maxIterations(); got != 100 {
		t.Fatalf("sem override: maxIterations() = %d, want 100", got)
	}

	comOverride := &turnState{agent: agent, opts: processOptions{MaxToolIterations: 400}}
	if got := comOverride.maxIterations(); got != 400 {
		t.Fatalf("com override: maxIterations() = %d, want 400", got)
	}

	// Zero não é "sem teto": é "usa o do agente".
	zero := &turnState{agent: agent, opts: processOptions{MaxToolIterations: 0}}
	if got := zero.maxIterations(); got != 100 {
		t.Fatalf("override zero: maxIterations() = %d, want 100", got)
	}
}

// O isolamento do evolution é o coração da feature: sem ele, uma skill nascida
// dentro de um loop é aplicada ao assistente inteiro.
func TestEvolutionBridge_TurnEndComLoopEscreveNaRaizDoLoop(t *testing.T) {
	workspace := t.TempDir()
	loopRoot := filepath.Join(workspace, "loops", "vendas")
	if err := os.MkdirAll(loopRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	al := newEvolutionTestLoop(t, workspace, config.EvolutionConfig{
		Enabled: true,
		Mode:    "observe",
	}, &simpleMockProvider{response: "ok"})
	defer al.Close()

	al.emitEvent(runtimeevents.KindAgentTurnEnd, EventMeta{
		AgentID:    "main",
		TurnID:     "turn-loop",
		SessionKey: "session-loop",
	}, TurnEndPayload{
		Status:       TurnEndStatusCompleted,
		Workspace:    workspace,
		UserMessage:  "tarefa do loop",
		FinalContent: "ok",
		Loop:         LoopScope{ID: "id", Slug: "vendas", Root: loopRoot},
	})

	record := waitForEvolutionRecord(t, filepath.Join(loopRoot, "state", "evolution", "task-records.jsonl"))
	if got := record["summary"]; got != "tarefa do loop" {
		t.Fatalf("summary = %v, want tarefa do loop", got)
	}

	// E o global NÃO foi tocado — é a metade do teste que prova isolamento.
	global := filepath.Join(workspace, "state", "evolution", "task-records.jsonl")
	if _, err := os.Stat(global); !os.IsNotExist(err) {
		data, _ := os.ReadFile(global)
		t.Fatalf("record vazou para o workspace global (%v):\n%s", err, data)
	}
}
