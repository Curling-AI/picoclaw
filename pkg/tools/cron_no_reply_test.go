package tools

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/cron"
	"github.com/sipeed/picoclaw/pkg/turnresult"
)

// runAutomation runs one scheduled execution of a WhatsApp automation whose
// turn ends with status and answer. Adapted from picoclaw#128
// (cron_silent_test.go): here the run's answer decides what is delivered, not
// a flag on the job. (seucaranguejo fork — MST-795)
func runAutomation(t *testing.T, status turnresult.Status, answer string) (turnresult.Result, *typedJobExecutor) {
	t.Helper()
	var executor typedJobExecutor
	executor.status = status
	executor.response = answer
	return runAutomationOn(t, &executor), &executor
}

// runAutomationOn runs that automation on executor.
func runAutomationOn(t *testing.T, executor JobExecutor) turnresult.Result {
	t.Helper()
	tool := newTestCronToolWithExecutorAndConfig(t, executor, config.DefaultConfig())
	var job cron.CronJob
	job.ID = "job-no-reply"
	job.Name = "Novas vagas"
	job.Payload.Channel = "whatsapp_central"
	job.Payload.To = "owner"
	job.Payload.Message = "me avise só quando houver vaga nova"
	result, err := tool.ExecuteJobWithResult(
		context.Background(),
		cron.Execution{Progress: nil, RunID: uuid.NewString(), Trigger: cron.Scheduled, Job: &job},
	)
	if err != nil {
		t.Fatalf("ExecuteJobWithResult: %v", err)
	}
	return result
}

func TestAutomationRunDeliversByItsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    turnresult.Status
		answer    string
		published string
	}{
		{"nothing to deliver", turnresult.Completed, "NO_REPLY", ""},
		{"news", turnresult.Completed, "Nova vaga: casa em Lisboa", "Nova vaga: casa em Lisboa"},
		{"token mixed with news", turnresult.Completed, "NO_REPLY\nNova vaga: casa em Lisboa", "Nova vaga: casa em Lisboa"},
		{"failed run keeps its status", turnresult.Failed, "NO_REPLY", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, executor := runAutomation(t, tc.status, tc.answer)
			if called := executor.publishedChan != ""; called != (tc.published != "") {
				t.Fatalf("PublishResponseIfNeeded called = %v, want %v", called, tc.published != "")
			}
			if executor.publishedResp != tc.published {
				t.Fatalf("published %q, want %q", executor.publishedResp, tc.published)
			}
			if result.Status != tc.status || result.Content != tc.published {
				t.Fatalf("result = %+v, want %s with %q", result, tc.status, tc.published)
			}
		})
	}
}

// dismissingJobExecutor takes a chat's tool-feedback message out when asked,
// as agent.AgentLoop does.
type dismissingJobExecutor struct {
	typedJobExecutor
	dismissed []string
}

func (e *dismissingJobExecutor) DismissToolFeedback(_ context.Context, channel, chatID string) {
	e.dismissed = append(e.dismissed, channel+":"+chatID)
}

// The final outbound is what closes the chat's tool-feedback message: a run
// that delivers nothing takes it out instead, and one that delivers leaves it
// to that outbound. (seucaranguejo fork — MST-795)
func TestAutomationRunThatDeliversNothingDismissesToolFeedback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    turnresult.Status
		answer    string
		dismissed []string
	}{
		{"nothing to deliver", turnresult.Completed, "NO_REPLY", []string{"whatsapp_central:owner"}},
		{"empty failed run", turnresult.Failed, "", []string{"whatsapp_central:owner"}},
		{"news", turnresult.Completed, "Nova vaga: casa em Lisboa", nil},
		{"token mixed with news", turnresult.Completed, "NO_REPLY\nNova vaga: casa em Lisboa", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var executor dismissingJobExecutor
			executor.status = tc.status
			executor.response = tc.answer
			runAutomationOn(t, &executor)
			if !slices.Equal(executor.dismissed, tc.dismissed) {
				t.Fatalf("dismissed %q, want %q", executor.dismissed, tc.dismissed)
			}
		})
	}
}

// Memory refreshes (cli/direct) and loop rounds also run through
// ExecuteJobWithResult: the channel tells an owner's automation apart in these
// logs. (seucaranguejo fork — MST-795)
func TestAutomationRunDeliveryLogsNameTheChannel(t *testing.T) {
	for _, tc := range []struct{ answer, message string }{
		{"NO_REPLY", "Automation run had nothing to deliver"},
		{"NO_REPLY\nNova vaga: casa em Lisboa", "Automation answer mixed NO_REPLY with text; delivering the text"},
	} {
		t.Run(tc.message, func(t *testing.T) {
			readLog := captureLog(t)
			runAutomation(t, turnresult.Completed, tc.answer)
			entry := logEntry(t, readLog(), tc.message)
			if entry["channel"] != "whatsapp_central" {
				t.Fatalf("channel = %v, want whatsapp_central: %v", entry["channel"], entry)
			}
		})
	}
}

// logEntry is the line of logs whose message is message, decoded.
func logEntry(t *testing.T, logs, message string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["message"] == message {
			return entry
		}
	}
	t.Fatalf("no %q in the log:\n%s", message, logs)
	return nil
}

func TestAutomationRunNoteAsksForTheTokenAndKeepsItsEnds(t *testing.T) {
	_, executor := runAutomation(t, turnresult.Completed, "NO_REPLY")
	prompt := executor.request.Content
	for _, want := range []string{
		"(System note: this is a routine scheduled run.\n",
		"• Delivery: your final answer is delivered to the user as this run's result.",
		"your final answer must be exactly NO_REPLY and nothing else",
		"never mention NO_REPLY.\n• Notes: do NOT record routine runs in the daily notes.",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "run silently") {
		t.Fatalf("prompt still asks to run silently:\n%s", prompt)
	}
	// ethos/internal/privacy/redact.go finds the note by its first and last words.
	if !strings.HasSuffix(prompt, "never use the daily notes as a state store.)") {
		t.Fatalf("prompt no longer ends where the privacy export looks for it:\n%s", prompt)
	}
}
