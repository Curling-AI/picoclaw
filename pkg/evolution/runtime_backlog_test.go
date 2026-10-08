package evolution_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/evolution"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/common"
)

// capturingClusterer stands in for the LLM clusterer and never forms a pattern:
// the outcome that, before the window, left every record "new" and made each
// run resend the whole history.
type capturingClusterer struct {
	runs [][]string
}

func (c *capturingClusterer) BuildPatterns(
	_ context.Context,
	_ string,
	tasks []evolution.LearningRecord,
	_ []evolution.LearningRecord,
) ([]evolution.LearningRecord, []string, error) {
	c.runs = append(c.runs, recordIDs(tasks))
	return nil, nil, nil
}

func (c *capturingClusterer) BuildPatternsWithEvidence(
	_ context.Context,
	_ string,
	_ []evolution.LearningRecord,
	evidence []evolution.LearningRecord,
	_ []evolution.LearningRecord,
	_ float64,
) ([]evolution.LearningRecord, []string, error) {
	c.runs = append(c.runs, recordIDs(evidence))
	return nil, nil, nil
}

type noCreditJudge struct{ calls int }

func (j *noCreditJudge) JudgeTaskRecord(
	_ context.Context,
	_ evolution.LearningRecord,
) (evolution.TaskSuccessDecision, error) {
	j.calls++
	return evolution.TaskSuccessDecision{}, fmt.Errorf("judge: %w", evolution.ErrNoCredit)
}

type erroringProvider struct {
	err   error
	calls int
}

func (p *erroringProvider) Chat(
	context.Context,
	[]providers.Message,
	[]providers.ToolDefinition,
	string,
	map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	return nil, p.err
}

func (p *erroringProvider) GetDefaultModel() string { return "test-model" }

// errGatewayNoCredit is what the hulk gateway answers when the account is out of
// credit; errGatewayDown is an ordinary failure that must keep the old fallback.
var (
	errGatewayNoCredit = &common.HTTPError{StatusCode: http.StatusTooManyRequests, ErrorCode: "insufficient_balance"}
	errGatewayDown     = &common.HTTPError{StatusCode: http.StatusBadGateway, BodyPreview: "upstream unavailable"}
)

func recordIDs(records []evolution.LearningRecord) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.ID)
	}
	return out
}

func backlogTaskID(i int) string { return fmt.Sprintf("task-%04d", i) }

// seedTaskBacklog appends successful, unclustered task records, one minute
// apart, with ids task-<from>..task-<from+n-1>. Production stores reach
// thousands of these: one per turn, never drained.
func seedTaskBacklog(t *testing.T, store *evolution.Store, workspace string, from, n int) {
	t.Helper()
	ok := true
	base := time.Unix(1700000000, 0).UTC()
	records := make([]evolution.LearningRecord, 0, n)
	for i := from; i < from+n; i++ {
		records = append(records, evolution.LearningRecord{
			ID:          backlogTaskID(i),
			Kind:        evolution.RecordKindTask,
			WorkspaceID: workspace,
			CreatedAt:   base.Add(time.Duration(i) * time.Minute),
			Summary:     fmt.Sprintf("distinct task number %d", i),
			FinalOutput: "done",
			Status:      evolution.RecordStatus("new"),
			Success:     &ok,
		})
	}
	if err := store.AppendLearningRecords(records); err != nil {
		t.Fatalf("AppendLearningRecords: %v", err)
	}
}

func taskStatusCounts(t *testing.T, store *evolution.Store) map[evolution.RecordStatus]int {
	t.Helper()
	records, err := store.LoadTaskRecords()
	if err != nil {
		t.Fatalf("LoadTaskRecords: %v", err)
	}
	counts := make(map[evolution.RecordStatus]int)
	for _, record := range records {
		counts[record.Status]++
	}
	return counts
}

