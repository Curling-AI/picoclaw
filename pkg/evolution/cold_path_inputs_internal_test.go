package evolution

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type stubJudge struct {
	verdict bool
	err     error
	calls   int
}

func (j *stubJudge) JudgeTaskRecord(context.Context, LearningRecord) (TaskSuccessDecision, error) {
	j.calls++
	if j.err != nil {
		return TaskSuccessDecision{}, j.err
	}
	return TaskSuccessDecision{Success: j.verdict}, nil
}

func ptr(b bool) *bool { return &b }

// admissible is a record that clears every cold-path filter. Cases below break
// exactly one field, so a filter that stops working shows up as one failure.
func admissible(id string) LearningRecord {
	return LearningRecord{
		ID:          id,
		Kind:        RecordKindTask,
		WorkspaceID: "ws",
		Status:      RecordStatus("new"),
		Summary:     "did the thing",
		FinalOutput: "done",
		Success:     ptr(true),
	}
}

func ids(records []LearningRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, r.ID)
	}
	return out
}

func TestRecordsForColdPathInputsKeepsOnlyWhatEachFilterAllows(t *testing.T) {
	notATask := admissible("not-a-task")
	notATask.Kind = RecordKindPattern
	otherWorkspace := admissible("other-workspace")
	otherWorkspace.WorkspaceID = "other-ws"
	noVerdict := admissible("no-verdict")
	noVerdict.Success = nil
	alreadyProcessed := admissible("already-processed")
	alreadyProcessed.Status = RecordStatus("clustered")
	heartbeat := admissible("heartbeat")
	heartbeat.SessionKey = "heartbeat"
	noSummary := admissible("no-summary")
	noSummary.Summary = "   "
	noFinalOutput := admissible("no-final-output")
	noFinalOutput.FinalOutput = ""
	failed := admissible("failed")
	failed.Success = ptr(false)

	input := []LearningRecord{
		notATask, otherWorkspace, noVerdict, alreadyProcessed,
		heartbeat, noSummary, noFinalOutput, failed, admissible("ok"),
	}

	rt := &Runtime{}
	rt.cfg.StateDir = t.TempDir()
	admitted, evidence, err := rt.recordsForColdPathInputs(context.Background(), "ws", input)
	if err != nil {
		t.Fatalf("recordsForColdPathInputs: %v", err)
	}

	// Evidence keeps the failed record; admitted does not.
	if got, want := ids(evidence), []string{"failed", "ok"}; !equalStrings(got, want) {
		t.Errorf("evidence = %v, want %v", got, want)
	}
	if got, want := ids(admitted), []string{"ok"}; !equalStrings(got, want) {
		t.Errorf("admitted = %v, quer %v", got, want)
	}
}

func TestRecordsForColdPathInputsJudgesEachRecordAtMostOnce(t *testing.T) {
	alreadyJudged := admissible("already-judged")
	alreadyJudged.SuccessJudged = true

	judge := &stubJudge{verdict: true}
	rt := &Runtime{successJudge: judge}
	rt.cfg.StateDir = t.TempDir()

	_, _, err := rt.recordsForColdPathInputs(
		context.Background(), "ws", []LearningRecord{alreadyJudged, admissible("fresh")})
	if err != nil {
		t.Fatalf("recordsForColdPathInputs: %v", err)
	}
	if judge.calls != 1 {
		t.Errorf("judge called %d time(s), want 1 — only the unjudged one", judge.calls)
	}
}

func TestRecordsForColdPathInputsDropsWhatTheJudgeRejects(t *testing.T) {
	judge := &stubJudge{verdict: false}
	rt := &Runtime{successJudge: judge}
	rt.cfg.StateDir = t.TempDir()

	admitted, evidence, err := rt.recordsForColdPathInputs(
		context.Background(), "ws", []LearningRecord{admissible("rejected")})
	if err != nil {
		t.Fatalf("recordsForColdPathInputs: %v", err)
	}
	if len(admitted) != 0 {
		t.Errorf("admitted = %v, quer vazio", ids(admitted))
	}
	if len(evidence) != 1 {
		t.Fatalf("evidence = %v, want 1 — a rejected record is still evidence", ids(evidence))
	}
	if evidence[0].Success == nil || *evidence[0].Success {
		t.Error("the judge's verdict was not written onto the evidence")
	}
}

func TestRecordsForColdPathInputsPropagatesJudgeFailure(t *testing.T) {
	judgeDown := errors.New("judge unavailable")
	rt := &Runtime{successJudge: &stubJudge{err: judgeDown}}
	rt.cfg.StateDir = t.TempDir()

	if _, _, err := rt.recordsForColdPathInputs(
		context.Background(), "ws", []LearningRecord{admissible("x")}); !errors.Is(err, judgeDown) {
		t.Errorf("err = %v, want %v", err, judgeDown)
	}
}

func TestRecordsForColdPathInputsReturnsEmptyNotNil(t *testing.T) {
	rt := &Runtime{}
	rt.cfg.StateDir = t.TempDir()
	admitted, evidence, err := rt.recordsForColdPathInputs(context.Background(), "ws", nil)
	if err != nil {
		t.Fatalf("recordsForColdPathInputs: %v", err)
	}
	if admitted == nil || evidence == nil {
		t.Error("returned nil; the previous contract returned a non-nil empty slice")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The store holds every record ever written, and the cold path admits a handful.
// This is the shape that mattered in production: 105k records on disk, a few
// hundred surviving the filters.
func BenchmarkRecordsForColdPathInputs(b *testing.B) {
	const total = 100_000
	input := make([]LearningRecord, 0, total)
	for i := range total {
		if i%500 == 0 {
			input = append(input, admissible(fmt.Sprintf("kept-%d", i)))
			continue
		}
		other := admissible(fmt.Sprintf("dropped-%d", i))
		other.WorkspaceID = "other-ws"
		input = append(input, other)
	}
	rt := &Runtime{}
	rt.cfg.StateDir = b.TempDir()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, _, err := rt.recordsForColdPathInputs(ctx, "ws", input); err != nil {
			b.Fatal(err)
		}
	}
}
