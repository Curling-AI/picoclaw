package tools

import (
	"context"
	"fmt"
	"testing"
)

// stepSpawner is a sub-turn that runs steps, the last one failing with err.
type stepSpawner struct {
	steps []SubTurnStep
	err   error
}

func (s *stepSpawner) SpawnSubTurn(_ context.Context, cfg SubTurnConfig) (*ToolResult, error) {
	for i, step := range s.steps {
		cfg.OnStep(step)(i == len(s.steps)-1 && s.err != nil)
	}
	if s.err != nil {
		return nil, s.err
	}
	return &ToolResult{ForLLM: "done"}, nil
}

// A synchronous subagent is a task like a spawn: found by the call that
// started it, with its steps and how it ended.
func TestSubagentToolTracksItsTaskAndSteps(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		status     string
		stopReason string
		lastStep   string
	}{
		{"completed", nil, "completed", "", "done"},
		{"time limit", fmt.Errorf("%w (20 min, after 9 iterations)", ErrSubagentTimeLimit), "failed", "time_limit", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := NewSubagentManager(&MockLLMProvider{}, "test-model", t.TempDir())
			tool := NewSubagentTool(manager)
			tool.SetSpawner(&stepSpawner{
				steps: []SubTurnStep{{Name: "exec", Arguments: `{"command":"ls"}`}, {Name: "read_file"}},
				err:   tc.err,
			})
			ctx := WithToolCallID(WithToolContext(t.Context(), "grpc", "chat-1"), "call-7")
			tool.Execute(ctx, map[string]any{"task": "list the files", "label": "Listar"})

			tasks := manager.ListTaskCopies()
			if len(tasks) != 1 {
				t.Fatalf("tasks = %d, want 1", len(tasks))
			}
			got := tasks[0]
			if got.ToolCallID != "call-7" || got.OriginChannel != "grpc" || got.Label != "Listar" {
				t.Fatalf("task = %+v, want it under call-7 on grpc", got)
			}
			if got.Status != tc.status || got.StopReason != tc.stopReason || got.Finished == 0 {
				t.Fatalf("status %q, stop %q, finished %d; want %q, %q, set",
					got.Status, got.StopReason, got.Finished, tc.status, tc.stopReason)
			}
			if got.StepCount != 2 || got.Steps[0].Name != "exec" || got.Steps[0].Arguments != `{"command":"ls"}` ||
				got.Steps[0].Status != "done" || got.Steps[1].Status != tc.lastStep {
				t.Fatalf("steps = %+v (count %d)", got.Steps, got.StepCount)
			}
		})
	}
}

// A task keeps its latest steps and counts all of them; a step trimmed away or
// cut by the end of the task changes nothing when it ends late.
func TestSubagentStepsKeepTheLatest(t *testing.T) {
	manager := NewSubagentManager(&MockLLMProvider{}, "test-model", t.TempDir())
	taskID, onStep := manager.trackCall(WithToolCallID(t.Context(), "call-1"), "task", "", "")
	first := onStep(SubTurnStep{Name: "s0"})
	var last func(bool)
	for i := 1; i < maxSubagentSteps+5; i++ {
		last = onStep(SubTurnStep{Name: fmt.Sprintf("s%d", i)})
		if i < maxSubagentSteps+4 {
			last(false)
		}
	}
	first(false)

	got, _ := manager.GetTaskCopy(taskID)
	if got.StepCount != maxSubagentSteps+5 || len(got.Steps) != maxSubagentSteps || got.Steps[0].Name != "s5" {
		t.Fatalf("count %d, kept %d from %q; want %d, %d from s5",
			got.StepCount, len(got.Steps), got.Steps[0].Name, maxSubagentSteps+5, maxSubagentSteps)
	}
	if got.Steps[len(got.Steps)-1].Status != "running" {
		t.Fatalf("open step = %q, want running", got.Steps[len(got.Steps)-1].Status)
	}

	manager.finishCall(taskID, &ToolResult{ForLLM: "stopped", IsError: true}, nil)
	last(false)
	got, _ = manager.GetTaskCopy(taskID)
	if step := got.Steps[len(got.Steps)-1]; step.Status != "canceled" || got.Status != "failed" {
		t.Fatalf("after the end: step %q, task %q; want canceled, failed", step.Status, got.Status)
	}
}
