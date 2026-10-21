package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const (
	reportText  = "Relatório semanal: 12 PRs mesclados."
	reportReply = "Quer que eu agende para toda segunda?"
	scriptLabel = "[Sent to this chat by a script this conversation started at "
)

// reportProvider answers each call with the next step, "tool:<name>" or
// "say:<text>", then with the question about the report. Sub-turns call it
// too, in the order they run.
type reportProvider struct {
	mu    sync.Mutex
	steps []string
	calls int
	// inputs are the messages each call got, in order.
	inputs [][]providers.Message
}

func (p *reportProvider) GetDefaultModel() string { return "test-model" }

func (p *reportProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.inputs = append(p.inputs, append([]providers.Message(nil), messages...))
	if p.calls > len(p.steps) {
		return &providers.LLMResponse{Content: reportReply}, nil
	}
	step := p.steps[p.calls-1]
	if name, ok := strings.CutPrefix(step, "tool:"); ok {
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{
			{ID: name, Name: name, Arguments: map[string]any{}},
		}}, nil
	}
	return &providers.LLMResponse{Content: strings.TrimPrefix(step, "say:")}, nil
}

// reportScript stands for exec: as run_report it runs report.py, which posts
// the report to the chat (the pod's tap hands it to MirrorDelivery) and prints
// only "ok"; as stop_now, the user's /stop arriving during a tool; as delegate,
// a subagent that runs report.py; anything else, a plain command.
type reportScript struct {
	name string
	loop *AgentLoop
	chat *string
}

func (s *reportScript) Name() string        { return s.name }
func (s *reportScript) Description() string { return "Test tool " + s.name }
func (s *reportScript) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (s *reportScript) Execute(ctx context.Context, _ map[string]any) *tools.ToolResult {
	origin, turnID := tools.ExecOrigin(ctx)
	switch s.name {
	case "run_report":
		*s.chat = origin
		s.loop.MirrorDelivery(tools.ExternalDelivery{
			Origin: origin, TurnID: turnID, Channel: "telegram", ChatID: "123", Text: reportText,
		})
		return tools.NewToolResult("ok")
	case "stop_now":
		_ = s.loop.HardAbort(origin)
		return tools.NewToolResult("stopping")
	case "delegate":
		result, err := SpawnSubTurn(ctx, SubTurnConfig{
			Model:   "test-model",
			Tools:   []tools.Tool{&reportScript{name: "run_report", loop: s.loop, chat: s.chat}},
			Timeout: time.Minute,
		})
		if err != nil {
			return tools.ErrorResult(err.Error())
		}
		return result
	}
	return tools.NewToolResult("README.md")
}

// runReportTurn runs one chat turn whose model takes steps, and returns the
// chat's history afterwards.
func runReportTurn(t *testing.T, steps ...string) []providers.Message {
	t.Helper()
	return runReportTurnOn(t, &reportProvider{steps: steps})
}

func runReportTurnOn(t *testing.T, provider *reportProvider) []providers.Message {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	t.Cleanup(al.Close)
	sessions := session.NewSessionManager(filepath.Join(cfg.Agents.Defaults.Workspace, "sessions"))
	al.registry.GetDefaultAgent().Sessions = sessions
	var chat string
	for _, name := range []string{"run_report", "stop_now", "delegate", "list_files"} {
		al.RegisterTool(&reportScript{name: name, loop: al, chat: &chat})
	}
	al.SetDeliverySessionResolver(func(string, string) string { return chat })

	_, _ = al.ProcessDirect(context.Background(), "me manda o relatório", "report-chat")
	if chat == "" {
		t.Fatal("the script did not run")
	}
	return sessions.GetHistory(chat)
}

func indexOfContent(history []providers.Message, content string) int {
	for i, msg := range history {
		if msg.Content == content {
			return i
		}
	}
	return -1
}

