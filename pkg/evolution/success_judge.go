package evolution

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
	"github.com/sipeed/picoclaw/pkg/session"
)

type TaskSuccessDecision struct {
	Success bool
	Reason  string
	// P is the decision model's probability, nil when it gave none.
	P *float64
	// Source names what decided the verdict: one of the successSource values.
	Source string
}

const (
	successSourceDecision      = "decision"
	successSourceDoubtFallback = "doubt_fallback"
	successSourceErrorFallback = "error_fallback"
	successSourceLLM           = "llm"
	successSourceHeuristic     = "heuristic"
)

type SuccessJudge interface {
	JudgeTaskRecord(ctx context.Context, record LearningRecord) (TaskSuccessDecision, error)
}

type HeuristicSuccessJudge struct{}

func (j *HeuristicSuccessJudge) JudgeTaskRecord(
	_ context.Context,
	record LearningRecord,
) (TaskSuccessDecision, error) {
	if record.Success == nil || !*record.Success {
		return TaskSuccessDecision{Success: false, Reason: "task not completed", Source: successSourceHeuristic}, nil
	}
	if strings.TrimSpace(record.Summary) == "" {
		return TaskSuccessDecision{Success: false, Reason: "missing summary", Source: successSourceHeuristic}, nil
	}
	if strings.EqualFold(strings.TrimSpace(record.SessionKey), "heartbeat") {
		return TaskSuccessDecision{Success: false, Reason: "heartbeat session", Source: successSourceHeuristic}, nil
	}
	if strings.EqualFold(strings.TrimSpace(record.FinalOutput), "HEARTBEAT_OK") {
		return TaskSuccessDecision{Success: false, Reason: "heartbeat output", Source: successSourceHeuristic}, nil
	}
	if strings.TrimSpace(record.FinalOutput) == "" {
		return TaskSuccessDecision{Success: false, Reason: "missing final output", Source: successSourceHeuristic}, nil
	}
	return TaskSuccessDecision{Success: true, Reason: "heuristic success", Source: successSourceHeuristic}, nil
}

type LLMTaskSuccessJudge struct {
	provider providers.LLMProvider
	model    string
	fallback SuccessJudge
}

type llmTaskSuccessResponse struct {
	Success bool   `json:"success"`
	Reason  string `json:"reason"`
}

func NewLLMTaskSuccessJudge(provider providers.LLMProvider, model string, fallback SuccessJudge) *LLMTaskSuccessJudge {
	if fallback == nil {
		fallback = &HeuristicSuccessJudge{}
	}
	return &LLMTaskSuccessJudge{
		provider: provider,
		model:    strings.TrimSpace(model),
		fallback: fallback,
	}
}

func (j *LLMTaskSuccessJudge) JudgeTaskRecord(
	ctx context.Context,
	record LearningRecord,
) (TaskSuccessDecision, error) {
	if j == nil || j.provider == nil {
		return j.fallbackDecision(ctx, record)
	}

	model := strings.TrimSpace(j.model)
	if model == "" {
		model = strings.TrimSpace(j.provider.GetDefaultModel())
	}
	if model == "" {
		return j.fallbackDecision(ctx, record)
	}

	callCtx, cancel := withLLMCallTimeout(ctx, llmTaskSuccessJudgeTimeout())
	defer cancel()
	resp, err := j.provider.Chat(callCtx, []providers.Message{
		{
			Role:    "system",
			Content: "Return exactly one JSON object with fields success:boolean and reason:string. No markdown fences.",
		},
		{
			Role:    "user",
			Content: buildTaskSuccessJudgePrompt(record),
		},
	}, nil, model, map[string]any{"temperature": 0})
	if isNoCreditError(err) {
		return TaskSuccessDecision{}, noCreditError(err)
	}
	// A canceled run (Close on every sleep and config reload) is not this step
	// failing: falling back would persist a heuristic answer as the model's.
	// The run's ctx, not callCtx, so a per-call timeout keeps its fallback.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return TaskSuccessDecision{}, ctxErr
	}
	if err != nil || resp == nil {
		return j.fallbackDecision(ctx, record)
	}

	content := strings.TrimSpace(resp.Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	if content == "" {
		return j.fallbackDecision(ctx, record)
	}

	var payload llmTaskSuccessResponse
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return j.fallbackDecision(ctx, record)
	}
	return TaskSuccessDecision{
		Success: payload.Success,
		Reason:  strings.TrimSpace(payload.Reason),
		Source:  successSourceLLM,
	}, nil
}

