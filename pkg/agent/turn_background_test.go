package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const backgroundChildPrompt = "start the lifecycle regression background process"

type parentBackgroundProvider struct {
	child providers.LLMProvider
	root  func(context.Context) (*providers.LLMResponse, error)
}

func (p *parentBackgroundProvider) Chat(
	ctx context.Context, messages []providers.Message, defs []providers.ToolDefinition,
	model string, opts map[string]any,
) (*providers.LLMResponse, error) {
	for _, message := range messages {
		if message.Role == "user" && strings.Contains(message.Content, backgroundChildPrompt) {
			return p.child.Chat(ctx, messages, defs, model, opts)
		}
	}
	return p.root(ctx)
}

func (p *parentBackgroundProvider) GetDefaultModel() string { return "mock-model" }

func assertBackgroundCommandRunning(t *testing.T, execTool *tools.ExecTool, command string, want bool) {
	t.Helper()
	var listed struct {
		Sessions []tools.SessionInfo `json:"sessions"`
	}
	raw := execTool.Execute(context.Background(), map[string]any{"action": "list"}).ForLLM
	if err := json.Unmarshal([]byte(raw), &listed); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, process := range listed.Sessions {
		if process.Command == command {
			if got := process.Status == "running"; got != want {
				t.Errorf("process running = %v, want %v (status %s)", got, want, process.Status)
			}
			// Cleanup also runs in the failing pre-fix case, regardless of ownership.
			t.Cleanup(func() {
				_ = execTool.Execute(context.Background(), map[string]any{"action": "kill", "sessionId": process.ID})
			})
			return
		}
	}
	t.Fatal("child process was never started")
}

func TestRunTurn_ParentOwnsFinishedChildProcesses(t *testing.T) {
	for _, outcome := range []string{"error", "stop", "panic", "success"} {
		t.Run(outcome, func(t *testing.T) {
			command := "sleep 30 # " + t.Name()
			provider := &parentBackgroundProvider{child: &backgroundThenAnswerProvider{command: command}}
			al, agent, cleanup := newTurnCoordTestLoop(t, provider)
			defer cleanup()
			execTool, err := tools.NewExecTool("", false)
			if err != nil {
				t.Fatal(err)
			}
			agent.Tools.Register(execTool)
			opts := makeTestProcessOpts(t.Name())
			opts.Dispatch = DispatchRequest{SessionKey: t.Name(), UserMessage: "parent task"}
			parent := newTurnState(agent, opts, al.newTurnEventScope(agent.ID, t.Name(), nil))
			var once sync.Once
			provider.root = func(ctx context.Context) (*providers.LLMResponse, error) {
				once.Do(func() {
					_, err = spawnSubTurn(ctx, al, parent, SubTurnConfig{
						Model:        "test-model",
						Tools:        []tools.Tool{},
						SystemPrompt: backgroundChildPrompt,
						Timeout:      5 * time.Second,
					})
					if err != nil {
						t.Fatalf("child: %v", err)
					}
				})
				switch outcome {
				case "error":
					return nil, errors.New("parent provider failed")
				case "stop":
					if _, stopErr := al.stopActiveTurnForSession(parent.sessionKey); stopErr != nil {
						t.Fatal(stopErr)
					}
					return nil, ctx.Err()
				case "panic":
					panic("parent provider panicked")
				default:
					return &providers.LLMResponse{Content: "done"}, nil
				}
			}
			panicked := false
			func() {
				defer func() { panicked = recover() != nil }()
				_, err = al.runTurn(context.Background(), parent, NewPipeline(al))
			}()
			if panicked != (outcome == "panic") {
				t.Fatalf("panic = %v, outcome %s", panicked, outcome)
			}
			if outcome == "error" && err == nil {
				t.Fatal("expected parent failure")
			}
			assertBackgroundCommandRunning(t, execTool, command, outcome == "success")
		})
	}
}