func newBacklogRuntime(
	t *testing.T,
	workspace string,
	store *evolution.Store,
	judge evolution.SuccessJudge,
	clusterer evolution.PatternClusterer,
) *evolution.Runtime {
	t.Helper()
	rt, err := evolution.NewRuntime(evolution.RuntimeOptions{
		Config:           config.EvolutionConfig{Enabled: true, Mode: "draft", MinTaskCount: 3},
		Store:            store,
		SuccessJudge:     judge,
		PatternClusterer: clusterer,
		SkillsRecaller:   evolution.NewSkillsRecaller(workspace),
		DraftGenerator:   stubDraftGenerator{},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt
}

func TestRuntime_RunColdPathOnce_LegacyBacklogReachesOnlyTheWindow(t *testing.T) {
	root := t.TempDir()
	store := evolution.NewStore(evolution.NewPaths(root, ""))
	const backlog = 500
	seedTaskBacklog(t, store, root, 0, backlog)

	judge := &stubSuccessJudge{}
	clusterer := &capturingClusterer{}
	rt := newBacklogRuntime(t, root, store, judge, clusterer)

	if err := rt.RunColdPathOnce(context.Background(), root); err != nil {
		t.Fatalf("RunColdPathOnce: %v", err)
	}

	window := evolution.ColdPathTaskWindow
	if len(clusterer.runs) != 1 {
		t.Fatalf("clusterer runs = %d, want 1", len(clusterer.runs))
	}
	sent := append([]string(nil), clusterer.runs[0]...)
	sort.Strings(sent)
	if len(sent) != window {
		t.Fatalf("clusterer received %d records, want the window of %d", len(sent), window)
	}
	if oldest := sent[0]; oldest != backlogTaskID(backlog-window) {
		t.Errorf("oldest record sent = %s, want %s (only the most recent belong to the window)",
			oldest, backlogTaskID(backlog-window))
	}
	if len(judge.calls) != window {
		t.Errorf("judge calls = %d, want %d: records outside the window must not be judged", len(judge.calls), window)
	}

	counts := taskStatusCounts(t, store)
	if counts["new"] != window || counts["expired"] != backlog-window {
		t.Errorf("statuses = %v, want new=%d expired=%d", counts, window, backlog-window)
	}
}

func TestRuntime_RunColdPathOnce_BacklogDrainsWhileClusteringFormsNothing(t *testing.T) {
	root := t.TempDir()
	store := evolution.NewStore(evolution.NewPaths(root, ""))
	window := evolution.ColdPathTaskWindow
	seedTaskBacklog(t, store, root, 0, window+10)

	judge := &stubSuccessJudge{}
	clusterer := &capturingClusterer{}
	rt := newBacklogRuntime(t, root, store, judge, clusterer)

	if err := rt.RunColdPathOnce(context.Background(), root); err != nil {
		t.Fatalf("RunColdPathOnce #1: %v", err)
	}
	const arrived = 5
	seedTaskBacklog(t, store, root, window+10, arrived)
	if err := rt.RunColdPathOnce(context.Background(), root); err != nil {
		t.Fatalf("RunColdPathOnce #2: %v", err)
	}

	if len(clusterer.runs) != 2 {
		t.Fatalf("clusterer runs = %d, want 2", len(clusterer.runs))
	}
	for i, run := range clusterer.runs {
		if len(run) != window {
			t.Errorf("run #%d sent %d records, want %d", i+1, len(run), window)
		}
	}
	second := strings.Join(clusterer.runs[1], ",")
	for i := window + 10; i < window+10+arrived; i++ {
		if !strings.Contains(second, backlogTaskID(i)) {
			t.Errorf("run #2 did not include newly arrived %s", backlogTaskID(i))
		}
	}
	// Each record is judged once, and only while inside the window.
	if got, want := len(judge.calls), window+arrived; got != want {
		t.Errorf("judge calls = %d, want %d", got, want)
	}
	counts := taskStatusCounts(t, store)
	if counts["new"] != window || counts["expired"] != 10+arrived {
		t.Errorf("statuses = %v, want new=%d expired=%d", counts, window, 10+arrived)
	}
}

func TestRuntime_RunColdPathOnce_NoCreditStopsBeforeClusteringAndLeavesRecordsUnjudged(t *testing.T) {
	root := t.TempDir()
	store := evolution.NewStore(evolution.NewPaths(root, ""))
	seedTaskBacklog(t, store, root, 0, 3)

	judge := &noCreditJudge{}
	clusterer := &capturingClusterer{}
	rt := newBacklogRuntime(t, root, store, judge, clusterer)

	err := rt.RunColdPathOnce(context.Background(), root)
	if !errors.Is(err, evolution.ErrNoCredit) {
		t.Fatalf("RunColdPathOnce err = %v, want ErrNoCredit", err)
	}
	if judge.calls != 1 {
		t.Errorf("judge calls = %d, want 1: the run stops at the first no-credit answer", judge.calls)
	}
	if len(clusterer.runs) != 0 {
		t.Errorf("clusterer ran %d times without credit", len(clusterer.runs))
	}
	records, loadErr := store.LoadTaskRecords()
	if loadErr != nil {
		t.Fatalf("LoadTaskRecords: %v", loadErr)
	}
	for _, record := range records {
		if record.SuccessJudged {
			t.Errorf("%s marked judged although no verdict came back", record.ID)
		}
	}
}

func TestLLMTaskSuccessJudge_NoCreditIsReportedNotJudgedHeuristically(t *testing.T) {
	ok := true
	record := evolution.LearningRecord{ID: "task-1", Summary: "s", FinalOutput: "done", Success: &ok}

	judge := evolution.NewLLMTaskSuccessJudge(
		&erroringProvider{err: errGatewayNoCredit},
		"m",
		&evolution.HeuristicSuccessJudge{},
	)
	if _, err := judge.JudgeTaskRecord(context.Background(), record); !errors.Is(err, evolution.ErrNoCredit) {
		t.Fatalf("err = %v, want ErrNoCredit", err)
	}

	// Any other failure keeps the heuristic fallback the judge always had.
	judge = evolution.NewLLMTaskSuccessJudge(
		&erroringProvider{err: errGatewayDown},
		"m",
		&evolution.HeuristicSuccessJudge{},
	)
	decision, err := judge.JudgeTaskRecord(context.Background(), record)
	if err != nil {
		t.Fatalf("err = %v, want heuristic fallback", err)
	}
	if !decision.Success {
		t.Errorf("heuristic fallback rejected a completed record: %+v", decision)
	}
}

func TestLLMPatternClusterer_NoCreditIsReportedInsteadOfFallingBack(t *testing.T) {
	root := "ws"
	ok := true
	tasks := make([]evolution.LearningRecord, 0, 3)
	for i := 0; i < 3; i++ {
		tasks = append(tasks, evolution.LearningRecord{
			ID:          "task-" + strconv.Itoa(i),
			Kind:        evolution.RecordKindTask,
			WorkspaceID: root,
			Summary:     "check weather in shanghai",
			FinalOutput: "sunny",
			Status:      evolution.RecordStatus("new"),
			Success:     &ok,
		})
	}

	clusterer := evolution.NewLLMPatternClusterer(&erroringProvider{err: errGatewayNoCredit}, "m", nil, 3, nil)
	patterns, clustered, err := clusterer.BuildPatternsWithEvidence(context.Background(), root, tasks, tasks, nil, 0)
	if !errors.Is(err, evolution.ErrNoCredit) {
		t.Fatalf("err = %v, want ErrNoCredit", err)
	}
	if len(patterns) != 0 || len(clustered) != 0 {
		t.Errorf("no-credit run produced patterns=%d clustered=%d, want none", len(patterns), len(clustered))
	}

	// Any other failure keeps the heuristic fallback.
	clusterer = evolution.NewLLMPatternClusterer(&erroringProvider{err: errGatewayDown}, "m", nil, 3, nil)
	patterns, _, err = clusterer.BuildPatternsWithEvidence(context.Background(), root, tasks, tasks, nil, 0)
	if err != nil {
		t.Fatalf("err = %v, want heuristic fallback", err)
	}
	if len(patterns) != 1 {
		t.Errorf("heuristic fallback patterns = %d, want 1", len(patterns))
	}
}

func TestRuntime_FinalizeTurn_CronRunTracksSkillUseWithoutLearningRecord(t *testing.T) {
	root := t.TempDir()
	rt, err := evolution.NewRuntime(evolution.RuntimeOptions{
		Config: config.EvolutionConfig{Enabled: true, Mode: "apply"},
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	cronKeys := []string{"agent:cron-job1-6f1c", "agent:cronmodel-job2-9a2b"}
	for _, key := range cronKeys {
		finalizeErr := rt.FinalizeTurn(context.Background(), evolution.TurnCaseInput{
			Workspace:           root,
			TurnID:              "turn-" + key,
			SessionKey:          key,
			AgentID:             "main",
			Status:              "completed",
			UserMessage:         "[Scheduled run of cron job] run the weather report",
			FinalContent:        "sunny",
			FinalSuccessfulPath: []string{"weather"},
		})
		if finalizeErr != nil {
			t.Fatalf("FinalizeTurn(%s): %v", key, finalizeErr)
		}
	}

	store := evolution.NewStore(evolution.NewPaths(root, ""))
	records, err := store.LoadTaskRecords()
	if err != nil {
		t.Fatalf("LoadTaskRecords: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("cron runs wrote %d learning records, want 0", len(records))
	}

	// The skill the cron uses keeps counting as used, otherwise the lifecycle
	// would cool, archive and finally trash a skill that runs every day.
	profile, err := store.LoadProfile("weather")
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if profile.UseCount != len(cronKeys) || profile.LastUsedAt.IsZero() {
		t.Errorf("weather profile use_count=%d last_used=%v, want %d and set",
			profile.UseCount, profile.LastUsedAt, len(cronKeys))
	}
}
