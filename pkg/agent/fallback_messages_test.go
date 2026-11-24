package agent

import (
	"context"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestFallbackTextsComeFromConfigWithEnglishDefaults(t *testing.T) {
	worded := config.MessagesConfig{
		EmptyResponse: "Não consegui terminar esta resposta.",
		ToolLimit:     "Parei no limite de etapas.",
		RepeatStop:    "Parei: repeti {tool} demais.",
	}
	tests := []struct {
		name     string
		messages config.MessagesConfig
		text     func(*AgentLoop) string
		want     string
	}{
		{"empty response, default", config.MessagesConfig{}, (*AgentLoop).emptyResponseText, defaultResponse},
		{"empty response, worded", worded, (*AgentLoop).emptyResponseText, worded.EmptyResponse},
		{"tool limit, default", config.MessagesConfig{}, (*AgentLoop).toolLimitText, toolLimitResponse},
		{"tool limit, worded", worded, (*AgentLoop).toolLimitText, worded.ToolLimit},
		{
			"repeat stop, default",
			config.MessagesConfig{},
			func(al *AgentLoop) string { return al.repeatStopText("criar_projeto") },
			repeatStopSummary("criar_projeto"),
		},
		{
			"repeat stop, worded, names the tool",
			worded,
			func(al *AgentLoop) string { return al.repeatStopText("criar_projeto") },
			"Parei: repeti criar_projeto demais.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			al := &AgentLoop{cfg: &config.Config{Messages: tt.messages}}
			if got := tt.text(al); got != tt.want {
				t.Errorf("text = %q, want %q", got, tt.want)
			}
		})
	}
}

// The worded texts are what the turn actually sends, and what the delivery
// mirror recognizes as synthesized.
func TestWordedFallbacksReachTheTurn(t *testing.T) {
	al, _ := newScriptedLoopWithCap(t, 1, readFileCall("call_1"))
	al.GetConfig().Messages.ToolLimit = "Parei no limite de etapas."
	resp, err := al.ProcessDirect(context.Background(), "lê o README", "worded-tool-limit")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "Parei no limite de etapas." {
		t.Errorf("tool-limit reply = %q, want the worded text", resp)
	}

	al, _ = newScriptedLoop(t, providers.LLMResponse{}, providers.LLMResponse{})
	al.GetConfig().Messages.EmptyResponse = "Não consegui terminar esta resposta."
	resp, err = al.ProcessDirect(context.Background(), "oi", "worded-empty")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "Não consegui terminar esta resposta." {
		t.Errorf("empty-response reply = %q, want the worded text", resp)
	}
	if !al.synthesizedResponse(resp) || !al.synthesizedResponse(defaultResponse) {
		t.Error("the delivery mirror would copy a synthesized text into a chat conversation")
	}
}