// The user saw the report, then the question about it, and answers "sim": the
// conversation must hold them in that order, the report right after the result
// of the script that sent it, and the model must read them as two things.
func TestSelfDelivery_ScriptSendLandsBeforeTheFinalReply(t *testing.T) {
	history := runReportTurn(t, "tool:run_report")

	report, reply := indexOfContent(history, reportText), indexOfContent(history, reportReply)
	if report < 0 || reply < 0 || report > reply {
		t.Fatalf("report at %d, reply at %d in %d messages; want the report before the reply",
			report, reply, len(history))
	}
	if prev := history[report-1]; prev.Role != "tool" || prev.Content != "ok" {
		t.Fatalf("message before the report = %s %q, want the script's result", prev.Role, prev.Content)
	}
	if d := history[report].Delivery; d == nil || !d.Self {
		t.Fatalf("report delivery = %+v, want it marked as the chat's own script", d)
	}
	if indexOfContent(history[report+1:], reportText) >= 0 {
		t.Fatal("the report was written twice")
	}
	sent := sanitizeHistoryForProvider(history)
	if last := sent[len(sent)-1]; last.Role != "assistant" || last.Content != reportReply {
		t.Fatalf("model's last message = %s %q, want the reply on its own", last.Role, last.Content)
	}
}

// The report is written between two tool rounds. The model must still get both
// rounds whole, the report with the result of the script that sent it, and the
// reply on its own.
func TestSelfDelivery_ModelSeesBothRoundsAndTheReplyApart(t *testing.T) {
	sent := sanitizeHistoryForProvider(runReportTurn(t, "tool:run_report", "tool:list_files"))

	var calls []string
	var results []providers.Message
	for _, msg := range sent {
		for _, call := range msg.ToolCalls {
			calls = append(calls, call.Name)
		}
		if msg.Role == "tool" {
			results = append(results, msg)
		}
	}
	if strings.Join(calls, ",") != "run_report,list_files" || len(results) != 2 {
		t.Fatalf("model got calls %v and %d results, want both rounds", calls, len(results))
	}
	if got := results[0].Content; !strings.HasPrefix(got, "ok\n\n"+scriptLabel) || !strings.Contains(got, reportText) {
		t.Fatalf("report script result = %q, want its output and then the dated report", results[0].Content)
	}
	if results[1].Content != "README.md" {
		t.Fatalf("second round's result = %q", results[1].Content)
	}
	if last := sent[len(sent)-1]; last.Role != "assistant" || last.Content != reportReply {
		t.Fatalf("model's last message = %s %q, want the reply on its own", last.Role, last.Content)
	}
}

// The report is written after its round, then a /stop in the next round cuts
// the turn out of the history, report included. It was still delivered, so it
// is written again once the turn is gone.
func TestSelfDelivery_AbortedTurnKeepsTheSend(t *testing.T) {
	history := runReportTurn(t, "tool:run_report", "tool:stop_now")

	report := indexOfContent(history, reportText)
	if report < 0 {
		t.Fatalf("the report is missing after the abort: %d messages", len(history))
	}
	if indexOfContent(history, reportReply) >= 0 {
		t.Fatal("the aborted turn's reply is in the history")
	}
	if indexOfContent(history[report+1:], reportText) >= 0 {
		t.Fatal("the report was written twice")
	}
}

// A subagent the chat's turn delegated to runs report.py: the send is the
// chat's own, written with the round that ran the subagent and before the
// chat's reply, not as another conversation's after it.
func TestSelfDelivery_SubagentScriptSendIsTheChats(t *testing.T) {
	history := runReportTurn(t, "tool:delegate", "tool:run_report", "say:subagent done")

	report, reply := indexOfContent(history, reportText), indexOfContent(history, reportReply)
	if report < 0 || reply < 0 || report > reply {
		t.Fatalf("report at %d, reply at %d in %d messages; want the report before the reply",
			report, reply, len(history))
	}
	if d := history[report].Delivery; d == nil || !d.Self {
		t.Fatalf("report delivery = %+v, want it marked as the chat's own script", d)
	}
	if prev := history[report-1]; prev.Role != "tool" {
		t.Fatalf("message before the report = %s %q, want the subagent's result", prev.Role, prev.Content)
	}
}

