package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/turnresult"
)

// automationRunKey is the session key ExecuteJobWithResult gives a run
// (pkg/tools/cron.go): "agent:cron-<job>-<run>".
const automationRunKey = "agent:cron-job-1-6f1c2f3e-4b5a-4c6d-8e9f-0a1b2c3d4e5f"

func automationRequest(sessionKey string) turnresult.Request {
	return turnresult.Request{
		Progress:   nil,
		Media:      nil,
		Content:    "[Scheduled run of cron job \"Novas vagas\"]\nme avise só quando houver vaga nova",
		SessionKey: sessionKey,
		Channel:    "telegram",
		ChatID:     "chat-1",
	}
}

func TestIsCronRunMainTurn(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want bool
	}{
		{automationRunKey, true},
		{"agent:cronmodel-job-1-run", true},
		{"subturn-1", false},
		{"sk_v1_abc", false},
		{"", false},
	} {
		ts := &turnState{sessionKey: tc.key}
		if got := ts.isCronRunMainTurn(); got != tc.want {
			t.Errorf("isCronRunMainTurn(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
	var none *turnState
	if none.isCronRunMainTurn() {
		t.Error("a nil turn is not a cron run")
	}
	// A turn the run spawned: isCronRun counts it, by the job's cache scope it
	// inherits, but it is not the run itself.
	child := &turnState{sessionKey: "subturn-1", promptCacheScopeOverride: "agent:cron-job-1"}
	if !child.isCronRun() || child.isCronRunMainTurn() {
		t.Errorf("a run's subturn: isCronRun = %v, isCronRunMainTurn = %v; want true, false",
			child.isCronRun(), child.isCronRunMainTurn())
	}
}

// Adapted from picoclaw#128 (silent_turn_test.go). Here an empty end is still
// a failure: only what is delivered changes. (seucaranguejo fork — MST-795)
func TestEmptyAnswerOfAnAutomationRunDeliversNothing(t *testing.T) {
	al, provider := newScriptedLoop(t, providers.LLMResponse{Content: ""}, providers.LLMResponse{Content: ""})
	result, err := al.ProcessDirectWithResult(context.Background(), automationRequest(automationRunKey))
	if err != nil {
		t.Fatalf("ProcessDirectWithResult: %v", err)
	}
	if result.Status != turnresult.Failed || result.Content != "" {
		t.Fatalf("result = %+v, want failed with no content", result)
	}
	if got := provider.calls(); got != 1+maxEmptyResponseRetries {
		t.Fatalf("provider calls = %d, want %d (the empty-answer retry still runs)", got, 1+maxEmptyResponseRetries)
	}
}

func TestEmptyAnswerOutsideAnAutomationRunKeepsTheFallback(t *testing.T) {
	al, _ := newScriptedLoop(t, providers.LLMResponse{Content: ""}, providers.LLMResponse{Content: ""})
	result, err := al.ProcessDirectWithResult(context.Background(), automationRequest("subturn-1"))
	if err != nil {
		t.Fatalf("ProcessDirectWithResult: %v", err)
	}
	if result.Status != turnresult.Failed || result.Content != defaultResponse {
		t.Fatalf("result = %+v, want failed with the default response", result)
	}
}

func TestAutomationRunAtTheIterationCapKeepsTheToolLimitReply(t *testing.T) {
	al, _ := newScriptedLoopWithCap(t, 1, readFileCall("call-1"), readFileCall("call-2"))
	result, err := al.ProcessDirectWithResult(context.Background(), automationRequest(automationRunKey))
	if err != nil {
		t.Fatalf("ProcessDirectWithResult: %v", err)
	}
	if result.Content != toolLimitResponse {
		t.Fatalf("result = %+v, want the tool-limit reply", result)
	}
}
