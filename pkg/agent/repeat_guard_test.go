package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const (
	createToolName = "mcp_skip_skip_project_create"
	statusToolName = "mcp_skip_skip_project_status"
)

// mcpTestTool is an MCP-like tool: it declares what a repeat does, the way MCP
// annotations do.
type mcpTestTool struct {
	countingTool
	safety tools.RepeatSafety
	fail   bool
}

func newMCPTestTool(name string, safety tools.RepeatSafety) *mcpTestTool {
	return &mcpTestTool{countingTool: countingTool{name: name}, safety: safety}
}

func (m *mcpTestTool) RepeatSafety() tools.RepeatSafety { return m.safety }

// Parameters: nothing required, so {} is valid, like skip_project_create.
func (m *mcpTestTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":      map[string]any{"type": "string"},
			"projectId": map[string]any{"type": "integer"},
		},
	}
}

func (m *mcpTestTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	m.calls.Add(1)
	if m.fail {
		return tools.ErrorResult("You are not allowed to create new projects. Upgrade your plan.")
	}
	return tools.NewToolResult(`{"projectId":64498,"name":"Unnamed"}`)
}

type stepCall struct {
	tool string
	args map[string]any
}

// stepProvider replies with one step of tool calls per round, in order,
// whatever the results say — the prod model kept emitting its call while
// writing that it would stop — and answers once the steps run out.
type stepProvider struct {
	steps      [][]stepCall
	next       int
	beforeStep func(step int)
}

func (p *stepProvider) Chat(
	context.Context,
	[]providers.Message,
	[]providers.ToolDefinition,
	string,
	map[string]any,
) (*providers.LLMResponse, error) {
	if p.next >= len(p.steps) {
		return &providers.LLMResponse{Content: "fim"}, nil
	}
	p.next++
	if p.beforeStep != nil {
		p.beforeStep(p.next)
	}
	var calls []providers.ToolCall
	for i, call := range p.steps[p.next-1] {
		args := call.args
		if args == nil {
			args = map[string]any{}
		}
		calls = append(calls, providers.ToolCall{
			ID:        fmt.Sprintf("call-%d-%d", p.next, i),
			Type:      "function",
			Name:      call.tool,
			Arguments: args,
		})
	}
	return &providers.LLMResponse{ToolCalls: calls}, nil
}

func (p *stepProvider) GetDefaultModel() string { return "mock-model" }

// oneCallPerStep turns a sequence of calls into one reply each.
func oneCallPerStep(calls ...stepCall) [][]stepCall {
	steps := make([][]stepCall, len(calls))
	for i, call := range calls {
		steps[i] = []stepCall{call}
	}
	return steps
}

func times(call stepCall, n int) []stepCall {
	calls := make([]stepCall, n)
	for i := range calls {
		calls[i] = call
	}
	return calls
}

type guardRun struct {
	al       *AgentLoop
	provider *stepProvider
	resp     string
	history  []providers.Message
}

func (r guardRun) toolResults(content string) int {
	n := 0
	for _, m := range r.history {
		if m.Role == "tool" && m.Content == content {
			n++
		}
	}
	return n
}

func runSteps(t *testing.T, provider *stepProvider, registered ...tools.Tool) guardRun {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	for _, tool := range registered {
		al.registry.GetDefaultAgent().Tools.Register(tool)
	}
	resp, err := al.ProcessDirect(context.Background(), "lê o projeto 40235", "repeat-guard")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	return guardRun{al: al, provider: provider, resp: resp, history: directSessionHistory(t, al)}
}

var createCall = stepCall{tool: createToolName}

// The prod loop: the same create call every round, whatever it is told.
func TestRepeatedSideEffectCallStopsAndEndsTheTurn(t *testing.T) {
	create := newMCPTestTool(createToolName, tools.RepeatUnsafe)

	run := runSteps(t, &stepProvider{steps: oneCallPerStep(times(createCall, 10)...)}, create)

	if got := create.calls.Load(); got != maxUnchangedRuns {
		t.Fatalf("project created %d times, want %d", got, maxUnchangedRuns)
	}
	if got := run.toolResults(repeatedSideEffectContent); got != maxRepeatRefusals {
		t.Errorf("history has %d refusals, want %d (each refused call keeps its answer)", got, maxRepeatRefusals)
	}
	if run.provider.next != maxUnchangedRuns+maxRepeatRefusals {
		t.Errorf("LLM rounds = %d, want %d: the turn must end instead of looping to max_tool_iterations",
			run.provider.next, maxUnchangedRuns+maxRepeatRefusals)
	}
	if !strings.HasPrefix(run.resp, "I stopped this turn") {
		t.Errorf("response = %q, want the stop summary", run.resp)
	}
	if last := run.history[len(run.history)-1]; last.Role != "assistant" || last.Content != run.resp {
		t.Errorf("last history message = %s %q, want the stop summary as the turn's reply", last.Role, last.Content)
	}
}

