package agent

import (
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

type deliveryMirror struct {
	mu      sync.Mutex
	resolve DeliverySessionResolver
	// pending segura o que chegou com um turno ativo na sessão de destino. O
	// turno grava o histórico aos pedaços (tool_calls, depois cada resultado) e
	// restaura o snapshot inicial quando é abortado: escrever no meio dele pode
	// separar uma chamada do seu resultado, ou ser apagado pela restauração.
	// Fica em memória: um pod que reinicia no meio do turno perde a cópia (a
	// entrega em si já aconteceu).
	pending map[string][]providers.Message
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
	if al == nil || text == "" || synthesizedResponse(text) {
		return
	}
	al.mirror.mu.Lock()
	resolve := al.mirror.resolve
	al.mirror.mu.Unlock()
	if resolve == nil {
		return
	}
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

	msg := providers.Message{Role: "assistant", Content: text}
	al.mirror.mu.Lock()
	defer al.mirror.mu.Unlock()
	deferred := al.getActiveTurnState(target) != nil
	if deferred {
		if al.mirror.pending == nil {
			al.mirror.pending = make(map[string][]providers.Message)
		}
		al.mirror.pending[target] = append(al.mirror.pending[target], msg)
	} else {
		agent.Sessions.AddFullMessage(target, msg)
	}
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
	defer al.mirror.mu.Unlock()
	pending := al.mirror.pending[sessionKey]
	// Outro turno pode ter assumido a sessão logo em seguida: espera por ele.
	if len(pending) == 0 || al.getActiveTurnState(sessionKey) != nil {
		return
	}
	delete(al.mirror.pending, sessionKey)
	agent := al.agentForSession(sessionKey)
	if agent == nil {
		return
	}
	for _, msg := range pending {
		agent.Sessions.AddFullMessage(sessionKey, msg)
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

// hasConversation restringe o espelho a chats que já falaram com o agente. Um
// chat id que nenhuma mensagem de entrada usa (um destino digitado errado, um DM
// do Slack endereçado pelo id do usuário em vez do canal) viraria uma sessão
// órfã, que só aparece na listagem e nunca é lida.
func hasConversation(store session.SessionStore, key string) bool {
	if store == nil {
		return false
	}
	if meta, ok := store.(session.MetadataAwareSessionStore); ok {
		return meta.GetSessionScope(key) != nil
	}
	return len(store.GetHistory(key)) > 0
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
	content = strings.TrimSpace(content)
	if len(names) == 0 {
		return content
	}
	attachments := "[attachments: " + strings.Join(names, ", ") + "]"
	if content == "" {
		return attachments
	}
	return content + "\n\n" + attachments
}
