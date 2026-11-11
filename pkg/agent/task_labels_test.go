package agent

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

func requireCallLabels(t *testing.T, got [][]string, want ...[]string) {
	t.Helper()
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Fatalf("call labels = %q, want %q", got, want)
	}
}

// A message starts a task labeled with its channel, and the turn's own calls
// say they are the agent's.
func TestRunTurn_LabelsAMessageTaskByItsChannel(t *testing.T) {
	provider := &taskIDRecorder{tool: "note"}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	agent.Tools.Register(noteTool{})

	opts := makeTestProcessOpts("agent:main:task-labels")
	opts.Channel = "grpc"
	if _, err := al.runAgentLoop(context.Background(), agent, opts); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}

	want := []string{protocoltypes.TaskMessage, "channel:web", protocoltypes.RoleAgent}
	requireCallLabels(t, provider.taskLabels(), want, want)
}

// A sub-turn carries its parent's task labels and says it is a subagent.
func TestSpawnSubTurn_CarriesTheTaskLabelsAsASubagent(t *testing.T) {
	provider := &taskIDRecorder{tool: "note"}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	agent.Tools.Register(noteTool{})

	ctx := protocoltypes.WithTaskID(context.Background(), "task-parent", protocoltypes.TaskAutomation)
	if _, err := spawnSubTurn(ctx, al, newStopTestParent(agent), SubTurnConfig{
		Model:        "test-model",
		SystemPrompt: "write a note",
	}); err != nil {
		t.Fatalf("spawnSubTurn: %v", err)
	}

	want := []string{protocoltypes.TaskAutomation, protocoltypes.RoleSubagent}
	requireCallLabels(t, provider.taskLabels(), want, want)
}

// The turn that reads a spawn's result keeps the labels of the task that
// launched it, not ones read off the system channel it arrives on.
func TestAsyncResult_KeepsTheLabelsOfTheTaskThatLaunchedIt(t *testing.T) {
	provider := &taskIDRecorder{tool: "late_async"}
	al, msgBus, _ := newSystemMessageTestLoop(t, provider)
	tool := &lateAsyncTool{}
	al.RegisterTool(tool)

	ctx := protocoltypes.WithTaskID(context.Background(), "task-auto", protocoltypes.TaskAutomation)
	if _, err := al.runAgentLoop(ctx, al.registry.GetDefaultAgent(), processOptions{
		SessionKey:      conversationSession,
		Channel:         "grpc",
		ChatID:          "run-1",
		UserMessage:     "run async tool",
		DefaultResponse: defaultResponse,
	}); err != nil {
		t.Fatalf("runAgentLoop: %v", err)
	}
	tool.finish("background result")
	var result bus.InboundMessage
	select {
	case result = <-msgBus.InboundChan():
	case <-time.After(2 * time.Second):
		t.Fatal("async result was not published")
	}
	if _, err := al.processSystemMessage(context.Background(), result); err != nil {
		t.Fatalf("processSystemMessage: %v", err)
	}

	want := []string{protocoltypes.TaskAutomation, protocoltypes.RoleAgent}
	requireCallLabels(t, provider.taskLabels(), want, want, want, want)
}

// A message sent mid-turn starts a message task, whatever task the turn was on.
func TestSteering_LabelsTheNewTaskAsAMessage(t *testing.T) {
	provider := &steeredTaskRecorder{taskIDRecorder: taskIDRecorder{tool: "late_async"}}
	al, _, _ := newSystemMessageTestLoop(t, provider)
	al.RegisterTool(&lateAsyncTool{})
	provider.steer = func() {
		if err := al.Steer(providers.Message{Role: "user", Content: "and also check the docs"}); err != nil {
			t.Errorf("steer: %v", err)
		}
	}

	ctx := protocoltypes.WithTaskID(t.Context(), "task-auto", protocoltypes.TaskAutomation)
	if _, err := al.ProcessDirectWithChannel(ctx, "run the job", conversationSession, "grpc", "run-1"); err != nil {
		t.Fatalf("ProcessDirectWithChannel: %v", err)
	}

	requireCallLabels(t, provider.taskLabels(),
		[]string{protocoltypes.TaskAutomation, protocoltypes.RoleAgent},
		[]string{protocoltypes.TaskMessage, "channel:web", protocoltypes.RoleAgent},
	)
}

// summaryRecorder answers summaries and reports the task and labels of each call.
type summaryRecorder struct {
	calls chan []string
}

