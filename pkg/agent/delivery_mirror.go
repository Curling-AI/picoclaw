package agent

import (
	"context"
	"hash/fnv"
	"strings"
	"sync"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
)

// Espelho de entregas (fork seucaranguejo): o que o agente entrega num chat
// externo a partir de OUTRA sessão — uma rodada de cron, uma conversa da web, o
// resultado de um subagente — entra também no histórico da sessão daquele chat.
// Sem isso, a automação manda "posso remarcar a reunião?" no Telegram, o usuário
// responde "pode" e o turno que recebe a resposta não sabe do que se trata: a
// mensagem só existia na sessão do cron.
//
// Qual sessão processa as mensagens que chegam de um chat é fato do
// control-plane (as chaves do webhook não saem do alocador de rotas daqui), então
// a tradução chat → sessão vem de fora, como o LoopResolver.
//
// O texto entra como mensagem do assistente, sem marca de origem: é o que o
// usuário viu no chat, e o histórico do chat passa a ser a mesma conversa que ele
// tem na tela. Uma entrega publicada no bus conta como entregue; a ferramenta
// message só espelha depois do envio confirmado pelo canal.

// DeliverySessionResolver traduz o endereço de saída de um chat (canal + chat
// id, como o bus o carrega) na sessão em que as mensagens desse chat são
// processadas. "" = chat sem conversa para espelhar.
type DeliverySessionResolver func(channel, chatID string) string

// mirrorStripes é o número de faixas de lock do espelho. Uma faixa segura, para
// as sessões que caem nela, a checagem "a sessão está num turno?" junto com a
// escrita, e o registro de turno. São faixas, e não um lock por chave, porque
// cada rodada de cron é uma chave nova e o mapa de locks cresceria sem fim; e
// não um lock só, porque uma escrita lenta no EFS pararia o início de turno de
// todos os chats do pod.
const mirrorStripes = 64

type deliveryMirror struct {
	stripes [mirrorStripes]sync.Mutex

	// mu protege os campos abaixo e nunca é segurado durante I/O. Ordem: a faixa
	// antes do mu.
	mu      sync.Mutex
	resolve DeliverySessionResolver
	// pending segura o que chegou com um turno ativo na sessão de destino. O
	// turno grava o histórico aos pedaços (tool_calls, depois cada resultado) e
	// restaura o snapshot inicial quando é abortado: escrever no meio dele pode
	// separar uma chamada do seu resultado, ou ser apagado pela restauração.
	// Fica em memória: um pod que reinicia no meio do turno perde a cópia (a
	// entrega em si já aconteceu).
	pending map[string][]providers.Message
	// parked segura resultados de trabalho em background (spawn) que chegaram
	// com a conversa num turno; voltam ao bus quando o turno termina. Ver
	// background_result.go.
	parked map[string][]bus.InboundMessage
	// quiet marca as sessões em que um /stop pegou um turno vivo: o que está
	// estacionado, e o que estacionar até o turno acabar, vira nota quando ele
	// acaba, em vez de voltar ao bus.
	quiet map[string]bool
	// turns conta os turnos vivos por sessão. O activeTurnStates guarda um só
	// por chave, e no webhook dois turnos da mesma sessão correm juntos (uma
	// goroutine por requisição, sem reserva): o segundo sobrescreve o primeiro
	// e, ao terminar, apagaria a marca com o primeiro ainda gravando.
	turns map[string]int
}

func (m *deliveryMirror) stripe(sessionKey string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = h.Write([]byte(sessionKey))
	return &m.stripes[h.Sum32()%mirrorStripes]
}

// beginTurn e endTurn marcam um turno vivo na sessão. A faixa é segurada pelo
// chamador de beginTurn, para que uma entrega veja o turno ou seja gravada
// antes de ele começar.
func (m *deliveryMirror) beginTurn(sessionKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.turns == nil {
		m.turns = make(map[string]int)
	}
	m.turns[sessionKey]++
}

func (m *deliveryMirror) endTurn(sessionKey string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.turns[sessionKey] <= 1 {
		delete(m.turns, sessionKey)
		return
	}
	m.turns[sessionKey]--
}

// mirrorBusyLocked: há turno vivo na sessão, ou um placeholder que a reservou. Exige
// mu.
func (al *AgentLoop) mirrorBusyLocked(sessionKey string) bool {
	return al.mirror.turns[sessionKey] > 0 || al.getActiveTurnState(sessionKey) != nil
}

// SetDeliverySessionResolver liga o espelho. Sem resolver nada é espelhado, que
// é o comportamento anterior.
func (al *AgentLoop) SetDeliverySessionResolver(fn DeliverySessionResolver) {
	if al == nil {
		return
	}
	al.mirror.mu.Lock()
	defer al.mirror.mu.Unlock()
	al.mirror.resolve = fn
}

