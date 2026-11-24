package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// promiseQuestion asks the decision model whether a reply that ends the turn
// left the person waiting on a step it only announced. It replaced the verb
// lists and exceptions of the regex guards: probed against every labeled reply
// in their tables (prod, eval and review cases, in Portuguese and English),
// none that delivers, asks or waits came back yes. (seucaranguejo fork)
const promiseQuestion = "Does the assistant's last message end by announcing an action it was about to take right then, such " +
	"as searching, opening, reading, running, creating, writing, fixing, sending, publishing or " +
	"scheduling something, without having taken it, leaving it undone and without asking the person " +
	"anything? Answer yes, for example, for \"I'll redo the chart with a clearer idea.\", \"Now I'll publish " +
	"the page.\", \"Let me check the reminder.\", \"Vou refazer o gráfico.\", \"Deixa eu abrir o artigo.\". " +
	"Creating a reminder, an automation or a scheduled task is an action to take right then even when " +
	"what it sets up is for later: \"I'll schedule the reminder for tomorrow\" is yes. A message that ends " +
	"by introducing something (a report, a list, a batch, the next step) with a colon and nothing after " +
	"it is also yes: what it introduced never came. Answer no when the message delivers what the person " +
	"asked for (an answer, a text, a list, a file, a confirmation of what was done), even if it ends " +
	"offering more (\"if you want, I can adjust the tone\") or giving tips; when it asks the person a " +
	"question or for confirmation; when it says it is waiting for someone else, an approval or something " +
	"still to be sent; when it says the assistant itself will do something at a later time (\"I'll remind " +
	"you tomorrow at 9\"); or when it promises a way of behaving (\"I won't store your ID\", \"I'll keep that " +
	"in mind\"). Words like \"I'll check\" inside a text the assistant wrote for the person to use (a draft, " +
	"a script, a caption, a quoted phrase) are not the assistant announcing anything."

const promiseQuestionName = "stopped_on_promise"

const (
	// promiseYes and promiseNo are the judge's thresholds; in between is doubt,
	// and a doubtful reply goes out as it is.
	promiseYes = 0.8
	promiseNo  = 0.2
	// promiseJudgeTimeout bounds the call at the end of the turn.
	promiseJudgeTimeout = 3 * time.Second
	// promiseAskedClip and promiseReplyClip bound what the judge reads, keeping
	// the end of each, where a promise is.
	promiseAskedClip = 1000
	promiseReplyClip = 1500
	// maxPromiseRetries caps retries IN A ROW after a reply judged a promise; a
	// real tool call resets it, so a long run that stops this way again later
	// gets its own retry.
	maxPromiseRetries = 1
)

// promiseVerdict is the judge's answer about one reply. Without a decision
// model, a provider that can decide, or an answer, nothing was judged and the
// reply goes out as it is.
type promiseVerdict struct {
	promised    bool
	probability float64
	answered    bool
	model       string
	elapsed     time.Duration
	err         error
}

// judgePromise asks the decision model whether reply ends on a step it only
// announced, before or after the turn's tools ran.
func (p *Pipeline) judgePromise(ctx context.Context, ts *turnState, exec *turnExecution, reply string) promiseVerdict {
	model := strings.TrimSpace(p.Cfg.Agents.Defaults.DecisionModel)
	if model == "" || strings.TrimSpace(reply) == "" {
		return promiseVerdict{}
	}
	decider, ok := ts.agent.Provider.(providers.Decider)
	if !ok {
		p.warnUndecided(ts.agent)
		return promiseVerdict{}
	}
	callCtx, cancel := context.WithTimeout(ctx, promiseJudgeTimeout)
	defer cancel()
	start := time.Now()
	answers, err := decider.Decide(callCtx, model, promiseInput(ts.userMessage, exec.toolRanThisTurn, reply),
		[]providers.DecisionQuestion{{Name: promiseQuestionName, Instructions: promiseQuestion}})
	verdict := promiseVerdict{model: model, elapsed: time.Since(start), err: err}
	if err != nil {
		return verdict
	}
	verdict.probability, verdict.answered = answers[promiseQuestionName]
	verdict.promised = verdict.answered && verdict.probability >= promiseYes
	return verdict
}

// warnUndecided says, once per agent, that a decision model is configured but
// the agent's provider cannot ask it: the guard is off for that agent, and
// without the line nothing showed it.
func (p *Pipeline) warnUndecided(agent *AgentInstance) {
	if _, warned := p.al.undecidedAgents.LoadOrStore(agent.ID, struct{}{}); warned {
		return
	}
	logger.WarnCF("agent", "Decision model configured but the provider cannot answer it; promise guard off",
		map[string]any{"agent_id": agent.ID, "provider": fmt.Sprintf("%T", agent.Provider)})
}

// fields are the verdict for a log line; the reply's text never goes there.
func (v promiseVerdict) fields(into map[string]any) map[string]any {
	into["judge_model"] = v.model
	into["judge_ms"] = v.elapsed.Milliseconds()
	if v.answered {
		into["judge_p"] = v.probability
	}
	if v.err != nil {
		into["judge_error"] = v.err.Error()
	}
	return into
}

// promiseInput is what the judge reads: the person's message, whether a tool
// ran, and the reply that ends the turn.
func promiseInput(asked string, toolRan bool, reply string) string {
	tools := "none"
	if toolRan {
		tools = "yes, some tools ran"
	}
	return "The assistant's turn ends after its last message below: it stopped and waited for the person to " +
		"write again.\n\nThe person wrote: " + clipMiddle(asked, promiseAskedClip) +
		"\nTools the assistant used in this turn: " + tools +
		"\n\nThe assistant's last message:\n" + clipMiddle(reply, promiseReplyClip)
}

// clipMiddle keeps text within limit runes by cutting its middle: the end of a
// reply is where it promises, and its start says what it is about.
func clipMiddle(text string, limit int) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= limit {
		return string(runes)
	}
	head := limit / 4
	return string(runes[:head]) + "\n[…]\n" + string(runes[len(runes)-(limit-head):])
}

// promiseNudgeTail is how much of the discarded reply the nudge quotes.
const promiseNudgeTail = 220

// promiseNudge quotes the end of the reply it discarded: the model never sees
// that reply again, and the quote is how it knows which step it was on.
func promiseNudge(reply string) string {
	tail := []rune(strings.TrimSpace(reply))
	if len(tail) > promiseNudgeTail {
		tail = tail[len(tail)-promiseNudgeTail:]
	}
	return "[System] Your last reply ended by announcing a step it did not take: «" + string(tail) + "». " +
		"It carried no tool call, so that step never ran and the user is still waiting. " +
		"Take the step now by emitting its tool call. If a tool in this turn already did it, do not run it " +
		"again: write the final answer, saying what was done. If the user asked you only to describe or plan " +
		"the step, not to do it, write that answer again in full."
}