func (j *LLMTaskSuccessJudge) fallbackDecision(
	ctx context.Context,
	record LearningRecord,
) (TaskSuccessDecision, error) {
	if j == nil || j.fallback == nil {
		return TaskSuccessDecision{Success: false, Reason: "no success judge available"}, nil
	}
	return j.fallback.JudgeTaskRecord(ctx, record)
}

func buildTaskSuccessJudgePrompt(record LearningRecord) string {
	lines := []string{
		"Decide whether this agent task truly achieved the user's goal.",
		"Reject tasks that are only partial reasoning, only describe future steps, or obviously did not complete the requested outcome.",
		"Accept completed custom workspace skill/theorem tasks when the final output gives a concrete result or concrete completed procedure.",
		"",
		"Summary: " + fallbackString(record.Summary, "none"),
		"Final output: " + fallbackString(record.FinalOutput, "none"),
		"Used skills: " + joinOrFallback(record.UsedSkillNames, "none"),
	}
	return strings.Join(lines, "\n")
}

// taskSuccessQuestion is the success judge's question to a decision model: a
// yes/no read of the record, in any language, with no JSON to parse.
// (seucaranguejo fork)
const taskSuccessQuestion = "Did this assistant task truly achieve what the person asked for? Answer yes when the final output gives " +
	"the person a concrete result, a finished piece of work, a completed procedure or a direct answer to " +
	"their question, even when it then offers more help or asks a follow-up question about that result. A " +
	"result shown as a card, table or interface markup (such as an openui-lang block), an image or an " +
	"attached file counts as delivered. When the request is an automated event (a webhook, payload or message " +
	"notification) with handling rules, applying those rules counts as achieved: replying, logging it, " +
	"ignoring it as the rules say, or reporting that no action was needed; only logging or ignoring an event " +
	"the rules say to act on does not. When the person asked to be asked questions, asking them counts as " +
	"achieved. Answer no when it only plans, describes or promises future steps, stops partway or delivers " +
	"only part of what was asked, reports a failure, waits for something the person still has to send, asks " +
	"for information before doing the task, or does not deliver the requested outcome, even when it ends " +
	"offering to try again or another way. Answer no as well when there was no task, such as a thanks, an " +
	"okay, a goodbye or a greeting, however friendly the reply."

const taskSuccessQuestionName = "task_succeeded"

// taskSuccessYes and taskSuccessNo are the decision model's thresholds; in
// between is doubt, which falls back.
const (
	taskSuccessYes = 0.8
	taskSuccessNo  = 0.2
)

// DecisionTaskSuccessJudge asks a decision model whether a task record
// achieved the person's goal: cheaper and faster than a chat model writing
// JSON, and it reads any language. Doubt, a refusal or an error falls back;
// the account out of credit stops the cold path, as the LLM judge does.
// (seucaranguejo fork)
type DecisionTaskSuccessJudge struct {
	decider  providers.Decider
	model    string
	fallback SuccessJudge
}

func NewDecisionTaskSuccessJudge(
	decider providers.Decider,
	model string,
	fallback SuccessJudge,
) *DecisionTaskSuccessJudge {
	if fallback == nil {
		fallback = &HeuristicSuccessJudge{}
	}
	return &DecisionTaskSuccessJudge{decider: decider, model: strings.TrimSpace(model), fallback: fallback}
}