// mirrorDelivery registra, na sessão do chat channel/chatID, o texto que acabou
// de ser entregue nele por um turno da sessão origin.
func (al *AgentLoop) mirrorDelivery(origin, channel, chatID, text string) {
	text = strings.TrimSpace(text)
	// Sem origem não dá para saber se a entrega é do próprio chat: é o caso da
	// mensagem que o pod recebe direto (polling, Socket Mode), que chega sem
	// chave de sessão.
	if al == nil || origin == "" || text == "" || synthesizedResponse(text) {
		return
	}
	al.mirror.mu.Lock()
	resolve := al.mirror.resolve
	al.mirror.mu.Unlock()
	if resolve == nil {
		return
	}
	// O mesmo endereço que a entrega usou: o canal recebe channel e chat id
	// depois do TrimSpace do bus.NormalizeOutboundMessage.
	channel, chatID = strings.TrimSpace(channel), strings.TrimSpace(chatID)
	target := resolve(channel, chatID)
	// Na própria sessão do chat o turno já grava o que disse: a resposta final
	// ou a chamada da ferramenta message, que leva o texto. A comparação é crua:
	// resolver alias aqui custaria varrer os metadados de todas as sessões a
	// cada entrega (a origem de um cron é uma chave nova por rodada).
	if target == "" || target == origin {
		return
	}
	// Resolvidos fora do lock: tocam o disco (EFS em produção).
	agent := al.agentForSession(target)
	if agent == nil || !hasConversation(agent.Sessions, target) {
		return
	}

	deferred := al.deliverToConversation(agent, target, providers.Message{Role: "assistant", Content: text})
	logger.InfoCF("agent", "Mirrored delivery into chat session", map[string]any{
		"channel":        channel,
		"chat_id":        chatID,
		"origin_session": origin,
		"session_key":    target,
		"content_len":    len(text),
		"deferred":       deferred,
	})
}

// flushMirroredDeliveries grava o que esperava o turno da sessão terminar.
func (al *AgentLoop) flushMirroredDeliveries(sessionKey string) {
	al.mirror.mu.Lock()
	waiting := len(al.mirror.pending[sessionKey]) > 0
	al.mirror.mu.Unlock()
	if !waiting {
		return
	}
	// Fora dos locks, como em mirrorDelivery: registry e disco.
	agent := al.agentForSession(sessionKey)
	if agent == nil {
		logger.WarnCF("agent", "No agent for mirrored deliveries; kept pending",
			map[string]any{"session_key": sessionKey})
		return
	}
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	al.mirror.mu.Lock()
	pending := al.mirror.pending[sessionKey]
	// Outro turno pode estar vivo (dois pelo webhook) ou ter assumido a sessão
	// logo em seguida: espera por ele.
	if len(pending) == 0 || al.mirrorBusyLocked(sessionKey) {
		al.mirror.mu.Unlock()
		return
	}
	delete(al.mirror.pending, sessionKey)
	al.mirror.mu.Unlock()
	al.writeMirrored(agent, sessionKey, pending)
}

// deliverToConversation writes msg into the session's history now, after
// whatever was still waiting for a flush, or queues it when a turn holds the
// session (true = deferred). It is the one place that takes the stripe and then
// mu: registerActiveTurn holds the same stripe, so a write either sees the turn
// or lands before it starts.
func (al *AgentLoop) deliverToConversation(agent *AgentInstance, sessionKey string, msg providers.Message) bool {
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	al.mirror.mu.Lock()
	if al.mirrorBusyLocked(sessionKey) {
		al.queueMirroredLocked(sessionKey, msg)
		al.mirror.mu.Unlock()
		return true
	}
	// Uma entrega que ainda espera o flush (o turno acabou de liberar a sessão)
	// vai antes desta, para a conversa manter a ordem de envio.
	batch := append(al.mirror.pending[sessionKey], msg)
	delete(al.mirror.pending, sessionKey)
	al.mirror.mu.Unlock()
	al.writeMirrored(agent, sessionKey, batch)
	return false
}

// dropMirroredDeliveries discards what waits for the session's turn to end:
// mirrored deliveries and parked background results. A task launched before
// /clear ends with no trace in the cleared conversation, on purpose.
func (al *AgentLoop) dropMirroredDeliveries(sessionKey string) {
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	al.mirror.mu.Lock()
	dropped := len(al.mirror.pending[sessionKey]) + len(al.mirror.parked[sessionKey])
	delete(al.mirror.pending, sessionKey)
	delete(al.mirror.parked, sessionKey)
	al.mirror.mu.Unlock()
	if dropped > 0 {
		logger.InfoCF("agent", "Dropped deliveries waiting for a turn of a cleared session",
			map[string]any{"session_key": sessionKey, "dropped": dropped})
	}
}