func TestRunTurn_LaterFailurePreservesEarlierSuccessfulProcesses(t *testing.T) {
	command := "sleep 30 # " + t.Name()
	provider := &parentBackgroundProvider{child: &backgroundThenAnswerProvider{command: command}}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	execTool, err := tools.NewExecTool("", false)
	if err != nil {
		t.Fatal(err)
	}
	agent.Tools.Register(execTool)
	const sessionKey = "same-background-conversation"
	opts := makeTestProcessOpts(sessionKey)
	opts.Dispatch = DispatchRequest{SessionKey: sessionKey, UserMessage: "parent task"}
	first := newTurnState(agent, opts, al.newTurnEventScope(agent.ID, sessionKey, nil))
	provider.root = func(ctx context.Context) (*providers.LLMResponse, error) {
		_, childErr := spawnSubTurn(ctx, al, first, SubTurnConfig{
			Model: "test-model", Tools: []tools.Tool{}, SystemPrompt: backgroundChildPrompt, Timeout: 5 * time.Second,
		})
		return &providers.LLMResponse{Content: "done"}, childErr
	}
	if _, err = al.runTurn(context.Background(), first, NewPipeline(al)); err != nil {
		t.Fatal(err)
	}
	assertBackgroundCommandRunning(t, execTool, command, true)
	provider.root = func(context.Context) (*providers.LLMResponse, error) {
		return nil, errors.New("a later unrelated turn failed")
	}
	later := newTurnState(agent, opts, al.newTurnEventScope(agent.ID, sessionKey, nil))
	if _, err = al.runTurn(context.Background(), later, NewPipeline(al)); err == nil {
		t.Fatal("expected later failure")
	}
	assertBackgroundCommandRunning(t, execTool, command, true)
}

type lateBackgroundProvider struct {
	backgroundThenAnswerProvider
	started chan struct{}
	finish  chan struct{}
	calls   atomic.Int32
}

func (p *lateBackgroundProvider) Chat(
	ctx context.Context, messages []providers.Message, defs []providers.ToolDefinition,
	model string, opts map[string]any,
) (*providers.LLMResponse, error) {
	if p.calls.Add(1) == 1 {
		return p.backgroundThenAnswerProvider.Chat(ctx, messages, defs, model, opts)
	}
	close(p.started)
	select {
	case <-p.finish:
		return &providers.LLMResponse{Content: "done"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRunTurn_LateChildCannotHandProcessesToFailedParent(t *testing.T) {
	command := "sleep 30 # " + t.Name()
	child := &lateBackgroundProvider{
		backgroundThenAnswerProvider: backgroundThenAnswerProvider{command: command},
		started:                      make(chan struct{}), finish: make(chan struct{}),
	}
	provider := &parentBackgroundProvider{child: child}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()
	execTool, err := tools.NewExecTool("", false)
	if err != nil {
		t.Fatal(err)
	}
	agent.Tools.Register(execTool)
	opts := makeTestProcessOpts(t.Name())
	opts.Dispatch = DispatchRequest{SessionKey: t.Name(), UserMessage: "parent task"}
	parent := newTurnState(agent, opts, al.newTurnEventScope(agent.ID, t.Name(), nil))
	done := make(chan error, 1)
	var once sync.Once
	provider.root = func(ctx context.Context) (*providers.LLMResponse, error) {
		once.Do(func() {
			go func() {
				_, childErr := spawnSubTurn(ctx, al, parent, SubTurnConfig{
					Model: "test-model", Tools: []tools.Tool{}, SystemPrompt: backgroundChildPrompt,
					Timeout: 5 * time.Second, Async: true,
				})
				done <- childErr
			}()
		})
		select {
		case <-child.started:
			return nil, errors.New("parent failed before child finished")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if _, err = al.runTurn(context.Background(), parent, NewPipeline(al)); err == nil {
		t.Fatal("expected parent failure")
	}
	close(child.finish)
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("independent child failed: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("child did not finish")
	}
	assertBackgroundCommandRunning(t, execTool, command, false)
}
