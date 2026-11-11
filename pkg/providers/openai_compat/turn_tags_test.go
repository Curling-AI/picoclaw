package openai_compat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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

func TestTurnTagsPreserveRequestsThatCannotAcceptATurnTag(t *testing.T) {
	fullTags := []string{"tag-1", "tag-2", "tag-3", "tag-4", "tag-5", "tag-6", "tag-7", "tag-8", "tag-9", "tag-10"}
	cases := []struct {
		name   string
		turnID string
		tags   any
	}{
		{
			name:   "tag exceeds 64 characters",
			turnID: strings.Repeat("a", 65-len("turn_id:")),
			tags:   []string{"existing"},
		},
		{
			name:   "ten tags already configured",
			turnID: "main-turn-1",
			tags:   fullTags,
		},
		{
			name:   "tags have an unexpected type",
			turnID: "main-turn-1",
			tags:   "invalid",
		},
	}
	provider := NewProvider("key", "https://maestro/llm-proxy/a/hulk", "")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"model": "model", "providerOptions": map[string]any{
				"gateway": map[string]any{"user": "owner", "tags": tc.tags},
			}}
			before, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("encode original request: %v", err)
			}
			got := provider.tagTurnRequest(protocoltypes.WithTurnID(t.Context(), tc.turnID), body)
			after, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("encode returned request: %v", err)
			}
			if string(after) != string(before) {
				t.Fatalf("request changed: got %s, want %s", after, before)
			}
			original, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("encode original request after tagging: %v", err)
			}
			if string(original) != string(before) {
				t.Fatalf("original request mutated: got %s, want %s", original, before)
			}
		})
	}
}

func TestTurnTagsAcceptA64CharacterTagAsTheTenthTag(t *testing.T) {
	tags := []string{"tag-1", "tag-2", "tag-3", "tag-4", "tag-5", "tag-6", "tag-7", "tag-8", "tag-9"}
	turnID := strings.Repeat("a", 64-len("turn_id:"))
	provider := NewProvider("key", "https://maestro/llm-proxy/a/hulk", "")
	body := map[string]any{"providerOptions": map[string]any{"gateway": map[string]any{"tags": tags}}}
	got := provider.tagTurnRequest(protocoltypes.WithTurnID(t.Context(), turnID), body)
	wantTags := append(append([]string(nil), tags...), "turn_id:"+turnID)
	want := map[string]any{"providerOptions": map[string]any{"gateway": map[string]any{"tags": wantTags}}}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("encode tagged request: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("encode expected request: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("gateway tags at contract limits = %s, want %s", gotJSON, wantJSON)
	}
}

func TestTaskIDHeaderReachesHulkOnly(t *testing.T) {
	for _, tc := range []struct {
		path string
		want string
	}{
		{"/llm-proxy/a/hulk", "task-1"},
		{"/llm-proxy/a/gateway", ""},
		{"/v1", ""},
	} {
		for _, stream := range []bool{false, true} {
			seen := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Get(taskIDHeader)
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
			provider := NewProvider("key", server.URL+tc.path, "")
			ctx := protocoltypes.WithTaskID(t.Context(), "task-1")
			messages := []Message{{Role: "user", Content: "hello"}}
			var err error
			if stream {
				_, err = provider.ChatStreamEvents(ctx, messages, nil, "model", nil, nil)
			} else {
				_, err = provider.Chat(ctx, messages, nil, "model", nil)
			}
			server.Close()
			if err != nil {
				t.Fatalf("%s stream=%v: model call: %v", tc.path, stream, err)
			}
			if got := <-seen; got != tc.want {
				t.Fatalf("%s stream=%v: task header = %q, want %q", tc.path, stream, got, tc.want)
			}
		}
	}
}

// The labels ride next to the task id on hulk's route only: hulk refuses
// labels on a call without a task.
func TestTaskLabelsReachHulkOnlyNextToATask(t *testing.T) {
	labeled := protocoltypes.WithTaskRole(
		protocoltypes.WithTaskID(t.Context(), "task-1", protocoltypes.MessageTaskLabels("grpc")...),
		protocoltypes.RoleAgent,
	)
	for _, tc := range []struct {
		name string
		path string
		ctx  context.Context
		want string
	}{
		{"hulk", "/llm-proxy/a/hulk", labeled, `["task:message","channel:web","role:agent"]`},
		{"no task", "/llm-proxy/a/hulk", protocoltypes.WithTaskRole(t.Context(), protocoltypes.RoleAgent), ""},
		{"gateway", "/llm-proxy/a/gateway", labeled, ""},
	} {
		for _, stream := range []bool{false, true} {
			seen := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Get(taskLabelsHeader)
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
			provider := NewProvider("key", server.URL+tc.path, "")
			messages := []Message{{Role: "user", Content: "hello"}}
			var err error
			if stream {
				_, err = provider.ChatStreamEvents(tc.ctx, messages, nil, "model", nil, nil)
			} else {
				_, err = provider.Chat(tc.ctx, messages, nil, "model", nil)
			}
			server.Close()
			if err != nil {
				t.Fatalf("%s stream=%v: model call: %v", tc.name, stream, err)
			}
			if got := <-seen; got != tc.want {
				t.Fatalf("%s stream=%v: labels header = %q, want %q", tc.name, stream, got, tc.want)
			}
		}
	}
}
