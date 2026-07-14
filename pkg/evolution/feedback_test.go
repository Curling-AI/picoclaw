package evolution

import (
	"context"
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestDecayRetentionScore(t *testing.T) {
	if got := decayRetentionScore(0.8); !approx(got, 0.6) {
		t.Fatalf("decay(0.8) = %v, want 0.6", got)
	}
	if got := decayRetentionScore(0.1); !approx(got, 0) {
		t.Fatalf("decay(0.1) = %v, want 0 (floored)", got)
	}
}

func TestMarkTaskRecordUnsuccessful(t *testing.T) {
	ws := t.TempDir()
	store := NewStore(NewPaths(ws, ""))
	writer := NewCaseWriter(NewPaths(ws, ""))
	yes := true
	rec := LearningRecord{
		ID: "r1", Kind: RecordKindTask, WorkspaceID: ws, SessionKey: "sess-1",
		FinalOutput: "Here is the extracted PDF text you asked for.", Status: "new",
		Success: &yes, UsedSkillNames: []string{"pdf-skill"},
	}
	if err := writer.AppendCase(context.Background(), rec); err != nil {
		t.Fatalf("AppendCase: %v", err)
	}

	// Excerpt (normalized) matches FinalOutput → record flipped, skill returned.
	skills, err := store.MarkTaskRecordUnsuccessful("sess-1", "extracted PDF text")
	if err != nil {
		t.Fatalf("MarkTaskRecordUnsuccessful: %v", err)
	}
	if len(skills) != 1 || skills[0] != "pdf-skill" {
		t.Fatalf("returned skills = %v, want [pdf-skill]", skills)
	}
	got, _ := store.LoadTaskRecords()
	if len(got) != 1 || got[0].Success == nil || *got[0].Success != false || got[0].Status != negativeFeedbackStatus {
		t.Fatalf("record not marked failed: %+v", got)
	}

	// Wrong session → no match.
	if s, _ := store.MarkTaskRecordUnsuccessful("other", "extracted PDF text"); len(s) != 0 {
		t.Fatalf("wrong session should not match, got %v", s)
	}
}

func TestApplyNegativeFeedbackDecaysRetention(t *testing.T) {
	ws := t.TempDir()
	store := NewStore(NewPaths(ws, ""))
	writer := NewCaseWriter(NewPaths(ws, ""))
	yes := true
	_ = writer.AppendCase(context.Background(), LearningRecord{
		ID: "r1", Kind: RecordKindTask, WorkspaceID: ws, SessionKey: "sess-1",
		FinalOutput: "answer", Status: "new", Success: &yes,
		UsedSkillNames: []string{"myskill"},
	})
	// Seed a profile for the skill under the workspace scope.
	if err := store.UpdateProfile(ws, "myskill", func(p *SkillProfile, _ bool) error {
		p.SkillName = "myskill"
		p.WorkspaceID = ws
		p.RetentionScore = 0.8
		return nil
	}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	if err := store.ApplyNegativeFeedback(ws, "sess-1", ""); err != nil {
		t.Fatalf("ApplyNegativeFeedback: %v", err)
	}

	var got float64
	_ = store.UpdateProfile(ws, "myskill", func(p *SkillProfile, _ bool) error {
		got = p.RetentionScore
		return nil
	})
	if !approx(got, 0.6) {
		t.Fatalf("retention after downvote = %v, want 0.6", got)
	}
}
