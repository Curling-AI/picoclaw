package evolution

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tokenizer"
)

// clusterPromptTokenBudget is measured with the estimator the agent uses for
// its own context budget (2.5 characters per token), not the model's
// tokenizer, which the repo does not have. It holds for Latin-script text;
// CJK costs closer to a token per character. What the test guarantees is the
// shape: the prompt stops following the size of the history. Whether the real
// calls land under ~10k tokens, where production hit the 32k output cap 0.4%
// of the time (82% at 80–160k), is checked on usage_events input_tokens after
// deploy.
const clusterPromptTokenBudget = 12_000

func TestBuildPatternClusterPromptStaysWithinBudgetAtFullWindow(t *testing.T) {
	// Every free-text field oversized, as a legacy or foreign record could be;
	// ids keep the shape buildTaskRecordID gives them.
	tasks := make([]LearningRecord, 0, ColdPathTaskWindow)
	for i := 0; i < ColdPathTaskWindow; i++ {
		tasks = append(tasks, LearningRecord{
			ID:          fmt.Sprintf("main-turn-%d-0123456789ab", i),
			Kind:        RecordKindTask,
			WorkspaceID: "ws",
			Summary:     strings.Repeat("é", 5000),
			FinalOutput: strings.Repeat("ã", 5000),
		})
	}
	base := time.Unix(1700000000, 0).UTC()
	existing := make([]LearningRecord, 0, 200)
	for i := 0; i < 200; i++ {
		existing = append(existing, LearningRecord{
			ID:          fmt.Sprintf("pattern-%03d", i),
			Kind:        RecordKindPattern,
			WorkspaceID: "ws",
			CreatedAt:   base.Add(time.Duration(i) * time.Hour),
			Label:       fmt.Sprintf("pattern-label-%03d", i),
			Summary:     strings.Repeat("ç", 1000),
		})
	}

	prompt := buildPatternClusterPrompt("ws", tasks, existing)
	estimate := tokenizer.EstimateMessageTokens(providers.Message{Role: "user", Content: prompt})
	if estimate > clusterPromptTokenBudget {
		t.Fatalf("prompt ≈ %d tokens (%d chars), want <= %d",
			estimate, utf8.RuneCountInString(prompt), clusterPromptTokenBudget)
	}

	var payload struct {
		ExistingPatterns []struct {
			Label   string `json:"label"`
			Summary string `json:"summary"`
		} `json:"existing_patterns"`
		Tasks []struct {
			FinalOutputExcerpt string `json:"final_output_excerpt"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(prompt), &payload); err != nil {
		t.Fatalf("prompt is not the JSON payload: %v", err)
	}
	if len(payload.Tasks) != ColdPathTaskWindow {
		t.Errorf("tasks = %d, want %d", len(payload.Tasks), ColdPathTaskWindow)
	}
	if len(payload.ExistingPatterns) != clusterPromptPatternLimit {
		t.Fatalf("existing patterns = %d, want %d", len(payload.ExistingPatterns), clusterPromptPatternLimit)
	}
	// The most recent patterns are the ones a new task is likeliest to repeat.
	for _, pattern := range payload.ExistingPatterns {
		var n int
		_, err := fmt.Sscanf(pattern.Label, "pattern-label-%d", &n)
		if err != nil || n < 200-clusterPromptPatternLimit {
			t.Errorf("pattern %q sent, want only the %d most recent", pattern.Label, clusterPromptPatternLimit)
		}
	}
}

func TestBuildPatternClusterPromptKeepsShortContentWhole(t *testing.T) {
	tasks := []LearningRecord{{ID: "t1", WorkspaceID: "ws", Summary: "check weather", FinalOutput: "sunny, 26C"}}
	prompt := buildPatternClusterPrompt("ws", tasks, nil)
	if !strings.Contains(prompt, `"final_output_excerpt": "sunny, 26C"`) {
		t.Errorf("short output was altered: %s", prompt)
	}
	excerpt := summarizeText(strings.Repeat("é", 1000), clusterPromptExcerptRunes)
	if utf8.RuneCountInString(excerpt) != clusterPromptExcerptRunes {
		t.Errorf("excerpt is not cut at %d runes", clusterPromptExcerptRunes)
	}
}