// Ending the turn mid-reply still answers every call of that reply, or the
// history drops the whole assistant message.
func TestEndingTheTurnAnswersTheRestOfTheReply(t *testing.T) {
	create := newMCPTestTool(createToolName, tools.RepeatUnsafe)
	exec := &countingTool{name: "exec"}
	reply := append(times(createCall, 4), stepCall{tool: "exec"})

	run := runSteps(t, &stepProvider{steps: [][]stepCall{reply}}, create, exec)

	if got := create.calls.Load(); got != maxUnchangedRuns {
		t.Fatalf("project created %d times, want %d", got, maxUnchangedRuns)
	}
	if got := exec.calls.Load(); got != 0 {
		t.Errorf("exec ran %d times after the turn ended", got)
	}
	if got := run.toolResults(repeatStopSkipContent); got != 1 {
		t.Errorf("%d calls answered as skipped, want 1 (the exec)", got)
	}
}

// Calls that change nothing between two repeats don't make the repeat mean
// anything new (prod: the model ran a tool_search and the create in one reply).
func TestCallsThatChangeNothingKeepTheRun(t *testing.T) {
	tests := []struct {
		name    string
		between stepCall
		tool    tools.Tool
	}{
		{
			name:    "tool discovery",
			between: stepCall{tool: tools.BM25SearchToolName},
			tool:    &countingTool{name: tools.BM25SearchToolName},
		},
		{name: "native read", between: stepCall{tool: "read_file"}, tool: &countingTool{name: "read_file"}},
		{
			name:    "MCP read declared read-only",
			between: stepCall{tool: statusToolName},
			tool:    newMCPTestTool(statusToolName, tools.RepeatReadOnly),
		},
		{
			name:    "failed side-effecting call",
			between: stepCall{tool: "mcp_skip_skip_project_publish"},
			tool:    &mcpTestTool{countingTool: countingTool{name: "mcp_skip_skip_project_publish"}, fail: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			create := newMCPTestTool(createToolName, tools.RepeatUnsafe)
			calls := []stepCall{createCall, tt.between, createCall, tt.between, createCall, tt.between, createCall}

			runSteps(t, &stepProvider{steps: oneCallPerStep(calls...)}, create, tt.tool)

			if got := create.calls.Load(); got != maxUnchangedRuns {
				t.Errorf("project created %d times, want %d", got, maxUnchangedRuns)
			}
		})
	}
}

func TestRepeatsThatMeanSomethingStillRun(t *testing.T) {
	named := func(name string) stepCall {
		return stepCall{tool: createToolName, args: map[string]any{"name": name}}
	}
	tests := []struct {
		name  string
		calls []stepCall
		tool  *mcpTestTool
		extra []tools.Tool
		want  int32
	}{
		{
			name:  "server declared it read-only",
			calls: times(createCall, 4),
			tool:  newMCPTestTool(createToolName, tools.RepeatReadOnly),
			want:  4,
		},
		{
			name:  "server declared it idempotent",
			calls: times(createCall, 4),
			tool:  newMCPTestTool(createToolName, tools.RepeatIdempotent),
			want:  4,
		},
		{
			name:  "every call failed, nothing to repeat",
			calls: times(createCall, 4),
			tool:  &mcpTestTool{countingTool: countingTool{name: createToolName}, fail: true},
			want:  4,
		},
		{
			name:  "alternating values, each call a change",
			calls: []stepCall{named("a"), named("b"), named("a"), named("b"), named("a")},
			tool:  newMCPTestTool(createToolName, tools.RepeatUnsafe),
			want:  5,
		},
		{
			name: "a native call in between may change what the repeat does",
			calls: []stepCall{
				createCall, {tool: "exec"}, createCall, {tool: "exec"}, createCall, {tool: "exec"}, createCall,
			},
			tool:  newMCPTestTool(createToolName, tools.RepeatUnsafe),
			extra: []tools.Tool{&countingTool{name: "exec"}},
			want:  4,
		},
		{
			name: "an idempotent MCP call in between changes things once",
			calls: []stepCall{
				createCall,
				{tool: "mcp_skip_skip_file_write"},
				createCall,
				{tool: "mcp_skip_skip_file_write"},
				createCall,
			},
			tool:  newMCPTestTool(createToolName, tools.RepeatUnsafe),
			extra: []tools.Tool{newMCPTestTool("mcp_skip_skip_file_write", tools.RepeatIdempotent)},
			want:  3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := runSteps(
				t,
				&stepProvider{steps: oneCallPerStep(tt.calls...)},
				append([]tools.Tool{tt.tool}, tt.extra...)...)

			if got := run.toolResults(repeatedSideEffectContent); got != 0 {
				t.Fatalf("%d calls refused, want none", got)
			}
			if got := tt.tool.calls.Load(); got != tt.want {
				t.Errorf("tool ran %d times, want %d", got, tt.want)
			}
		})
	}
}

