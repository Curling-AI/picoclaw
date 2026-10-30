package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// steerOnCall is a scripted provider that queues a message from the person
// while it answers call n, the way a message sent mid-turn arrives.
type steerOnCall struct {
	*scriptedProvider
	n     int
	steer func()
}

func (p *steerOnCall) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	resp, err := p.scriptedProvider.Chat(ctx, messages, tools, model, options)
	if p.scriptedProvider.calls() == p.n {
		p.steer()
	}
	return resp, err
}

func newSteeredLoop(
	t *testing.T,
	maxIter, n int,
	message string,
	script ...providers.LLMResponse,
) (*AgentLoop, *steerOnCall, *bus.MessageBus) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	if maxIter > 0 {
		cfg.Agents.Defaults.MaxToolIterations = maxIter
	}
	provider := &steerOnCall{scriptedProvider: &scriptedProvider{script: script}, n: n}
	msgBus := bus.NewMessageBus()
	al := NewAgentLoop(cfg, msgBus, provider)
	provider.steer = func() {
		if err := al.Steer(providers.Message{Role: "user", Content: message}); err != nil {
			t.Errorf("steer: %v", err)
		}
	}
	return al, provider, msgBus
}

// The earlier request had used the cap when the message arrived: the message
// still gets three iterations of its own, and the budget warning about the old
// budget does not ride along.
func TestSteeringRestartsTheToolBudget(t *testing.T) {
	al, provider, _ := newSteeredLoop(t, 3, 2, "also check the docs",
		readFileCall("call_1"),
		readFileCall("call_2"),
		readFileCall("call_3"),
		readFileCall("call_4"),
		providers.LLMResponse{Content: "Both requests delivered."},
	)

	resp, err := al.ProcessDirectWithChannel(t.Context(), "read the readme", "test-session", "test", "chat1")
	if err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}
	if resp != "Both requests delivered." || provider.calls() != 5 {
		t.Fatalf("want the reply of the 5th call, got %q after %d calls", resp, provider.calls())
	}
	if !provider.callCarried(2, toolBudgetNudgePrefix) {
		t.Fatal("the 2nd call should carry the budget check of the earlier request")
	}
	if !provider.callCarried(3, "also check the docs") || provider.callCarried(3, toolBudgetNudgePrefix) {
		t.Fatal("the call that reads the message should go without the old budget's warning")
	}
}

// The model is told the message joins the work in every call after it is read,
// and only then; it is asked to acknowledge it in the call that reads it alone.
func TestSteeringTellsTheModelTheMessageJoinsTheWork(t *testing.T) {
	al, provider, _ := newSteeredLoop(t, 0, 1, "and what is 37 x 41?",
		readFileCall("call_1"),
		readFileCall("call_2"),
		providers.LLMResponse{Content: "README read; 37 x 41 = 1517."},
	)

	if _, err := al.ProcessDirectWithChannel(
		t.Context(),
		"read the readme",
		"test-session",
		"test",
		"chat1",
	); err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}
	if provider.callCarried(1, joinedRequestNote) {
		t.Fatal("the note went out before any message joined")
	}
	if !provider.callCarried(2, joinedRequestNote) || !provider.callCarried(3, joinedRequestNote) {
		t.Fatal("every call after the message should carry the note")
	}
	if provider.callCarried(1, joinedAckNote) || !provider.callCarried(2, joinedAckNote) ||
		provider.callCarried(3, joinedAckNote) {
		t.Fatal("only the call that reads the message should ask to acknowledge it")
	}
	history := al.GetRegistry().GetDefaultAgent().Sessions.GetHistory("test-session")
	if !slices.ContainsFunc(history, func(m providers.Message) bool { return m.Content == "and what is 37 x 41?" }) {
		t.Fatalf("the message should be in the history, got %d messages", len(history))
	}
	if slices.ContainsFunc(history, func(m providers.Message) bool { return m.Content == joinedRequestNote }) {
		t.Fatal("the note is for this turn only and must stay out of the history")
	}
}

// A web chat learns where the turn read the message: after the results of the
// batch that was running, before what the model does with it.
func TestSteeringReadIsAnnouncedWhereTheMessageLands(t *testing.T) {
	al, _, msgBus := newSteeredLoop(t, 0, 1, "and what is 37 x 41?",
		readFileCall("call_1"),
		providers.LLMResponse{Content: "README read; 37 x 41 = 1517."},
	)
	var kinds []string
	var read []string
	collected := make(chan struct{})
	ctx, stop := context.WithCancel(t.Context())
	go func() {
		defer close(collected)
		for {
			select {
			case out := <-msgBus.OutboundChan():
				kind := out.Context.Raw[metadataKeyMessageKind]
				kinds = append(kinds, kind)
				if kind == messageKindSteeringRead {
					read = append(read, out.Content)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	if _, err := al.ProcessDirectWithChannel(
		t.Context(),
		"read the readme",
		"test-session",
		"grpc",
		"run-1",
	); err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}
	// Everything the turn published is in the channel by now: stop the reader,
	// then take what it had not reached yet.
	stop()
	<-collected
	for drained := false; !drained; {
		select {
		case out := <-msgBus.OutboundChan():
			kind := out.Context.Raw[metadataKeyMessageKind]
			kinds = append(kinds, kind)
			if kind == messageKindSteeringRead {
				read = append(read, out.Content)
			}
		default:
			drained = true
		}
	}

	at := slices.Index(kinds, messageKindSteeringRead)
	if len(read) != 1 || read[0] != "and what is 37 x 41?" {
		t.Fatalf("want one read of the message, got %q (kinds %v)", read, kinds)
	}
	if result := slices.Index(kinds, messageKindToolResult); result < 0 || at < result {
		t.Fatalf("the read should come after the batch's result, kinds %v", kinds)
	}
}

// A message no turn read before its turn ended is the whole request of the
// continuation that reads it: there is no earlier work for it to join.
func TestSteeringContinuationGoesWithoutTheJoinNote(t *testing.T) {
	al, provider, _ := newSteeredLoop(t, 0, 0, "",
		providers.LLMResponse{Content: "37 x 41 = 1517."},
	)
	if err := al.Steer(providers.Message{Role: "user", Content: "and what is 37 x 41?"}); err != nil {
		t.Fatalf("steer: %v", err)
	}

	if _, err := al.Continue(t.Context(), "test-session", "test", "chat1"); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if !provider.callCarried(1, "and what is 37 x 41?") || provider.callCarried(1, joinedRequestNote) ||
		provider.callCarried(1, joinedAckNote) {
		t.Fatal("the continuation should carry the message and not the note")
	}
}
