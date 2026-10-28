package agent

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
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
// O texto entra como mensagem do assistente, sem marca no conteúdo: é o que o
// usuário viu no chat, e o histórico do chat passa a ser a mesma conversa que ele
// tem na tela. A origem e a hora vão no campo Delivery, que só o histórico
// enviado ao modelo mostra, como uma linha datada antes do texto (ver
// historyContent). Sem ela, entregas seguidas viravam um bloco só na junção do
// sanitizador, e "me lembre disso" apontava para qualquer parte dele. Uma
// entrega publicada no bus conta como entregue; a ferramenta message só espelha
// depois do envio confirmado pelo canal.

// DeliverySessionResolver traduz o endereço de saída de um chat (canal + chat
// id, como o bus o carrega) na sessão em que as mensagens desse chat são
// processadas. "" = chat sem conversa para espelhar.
type DeliverySessionResolver func(channel, chatID string) string

// DeliveryConversationKept tells whether sessionKey is a conversation the
// control plane keeps before its first message (the light mode's main one),
// which a delivery may open. Any other target needs a conversation in course.
type DeliveryConversationKept func(sessionKey string) bool

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
	kept    DeliveryConversationKept
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
	// cleared guarda quando cada sessão passou por /clear: o resultado de um
	// trabalho lançado antes disso é da conversa apagada, não da nova.
	cleared map[string]time.Time
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

// SetDeliveryConversationKept names the conversations a delivery may open.
func (al *AgentLoop) SetDeliveryConversationKept(fn DeliveryConversationKept) {
	if al == nil {
		return
	}
	al.mirror.mu.Lock()
	defer al.mirror.mu.Unlock()
	al.mirror.kept = fn
}

// MirrorDelivery records a delivery made outside the agent loop: the output a
// command cron job published, or what a script sent through the chat's API and
// a proxy in the pod saw go by.
//
// Unlike a turn's own sends, it is recorded in the chat's own session too, even
// while the turn that ran the script is live: that turn's tool result carries
// the send only when a synchronous exec echoed it. A background script returns
// its pid, an aborted turn loses the result, a script may print just "ok".
// When the output did echo it, the conversation has the send twice. The turn
// that ran the script writes it after the tool round it came from
// (self_delivery.go); with another turn live it waits in the queue like any
// delivery.
func (al *AgentLoop) MirrorDelivery(d tools.ExternalDelivery) {
	al.mirrorDeliveryFrom(d, true)
}

// mirrorDelivery registra, na sessão do chat channel/chatID, o texto que acabou
// de ser entregue nele por um turno da sessão origin.
func (al *AgentLoop) mirrorDelivery(origin, channel, chatID, text string) {
	al.mirrorDeliveryFrom(tools.ExternalDelivery{Origin: origin, Channel: channel, ChatID: chatID, Text: text}, false)
}

