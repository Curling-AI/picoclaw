package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// promiseQuestion asks the decision model whether the reply that ends the turn
// left the person waiting on work it only announced. It judges the final
// sentence on its own, because a reply that delivers something first can still
// end on a step it never took, and it names each common way a reply promises
// or legitimately ends. The examples are in English except the scheduling pair
// and the closing-line one: in English, the Portuguese forms of both passed.
// (seucaranguejo fork)
const promiseQuestion = "Judge the final sentence of the assistant's last message, even when the rest of the message already " +
	"delivers something; when that sentence only says what the person does meanwhile or what is left, judge it " +
	"together with the sentence before it. The turn has ended: the assistant does nothing more until the person writes " +
	"again, unless the message says it set up a reminder, an automation or a scheduled task in this turn. " +
	"Answer yes when that final sentence announces or implies work the assistant has not done, leaving " +
	"the person waiting on it: (1) an action it was about to take right then, such as searching, opening, " +
	"reading, running, creating, writing, fixing, redoing, correcting, preparing, sending or publishing " +
	"something (\"Now I'll publish the page.\", \"Let me check the reminder.\", \"Let me open the article.\", " +
	"\"Let me take a look.\"), also after a partial delivery (\"The sales chart is updated. Now I'll redo " +
	"the margin chart.\", \"I'll classify this expense as rent.\"); (2) a confirmation followed by a promise " +
	"or by taking the task on (\"Perfect! I'll do it now.\", \"Great, I'll book it right now.\", \"Leave it " +
	"with me.\"); (3) a request to wait, or a status line with nothing after it (\"One moment, I'm " +
	"generating the PDF.\", \"Please wait while I check.\", \"Analyzing the data…\", \"Searching for the " +
	"information...\"); (4) coming back later with nothing set up to bring it back (\"I'll let you know " +
	"when it's ready.\", \"I'll be right back with the result.\", \"I'll send it in a few minutes.\", \"I'll " +
	"get back to you with the comparison.\"); (5) long work announced as continuing (\"I'll keep going and " +
	"report my progress.\"), an action in parallel (\"While you restart it, I'll prepare a test.\"), or a " +
	"self-correction (\"I need to fix that in my notes, fixing it now.\"); (6) a plan of steps it says it " +
	"will follow, not yet run (\"I'll follow these steps: 1. read the statement 2. match it with the ERP\", " +
	"\"Here's how I'll do it: 1. export the contacts 2. import them\"); (7) a reminder, automation or " +
	"scheduled task announced but not set up yet (\"I'll schedule the reminder for tomorrow.\", \"Vou " +
	"agendar o envio para segunda.\"); (8) something introduced with a colon and nothing after it (a " +
	"report, a list, the next step); (9) a closing line that only says what the person does meanwhile or " +
	"what is left, right after the assistant announced its own next step (\"Começo pela integração e te " +
	"aviso quando ficar pronta. Você confere os produtos em paralelo.\", \"My next step, without " +
	"waiting for your answers: the pricing table. Your part: the answers above.\", \"I'll re-read those " +
	"twelve by hand. After April, May and June are left.\"); (10) a tool call written out as text instead " +
	"of being made: JSON arguments or call markup standing where the action the person asked for should " +
	"have happened. Answer no when the final sentence: reports what was done or delivers " +
	"the answer, a file or a link, even if it then offers more (\"if you want, I can turn it into " +
	"minutes\") or gives tips; says a reminder, automation or scheduled task was set up in this turn, or " +
	"when the assistant itself will act at a set time (\"Agendei: amanhã às 9h te lembro.\", \"I'll remind " +
	"you tomorrow at 9.\"); asks the person a question, for a choice, for confirmation or for approval, " +
	"including a plan that ends asking to start (\"Shall I start?\"); waits on the person or someone else, " +
	"such as connecting an app, choosing an option, sending a file, filling in a form or tapping a button " +
	"or connection card shown with the message, also when it says what it will do once that arrives (\"As " +
	"soon as you send the contract, I'll review it.\", \"When the finance assistant replies, I'll update " +
	"the sheet.\"), or names the next item of a series the person sends one at a time (\"Next: chapter " +
	"4.\"); gives the person instructions to follow, or delivers a " +
	"plan the person asked for; explains that the assistant cannot do something; promises a way of " +
	"behaving (\"I won't store your ID\"); or is a courtesy closing (\"Let me know if you need anything " +
	"else.\"). Words like \"I'll check\" inside a text the assistant wrote for the person to use (a draft, a " +
	"script, a caption, a quoted phrase) are not the assistant announcing anything."

const promiseQuestionName = "stopped_on_promise"

// judgeLetThroughLine logs each reply the judge did not call a promise.
const judgeLetThroughLine = "Promise judge let the reply through"

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
	// maxPromiseRetriesPerTurn caps them over the whole turn: a model that
	// answers every retry with one call and the same promise would otherwise
	// loop up to the iteration cap.
	maxPromiseRetriesPerTurn = 3
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
		"\n\nThe assistant's last message:\n" + clipMiddle(reply, promiseReplyClip) +
		"\n\nIts final sentence: " + finalSentence(reply)
}

// finalSentenceClip bounds the final sentence the judge reads apart.
const finalSentenceClip = 300

// finalSentence is the reply's last sentence, given to the judge on its own: a
// reply that delivers something first can still end on a step it never took.
func finalSentence(reply string) string {
	lines := strings.Split(strings.TrimSpace(reply), "\n")
	line := ""
	for i := len(lines) - 1; i >= 0 && line == ""; i-- {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(lines[i]), "-*#> "))
	}
	runes := []rune(line)
	var sentences []string
	start := 0
	for i := 0; i+1 < len(runes); i++ {
		if strings.ContainsRune(".!?…", runes[i]) && unicode.IsSpace(runes[i+1]) {
			sentences = append(sentences, strings.TrimSpace(string(runes[start:i+1])))
			start = i + 1
		}
	}
	sentences = append(sentences, strings.TrimSpace(string(runes[start:])))
	last := sentences[len(sentences)-1]
	// A trailing emoji or "ok." is not the sentence that says what comes next.
	if len([]rune(last)) < 4 && len(sentences) > 1 {
		last = sentences[len(sentences)-2] + " " + last
	}
	if tail := []rune(last); len(tail) > finalSentenceClip {
		last = string(tail[len(tail)-finalSentenceClip:])
	}
	return last
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
