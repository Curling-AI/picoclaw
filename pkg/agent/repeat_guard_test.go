package agent

import (
	"context"
	"fmt"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const createToolName = "mcp_skip_skip_project_create"

// sideEffectTestTool is an MCP-like tool: it declares whether repeating it is
// harmless, the way MCP annotations do.
type sideEffectTestTool struct {
	countingTool
	harmless bool
	fail     bool
}

func (s *sideEffectTestTool) RepeatIsHarmless() bool { return s.harmless }

// Parameters mirrors skip_project_create: nothing required, so {} is valid.
func (s *sideEffectTestTool) Parameters() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
	}
}

func (s *sideEffectTestTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	s.calls.Add(1)
	if s.fail {
		return tools.ErrorResult("You are not allowed to create new projects. Upgrade your plan.")
	}
	return tools.NewToolResult(`{"projectId":64498,"name":"Unnamed"}`)
}

type toolStep struct {
	tool string
	args map[string]any
}

// toolStepProvider emits one tool call per round, in order, whatever the
// results say — the prod model kept emitting its call while writing that it
// would stop — and answers once the script runs out.
type toolStepProvider struct {
	script []toolStep
	next   int
}

func (p *toolStepProvider) Chat(
	context.Context,
	[]providers.Message,
	[]providers.ToolDefinition,
	string,
	map[string]any,
) (*providers.LLMResponse, error) {
	if p.next >= len(p.script) {
		return &providers.LLMResponse{Content: "fim"}, nil
	}
	call := p.script[p.next]
	p.next++
	args := call.args
	if args == nil {
		args = map[string]any{}
	}
	return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
		ID:        fmt.Sprintf("call-%d", p.next),
		Type:      "function",
		Name:      call.tool,
		Arguments: args,
	}}}, nil
}

func (p *toolStepProvider) GetDefaultModel() string { return "mock-model" }

func repeat(call toolStep, n int) []toolStep {
	script := make([]toolStep, n)
	for i := range script {
		script[i] = call
	}
	return script
}

func runScript(t *testing.T, script []toolStep, registered ...tools.Tool) (blocked int) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &toolStepProvider{script: script})
	for _, tool := range registered {
		al.registry.GetDefaultAgent().Tools.Register(tool)
	}
	if _, err := al.ProcessDirect(context.Background(), "lê o projeto 40235", "repeat-guard"); err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	for _, m := range directSessionHistory(t, al) {
		if m.Role == "tool" && m.Content == repeatedSideEffectContent {
			blocked++
		}
	}
	return blocked
}

func TestRepeatedSideEffectCallRunsOnce(t *testing.T) {
	create := &sideEffectTestTool{countingTool: countingTool{name: createToolName}}

	blocked := runScript(t, repeat(toolStep{tool: createToolName}, 5), create)

	if got := create.calls.Load(); got != 1 {
		t.Fatalf("project created %d times, want 1", got)
	}
	if blocked != 4 {
		t.Errorf("history has %d blocks, want 4 (each repeat answered, so the model sees why)", blocked)
	}
}

// Searching for tools between two repeats changes nothing (prod: the model ran
// a tool_search and the create call in the same reply).
func TestDiscoveryBetweenRepeatsStillBlocks(t *testing.T) {
	create := &sideEffectTestTool{countingTool: countingTool{name: createToolName}}
	search := &countingTool{name: tools.BM25SearchToolName}
	script := []toolStep{
		{tool: createToolName},
		{tool: tools.BM25SearchToolName},
		{tool: createToolName},
	}

	runScript(t, script, create, search)

	if got := create.calls.Load(); got != 1 {
		t.Fatalf("project created %d times, want 1", got)
	}
}

func TestRepeatsThatMeanSomethingStillRun(t *testing.T) {
	sideEffect := func() *sideEffectTestTool {
		return &sideEffectTestTool{countingTool: countingTool{name: createToolName}}
	}
	named := func(name string) toolStep {
		return toolStep{tool: createToolName, args: map[string]any{"name": name}}
	}
	harmless := sideEffect()
	harmless.harmless = true
	failing := sideEffect()
	failing.fail = true
	tests := []struct {
		name   string
		script []toolStep
		tool   *sideEffectTestTool
		extra  []tools.Tool
		want   int32
	}{
		{
			name:   "server declared it read-only or idempotent",
			script: repeat(toolStep{tool: createToolName}, 3),
			tool:   harmless,
			want:   3,
		},
		{
			name:   "earlier calls failed, nothing to repeat",
			script: repeat(toolStep{tool: createToolName}, 3),
			tool:   failing,
			want:   3,
		},
		{
			name:   "back to an earlier value is a revert",
			script: []toolStep{named("a"), named("b"), named("a")},
			tool:   sideEffect(),
			want:   3,
		},
		{
			name:   "a native call in between may have changed what the repeat does",
			script: []toolStep{{tool: createToolName}, {tool: "exec"}, {tool: createToolName}},
			tool:   sideEffect(),
			extra:  []tools.Tool{&countingTool{name: "exec"}},
			want:   2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocked := runScript(t, tt.script, append([]tools.Tool{tt.tool}, tt.extra...)...)
			if blocked != 0 {
				t.Fatalf("%d repeats blocked, want none", blocked)
			}
			if got := tt.tool.calls.Load(); got != tt.want {
				t.Errorf("tool ran %d times, want %d", got, tt.want)
			}
		})
	}
}

func TestNativeToolRepeatsAreNotGuarded(t *testing.T) {
	exec := &countingTool{name: "exec"}

	if blocked := runScript(t, repeat(toolStep{tool: "exec"}, 3), exec); blocked != 0 {
		t.Fatalf("%d native repeats blocked, want none", blocked)
	}
	if got := exec.calls.Load(); got != 3 {
		t.Errorf("exec ran %d times, want 3", got)
	}
}

// A new turn starts from a user message: repeating there is the user's call.
func TestSideEffectGuardResetsEachTurn(t *testing.T) {
	create := &sideEffectTestTool{countingTool: countingTool{name: createToolName}}
	provider := &callOnceProvider{tool: createToolName}
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.registry.GetDefaultAgent().Tools.Register(create)

	for _, msg := range []string{"cria um projeto", "cria outro"} {
		if _, err := al.ProcessDirect(context.Background(), msg, "repeat-next-turn"); err != nil {
			t.Fatalf("ProcessDirect(%q): %v", msg, err)
		}
	}
	if got := create.calls.Load(); got != 2 {
		t.Fatalf("project created %d times over two turns, want 2", got)
	}
}
