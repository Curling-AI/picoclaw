package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// steerWhileStreaming streams its replies and, as the first one ends, queues a
// message for the turn: the person sending while they watch it being written.
// (seucaranguejo fork)
type steerWhileStreaming struct {
	configuredStreamingProvider
	steer func()
	seen  [][]providers.Message
}

func (p *steerWhileStreaming) ChatStreamEvents(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
	onChunk func(providers.StreamChunk),
) (*providers.LLMResponse, error) {
	p.seen = append(p.seen, append([]providers.Message(nil), messages...))
	resp, err := p.configuredStreamingProvider.ChatStreamEvents(ctx, messages, tools, model, opts, onChunk)
	if len(p.seen) == 1 && p.steer != nil {
		p.steer()
	}
	return resp, err
}

func (p *steerWhileStreaming) ChatStream(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	opts map[string]any,
	onChunk func(accumulated string),
) (*providers.LLMResponse, error) {
	p.seen = append(p.seen, append([]providers.Message(nil), messages...))
	resp, err := p.configuredStreamingProvider.ChatStream(ctx, messages, tools, model, opts, onChunk)
	if len(p.seen) == 1 && p.steer != nil {
		p.steer()
	}
	return resp, err
}

// replyThenFollowUp tells whether messages hold reply and, right after it,
// followUp: the order the history needs for the follow-up to answer the reply.
func replyThenFollowUp(messages []providers.Message, reply, followUp string) bool {
	for i := 0; i+1 < len(messages); i++ {
		if messages[i].Role == "assistant" && messages[i].Content == reply &&
			messages[i+1].Role == "user" && messages[i+1].Content == followUp {
			return true
		}
	}
	return false
}

// donwellus on picoclaw#125, with the web joining what it sends during a reply
// into the running turn (curling-products#2309): a direct reply the person
// watched being streamed was dropped as a stale answer when their message
// arrived. It vanished from the history the screen reloads, and the next call
// wrote it again without knowing it had.
func TestConfiguredStreaming_MessageAfterAWatchedReplyKeepsTheReply(t *testing.T) {
	const reply, followUp = "Vou montar os slides agora.", "prossiga"
	al, provider, streamer, opts := steeredWhileStreaming(t, reply, followUp)

	got, err := al.runAgentLoop(context.Background(), al.GetRegistry().GetDefaultAgent(), opts)
	if err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}

	// Miguel-MCM on picoclaw#125: canceled, the throttled stream may never
	// have pushed the tail the person was reading. The tool-call path
	// flushes before it moves on for the same reason.
	if len(streamer.finalized) == 0 || streamer.finalized[0] != reply {
		t.Errorf("stream finals %q, want the kept reply flushed first", streamer.finalized)
	}
	if got != "Pronto!" {
		t.Errorf("answer %q, want the reply to the follow-up", got)
	}
	if len(provider.seen) != 2 || !replyThenFollowUp(provider.seen[1], reply, followUp) {
		t.Fatal("the call that read the follow-up did not see the reply the person had watched")
	}
	history := al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(opts.SessionKey)
	if !replyThenFollowUp(history, reply, followUp) {
		t.Error("the history lost the watched reply, or keeps it after the follow-up")
	}
}

// steeredWhileStreaming runs a turn whose first call streams reply and,
// as it ends, gets followUp queued; the second call answers "Pronto!".
func steeredWhileStreaming(
	t *testing.T,
	reply, followUp string,
) (*AgentLoop, *steerWhileStreaming, *recordingStreamer, processOptions) {
	t.Helper()
	cfg := newConfiguredStreamingTestConfig(t, true, true, nil)
	msgBus := bus.NewMessageBus()
	streamer := &recordingStreamer{}
	msgBus.SetStreamDelegate(configuredStreamingDelegate{streamer: streamer})
	half := string([]rune(reply)[:len([]rune(reply))/2])
	provider := &steerWhileStreaming{configuredStreamingProvider: configuredStreamingProvider{
		streamPlan: []configuredStreamingCall{
			{chunks: []string{half, reply}, response: &providers.LLMResponse{Content: reply}},
			{response: &providers.LLMResponse{Content: "Pronto!"}},
		},
	}}
	al := NewAgentLoop(cfg, msgBus, provider)
	provider.steer = func() {
		if err := al.Steer(providers.Message{Role: "user", Content: followUp}); err != nil {
			t.Errorf("Steer: %v", err)
		}
	}
	opts := configuredStreamingProcessOptions("pico")
	opts.NoHistory = false
	return al, provider, streamer, opts
}

// Miguel-MCM on picoclaw#125. The guards below the steering check keep two
// direct answers out of the session, a tool call written out as markup and
// a promise of one before any tool ran: persisted, either teaches the model
// to write its calls as prose. Watched or not, a message arriving as one of
// them ends does not let it in.
func TestConfiguredStreaming_MessageAfterAWatchedStalledCallDropsTheReply(t *testing.T) {
	for name, reply := range map[string]string{
		"call written as markup": "O resumo do artigo ficou assim.\n<parameter=path>resumo.md</parameter>",
		"call only announced":    announcedToolCall,
	} {
		t.Run(name, func(t *testing.T) {
			al, _, _, opts := steeredWhileStreaming(t, reply, "prossiga")

			if _, err := al.runAgentLoop(context.Background(), al.GetRegistry().GetDefaultAgent(), opts); err != nil {
				t.Fatalf("runAgentLoop: %v", err)
			}

			history := al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(opts.SessionKey)
			if len(history) == 0 {
				t.Fatal("session history is empty: the check below would be vacuous")
			}
			for _, m := range history {
				if m.Role == "assistant" && m.Content == reply {
					t.Fatal("a stalled call went into the history")
				}
			}
		})
	}
}

// A reply nobody saw yet (no streaming, as on a channel that sends only the
// final) is still the stale answer: the next call answers both messages.
func TestConfiguredStreaming_MessageAfterAnUnseenReplyDropsTheReply(t *testing.T) {
	const reply, followUp = "Vou montar os slides agora.", "prossiga"
	cfg := newConfiguredStreamingTestConfig(t, false, false, nil)
	provider := &steeringDuringChatProvider{replies: []string{reply, "Pronto!"}, followUp: followUp}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	provider.al = al
	opts := configuredStreamingProcessOptions("pico")
	opts.NoHistory = false

	if _, err := al.runAgentLoop(context.Background(), al.GetRegistry().GetDefaultAgent(), opts); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}

	for _, m := range al.GetRegistry().GetDefaultAgent().Sessions.GetHistory(opts.SessionKey) {
		if m.Role == "assistant" && m.Content == reply {
			t.Fatal("a reply nobody saw went into the history")
		}
	}
}

// steeringDuringChatProvider answers without streaming and queues followUp as
// its first reply ends.
type steeringDuringChatProvider struct {
	al       *AgentLoop
	replies  []string
	followUp string
	calls    int
}

func (p *steeringDuringChatProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls == 1 {
		if err := p.al.Steer(providers.Message{Role: "user", Content: p.followUp}); err != nil {
			return nil, err
		}
	}
	return &providers.LLMResponse{Content: p.replies[min(p.calls, len(p.replies))-1]}, nil
}

func (*steeringDuringChatProvider) GetDefaultModel() string { return "mock-model" }
