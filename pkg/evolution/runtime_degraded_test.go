package evolution_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/evolution"
	"github.com/sipeed/picoclaw/pkg/skills"
)

// failingClusterer answers every clustering request with err.
type failingClusterer struct{ err error }

func (c failingClusterer) BuildPatterns(
	context.Context,
	string,
	[]evolution.LearningRecord,
	[]evolution.LearningRecord,
) ([]evolution.LearningRecord, []string, error) {
	return nil, nil, c.err
}

func (c failingClusterer) BuildPatternsWithEvidence(
	context.Context,
	string,
	[]evolution.LearningRecord,
	[]evolution.LearningRecord,
	[]evolution.LearningRecord,
	float64,
) ([]evolution.LearningRecord, []string, error) {
	return nil, nil, c.err
}

type countingDraftGenerator struct{ calls int }

func (g *countingDraftGenerator) GenerateDraft(
	context.Context,
	evolution.LearningRecord,
	[]skills.SkillInfo,
) (evolution.SkillDraft, error) {
	g.calls++
	return evolution.SkillDraft{}, nil
}

func readyRule(id, workspace string) evolution.LearningRecord {
	return evolution.LearningRecord{
		ID:          id,
		Kind:        evolution.RecordKindRule,
		WorkspaceID: workspace,
		CreatedAt:   time.Unix(1700000000, 0).UTC(),
		Summary:     id + " path",
		Status:      evolution.RecordStatus("ready"),
		EventCount:  4,
	}
}

// Without credit the model steps stop, but nothing else in the run needs a
// model: a draft that is already a candidate still gets applied and idle skills
// still cool. Before, a 429 fell back to the heuristics and the run reached
// that tail anyway.
func TestRuntime_RunColdPathOnce_NoCreditStillAppliesReadyDraftAndCoolsIdleSkills(t *testing.T) {
	root := t.TempDir()
	paths := evolution.NewPaths(root, "")
	store := evolution.NewStore(paths)
	now := time.Unix(1700001000, 0).UTC()

	seedTaskBacklog(t, store, root, 0, 2)
	rules := []evolution.LearningRecord{readyRule("rule-drafted", root), readyRule("rule-undrafted", root)}
	if err := store.AppendLearningRecords(rules); err != nil {
		t.Fatalf("AppendLearningRecords: %v", err)
	}
	body := "---\nname: weather\ndescription: weather helper\n---\n" +
		"# Weather\n## Start Here\nUse native-name query first.\n"
	draft := evolution.SkillDraft{
		ID:              "draft-1",
		WorkspaceID:     root,
		SourceRecordID:  "rule-drafted",
		TargetSkillName: "weather",
		DraftType:       evolution.DraftTypeShortcut,
		ChangeKind:      evolution.ChangeKindCreate,
		HumanSummary:    "weather helper",
		BodyOrPatch:     body,
		Status:          evolution.DraftStatusCandidate,
	}
	if err := store.SaveDrafts([]evolution.SkillDraft{draft}); err != nil {
		t.Fatalf("SaveDrafts: %v", err)
	}
	if err := store.SaveProfile(evolution.SkillProfile{
		SkillName:      "idle-skill",
		WorkspaceID:    root,
		Status:         evolution.SkillStatusActive,
		Origin:         "evolved",
		HumanSummary:   "idle skill",
		LastUsedAt:     now.Add(-91 * 24 * time.Hour),
		RetentionScore: 0.1,
	}); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}

	generator := &countingDraftGenerator{}
	rt, err := evolution.NewRuntime(evolution.RuntimeOptions{
		Config:           config.EvolutionConfig{Enabled: true, Mode: "apply"},
		Now:              func() time.Time { return now },
		Store:            store,
		Applier:          evolution.NewApplier(paths, func() time.Time { return now }),
		SuccessJudge:     &stubSuccessJudge{},
		PatternClusterer: failingClusterer{err: fmt.Errorf("cluster: %w", evolution.ErrNoCredit)},
		DraftGenerator:   generator,
		SkillsRecaller:   evolution.NewSkillsRecaller(root),
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	runErr := rt.RunColdPathOnce(context.Background(), root)
	if !errors.Is(runErr, evolution.ErrNoCredit) {
		t.Fatalf("RunColdPathOnce err = %v, want ErrNoCredit so the runner pauses", runErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "skills", "weather", "SKILL.md")); statErr != nil {
		t.Errorf("ready candidate draft was not applied: %v", statErr)
	}
	if generator.calls != 0 {
		t.Errorf("draft generator called %d times without credit", generator.calls)
	}
	profile, err := store.LoadProfile("idle-skill")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if profile.Status != evolution.SkillStatusCold {
		t.Errorf("idle skill status = %q, want %q", profile.Status, evolution.SkillStatusCold)
	}
}

// Close() cancels the run on every pod sleep and config reload. A canceled run
// must not leave heuristic verdicts behind as if the model had judged them.
func TestRuntime_RunColdPathOnce_CanceledRunJudgesNothing(t *testing.T) {
	root := t.TempDir()
	store := evolution.NewStore(evolution.NewPaths(root, ""))
	seedTaskBacklog(t, store, root, 0, 3)

	judge := &stubSuccessJudge{}
	clusterer := &capturingClusterer{}
	rt := newBacklogRuntime(t, root, store, judge, clusterer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.RunColdPathOnce(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunColdPathOnce err = %v, want context.Canceled", err)
	}
	if len(judge.calls) != 0 || len(clusterer.runs) != 0 {
		t.Errorf("canceled run judged %d and clustered %d times", len(judge.calls), len(clusterer.runs))
	}
}

func TestLLMSteps_CanceledRunIsReportedButACallTimeoutStillFallsBack(t *testing.T) {
	ok := true
	record := evolution.LearningRecord{ID: "task-1", Summary: "s", FinalOutput: "done", Success: &ok}
	tasks := make([]evolution.LearningRecord, 0, 3)
	for _, id := range []string{"a", "b", "c"} {
		tasks = append(tasks, evolution.LearningRecord{
			ID:          id,
			Kind:        evolution.RecordKindTask,
			WorkspaceID: "ws",
			Summary:     "check weather",
			FinalOutput: "sunny",
			Success:     &ok,
		})
	}
	rule := readyRule("rule-1", "ws")
	fallbackDraft := stubDraftGenerator{draft: evolution.SkillDraft{TargetSkillName: "weather"}}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name         string
		ctx          context.Context
		err          error
		wantCanceled bool
	}{
		{"run canceled", canceled, context.Canceled, true},
		// A per-call timeout is the step's own deadline, not the run's: keep the
		// fallback it always had.
		{"call timed out", context.Background(), context.DeadlineExceeded, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			provider := &erroringProvider{err: c.err}

			judge := evolution.NewLLMTaskSuccessJudge(provider, "m", &evolution.HeuristicSuccessJudge{})
			_, judgeErr := judge.JudgeTaskRecord(c.ctx, record)

			clusterer := evolution.NewLLMPatternClusterer(provider, "m", nil, 3, nil)
			patterns, _, clusterErr := clusterer.BuildPatternsWithEvidence(c.ctx, "ws", tasks, tasks, nil, 0)

			generator := evolution.NewLLMDraftGenerator(provider, "m", fallbackDraft)
			draft, draftErr := generator.GenerateDraft(c.ctx, rule, nil)

			if c.wantCanceled {
				for step, err := range map[string]error{"judge": judgeErr, "cluster": clusterErr, "draft": draftErr} {
					if !errors.Is(err, context.Canceled) {
						t.Errorf("%s err = %v, want context.Canceled", step, err)
					}
				}
				if len(patterns) != 0 || draft.TargetSkillName != "" {
					t.Errorf("canceled run produced patterns=%d draft=%q", len(patterns), draft.TargetSkillName)
				}
				return
			}
			if judgeErr != nil || clusterErr != nil || draftErr != nil {
				t.Errorf("errs judge=%v cluster=%v draft=%v, want the heuristic fallbacks",
					judgeErr, clusterErr, draftErr)
			}
			if len(patterns) != 1 || !strings.EqualFold(draft.TargetSkillName, "weather") {
				t.Errorf("fallbacks produced patterns=%d draft=%q", len(patterns), draft.TargetSkillName)
			}
		})
	}
}

