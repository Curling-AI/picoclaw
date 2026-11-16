package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/common"
)

func TestSmartFallbackRequiresTheProxySignalAndModelRefusal(t *testing.T) {
	for _, tc := range []struct {
		status     int
		code, tier string
		want       bool
	}{
		{404, "model_not_found", "flash", true},
		{404, "model_not_found", "", false},
		{404, "user_not_found", "flash", false},
		{429, "insufficient_balance", "flash", false},
		{503, "provider_unavailable", "flash", false},
		{404, "model_not_found", "pro", false},
	} {
		err := fmt.Errorf(
			"wrapped: %w",
			&common.HTTPError{StatusCode: tc.status, ErrorCode: tc.code, FallbackTier: tc.tier},
		)
		if got := hasSmartFallback(err); got != tc.want {
			t.Errorf("%+v: got %v", tc, got)
		}
	}
}

func TestRemovedModelRetriesOnlyTheFailedCallOnSmart(t *testing.T) {
	for _, tc := range []struct{ streaming, smartMissing, selected bool }{
		{false, false, false},
		{true, false, false},
		{false, true, false},
		{true, true, false},
		{false, false, true},
		{true, false, true},
	} {
		streaming := tc.streaming
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			var models, ids []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Model  string `json:"model"`
					Stream bool   `json:"stream"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				models = append(models, body.Model)
				ids = append(ids, r.Header.Get("X-Request-Id"))
				if body.Stream != streaming {
					t.Errorf("stream=%v, want %v", body.Stream, streaming)
				}
				if tc.selected && len(models) == 2 {
					if body.Stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(
							w,
							"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"smart-tool\",\"type\":\"function\",\"function\":{\"name\":\"fallback_probe\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n",
						)
					} else {
						fmt.Fprint(
							w,
							`{"choices":[{"message":{"tool_calls":[{"id":"smart-tool","type":"function","function":{"name":"fallback_probe","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
						)
					}
					return
				}
				if body.Model == "test-model" || tc.smartMissing {
					w.Header().Set("X-Maestro-Fallback-Tier", "flash")
					w.WriteHeader(404)
					fmt.Fprint(w, `{"error":{"code":"model_not_found"}}`)
					return
				}
				if body.Stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(
						w,
						"data: {\"choices\":[{\"delta\":{\"content\":\"Recovered\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
					)
				} else {
					fmt.Fprint(w, `{"choices":[{"message":{"content":"Recovered"},"finish_reason":"stop"}]}`)
				}
			}))
			defer server.Close()
			cfg := newConfiguredStreamingTestConfig(t, streaming, streaming, nil)
			cfg.ModelList[0].Model = "test-model"
			cfg.ModelList[0].APIBase = server.URL
			cfg.ModelList = append(
				cfg.ModelList,
				&config.ModelConfig{
					ModelName: "smart-model",
					Model:     "smart-model",
					Provider:  "openai",
					APIBase:   server.URL,
					Streaming: config.ModelStreamingConfig{Enabled: streaming},
				},
			)
			cfg.Agents.Defaults.ModelTiers = map[string]string{"flash": "smart-model", "removed-extra": "test-model"}
			provider, _, err := providers.CreateProvider(cfg)
			if err != nil {
				t.Fatal(err)
			}
			msgBus := bus.NewMessageBus()
			if streaming {
				msgBus.SetStreamDelegate(configuredStreamingDelegate{streamer: &recordingStreamer{}})
			}
			loop := NewAgentLoop(cfg, msgBus, provider)
			opts := configuredStreamingProcessOptions("grpc")
			tool := &countingTool{name: "fallback_probe"}
			if tc.selected {
				opts.ModelTier = "removed-extra"
				loop.GetRegistry().GetDefaultAgent().Tools.Register(tool)
			}
			opts.NoHistory = false
			response, err := loop.runAgentLoop(context.Background(), loop.GetRegistry().GetDefaultAgent(), opts)
			if (!tc.smartMissing && (err != nil || response != "Recovered")) || (tc.smartMissing && err == nil) {
				t.Fatalf("response=%q error=%v", response, err)
			}
			wantModels := []string{"test-model", "smart-model"}
			if tc.selected {
				wantModels = append(wantModels, "smart-model")
			}
			if !reflect.DeepEqual(models, wantModels) {
				t.Fatalf("models=%v", models)
			}
			if tc.selected {
				if tool.calls.Load() != 1 {
					t.Fatalf("tool calls=%d, want one", tool.calls.Load())
				}
			}
			var event struct {
				FromTier string `json:"fromTier"`
				ToTier   string `json:"toTier"`
				Reason   string `json:"reason"`
			}
			select {
			case message := <-msgBus.OutboundChan():
				if message.Context.Raw[metadataKeyMessageKind] != "model_fallback" {
					t.Fatalf("unexpected control event: %+v", message)
				}
				if err := json.Unmarshal([]byte(message.Content), &event); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("missing model fallback notification")
			}
			fromTier := "test-model"
			if tc.selected {
				fromTier = "removed-extra"
			}
			if event.FromTier != fromTier || event.ToTier != "flash" || event.Reason != "model_not_found" {
				t.Fatalf("fallback event=%+v", event)
			}
			if !tc.smartMissing {
				history := loop.GetRegistry().GetDefaultAgent().Sessions.GetHistory(opts.SessionKey)
				if len(history) == 0 || history[len(history)-1].ModelName != "smart-model" {
					t.Fatalf("history did not record ONE Smart: %+v", history)
				}
			}
			if ids[0] == "" || ids[0] == ids[1] {
				t.Fatalf("request ids=%v", ids)
			}
		})
	}
}
