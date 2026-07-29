package agent

import (
	"path/filepath"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// Loops (fork seucaranguejo): um Loop é uma meta com prazo que vive DENTRO de um
// assistente, com instruções, memória e skills próprias em
// <workspace>/loops/<slug>/.
//
// Todo o maquinário que interessa (evolution, MemoryStore, SkillsLoader,
// Applier) já é parametrizado por uma única string de workspace, então um Loop
// é um mini-workspace e a feature inteira se resume a: descobrir de qual loop é
// este turno e passar essa raiz adiante.
//
// O vínculo sessão↔loop é fato do CONTROL-PLANE, não da chave de sessão. Podia
// ter virado uma dimensão `space` no scope — funcionaria —, mas isso mudaria a
// chave e, no dia em que alguém movesse uma conversa existente para um loop, o
// histórico em sessions/<key>.jsonl ficaria órfão. Um resolver externo evita
// esse acoplamento: mover conversa vira um UPDATE, sem rekey.

// loopsDirName é a raiz dos loops dentro do workspace. Tem mount próprio no
// deployment; sem ele um deploy apagaria o aprendizado do loop.
const loopsDirName = "loops"

// LoopScope identifica o loop de um turno. O zero value significa "sem loop",
// que é o caminho normal: conversa avulsa segue existindo e se comporta
// exatamente como antes desta feature.
type LoopScope struct {
	ID   string
	Slug string
	// Root é o mini-workspace do loop, absoluto. Vazio quando não há loop.
	Root string
	// MaxToolIterations sobrescreve o teto de passos de ferramenta dos turnos
	// deste loop. 0 = usar o do agente.
	//
	// Vem junto do escopo, e não por um campo novo na request de chat, porque
	// assim o turno de cron do loop herda o mesmo teto sem que a API de cron
	// precise saber que Loops existem.
	MaxToolIterations int
}

// Active informa se o turno pertence a um loop.
func (s LoopScope) Active() bool { return strings.TrimSpace(s.Root) != "" }

// maxLoopToolIterations é o teto do teto. Um Loop precisa de mais passos que uma
// conversa, mas o cap continua sendo o detector de laço preso — sem um limite
// superior, um erro de digitação em loop.json vira um turno que roda para sempre
// às 3h da manhã, que é exatamente o que o cap existe para impedir.
const maxLoopToolIterations = 1000

// LoopResolver traduz uma chave de sessão no loop a que ela pertence. É
// instalado pelo control-plane, que é quem conhece o vínculo (Postgres).
// Devolver o zero value é o padrão para "esta sessão não é de nenhum loop".
type LoopResolver func(sessionKey string) LoopScope

// SetLoopResolver instala o resolvedor. Sem ele, todo turno roda no escopo
// global — o comportamento anterior, byte a byte.
func (al *AgentLoop) SetLoopResolver(fn LoopResolver) {
	if al == nil {
		return
	}
	al.loopResolver = fn
}

// resolveLoop devolve o escopo do turno, já validado.
//
// A validação não é paranoia decorativa: o slug nasce numa URL do control-plane
// e o Root vira caminho de escrita — é para lá que o evolution grava
// skills/<nome>/SKILL.md. Um Root fora de <workspace>/loops/ faria o cold path
// de um loop escrever nas skills globais do assistente.
func (al *AgentLoop) resolveLoop(agent *AgentInstance, sessionKey string) LoopScope {
	if al == nil || al.loopResolver == nil || agent == nil {
		return LoopScope{}
	}
	sessionKey = strings.TrimSpace(sessionKey)
	if sessionKey == "" {
		return LoopScope{}
	}
	scope := al.loopResolver(sessionKey)
	if !scope.Active() {
		return LoopScope{}
	}

	workspace := strings.TrimSpace(agent.Workspace)
	if workspace == "" {
		return LoopScope{}
	}
	root := filepath.Clean(scope.Root)
	// filepath.Clean já resolve "..", então basta exigir que o resultado esteja
	// sob a raiz de loops — e não seja a própria raiz.
	loopsRoot := filepath.Join(filepath.Clean(workspace), loopsDirName)
	if !strings.HasPrefix(root, loopsRoot+string(filepath.Separator)) {
		logger.WarnCF("loops", "Loop root outside the workspace loops dir — ignoring", map[string]any{
			"root":  scope.Root,
			"slug":  scope.Slug,
			"agent": agent.ID,
		})
		return LoopScope{}
	}
	scope.Root = root
	if scope.MaxToolIterations > maxLoopToolIterations {
		logger.WarnCF("loops", "Loop max_tool_iterations above the hard ceiling — clamping", map[string]any{
			"requested": scope.MaxToolIterations,
			"ceiling":   maxLoopToolIterations,
			"slug":      scope.Slug,
		})
		scope.MaxToolIterations = maxLoopToolIterations
	}
	if scope.MaxToolIterations < 0 {
		scope.MaxToolIterations = 0
	}
	return scope
}

// loopMemoryFile é a camada de memória do loop. Mesmo formato de seções `### `
// da memória global, para as ferramentas de edição valerem sem exceção.
func loopMemoryFile(root string) string {
	return filepath.Join(root, "memory", "MEMORY.md")
}

// loopInstructionsFile é o LOOP.md, gravado pela UI do control-plane.
func loopInstructionsFile(root string) string {
	return filepath.Join(root, "LOOP.md")
}
