package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// The upstream's answer when the request is bigger than the model's window
// (Vercel AI Gateway, observed in prod).
var errInputExceedsContextWindow = errors.New("API request failed:\n  Status: 400\n  Body:   " +
	`{"error":{"message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}`)

// bigOutputTool answers every call with size characters, marked at both ends.
type bigOutputTool struct {
	size  int
	calls atomic.Int32
}

func (t *bigOutputTool) Name() string        { return "big_output" }
func (t *bigOutputTool) Description() string { return "Returns a large output" }
func (t *bigOutputTool) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (t *bigOutputTool) Execute(context.Context, map[string]any) *tools.ToolResult {
	t.calls.Add(1)
	return tools.NewToolResult(bigOutput(t.size))
}

// bigOutput is prose, not one long token: the registry already swaps
// base64-like payloads for a note before they reach the loop.
func bigOutput(size int) string {
	const head, tail, filler = "HEAD-OF-OUTPUT ", " TAIL-OF-OUTPUT", "lorem ipsum dolor sit amet, "
	body := strings.Repeat(filler, size/len(filler)+1)[:size-len(head)-len(tail)]
	return head + body + tail
}

// windowedToolProvider asks for big_output toolRounds times, then answers.
// Like the gateway, it rejects any call estimated above window.
type windowedToolProvider struct {
	mu         sync.Mutex
	window     int
	toolRounds int
	calls      []windowedCall
}

type windowedCall struct {
	tokens   int
	rejected bool
	content  string
}

func (p *windowedToolProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	toolDefs []providers.ToolDefinition,
	_ string,
	opts map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	call := windowedCall{tokens: EstimateToolDefsTokens(toolDefs)}
	if maxTokens, ok := opts["max_tokens"].(int); ok {
		call.tokens += maxTokens
	}
	toolResults := 0
	for _, m := range messages {
		call.tokens += EstimateMessageTokens(m)
		call.content += m.Content + "\n"
		if m.Role == "tool" {
			toolResults++
		}
	}
	call.rejected = p.window > 0 && call.tokens > p.window
	p.calls = append(p.calls, call)
	if call.rejected {
		return nil, errInputExceedsContextWindow
	}
	if toolResults < p.toolRounds {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{{
			ID:        fmt.Sprintf("call-%d", toolResults+1),
			Name:      "big_output",
			Arguments: map[string]any{},
		}}}, nil
	}
	return &providers.LLMResponse{Content: "done"}, nil
}

func (p *windowedToolProvider) GetDefaultModel() string { return "windowed-model" }

func (p *windowedToolProvider) snapshot() []windowedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]windowedCall(nil), p.calls...)
}

type windowedTurn struct {
	al       *AgentLoop
	provider *windowedToolProvider
	agent    *AgentInstance
	base     int // tokens of a call before any tool result
}

// newWindowedTurn builds an agent whose provider and context window agree, the
// window being base + headroom: base is measured on a throwaway turn, so the
// tests do not depend on the size of the system prompt or of the tool set.
func newWindowedTurn(t *testing.T, cfg *config.Config, tool *bigOutputTool, headroom int) *windowedTurn {
	t.Helper()
	provider := &windowedToolProvider{}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	al.RegisterTool(tool)
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("no default agent")
	}
	if _, err := al.ProcessDirectWithChannel(
		context.Background(),
		"ping",
		"sk_v1_calibration",
		"test",
		"calibration",
	); err != nil {
		t.Fatalf("calibration turn: %v", err)
	}
	base := provider.snapshot()[0].tokens
	provider.mu.Lock()
	provider.calls = nil
	provider.window = base + headroom
	provider.mu.Unlock()
	agent.ContextWindow = base + headroom
	return &windowedTurn{al: al, provider: provider, agent: agent, base: base}
}

func windowedConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         t.TempDir(),
				ModelName:         "test-model",
				MaxTokens:         4096,
				MaxToolIterations: 20,
			},
		},
	}
}

