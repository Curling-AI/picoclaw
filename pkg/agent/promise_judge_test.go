package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// judgeAnswer is what the decision model answers to one ask: a probability of
// yes, a refusal (no probability) or an error.
type judgeAnswer struct {
	p       float64
	refused bool
	err     error
}

var (
	judgedYes = judgeAnswer{p: 0.95}
	judgedNo  = judgeAnswer{p: 0.05}
)

// judgeCall is one ask the decision model received.
type judgeCall struct {
	model, input string
	questions    []providers.DecisionQuestion
}

// judgedProvider is the gateway of a deployment with a decision model: chat
// calls follow the script, and the Decisions API answers in order.
type judgedProvider struct {
	*scriptedProvider
	mu      sync.Mutex
	answers []judgeAnswer
	asked   []judgeCall
}

func (p *judgedProvider) Decide(
	_ context.Context,
	model, input string,
	questions []providers.DecisionQuestion,
) (map[string]float64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, judgeCall{model: model, input: input, questions: questions})
	answer := judgedNo
	if n := len(p.asked); n <= len(p.answers) {
		answer = p.answers[n-1]
	}
	switch {
	case answer.err != nil:
		return nil, answer.err
	case answer.refused:
		return map[string]float64{}, nil
	}
	return map[string]float64{questions[0].Name: answer.p}, nil
}

func (p *judgedProvider) judgeCalls() []judgeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]judgeCall(nil), p.asked...)
}

const testDecisionModel = "typesafe-ai/jev-test"

func newJudgedLoop(t *testing.T, answers []judgeAnswer, script ...providers.LLMResponse) (*AgentLoop, *judgedProvider) {
	t.Helper()
	return newJudgedLoopWithCap(t, 0, answers, script...)
}

// newJudgedLoopWithCap sets max_tool_iterations when maxIter > 0.
func newJudgedLoopWithCap(
	t *testing.T,
	maxIter int,
	answers []judgeAnswer,
	script ...providers.LLMResponse,
) (*AgentLoop, *judgedProvider) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.DecisionModel = testDecisionModel
	if maxIter > 0 {
		cfg.Agents.Defaults.MaxToolIterations = maxIter
	}
	provider := &judgedProvider{scriptedProvider: &scriptedProvider{script: script}, answers: answers}
	return NewAgentLoop(cfg, bus.NewMessageBus(), provider), provider
}

// The judge reads the person's message, whether a tool ran and the END of a
// long reply, where the promise is; the question names the decision model's
// predicate.
func TestPromiseJudgeReadsThePersonsMessageAndTheEndOfTheReply(t *testing.T) {
	long := strings.Repeat("Linha de contexto do relatório. ", 120) + "Agora vou publicar a versão nova no Skip."
	al, provider := newJudgedLoop(t, []judgeAnswer{judgedNo},
		readFileCall("call_1"),
		providers.LLMResponse{Content: long},
	)

	_, err := al.ProcessDirect(context.Background(), "publica a página do cardápio", "judge-input-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	calls := provider.judgeCalls()
	if len(calls) != 1 {
		t.Fatalf("judge asks = %d, want 1 (the final reply)", len(calls))
	}
	got := calls[0]
	if got.model != testDecisionModel {
		t.Errorf("judge model = %q, want the configured decision model", got.model)
	}
	if len(got.questions) != 1 || got.questions[0].Name != promiseQuestionName ||
		got.questions[0].Instructions != promiseQuestion {
		t.Errorf("questions = %+v, want the promise question", got.questions)
	}
	for _, want := range []string{
		"publica a página do cardápio", "yes, some tools ran", "Agora vou publicar a versão nova no Skip.", "[…]",
	} {
		if !strings.Contains(got.input, want) {
			t.Errorf("judge input lacks %q", want)
		}
	}
	if len([]rune(got.input)) > promiseAskedClip+promiseReplyClip+400 {
		t.Errorf("judge input has %d runes: the long reply was not cut", len([]rune(got.input)))
	}
}

// No decision model configured: the judge is never asked, and the reply goes
// out as it is.
func TestNoDecisionModelAsksNoJudge(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	provider := &judgedProvider{
		scriptedProvider: &scriptedProvider{script: []providers.LLMResponse{
			readFileCall("call_1"),
			{Content: promiseAfterTools},
		}},
		answers: []judgeAnswer{judgedYes},
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)

	resp, err := al.ProcessDirect(context.Background(), "o visual ficou fraco", "judge-off-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != promiseAfterTools {
		t.Errorf("response = %q, want the reply untouched", resp)
	}
	if n := len(provider.judgeCalls()); n != 0 {
		t.Errorf("judge asks = %d, want 0 without a decision model", n)
	}
	if got := provider.calls(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (no retry)", got)
	}
}

// A decision model configured over a provider that cannot ask it leaves the
// guard off: the reply goes out as it is, and the log says so once for the
// agent instead of on every turn, or never.
func TestAProviderThatCannotDecideIsLoggedOnce(t *testing.T) {
	logs := captureAgentLog(t)
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.Workspace = t.TempDir()
	cfg.Agents.Defaults.DecisionModel = testDecisionModel
	al := NewAgentLoop(cfg, bus.NewMessageBus(), &scriptedProvider{script: []providers.LLMResponse{
		readFileCall("call_1"),
		{Content: promiseAfterTools},
		readFileCall("call_2"),
		{Content: promiseAfterTools},
	}})

	for _, session := range []string{"undecided-1", "undecided-2"} {
		resp, err := al.ProcessDirect(context.Background(), "o visual ficou fraco", session)
		if err != nil {
			t.Fatalf("ProcessDirect: %v", err)
		}
		if resp != promiseAfterTools {
			t.Errorf("response = %q, want the reply untouched", resp)
		}
	}
	if n := strings.Count(logs(), "provider cannot answer it; promise guard off"); n != 1 {
		t.Errorf("guard-off warnings = %d, want 1 for the agent", n)
	}
}

// Whatever the judge does short of a clear yes — no, doubt, refusal, error —
// the reply goes out as it is, without a retry.
func TestReplyWithoutAClearYesIsDeliveredWithoutRetry(t *testing.T) {
	for name, answer := range map[string]judgeAnswer{
		"no":      judgedNo,
		"doubt":   {p: 0.5},
		"refused": {refused: true},
		"error":   {err: errors.New("gateway timeout")},
	} {
		t.Run(name, func(t *testing.T) {
			readLog := captureAgentLog(t)
			al, provider := newJudgedLoop(t, []judgeAnswer{answer},
				readFileCall("call_1"),
				providers.LLMResponse{Content: promiseAfterTools},
			)

			resp, err := al.ProcessDirect(context.Background(), "o visual ficou fraco", "judge-"+name+"-session")
			if err != nil {
				t.Fatalf("ProcessDirect: %v", err)
			}
			if resp != promiseAfterTools {
				t.Errorf("response = %q, want the reply untouched", resp)
			}
			if got := provider.calls(); got != 2 {
				t.Errorf("provider calls = %d, want 2 (no retry)", got)
			}
			log := readLog()
			if strings.Contains(log, deliveredPromiseLine) {
				t.Error("a reply the judge did not call a promise was logged as one")
			}
			if (answer.err != nil) != strings.Contains(log, "Promise judge unavailable") {
				t.Errorf("judge error logged = %v, want %v", !(answer.err != nil), answer.err != nil)
			}
		})
	}
}
