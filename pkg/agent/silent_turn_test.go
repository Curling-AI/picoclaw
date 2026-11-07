package agent

import (
	"context"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/turnresult"
)

// fixedReplyProvider answers every call with the same text and reasoning.
type fixedReplyProvider struct {
	content   string
	reasoning string
}

func (p *fixedReplyProvider) Chat(
	context.Context,
	[]providers.Message,
	[]providers.ToolDefinition,
	string,
	map[string]any,
) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{Content: p.content, ReasoningContent: p.reasoning}, nil
}

func (p *fixedReplyProvider) GetDefaultModel() string { return "fixed-reply-model" }

func silentTurnConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	cfg.Agents.Defaults.MaxTokens = 4096
	cfg.Agents.Defaults.MaxToolIterations = 10
	return cfg
}

func silentRequest(key, channel string) turnresult.Request {
	return turnresult.Request{
		Progress: nil, Media: nil,
		Content: "hello", SessionKey: key, Channel: channel, ChatID: "chat-1",
		Silent: true,
	}
}

func drainOutbound(msgBus *bus.MessageBus) []bus.OutboundMessage {
	var got []bus.OutboundMessage
	for {
		select {
		case msg := <-msgBus.OutboundChan():
			got = append(got, msg)
		case <-time.After(200 * time.Millisecond):
			return got
		}
	}
}

// Num turno silencioso a resposta vazia é o fim pedido: sem DefaultResponse e
// sem falha. Antes de qualquer ferramenta o retry do glitch ainda vale.
// (seucaranguejo fork)
func TestSilentTurnEndsEmptyWithoutFallback(t *testing.T) {
	msgBus := bus.NewMessageBus()
	provider := new(alwaysEmptyProvider)
	al := NewAgentLoop(silentTurnConfig(t), msgBus, provider)

	result, err := al.ProcessDirectWithResult(context.Background(), silentRequest("silent-empty", "telegram"))
	if err != nil || result.Status != turnresult.Completed || result.Content != "" {
		t.Fatalf("silent empty turn = %+v, %v; want completed with no content", result, err)
	}
	if calls := provider.calls.Load(); calls != 1+maxEmptyResponseRetries {
		t.Fatalf("provider calls = %d, want %d (retry before any tool ran)", calls, 1+maxEmptyResponseRetries)
	}
	if got := drainOutbound(msgBus); len(got) != 0 {
		t.Fatalf("silent turn published %+v, want nothing", got)
	}
}

// A ferramenta message continua entregando num turno silencioso, e a resposta
// vazia depois dela fecha o turno sem retry.
func TestSilentTurnStillDeliversThroughMessageTool(t *testing.T) {
	cfg := silentTurnConfig(t)
	cfg.Session.Dimensions = []string{"chat"}
	msgBus := bus.NewMessageBus()
	provider := &messageToolProvider{}
	al := NewAgentLoop(cfg, msgBus, provider)

	result, err := al.ProcessDirectWithResult(context.Background(), silentRequest("silent-message", "telegram"))
	if err != nil || result.Status != turnresult.Completed || result.Content != "" {
		t.Fatalf("silent message turn = %+v, %v; want completed with no content", result, err)
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (no retry after the tool ran)", provider.calls)
	}
	got := drainOutbound(msgBus)
	if len(got) != 1 || got[0].Content != "direct tool message" {
		t.Fatalf("outbound = %+v, want only the message tool send", got)
	}
}

// No web (pico) o texto final e o raciocínio iam ao chat pelo caminho de
// interim/stream, antes de o cron decidir publicar. Num turno silencioso o
// texto só volta no resultado, para o cron descartar.
func TestSilentTurnPublishesNothingOnPico(t *testing.T) {
	msgBus := bus.NewMessageBus()
	provider := &fixedReplyProvider{content: "(vazio)", reasoning: "nothing new"}
	al := NewAgentLoop(silentTurnConfig(t), msgBus, provider)

	result, err := al.ProcessDirectWithResult(context.Background(), silentRequest("silent-pico", "pico"))
	if err != nil || result.Content != "(vazio)" {
		t.Fatalf("silent pico turn = %+v, %v; want the narration returned to the caller", result, err)
	}
	if got := drainOutbound(msgBus); len(got) != 0 {
		t.Fatalf("silent pico turn published %+v, want nothing", got)
	}
}
