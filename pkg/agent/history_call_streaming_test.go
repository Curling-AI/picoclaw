package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type historyCallDelegate struct{ calls []string }

func TestHistoryCallNeverBorrowsTheAuditRequestForASynthesizedReply(t *testing.T) {
	cfg := newConfiguredStreamingTestConfig(t, true, true, nil)
	cfg.Agents.Defaults.MaxToolIterations = 10
	messageBus := bus.NewMessageBus()
	streams := &historyCallDelegate{}
	messageBus.SetStreamDelegate(streams)
	provider := &configuredStreamingProvider{}
	for range 4 {
		provider.streamPlan = append(provider.streamPlan, configuredStreamingCall{
			chunks: []string{"Creating the project"}, response: &providers.LLMResponse{
				Content: "Creating the project",
				ToolCalls: []providers.ToolCall{
					{ID: "create", Type: "function", Name: createToolName, Arguments: map[string]any{}},
				},
			},
		})
	}
	loop := NewAgentLoop(cfg, messageBus, provider)
	t.Cleanup(loop.Close)
	instance := loop.GetRegistry().GetDefaultAgent()
	instance.Tools.Register(newMCPTestTool(createToolName, tools.RepeatUnsafe))
	opts := configuredStreamingProcessOptions("pico")
	opts.NoHistory = false
	instance.Sessions.AddMessage(opts.SessionKey, "user", "seed")
	source, ok := instance.Sessions.(memory.HistoryContentLineage)
	if !ok {
		t.Fatal("native source has no content identity")
	}
	frame, err := source.ReadHistoryContentFrame(t.Context(), opts.SessionKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.InitializeHistoryContentIdentity(t.Context(), opts.SessionKey, frame.Observation); err != nil {
		t.Fatal(err)
	}
	got, err := loop.runAgentLoop(t.Context(), instance, opts)
	if err != nil || got != repeatStopSummary(createToolName) {
		t.Fatalf("repeat guard did not reach its real synthesized reply: %q %v", got, err)
	}
	if len(streams.calls) != 5 || streams.calls[4] != "" || !providers.ValidHistoryCallID(streams.calls[3]) {
		t.Fatalf("synthesized stream borrowed an earlier LLM identity: %v", streams.calls)
	}
	history := instance.Sessions.GetHistory(opts.SessionKey)
	last := history[len(history)-1]
	reader, ok := instance.Sessions.(interface {
		ReadPublicHistory(ctx context.Context, key string) (memory.PublicHistory, error)
	})
	if !ok {
		t.Fatal("native source has no public history")
	}
	public, err := reader.ReadPublicHistory(t.Context(), opts.SessionKey)
	if err != nil || len(public.Entries) == 0 {
		t.Fatal("synthesized reply did not reach public source", err)
	}
	lastPublic := public.Entries[len(public.Entries)-1]
	var native []string
	for _, entry := range public.Entries {
		if entry.Message.Role == "assistant" && entry.Message.Content == "Creating the project" {
			native = append(native, entry.CallID)
		}
	}
	if lastPublic.Message.Content != got || lastPublic.CallID != "" || len(native) != 4 ||
		native[0] != streams.calls[0] {
		t.Fatal("source inferred synthesized provenance from the audit record")
	}
	if last.Content != got || last.LLMCall == nil || last.LLMCall.RequestID != streams.calls[3] ||
		last.HistoryCallID != "" {
		t.Fatalf("synthesized reply lost audit or reused producer proof: %+v", last)
	}
}

func (d *historyCallDelegate) GetStreamer(ctx context.Context, _, _, _ string) (bus.Streamer, bool) {
	d.calls = append(d.calls, providers.HistoryCall(ctx))
	return &recordingStreamer{}, true
}

func TestHistoryCallMatchesEachPersistedLLMRoundWithIdenticalText(t *testing.T) {
	cfg := newConfiguredStreamingTestConfig(t, true, true, nil)
	messageBus := bus.NewMessageBus()
	streams := &historyCallDelegate{}
	messageBus.SetStreamDelegate(streams)
	provider := &configuredStreamingProvider{streamPlan: []configuredStreamingCall{
		{chunks: []string{"same"}, response: &providers.LLMResponse{Content: "same", ToolCalls: []providers.ToolCall{
			{ID: "tool-1", Type: "function", Name: "tool_limit_test_tool", Arguments: map[string]any{"value": "first"}},
		}}},
		{chunks: []string{"same"}, response: &providers.LLMResponse{Content: "same"}},
	}}
	loop := NewAgentLoop(cfg, messageBus, provider)
	t.Cleanup(loop.Close)
	instance := loop.GetRegistry().GetDefaultAgent()
	instance.Tools.Register(&toolLimitTestTool{})
	opts := configuredStreamingProcessOptions("pico")
	opts.NoHistory = false
	got, err := loop.runAgentLoop(t.Context(), instance, opts)
	if err != nil || got != "same" {
		t.Fatalf("ordinary final response changed: %q %v", got, err)
	}
	if len(streams.calls) != 2 || !providers.ValidHistoryCallID(streams.calls[0]) ||
		!providers.ValidHistoryCallID(streams.calls[1]) || streams.calls[0] == streams.calls[1] {
		t.Fatalf("separate LLM calls reused their live identity: %v", streams.calls)
	}
	var persisted []string
	for _, message := range instance.Sessions.GetHistory(configuredStreamingProcessOptions("pico").SessionKey) {
		if message.Role == "assistant" && message.LLMCall != nil {
			persisted = append(persisted, message.LLMCall.RequestID)
		}
	}
	if len(persisted) != 2 || persisted[0] != streams.calls[0] || persisted[1] != streams.calls[1] {
		t.Fatalf("live segments differ from producer commit identity: live=%v persisted=%v", streams.calls, persisted)
	}
}
