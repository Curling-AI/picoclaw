package openai_compat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

func TestTurnTagsReachHulkInJSONAndStreamingRequests(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			seen := make(chan []string, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					ProviderOptions struct {
						Gateway struct {
							Tags []string `json:"tags"`
						} `json:"gateway"`
					} `json:"providerOptions"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				seen <- body.ProviderOptions.Gateway.Tags
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(
						w,
						"data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
					)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			extra := map[string]any{"providerOptions": map[string]any{"gateway": map[string]any{
				"user": "owner", "tags": []string{"ethos:assistant:a"},
			}}}
			provider := NewProvider("key", server.URL+"/llm-proxy/a/hulk", "", WithExtraBody(extra))
			for _, id := range []string{"main-turn-1", "main-turn-2"} {
				ctx := protocoltypes.WithTurnID(t.Context(), id)
				messages := []Message{{Role: "user", Content: "hello"}}
				var err error
				if stream {
					_, err = provider.ChatStreamEvents(ctx, messages, nil, "model", nil, nil)
				} else {
					_, err = provider.Chat(ctx, messages, nil, "model", nil)
				}
				if err != nil {
					t.Fatalf("model call: %v", err)
				}
				if got, want := <-seen, []string{"ethos:assistant:a", "turn_id:" + id}; !reflect.DeepEqual(got, want) {
					t.Fatalf("gateway tags = %v, want %v", got, want)
				}
			}
		})
	}
}

func TestTurnTagsDoNotChangeOtherProvidersOrCallsWithoutATurn(t *testing.T) {
	for _, base := range []string{"https://api.openai.com/v1", "https://maestro/llm-proxy/a/crof"} {
		provider := NewProvider("key", base, "")
		body := map[string]any{"model": "model"}
		want := map[string]any{"model": "model"}
		if got := provider.tagTurnRequest(
			protocoltypes.WithTurnID(t.Context(), "main-turn-1"),
			body,
		); !reflect.DeepEqual(
			got,
			want,
		) {
			t.Fatalf("body changed for %s: %v", base, got)
		}
	}
	provider := NewProvider("key", "https://maestro/llm-proxy/a/hulk", "")
	body := map[string]any{"model": "model"}
	want := map[string]any{"model": "model"}
	if got := provider.tagTurnRequest(t.Context(), body); !reflect.DeepEqual(got, want) {
		t.Fatalf("body changed without a turn: %v", got)
	}
}
