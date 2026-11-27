package evolution_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/evolution"
)

func TestLLMDraftGenerator_FallbackDraftDoesNotCopyTheSourceAnswer(t *testing.T) {
	answer := "Seu saldo hoje é R$ 2.345,67 na conta final 0042."
	// Without a model the heuristic draft is the only one.
	generator := evolution.NewLLMDraftGenerator(nil, "", evolution.NewDefaultDraftGenerator(t.TempDir()))

	draft, err := generator.GenerateDraftWithEvidence(context.Background(), evolution.LearningRecord{
		ID:            "pattern-saldo",
		Label:         "consultar-saldo-da-conta",
		Summary:       "qual o saldo da minha conta",
		TaskRecordIDs: []string{"task-saldo"},
	}, nil, evolution.DraftEvidence{TaskRecords: []evolution.LearningRecord{{
		ID:          "task-saldo",
		Summary:     "qual o saldo da minha conta",
		FinalOutput: answer,
	}}})
	if err != nil {
		t.Fatalf("GenerateDraftWithEvidence: %v", err)
	}
	if strings.TrimSpace(draft.BodyOrPatch) == "" {
		t.Fatal("fallback produced no draft")
	}
	if strings.Contains(draft.BodyOrPatch, "2.345,67") || strings.Contains(draft.BodyOrPatch, "0042") {
		t.Fatalf("fallback draft copied the source answer:\n%s", draft.BodyOrPatch)
	}
	if !strings.Contains(draft.BodyOrPatch, "task-saldo") {
		t.Fatalf("fallback draft lost its evidence id:\n%s", draft.BodyOrPatch)
	}
}
