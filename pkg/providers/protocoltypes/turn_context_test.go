package protocoltypes

import (
	"context"
	"testing"
)

// A move reaches the contexts that share the holder and no other: a sub-turn
// keeps the task it started on.
func TestMoveTaskIDStaysInItsHolder(t *testing.T) {
	turn := WithTaskID(t.Context(), "task-a")
	derived, cancel := context.WithCancel(turn)
	defer cancel()
	subTurn := WithTaskID(turn, TaskIDFromContext(turn))

	MoveTaskID(turn, "task-b")

	if got := TaskIDFromContext(derived); got != "task-b" {
		t.Fatalf("derived context task = %q, want the moved one", got)
	}
	if got := TaskIDFromContext(subTurn); got != "task-a" {
		t.Fatalf("sub-turn task = %q, want the one it started on", got)
	}
	MoveTaskID(t.Context(), "task-c")
	if got := TaskIDFromContext(t.Context()); got != "" {
		t.Fatalf("a context without a task got %q", got)
	}
}