// The script printed only "ok": the model's next call in the same turn, the one
// that writes the reply, must already have the report it sent, with the result
// of the script that sent it.
func TestSelfDelivery_NextCallSeesTheSend(t *testing.T) {
	provider := &reportProvider{steps: []string{"tool:run_report"}}
	runReportTurnOn(t, provider)

	if len(provider.inputs) != 2 {
		t.Fatalf("model was called %d times, want the script round and the reply", len(provider.inputs))
	}
	var result *providers.Message
	for i, msg := range provider.inputs[1] {
		if msg.Role == "tool" {
			result = &provider.inputs[1][i]
		}
	}
	if result == nil {
		t.Fatal("the reply's call has no tool result")
	}
	if !strings.HasPrefix(result.Content, "ok\n\n"+scriptLabel) || !strings.Contains(result.Content, reportText) {
		t.Fatalf("script result in the reply's call = %q, want its output and then the dated report", result.Content)
	}
}

func TestFoldSelfDeliveries(t *testing.T) {
	self := func(text string) providers.Message {
		return providers.Message{Role: "assistant", Content: text, Delivery: &providers.Delivery{Self: true}}
	}
	tool := providers.Message{Role: "tool", ToolCallID: "c1", Content: "ok"}
	call := providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "c1"}}}

	plain := []providers.Message{{Role: "user", Content: "oi"}, call, tool}
	if got := foldSelfDeliveries(plain); &got[0] != &plain[0] {
		t.Fatal("without sends to join, the slice should come back as is")
	}

	stored := []providers.Message{
		{Role: "user", Content: "oi"}, call, tool, self("relatório"), self("anexo"), call, tool,
	}
	got := foldSelfDeliveries(stored)
	if len(got) != 5 || got[2].Role != "tool" || got[3].ToolCalls == nil {
		t.Fatalf("folded = %+v, want both sends joined to the first result", got)
	}
	if !strings.Contains(got[2].Content, "relatório") || !strings.Contains(got[2].Content, "anexo") {
		t.Fatalf("first result = %q, want both sends in it", got[2].Content)
	}
	if stored[2].Content != "ok" || len(stored) != 7 {
		t.Fatal("the stored messages were changed")
	}

	afterUser := []providers.Message{{Role: "user", Content: "oi"}, self("relatório")}
	if got := foldSelfDeliveries(afterUser); len(got) != 2 {
		t.Fatalf("a send with no tool result before it stays a message: %+v", got)
	}
}

// An elision drops the tool's output, not the report its script sent: the user
// saw that one.
func TestWithElidedToolResults_KeepsJoinedSelfSends(t *testing.T) {
	sent := providers.Message{Role: "assistant", Content: reportText, Delivery: &providers.Delivery{Self: true}}
	messages := joinToLastToolResult([]providers.Message{
		{Role: "user", Content: "oi"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "c1"}}},
		{Role: "tool", ToolCallID: "c1", Content: bigOutput(5_000, 1)},
	}, sent)

	got := withElidedToolResults(messages, 1, turnElisions{1: {toolCallID: "c1", chars: 5_000}})[2].Content

	if strings.Contains(got, "HEAD-OF-OUTPUT-001") || !strings.Contains(got, elidedToolResultMarker) {
		t.Fatalf("elided result = %.200q, want the note in place of the output", got)
	}
	if !strings.Contains(got, "\n\n"+selfDeliveryLine) || !strings.HasSuffix(got, reportText) {
		t.Fatalf("elided result = %.400q, want the dated report kept after the note", got)
	}
}
