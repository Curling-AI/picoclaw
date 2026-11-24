package openai_compat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// Decide speaks the gateway's Decisions API: one input, predicate questions,
// and the probability of yes back by name, with the credentials Chat uses.
func TestDecideAsksTheDecisionsAPIWithTheChatCredentials(t *testing.T) {
	var got struct {
		Model     string              `json:"model"`
		Input     string              `json:"input"`
		Questions []map[string]string `json:"questions"`
	}
	var path, auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"model":"jev","answers":[` +
			`{"type":"predicate","name":"stopped_on_promise","probability":0.93},` +
			`{"type":"predicate","name":"refused","probability":null}]}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer server.Close()

	p := NewProvider("pod-token", server.URL+"/v1", "")
	answers, err := p.Decide(t.Context(), "typesafe-ai/jev-1.14.0", "the reply", []protocoltypes.DecisionQuestion{
		{Name: "stopped_on_promise", Instructions: "does it stop on a promise?"},
		{Name: "refused", Instructions: "a question the model refuses"},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if path != "/v1/decisions" || auth != "Bearer pod-token" {
		t.Errorf("request went to %q with %q, want /v1/decisions with the bearer token", path, auth)
	}
	if got.Model != "typesafe-ai/jev-1.14.0" || got.Input != "the reply" || len(got.Questions) != 2 ||
		got.Questions[0]["type"] != "predicate" || got.Questions[0]["name"] != "stopped_on_promise" ||
		got.Questions[0]["instructions"] != "does it stop on a promise?" {
		t.Errorf("request body = %+v, want the model, the input and predicate questions", got)
	}
	if answers["stopped_on_promise"] != 0.93 {
		t.Errorf("stopped_on_promise = %v, want 0.93", answers["stopped_on_promise"])
	}
	if _, answered := answers["refused"]; answered {
		t.Error("a refused question came back with a probability")
	}
}

func TestDecideReportsAGatewayError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"model_not_found"}}`, http.StatusNotFound)
	}))
	defer server.Close()

	_, err := NewProvider("k", server.URL, "").Decide(t.Context(), "jev", "x",
		[]protocoltypes.DecisionQuestion{{Name: "q", Instructions: "?"}})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the gateway's 404", err)
	}
}
