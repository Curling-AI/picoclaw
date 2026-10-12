package agent

import (
	"context"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/turnresult"
)

type resultBlockingProvider struct{ started chan struct{} }

func (p *resultBlockingProvider) GetDefaultModel() string { return "test-model" }

func (p *resultBlockingProvider) Chat(
	ctx context.Context,
	_ []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDirectResultReportsHardAbortSynchronously(t *testing.T) {
	_, cfg, msgBus, _, cleanup := newTestAgentLoop(t)
	defer cleanup()
	provider := &resultBlockingProvider{started: make(chan struct{}, 1)}
	al := NewAgentLoop(cfg, msgBus, provider)
	key := "agent:cron-job-11111111-1111-4111-8111-111111111111"
	type outcome struct {
		result turnresult.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := al.ProcessDirectWithResult(
			context.Background(),
			turnresult.Request{
				Media:      nil,
				Progress:   nil,
				Content:    "hello",
				SessionKey: key,
				Channel:    "cli",
				ChatID:     "direct",
			},
		)
		done <- outcome{result, err}
	}()
	select {
	case <-provider.started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never started")
	}
	if err := al.HardAbort(key); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.result.Status != turnresult.Canceled || got.result.Content != "" {
			t.Fatal("hard-abort reported success", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hard-abort did not terminate executor")
	}
}

func TestDirectResultDoesNotInferSuccessFromFallbackText(t *testing.T) {
	_, cfg, msgBus, _, cleanup := newTestAgentLoop(t)
	defer cleanup()
	cfg.Agents.Defaults.MaxToolIterations = 1
	al := NewAgentLoop(cfg, msgBus, &toolLimitOnlyProvider{})
	al.RegisterTool(&toolLimitTestTool{})
	request := turnresult.Request{
		Media:      nil,
		Progress:   nil,
		Content:    "hello",
		SessionKey: "result-limit",
		Channel:    "test",
		ChatID:     "direct",
	}
	result, err := al.ProcessDirectWithResult(context.Background(), request)
	if err != nil || result.Status != turnresult.Failed || result.Content != toolLimitResponse {
		t.Fatal("fallback confused with completion", result, err)
	}
	legacy, err := al.ProcessDirectWithChannel(
		context.Background(),
		request.Content,
		"legacy-limit",
		request.Channel,
		request.ChatID,
	)
	if err != nil || legacy != toolLimitResponse {
		t.Fatal("legacy contract changed", legacy, err)
	}
}