func toolMessages(history []providers.Message) []providers.Message {
	var out []providers.Message
	for _, m := range history {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

// Prod, 14-15/10: one browser snapshot of 3.2M characters made the active turn
// alone larger than the model's window. The turn is never compacted, so the retry
// gave up ("refusing to drop active turn messages") and the chat failed. A
// result past the limit now enters the conversation trimmed, start and end,
// with the full output saved in the workspace.
func TestOversizedToolResult_IsTrimmedBeforeItEntersTheTurn(t *testing.T) {
	cfg := windowedConfig(t)
	cfg.Tools.MaxResultChars = 20_000
	tool := &bigOutputTool{size: 1_000_000}
	turn := newWindowedTurn(t, cfg, tool, 20_000)
	turn.provider.toolRounds = 1

	resp, err := turn.al.ProcessDirectWithChannel(
		context.Background(),
		"take a snapshot",
		"sk_v1_oversized",
		"test",
		"oversized",
	)
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if resp != "done" {
		t.Fatalf("response = %q, want done", resp)
	}
	for i, c := range turn.provider.snapshot() {
		if c.rejected {
			t.Fatalf("call %d (%d tokens) was over the window: the result reached the model untrimmed", i, c.tokens)
		}
	}

	results := toolMessages(turn.agent.Sessions.GetHistory("sk_v1_oversized"))
	if len(results) != 1 {
		t.Fatalf("tool results in history = %d, want 1", len(results))
	}
	got := results[0].Content
	if n := len([]rune(got)); n > 20_000 {
		t.Fatalf("stored result has %d characters, want at most the 20000 limit", n)
	}
	for _, want := range []string{"HEAD-OF-OUTPUT", "TAIL-OF-OUTPUT", "1000000 characters"} {
		if !strings.Contains(got, want) {
			t.Fatalf("trimmed result lacks %q:\n%.400s", want, got)
		}
	}

	saved, err := filepath.Glob(
		filepath.Join(cfg.Agents.Defaults.Workspace, "artifacts", "tool-output", "big_output_*.txt"),
	)
	if err != nil || len(saved) != 1 {
		t.Fatalf("saved outputs = %v (err %v), want exactly one", saved, err)
	}
	if !strings.Contains(got, saved[0]) {
		t.Fatalf("trimmed result does not point at the saved file %s", saved[0])
	}
	full, err := os.ReadFile(saved[0])
	if err != nil {
		t.Fatalf("read saved output: %v", err)
	}
	if string(full) != bigOutput(1_000_000) {
		t.Fatalf("saved output has %d bytes, want the full 1000000", len(full))
	}
}

// Prod, 13/10: 258 web_search results of up to 16K characters each, all in
// one turn. No single result is large, but together they outgrew the window,
// and the active turn cannot be compacted. The retry now stands in a short
// note for the earliest results of the turn, in what it sends only, and the
// rest of the turn keeps going.
func TestAccumulatedToolResults_OldestAreElidedFromTheCallInsteadOfFailingTheTurn(t *testing.T) {
	const resultSize = 15_000
	const rounds = 6
	resultTokens := EstimateMessageTokens(providers.Message{Role: "tool", Content: bigOutput(resultSize)})
	cfg := windowedConfig(t)
	tool := &bigOutputTool{size: resultSize}
	// Room for about three results (and the assistant tool calls around them).
	turn := newWindowedTurn(t, cfg, tool, resultTokens*7/2)
	turn.provider.toolRounds = rounds

	resp, err := turn.al.ProcessDirectWithChannel(
		context.Background(),
		"search a lot",
		"sk_v1_accumulated",
		"test",
		"accumulated",
	)
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if resp != "done" {
		t.Fatalf("response = %q, want done", resp)
	}

	calls := turn.provider.snapshot()
	last := calls[len(calls)-1]
	if last.rejected {
		t.Fatal("the final call was rejected")
	}
	if !strings.Contains(last.content, elidedToolResultMarker) {
		t.Fatal("final call carries no elision note")
	}
	if !strings.Contains(last.content, bigOutput(resultSize)) {
		t.Fatal("final call lost the newest result: elision must start with the oldest")
	}
	rejected := 0
	for _, c := range calls {
		if c.rejected {
			rejected++
		}
	}
	// Each overflow is answered once; the elisions then ride along on every
	// later call of the turn instead of overflowing again.
	if rejected == 0 || rejected >= rounds {
		t.Fatalf("rejected calls = %d, want between 1 and %d", rejected, rounds-1)
	}

	// The elision is a view of the call: what the session keeps is intact.
	results := toolMessages(turn.agent.Sessions.GetHistory("sk_v1_accumulated"))
	if len(results) != rounds {
		t.Fatalf("tool results in history = %d, want %d", len(results), rounds)
	}
	for i, m := range results {
		if m.Content != bigOutput(resultSize) {
			t.Fatalf("history result %d was altered (%d characters)", i, len(m.Content))
		}
	}
}

// The retry guard measured the active turn on the history read at the start
// of the turn, which lacks this turn's tool results. Seeing a turn of just the
// user message, it let the destructive compaction run when that could not
// help: in prod it dropped earlier messages of the conversation each time
// ("Emergency compression dropped 12 oldest messages"), and in a conversation
// with nothing before the turn it dropped the turn's own tool call and result,
// so the model ran the tool again until max_tool_iterations.
func TestContextRetry_DoesNotCompactWhenOnlyTheActiveTurnOverflows(t *testing.T) {
	for _, earlierTurn := range []bool{true, false} {
		name := map[bool]string{true: "earlier history", false: "active turn only"}[earlierTurn]
		t.Run(name, func(t *testing.T) {
			cfg := windowedConfig(t)
			// Under the per-result limit and still past the window, like a
			// capped result on the smaller window of the image model.
			cfg.Tools.MaxResultChars = 10_000_000
			tool := &bigOutputTool{size: 200_000}
			turn := newWindowedTurn(t, cfg, tool, 20_000)
			const session = "sk_v1_guard"
			if earlierTurn {
				if _, err := turn.al.ProcessDirectWithChannel(
					context.Background(),
					"earlier question",
					session,
					"test",
					"guard",
				); err != nil {
					t.Fatalf("earlier turn: %v", err)
				}
			}
			turn.provider.toolRounds = 1

			resp, err := turn.al.ProcessDirectWithChannel(
				context.Background(),
				"take a snapshot",
				session,
				"test",
				"guard",
			)
			if err != nil {
				t.Fatalf("turn failed: %v", err)
			}
			if resp != "done" {
				t.Fatalf("response = %q, want done", resp)
			}
			if n := tool.calls.Load(); n != 1 {
				t.Fatalf("tool ran %d times, want 1: the turn lost its own result and asked again", n)
			}
			history := turn.agent.Sessions.GetHistory(session)
			if earlierTurn && (len(history) == 0 || history[0].Content != "earlier question") {
				t.Fatalf("earlier history was dropped; history starts with %+v", history[:min(1, len(history))])
			}
			if results := toolMessages(history); len(results) != 1 || results[0].Content != bigOutput(200_000) {
				t.Fatalf("the turn's tool result is missing or altered in history (%d results)", len(results))
			}
		})
	}
}

func TestTruncateToolResult_KeepsStartAndEndWithinTheLimit(t *testing.T) {
	// Multi-byte runes: the cut must count and split runes, never bytes.
	content := "início " + strings.Repeat("ação ", 10_000) + " fim"
	const limit = 2_000
	got := truncateToolResult(content, limit, "/ws/artifacts/tool-output/exec_1.txt")

	if n := len([]rune(got)); n > limit {
		t.Fatalf("truncated to %d runes, want at most %d", n, limit)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune")
	}
	total := len([]rune(content))
	for _, want := range []string{
		fmt.Sprintf("%d characters", total),
		"/ws/artifacts/tool-output/exec_1.txt",
		"início ação",
		"ação  fim",
		"characters omitted",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("truncated result lacks %q:\n%s", want, got)
		}
	}
}

func TestTruncateToolResult_SaysWhenTheOutputWasNotSaved(t *testing.T) {
	got := truncateToolResult(strings.Repeat("line of output\n", 1_000), 500, "")
	if !strings.Contains(got, "could not be saved") || strings.Contains(got, "saved at") {
		t.Fatalf("notice without a saved file is wrong:\n%s", got)
	}
}

func TestBoundToolResultForLLM_LeavesResultsWithinTheLimitAlone(t *testing.T) {
	al := &AgentLoop{cfg: &config.Config{}}
	ts := &turnState{workspace: t.TempDir()}
	content := strings.Repeat("ç", config.DefaultToolMaxResultChars) // more bytes than the limit, as many runes
	if got := al.boundToolResultForLLM(ts, "exec", content); got != content {
		t.Fatal("a result at the limit was changed")
	}
	if entries, _ := os.ReadDir(filepath.Join(ts.workspace, "artifacts", toolOutputDir)); len(entries) != 0 {
		t.Fatalf("saved %d files for a result within the limit", len(entries))
	}
}

func TestSaveToolOutput_NeedsAWorkspace(t *testing.T) {
	if _, err := saveToolOutput("", "exec", "output"); err == nil {
		t.Fatal("saved without a workspace: the file would land in the process's working directory")
	}
}

func TestWithElidedToolResults_ReplacesOnlyTheNamedResultsInACopy(t *testing.T) {
	messages := []providers.Message{
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", ToolCallID: "a", Content: "first result"},
		{Role: "tool", ToolCallID: "b", Content: "second result"},
	}
	got := withElidedToolResults(messages, map[string]int{"a": 12})

	if !strings.HasPrefix(got[2].Content, elidedToolResultMarker) ||
		!strings.Contains(got[2].Content, "12 characters") {
		t.Fatalf("result a = %q, want its elision note", got[2].Content)
	}
	if got[2].ToolCallID != "a" {
		t.Fatal("the note lost its tool call id; the provider would reject the orphan call")
	}
	if got[3].Content != "second result" {
		t.Fatalf("result b = %q, want it untouched", got[3].Content)
	}
	if messages[2].Content != "first result" {
		t.Fatal("the turn's own messages were modified")
	}
	if same := withElidedToolResults(messages, nil); &same[0] != &messages[0] {
		t.Fatal("without elisions the messages should pass through without a copy")
	}
}

func TestElideTurnToolResultsToFit(t *testing.T) {
	result := func(id string, size int) providers.Message {
		return providers.Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("r", size)}
	}
	turn := []providers.Message{
		{Role: "user", Content: strings.Repeat("u", 5_000)},
		result("small", 500),
		result("old", 10_000),
		result("mid", 10_000),
		result("new", 10_000),
	}
	// The window is whatever the turn costs minus `excess`; every elision
	// gives back what the result costs over its note.
	measureWithWindow := func(window int) func(map[string]int) ([]providers.Message, int) {
		return func(elided map[string]int) ([]providers.Message, int) {
			msgs := withElidedToolResults(turn, elided)
			return msgs, contextBudgetOverflow(window, msgs, nil, 0)
		}
	}
	full := contextBudgetOverflow(0, turn, nil, 0)
	resultTokens := EstimateMessageTokens(turn[2])

	t.Run("oldest first, only as many as needed", func(t *testing.T) {
		elided, msgs, fits := elideTurnToolResultsToFit(turn, nil, 0, measureWithWindow(full-resultTokens/2))
		if !fits {
			t.Fatal("did not fit")
		}
		if len(elided) != 1 || elided["old"] != 10_000 {
			t.Fatalf("elided = %v, want only the oldest large result", elided)
		}
		if msgs[4].Content != turn[4].Content {
			t.Fatal("the newest result was elided")
		}
	})

	t.Run("headroom elides past the bare fit", func(t *testing.T) {
		elided, _, fits := elideTurnToolResultsToFit(turn, nil, resultTokens, measureWithWindow(full-resultTokens/2))
		if !fits || len(elided) != 2 {
			t.Fatalf("elided = %v (fits %v), want old and mid", elided, fits)
		}
	})

	t.Run("headroom out of reach still takes the fit", func(t *testing.T) {
		elided, _, fits := elideTurnToolResultsToFit(turn, nil, full, measureWithWindow(full-resultTokens/2))
		if !fits || len(elided) != 3 {
			t.Fatalf("elided = %v (fits %v), want every large result and a fitting call", elided, fits)
		}
	})

	t.Run("keeps what an earlier retry elided", func(t *testing.T) {
		before := map[string]int{"old": 10_000}
		elided, _, fits := elideTurnToolResultsToFit(turn, before, 0, measureWithWindow(full-resultTokens*3/2))
		if !fits || len(elided) != 2 || elided["mid"] == 0 {
			t.Fatalf("elided = %v (fits %v), want old kept and mid added", elided, fits)
		}
		if len(before) != 1 {
			t.Fatal("the caller's set was modified")
		}
	})

	t.Run("gives up when even every result elided does not fit", func(t *testing.T) {
		before := map[string]int{}
		elided, _, fits := elideTurnToolResultsToFit(turn, before, 0, measureWithWindow(100))
		if fits {
			t.Fatal("reported a fit below the user message alone")
		}
		if len(elided) != 0 {
			t.Fatalf("elided = %v, want the caller's set back unchanged", elided)
		}
	})
}