// The registry runs the tool with "40235" coerced to 40235: the same action.
func TestArgumentsAreComparedAsTheToolGetsThem(t *testing.T) {
	create := newMCPTestTool(createToolName, tools.RepeatUnsafe)
	asString := stepCall{tool: createToolName, args: map[string]any{"projectId": "40235"}}
	asNumber := stepCall{tool: createToolName, args: map[string]any{"projectId": 40235}}

	run := runSteps(t, &stepProvider{steps: oneCallPerStep(asString, asNumber, asString)}, create)

	if got := create.calls.Load(); got != maxUnchangedRuns {
		t.Fatalf("tool ran %d times, want %d", got, maxUnchangedRuns)
	}
	if got := run.toolResults(repeatedSideEffectContent); got != 1 {
		t.Errorf("%d refusals, want 1", got)
	}
}

func TestNativeToolRepeatsAreNotGuarded(t *testing.T) {
	exec := &countingTool{name: "exec"}

	run := runSteps(t, &stepProvider{steps: oneCallPerStep(times(stepCall{tool: "exec"}, 4)...)}, exec)

	if got := run.toolResults(repeatedSideEffectContent); got != 0 {
		t.Fatalf("%d native repeats refused, want none", got)
	}
	if got := exec.calls.Load(); got != 4 {
		t.Errorf("exec ran %d times, want 4", got)
	}
}

func TestIdenticalCallsInOneReplyShareTheRun(t *testing.T) {
	create := newMCPTestTool(createToolName, tools.RepeatUnsafe)

	run := runSteps(t, &stepProvider{steps: [][]stepCall{times(createCall, 3)}}, create)

	if got := create.calls.Load(); got != maxUnchangedRuns {
		t.Fatalf("project created %d times, want %d", got, maxUnchangedRuns)
	}
	if got := run.toolResults(repeatedSideEffectContent); got != 1 {
		t.Errorf("%d refusals, want 1", got)
	}
}

// The user steering the turn ("send it again") makes the next repeat theirs.
func TestSteeringStartsANewRun(t *testing.T) {
	create := newMCPTestTool(createToolName, tools.RepeatUnsafe)
	provider := &stepProvider{steps: oneCallPerStep(times(createCall, 4)...)}
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.registry.GetDefaultAgent().Tools.Register(create)
	provider.beforeStep = func(step int) {
		if step == maxUnchangedRuns {
			if err := al.Steer(providers.Message{Role: "user", Content: "cria mais dois"}); err != nil {
				t.Errorf("Steer: %v", err)
			}
		}
	}

	if _, err := al.ProcessDirect(context.Background(), "cria dois projetos", "repeat-steer"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if got := create.calls.Load(); got != 4 {
		t.Fatalf("project created %d times, want 4 (two before the steering, two after)", got)
	}
}

// A new turn starts from a user message: repeating there is the user's call.
func TestRepeatGuardResetsEachTurn(t *testing.T) {
	create := newMCPTestTool(createToolName, tools.RepeatUnsafe)
	provider := &callOnceProvider{tool: createToolName}
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.registry.GetDefaultAgent().Tools.Register(create)

	for _, msg := range []string{"cria um projeto", "cria outro", "e mais um"} {
		if _, err := al.ProcessDirect(context.Background(), msg, "repeat-next-turn"); err != nil {
			t.Fatalf("ProcessDirect(%q): %v", msg, err)
		}
	}
	if got := create.calls.Load(); got != 3 {
		t.Fatalf("project created %d times over three turns, want 3", got)
	}
}