func TestStore_MarkTaskRecordsExpiredLeavesProcessedRecords(t *testing.T) {
	root := t.TempDir()
	store := evolution.NewStore(evolution.NewPaths(root, ""))
	statuses := map[string]evolution.RecordStatus{
		"task-clustered": "clustered",
		"task-rejected":  "rejected",
		"task-new":       "new",
		"task-legacy":    "",
	}
	records := make([]evolution.LearningRecord, 0, len(statuses))
	ids := make([]string, 0, len(statuses))
	for id, status := range statuses {
		records = append(records, evolution.LearningRecord{
			ID: id, Kind: evolution.RecordKindTask, WorkspaceID: root, Summary: id, Status: status,
		})
		ids = append(ids, id)
	}
	if err := store.AppendLearningRecords(records); err != nil {
		t.Fatalf("AppendLearningRecords: %v", err)
	}

	expired, err := store.MarkTaskRecordsExpired(ids)
	if err != nil {
		t.Fatalf("MarkTaskRecordsExpired: %v", err)
	}
	if expired != 2 {
		t.Errorf("expired = %d, want 2 (only the unprocessed records)", expired)
	}
	stored, err := store.LoadTaskRecords()
	if err != nil {
		t.Fatalf("LoadTaskRecords: %v", err)
	}
	want := map[string]evolution.RecordStatus{
		"task-clustered": "clustered",
		"task-rejected":  "rejected",
		"task-new":       "expired",
		"task-legacy":    "expired",
	}
	for _, record := range stored {
		if record.Status != want[record.ID] {
			t.Errorf("%s status = %q, want %q", record.ID, record.Status, want[record.ID])
		}
	}
}