func (j *DecisionTaskSuccessJudge) JudgeTaskRecord(
	ctx context.Context,
	record LearningRecord,
) (TaskSuccessDecision, error) {
	if j.decider == nil || j.model == "" {
		return j.fallback.JudgeTaskRecord(ctx, record)
	}
	callCtx, cancel := withLLMCallTimeout(ctx, llmTaskSuccessJudgeTimeout())
	defer cancel()
	answers, err := j.decider.Decide(callCtx, j.model, buildTaskSuccessDecisionInput(record),
		[]providers.DecisionQuestion{{Name: taskSuccessQuestionName, Instructions: taskSuccessQuestion}})
	if isNoCreditError(err) {
		return TaskSuccessDecision{}, noCreditError(err)
	}
	// A canceled run is not this step failing: see LLMTaskSuccessJudge.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return TaskSuccessDecision{}, ctxErr
	}
	p, answered := answers[taskSuccessQuestionName]
	if err != nil || !answered {
		return j.fallbackAs(ctx, record, successSourceErrorFallback, nil)
	}
	if p > taskSuccessNo && p < taskSuccessYes {
		return j.fallbackAs(ctx, record, successSourceDoubtFallback, &p)
	}
	return TaskSuccessDecision{
		Success: p >= taskSuccessYes,
		Reason:  fmt.Sprintf("decision p=%.2f", p),
		P:       &p,
		Source:  successSourceDecision,
	}, nil
}

// fallbackAs takes the fallback's verdict but records why the decision model
// did not decide, so the log tells a doubt from a failed call.
func (j *DecisionTaskSuccessJudge) fallbackAs(
	ctx context.Context,
	record LearningRecord,
	source string,
	p *float64,
) (TaskSuccessDecision, error) {
	decision, err := j.fallback.JudgeTaskRecord(ctx, record)
	decision.Source, decision.P = source, p
	return decision, err
}

// buildTaskSuccessDecisionInput is what the decision model reads: what the
// person asked, the final output and the skills used.
func buildTaskSuccessDecisionInput(record LearningRecord) string {
	return strings.Join([]string{
		"The person asked: " + fallbackString(judgedAsk(record), "not recorded"),
		"Final output: " + fallbackString(record.FinalOutput, "none"),
		"Used skills: " + joinOrFallback(record.UsedSkillNames, "none"),
	}, "\n")
}

// judgedAsk is what the judge reads as the request: a task record's summary is
// the person's message, clipped; turns record no goal.
func judgedAsk(record LearningRecord) string {
	return fallbackString(record.UserGoal, messageutil.StripSenderTag(record.Summary))
}

// logSuccessVerdict writes one line per judged record, so the judge's verdicts,
// probabilities and fallbacks can be measured from the logs; the text stays out.
func logSuccessVerdict(record LearningRecord, decision TaskSuccessDecision, elapsed time.Duration) {
	verdict := "rejected"
	if decision.Success {
		verdict = "success"
	}
	outputRunes := utf8.RuneCountInString(record.FinalOutput)
	fields := map[string]any{
		"source":         decision.Source,
		"verdict":        verdict,
		"judge_ms":       elapsed.Milliseconds(),
		"session_kind":   sessionKind(record.SessionKey),
		"asked_chars":    utf8.RuneCountInString(judgedAsk(record)),
		"output_chars":   outputRunes,
		"output_clipped": outputRunes == finalOutputMaxRunes && strings.HasSuffix(record.FinalOutput, "..."),
		"used_skills":    len(record.UsedSkillNames),
	}
	if decision.P != nil {
		fields["judge_p"] = *decision.P
	}
	logger.InfoCF("evolution", "Success judge verdict", fields)
}

// sessionKind names the kind of session a record came from without its id.
func sessionKind(key string) string {
	switch {
	case strings.TrimSpace(key) == "":
		return "none"
	case session.IsOpaqueSessionKey(key):
		return "opaque"
	}
	if parsed := session.ParseLegacyAgentSessionKey(key); parsed != nil {
		return "agent:" + parsed.AgentID
	}
	return "other"
}
