package evolution_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/evolution"
)

// A partial append whose target skill does not exist becomes a new skill
// document; its Expected Result must not be the source task's answer.
func TestRuntime_PartialDraftForAMissingSkillDoesNotCopyTheSourceAnswer(t *testing.T) {
	root := t.TempDir()
	store := evolution.NewStore(evolution.NewPaths(root, ""))
	answer := "Seu saldo hoje é R$ 2.345,67 na conta final 0042."
	success := true
	if err := store.SaveTaskRecords([]evolution.LearningRecord{{
		ID:          "task-saldo",
		Kind:        evolution.RecordKindTask,
		WorkspaceID: root,
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
		Summary:     "qual o saldo da minha conta",
		FinalOutput: answer,
		Status:      evolution.RecordStatus("clustered"),
		Success:     &success,
	}}); err != nil {
		t.Fatalf("SaveTaskRecords: %v", err)
	}
	if err := store.SavePatternRecords([]evolution.LearningRecord{{
		ID:            "pattern-saldo",
		Kind:          evolution.RecordKindPattern,
		WorkspaceID:   root,
		CreatedAt:     time.Unix(1700000001, 0).UTC(),
		Summary:       "qual o saldo da minha conta",
		Status:        evolution.RecordStatus("ready"),
		TaskRecordIDs: []string{"task-saldo"},
	}}); err != nil {
		t.Fatalf("SavePatternRecords: %v", err)
	}

	rt, err := evolution.NewRuntime(evolution.RuntimeOptions{
		Config: config.EvolutionConfig{Enabled: true, Mode: "draft"},
		Now:    func() time.Time { return time.Unix(1700001000, 0).UTC() },
		Store:  store,
		DraftGenerator: stubDraftGenerator{draft: evolution.SkillDraft{
			ID:              "draft-saldo",
			WorkspaceID:     root,
			SourceRecordID:  "pattern-saldo",
			TargetSkillName: "consultar-saldo",
			DraftType:       evolution.DraftTypeShortcut,
			ChangeKind:      evolution.ChangeKindAppend,
			HumanSummary:    "consultar o saldo da conta",
			BodyOrPatch:     "- Abra o extrato do banco conectado e leia o saldo disponível.",
		}},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if runErr := rt.RunColdPathOnce(context.Background(), root); runErr != nil {
		t.Fatalf("RunColdPathOnce: %v", runErr)
	}

	drafts, err := store.LoadDrafts()
	if err != nil {
		t.Fatalf("LoadDrafts: %v", err)
	}
	if len(drafts) != 1 || drafts[0].ChangeKind != evolution.ChangeKindCreate ||
		!strings.Contains(drafts[0].BodyOrPatch, "## Expected Result") {
		t.Fatalf("drafts = %+v, want one partial draft rewritten as a new skill document", drafts)
	}
	if strings.Contains(drafts[0].BodyOrPatch, "2.345,67") || strings.Contains(drafts[0].BodyOrPatch, "0042") {
		t.Fatalf("new skill copied the source answer:\n%s", drafts[0].BodyOrPatch)
	}
	if !strings.Contains(drafts[0].BodyOrPatch, "task-saldo") {
		t.Fatalf("new skill lost its evidence id:\n%s", drafts[0].BodyOrPatch)
	}
}
