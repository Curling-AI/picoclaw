package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/session"
	"github.com/sipeed/picoclaw/pkg/tools"
)

const (
	reportText  = "Relatório semanal: 12 PRs mesclados."
	reportReply = "Quer que eu agende para toda segunda?"
)

// reportProvider calls each tool of rounds in turn, one per round, then asks
// the user about the report.
type reportProvider struct {
	rounds []string
	calls  int
}

func (p *reportProvider) GetDefaultModel() string { return "test-model" }

func (p *reportProvider) Chat(
	context.Context,
	[]providers.Message,
	[]providers.ToolDefinition,
	string,
	map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	if p.calls <= len(p.rounds) {
		name := p.rounds[p.calls-1]
		return &providers.LLMResponse{ToolCalls: []providers.ToolCall{
			{ID: name, Name: name, Arguments: map[string]any{}},
		}}, nil
	}
	return &providers.LLMResponse{Content: reportReply}, nil
}

// reportScript stands for exec running report.py: the script posts the report
// to the chat, where the pod's tap hands it to MirrorDelivery, and prints only
// "ok". As "stop_now" it stands for the user's /stop arriving during a tool.
type reportScript struct {
	name    string
	loop    *AgentLoop
	session *string
}

func (s *reportScript) Name() string        { return s.name }
func (s *reportScript) Description() string { return "Test tool " + s.name }
func (s *reportScript) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (s *reportScript) Execute(ctx context.Context, _ map[string]any) *tools.ToolResult {
	*s.session = tools.ToolSessionKey(ctx)
	if s.name == "stop_now" {
		_ = s.loop.HardAbort(*s.session)
		return tools.NewToolResult("stopping")
	}
	s.loop.MirrorDelivery(tools.ExternalDelivery{
		Origin: *s.session, TurnID: tools.ToolTurnID(ctx), Channel: "telegram", ChatID: "123", Text: reportText,
	})
	return tools.NewToolResult("ok")
}

// runReportTurn runs one chat turn whose model calls the tools of rounds, and
// returns the chat's history afterwards.
func runReportTurn(t *testing.T, rounds ...string) []providers.Message {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.ModelName = "test-model"
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &reportProvider{rounds: rounds})
	t.Cleanup(al.Close)
	sessions := session.NewSessionManager(filepath.Join(cfg.Agents.Defaults.Workspace, "sessions"))
	al.registry.GetDefaultAgent().Sessions = sessions
	var chat string
	al.RegisterTool(&reportScript{name: "run_report", loop: al, session: &chat})
	al.RegisterTool(&reportScript{name: "stop_now", loop: al, session: &chat})
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
// of the script that sent it.
func TestSelfDelivery_ScriptSendLandsBeforeTheFinalReply(t *testing.T) {
	history := runReportTurn(t, "run_report")

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
}

// The report is written after its round, then a /stop in the next round cuts
// the turn out of the history, report included. It was still delivered, so it
// is written again once the turn is gone.
func TestSelfDelivery_AbortedTurnKeepsTheSend(t *testing.T) {
	history := runReportTurn(t, "run_report", "stop_now")

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