func (p *summaryRecorder) Chat(
	ctx context.Context, _ []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any,
) (*providers.LLMResponse, error) {
	p.calls <- append([]string{protocoltypes.TaskIDFromContext(ctx)}, protocoltypes.TaskCallLabels(ctx)...)
	return &providers.LLMResponse{Content: "summary text"}, nil
}

func (p *summaryRecorder) GetDefaultModel() string { return "test-model" }

// The summary a turn leaves behind runs after the turn ends, still in its task,
// as compaction.
func TestLegacySummary_RunsInTheTaskOfTheTurnThatLeftIt(t *testing.T) {
	provider := &summaryRecorder{calls: make(chan []string, 8)}
	al := NewAgentLoop(&config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace:                 t.TempDir(),
		ModelName:                 "test-model",
		MaxTokens:                 4096,
		MaxToolIterations:         10,
		ContextWindow:             8000,
		SummarizeMessageThreshold: 2,
		SummarizeTokenPercent:     75,
	}}}, bus.NewMessageBus(), provider)
	al.registry.GetDefaultAgent().Sessions.SetHistory("session-1", []providers.Message{
		{Role: "user", Content: "Question one"},
		{Role: "assistant", Content: "Answer one"},
		{Role: "user", Content: "Question two"},
		{Role: "assistant", Content: "Answer two"},
		{Role: "user", Content: "Question three"},
		{Role: "assistant", Content: "Answer three"},
	})

	turnCtx, endTurn := context.WithCancel(protocoltypes.WithTaskRole(
		protocoltypes.WithTaskID(context.Background(), "task-turn", protocoltypes.MessageTaskLabels("grpc")...),
		protocoltypes.RoleAgent,
	))
	lcm := &legacyContextManager{al: al}
	if err := lcm.Compact(turnCtx, &CompactRequest{
		SessionKey: "session-1",
		Reason:     ContextCompressReasonSummarize,
	}); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	endTurn()

	select {
	case got := <-provider.calls:
		want := []string{"task-turn", protocoltypes.TaskMessage, "channel:web", protocoltypes.RoleCompaction}
		if !slices.Equal(got, want) {
			t.Fatalf("summary call = %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the summary never called the model")
	}
	// The summary goes on writing the session; the test's directory outlives it.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, busy := lcm.summarizing.Load(al.registry.GetDefaultAgent().ID + ":session-1"); !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the summary never finished")
		}
	}
}

// Seahorse's summaries go through the observed provider, as compaction; the
// evolution provider adds no role.
func TestObservedProvider_TagsSummariesAsCompaction(t *testing.T) {
	for source, want := range map[string][]string{
		"summarize": {"task-turn", protocoltypes.TaskMessage, protocoltypes.RoleCompaction},
		"evolution": {"task-turn", protocoltypes.TaskMessage},
	} {
		provider := &summaryRecorder{calls: make(chan []string, 1)}
		ctx := protocoltypes.WithTaskID(context.Background(), "task-turn", protocoltypes.TaskMessage)
		if _, err := newObservedProvider(provider, source).Chat(ctx, nil, nil, "test-model", nil); err != nil {
			t.Fatalf("%s: Chat: %v", source, err)
		}
		if got := <-provider.calls; !slices.Equal(got, want) {
			t.Fatalf("%s call = %q, want %q", source, got, want)
		}
	}
}

// The vision sub-call a turn makes for an image says it is the vision delegate.
func TestVisionDelegate_TagsItsCallAsVision(t *testing.T) {
	p, ts, exec := delegationFixture(true, &recordingVisionProvider{})
	provider := &summaryRecorder{calls: make(chan []string, 1)}
	ts.agent.Provider = provider
	ctx := protocoltypes.WithTaskRole(
		protocoltypes.WithTaskID(context.Background(), "task-turn", protocoltypes.TaskMessage),
		protocoltypes.RoleAgent,
	)

	if _, err := p.delegateMediaTurn(ctx, ts, exec); err != nil {
		t.Fatalf("delegateMediaTurn: %v", err)
	}

	want := []string{"task-turn", protocoltypes.TaskMessage, protocoltypes.RoleVision}
	select {
	case got := <-provider.calls:
		if !slices.Equal(got, want) {
			t.Fatalf("vision call = %q, want %q", got, want)
		}
	default:
		t.Fatal("the turn never called the vision delegate")
	}
}