// maxPendingMirrors limita a fila de uma sessão presa num turno longo: uma
// automação de minuto contra um chat ocupado não cresce a fila sem fim. Sai a
// mais antiga, que a conversa já não usaria.
const maxPendingMirrors = 20

// queueMirroredLocked põe a entrega na fila da sessão. Exige mu.
func (al *AgentLoop) queueMirroredLocked(sessionKey string, msg providers.Message) {
	if al.mirror.pending == nil {
		al.mirror.pending = make(map[string][]providers.Message)
	}
	queue := append(al.mirror.pending[sessionKey], msg)
	if dropped := len(queue) - maxPendingMirrors; dropped > 0 {
		queue = queue[dropped:]
		logger.WarnCF("agent", "Dropped oldest mirrored deliveries waiting for a turn",
			map[string]any{"session_key": sessionKey, "dropped": dropped})
	}
	al.mirror.pending[sessionKey] = queue
}

// writeMirrored grava como um turno grava: o store e o context manager (o
// seahorse monta o prompt da própria base, não do store).
func (al *AgentLoop) writeMirrored(agent *AgentInstance, sessionKey string, msgs []providers.Message) {
	for _, msg := range msgs {
		agent.Sessions.AddFullMessage(sessionKey, msg)
		if al.contextManager == nil {
			continue
		}
		if err := al.contextManager.Ingest(context.Background(), &IngestRequest{
			SessionKey: sessionKey,
			Message:    msg,
		}); err != nil {
			logger.WarnCF("agent", "Context manager ingest failed for mirrored delivery",
				map[string]any{"session_key": sessionKey, "error": err.Error()})
		}
	}
	// O backend JSON (fallback quando o JSONL não sobe) só persiste no Save; no
	// JSONL cada mensagem já foi gravada e o Save só compacta, como no fim de
	// todo turno.
	if err := agent.Sessions.Save(sessionKey); err != nil {
		logger.WarnCF("agent", "Failed to save mirrored delivery", map[string]any{
			"session_key": sessionKey,
			"error":       err.Error(),
		})
	}
}

// synthesizedResponse reconhece os textos que o coordenador põe no lugar de uma
// resposta que o modelo não deu. O turno não grava o de resposta vazia no
// histórico (sessões alternando vazias, ver finalIsFallback), e os outros só
// fazem sentido na sessão que os produziu; copiá-los para a conversa do chat
// traria o problema de volta.
func synthesizedResponse(text string) bool {
	switch text {
	case defaultResponse, toolLimitResponse, handledToolResponseSummary:
		return true
	}
	return false
}

// hasConversation restringe o espelho a chats com conversa em curso. Um chat id
// que nenhuma mensagem de entrada usa (um destino digitado errado, um DM do Slack
// endereçado pelo id do usuário em vez do canal) viraria uma sessão órfã, que só
// aparece na listagem e nunca é lida. E uma conversa vazia (logo depois do
// /clear) começaria pela fala do assistente, que há provedor que recusa; a
// entrega volta a ser espelhada depois da próxima mensagem do usuário. Vale
// também para sessão migrada do JSON antigo, que tem histórico e não tem scope.
func hasConversation(store session.SessionStore, key string) bool {
	return store != nil && len(store.GetHistory(key)) > 0
}

// isPlainAssistant: fala do assistente só com texto, que pode ser juntada à
// vizinha sem perder chamada de ferramenta nem anexo.
func isPlainAssistant(msg providers.Message) bool {
	return msg.Role == "assistant" && len(msg.ToolCalls) == 0 &&
		len(msg.Media) == 0 && len(msg.Attachments) == 0
}

func joinAssistantText(first, second string) string {
	first, second = strings.TrimSpace(first), strings.TrimSpace(second)
	switch {
	case first == "":
		return second
	case second == "":
		return first
	}
	return first + "\n\n" + second
}

// deliveredText é o que o usuário recebeu: o texto e, quando houver, os anexos —
// "resume esse PDF" só faz sentido se o histórico disser que um PDF foi enviado.
func deliveredText(content string, parts []bus.MediaPart) string {
	names := make([]string, 0, len(parts))
	for _, part := range parts {
		name := strings.TrimSpace(part.Filename)
		if name == "" {
			name = part.Type
		}
		if name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return strings.TrimSpace(content)
	}
	return joinAssistantText(content, "[attachments: "+strings.Join(names, ", ")+"]")
}
