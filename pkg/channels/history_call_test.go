package channels

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestSplitStreamKeepsTheProducerCallWhenUpdateHasAnotherContext(t *testing.T) {
	const call = "pc-00112233445566778899aabb"
	m := newTestManager()
	m.config = &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{SplitOnMarker: true}}}
	var calls []string
	m.channels["test"] = &mockStreamingChannel{beginStreamFn: func(ctx context.Context, _ string) (Streamer, error) {
		calls = append(calls, providers.HistoryCall(ctx))
		return &recordingStreamSegment{}, nil
	}}
	stream, ok := m.GetStreamer(providers.WithHistoryCall(t.Context(), call), "test", "run", "session")
	if !ok {
		t.Fatal("stream unavailable")
	}
	if err := stream.Update(context.Background(), "first<|[SPLIT]|>second"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0] != call || calls[1] != call {
		t.Fatalf("split changed call provenance: %v", calls)
	}
}