func (al *AgentLoop) mirrorDeliveryFrom(d tools.ExternalDelivery, external bool) {
	origin, text := d.Origin, strings.TrimSpace(d.Text)
	// Sem origem não dá para saber se a entrega é do próprio chat: é o caso da
	// mensagem que o pod recebe direto (polling, Socket Mode), que chega sem
	// chave de sessão.
	if al == nil || origin == "" || text == "" || synthesizedResponse(text) {
		return
	}
	al.mirror.mu.Lock()
	resolve, kept := al.mirror.resolve, al.mirror.kept
	al.mirror.mu.Unlock()
	if resolve == nil {
		return
	}
	// O mesmo endereço que a entrega usou: o canal recebe channel e chat id
	// depois do TrimSpace do bus.NormalizeOutboundMessage.
	channel, chatID := strings.TrimSpace(d.Channel), strings.TrimSpace(d.ChatID)
	target := resolve(channel, chatID)
	if target == "" {
		return
	}
	// Na própria sessão do chat o turno já grava o que disse: a resposta final
	// ou a chamada da ferramenta message, que leva o texto. A comparação é crua:
	// resolver alias aqui custaria varrer os metadados de todas as sessões a
	// cada entrega (a origem de um cron é uma chave nova por rodada).
	self := target == origin
	if self && !external {
		return
	}
	// Resolvidos fora do lock: tocam o disco (EFS em produção).
	agent := al.agentForSession(target)
	if agent == nil || !(hasConversation(agent.Sessions, target) || kept != nil && kept(target)) {
		return
	}

	msg := providers.Message{
		Role:     "assistant",
		Content:  text,
		Delivery: &providers.Delivery{Origin: origin, At: time.Now().Round(0), Self: self},
	}
	heldByTurn := self && al.holdForOwnTurn(target, d.TurnID, msg)
	deferred := heldByTurn
	if !heldByTurn {
		var err error
		if deferred, err = al.deliverToConversation(agent, target, msg); err != nil {
			return // writeMirrored logged it
		}
	}
	logger.InfoCF("agent", "Mirrored delivery into chat session", map[string]any{
		"channel":        channel,
		"chat_id":        chatID,
		"origin_session": origin,
		"origin_turn_id": d.TurnID,
		"session_key":    target,
		"content_len":    len(text),
		"deferred":       deferred,
		"held_by_turn":   heldByTurn,
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
	_ = al.writeMirrored(agent, sessionKey, pending) // logged there
}

// deliverToConversation writes msg into the session's history now, after
// whatever was still waiting for a flush, or queues it when a turn holds the
// session (deferred). err is a write that failed now (already logged). It is
// the one place that takes the stripe and then mu: registerActiveTurn holds the
// same stripe, so a write either sees the turn or lands before it starts.
func (al *AgentLoop) deliverToConversation(
	agent *AgentInstance,
	sessionKey string,
	msg providers.Message,
) (deferred bool, err error) {
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	return al.deliverToConversationLocked(agent, sessionKey, msg)
}

// deliverToConversationLocked is deliverToConversation for a caller that
// holds the session's stripe.
func (al *AgentLoop) deliverToConversationLocked(
	agent *AgentInstance,
	sessionKey string,
	msg providers.Message,
) (deferred bool, err error) {
	al.mirror.mu.Lock()
	if al.mirrorBusyLocked(sessionKey) {
		al.queueMirroredLocked(sessionKey, msg)
		al.mirror.mu.Unlock()
		return true, nil
	}
	// Uma entrega que ainda espera o flush (o turno acabou de liberar a sessão)
	// vai antes desta, para a conversa manter a ordem de envio.
	batch := append(al.mirror.pending[sessionKey], msg)
	delete(al.mirror.pending, sessionKey)
	al.mirror.mu.Unlock()
	return false, al.writeMirrored(agent, sessionKey, batch)
}

// dropMirroredDeliveries discards what waits for the session's turn to end:
// mirrored deliveries and parked background results. A task launched before
// /clear ends with no trace in the cleared conversation, on purpose; the clear
// is stamped so its results that arrive later are dropped too (clearedSince).
func (al *AgentLoop) dropMirroredDeliveries(sessionKey string) {
	stripe := al.mirror.stripe(sessionKey)
	stripe.Lock()
	defer stripe.Unlock()
	al.mirror.mu.Lock()
	dropped := len(al.mirror.pending[sessionKey]) + len(al.mirror.parked[sessionKey])
	delete(al.mirror.pending, sessionKey)
	delete(al.mirror.parked, sessionKey)
	now := time.Now()
	if al.mirror.cleared == nil {
		al.mirror.cleared = make(map[string]time.Time)
	}
	for key, at := range al.mirror.cleared {
		if now.Sub(at) > clearStampTTL {
			delete(al.mirror.cleared, key)
		}
	}
	al.mirror.cleared[sessionKey] = now
	al.mirror.mu.Unlock()
	if dropped > 0 {
		logger.InfoCF("agent", "Dropped deliveries waiting for a turn of a cleared session",
			map[string]any{"session_key": sessionKey, "dropped": dropped})
	}
}

// clearStampTTL is how long a /clear is remembered, far beyond any sub-turn's
// time limit: work launched before the clear has ended by then.
const clearStampTTL = 24 * time.Hour

// clearedSince reports whether the session was cleared after t.
func (al *AgentLoop) clearedSince(sessionKey string, t time.Time) bool {
	al.mirror.mu.Lock()
	defer al.mirror.mu.Unlock()
	cleared, ok := al.mirror.cleared[sessionKey]
	return ok && cleared.After(t)
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
// seahorse monta o prompt da própria base, não do store). Devolve, já logado,
// o erro de uma escrita que não chegou ao store: o que está aqui pode ser a
// única cópia (resultado de subagente numa conversa web, nota de um /stop).
func (al *AgentLoop) writeMirrored(agent *AgentInstance, sessionKey string, msgs []providers.Message) error {
	var lost []error
	var committed []memory.CommittedHistoryMessage
	var content []memory.ContentEntry
	var contentCommits []memory.ContentAppendResult
	var ingested []providers.Message
	written := 0
	unproven := 0
	observeContent := al.observesSessionHistoryContent()
	for _, msg := range msgs {
		if appender, supported := agent.Sessions.(memory.HistoryContentAppender); observeContent && supported {
			result, err := appender.AppendHistoryContent(context.Background(), sessionKey, msg)
			if errors.Is(err, memory.ErrHistoryInputCommitted) || !errors.Is(err, memory.ErrHistoryUnsupported) {
				if err != nil {
					if errors.Is(err, memory.ErrHistoryInputCommitted) && result.Entry.Message.Role != "" {
						warnCommittedHistoryRecovery(sessionKey, err)
					} else {
						lost = append(lost, err)
						continue
					}
				}
				if result.Entry.Message.Role == "" {
					continue
				}
				content = append(content, result.Entry)
				contentCommits = append(contentCommits, result)
				if result.Receipt != nil && result.Receipt.Order.Valid() {
					committed = append(committed, *result.Receipt)
				} else {
					unproven++
				}
				written++
				ingested = append(ingested, result.Entry.Message)
				continue
			}
		}
		var appendErr error
		hasReceipt := false
		if receiptStore, ok := agent.Sessions.(session.HistoryReceiptStore); ok {
			receipt, receiptErr := receiptStore.AppendHistoryReceipt(context.Background(), sessionKey, msg)
			appendErr = receiptErr
			if receiptErr == nil && receipt.Order.Valid() {
				committed = append(committed, receipt)
				hasReceipt = true
			} else if receiptErr == nil {
				if receipt.Message.Role == "" {
					continue // Filtered transient thought: no commit or notification.
				}
				appendErr = memory.ErrHistoryOrderUnavailable
			}
			if errors.Is(receiptErr, memory.ErrHistoryInputCommitted) {
				warnCommittedHistoryRecovery(sessionKey, receiptErr)
				appendErr = nil
			} else if errors.Is(receiptErr, memory.ErrHistoryUnsupported) ||
				errors.Is(receiptErr, memory.ErrHistoryOwnershipUnavailable) ||
				errors.Is(receiptErr, memory.ErrHistoryReceiptUnavailable) {
				// Receipt eligibility failed before this message's write began.
				// Keep the legacy persistence/notification contract; never retry
				// a failed or uncertain commit through this fallback.
				appendErr = appendToSession(agent.Sessions, sessionKey, msg)
			}
		} else {
			appendErr = appendToSession(agent.Sessions, sessionKey, msg)
		}
		if errors.Is(appendErr, memory.ErrHistoryInputCommitted) {
			warnCommittedHistoryRecovery(sessionKey, appendErr)
			appendErr = nil
		}
		if appendErr != nil {
			lost = append(lost, appendErr)
			continue
		}
		written++
		if !hasReceipt {
			unproven++
		}
		ingested = append(ingested, msg)
	}
	// A failed member does not discard later successful writes (a,c when b
	// fails). Deliver exact receipts before ingestion can enqueue network work.
	if len(contentCommits) > 0 {
		if err := al.notifySessionHistoryContentCommits(sessionKey, contentCommits); err != nil {
			logger.WarnCF("agent", "Could not snapshot committed history for observer", map[string]any{
				"session_key": sessionKey, "error": err.Error(),
			})
		}
	}
	if len(content) > 0 {
		al.notifySessionHistoryContent(sessionKey, content)
	}
	if len(committed) > 0 {
		al.notifySessionHistoryReceipts(sessionKey, committed)
	}
	for _, msg := range ingested {
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
	if len(lost) > 0 {
		logger.ErrorCF("agent", "Could not write into the conversation", map[string]any{
			"session_key": sessionKey,
			"lost":        len(lost),
			"of":          len(msgs),
			"error":       lost[0].Error(),
		})
	}
	// O backend JSON (fallback quando o JSONL não sobe) só persiste no Save; no
	// JSONL cada mensagem já foi gravada e o Save só compacta, como no fim de
	// todo turno: lá uma falha do Save não perde o que o append confirmou.
	if err := agent.Sessions.Save(sessionKey); err != nil {
		logger.WarnCF("agent", "Failed to save mirrored delivery", map[string]any{
			"session_key": sessionKey,
			"error":       err.Error(),
		})
		if _, checked := agent.Sessions.(session.CheckedAppender); !checked {
			lost = append(lost, err)
			written = 0
		}
	}
	// What got written is in the history even when other messages of the
	// batch were lost; whoever keeps a copy of it must hear so.
	if written > 0 {
		if unproven > 0 {
			// No raw input is labeled a receipt. The legacy notification remains
			// useful for source amendment after Save/checked persistence only.
			al.notifySessionHistoryReceipts(sessionKey, nil)
		}
		al.notifySessionHistoryChanged(sessionKey)
	}
	return errors.Join(lost...)
}

// The source exposes the I/O cause, but this member is already in history.
// Report recovery separately instead of inviting the delivery caller to append
// it again. There is no receipt or extra read confirmation for this outcome.
func warnCommittedHistoryRecovery(sessionKey string, err error) {
	logger.WarnCF("agent", "History input committed with pending operational recovery", map[string]any{
		"session_key": sessionKey, "error": err.Error(),
	})
}

// appendToSession appends msg and reports a failed write when the store can
// tell (session.CheckedAppender); other stores only report it on Save.
func appendToSession(store session.SessionStore, sessionKey string, msg providers.Message) error {
	if checked, ok := store.(session.CheckedAppender); ok {
		return checked.AppendMessage(sessionKey, msg)
	}
	store.AddFullMessage(sessionKey, msg)
	return nil
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

// historyContent é o texto de msg no histórico enviado ao modelo. Uma entrega
// espelhada ganha uma linha com a hora antes do texto: sem ela, a resposta do
// turno anterior e as entregas que vieram depois chegam ao modelo como uma fala
// só, e ele não tem como saber qual é a mais recente. A hora é a da entrega, no
// fuso do pod, o mesmo do [Current time] da mensagem do usuário; é fixa, então o
// prefixo do prompt continua o mesmo de um turno para o outro (cache).
func historyContent(msg providers.Message) string {
	if msg.Delivery == nil {
		return msg.Content
	}
	at := msg.Delivery.At
	if at.IsZero() && msg.CreatedAt != nil {
		at = *msg.CreatedAt
	}
	// Um cron ou o heartbeat é "uma automação"; um script que a própria
	// conversa deixou rodando é dela; o resto (conversa da web, subagente) é
	// outra conversa deste assistente.
	line := "[Sent to this chat from another conversation"
	switch {
	case msg.Delivery.Self:
		line = selfDeliveryLine
	case isAutomationOrigin(msg.Delivery.Origin):
		line = "[Sent to this chat by an automation"
	}
	if !at.IsZero() {
		line += " at " + at.Local().Format("2006-01-02 15:04 (Monday)")
	}
	return line + "]\n" + strings.TrimSpace(msg.Content)
}

// selfDeliveryLine opens the history line of a send of the conversation's own
// script.
const selfDeliveryLine = "[Sent to this chat by a script this conversation started"

// deliveryOpening is the user turn the provider gets before a conversation a
// delivery opened.
const deliveryOpening = "[The conversation starts with a message sent to this chat]"

// heartbeatSessionKey is the session heartbeat turns run in (agent_message.go).
const heartbeatSessionKey = "heartbeat"

// isAutomationOrigin: a scheduled run (cron) or the heartbeat, not a person's
// conversation.
func isAutomationOrigin(origin string) bool {
	return strings.HasPrefix(origin, CronSessionPrefix) ||
		strings.HasPrefix(origin, CronModelSessionPrefix) ||
		origin == heartbeatSessionKey
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
