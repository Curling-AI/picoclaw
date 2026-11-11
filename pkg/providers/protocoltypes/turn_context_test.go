package protocoltypes

import (
	"context"
	"slices"
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

// A task's labels go where its id goes, a move replaces both, and each call
// adds its own role.
func TestTaskLabelsTravelWithTheTask(t *testing.T) {
	turn := WithTaskID(t.Context(), "task-a", TaskAutomation)
	subTurn := WithTaskRole(WithTaskID(turn, TaskIDFromContext(turn), TaskLabelsFromContext(turn)...), RoleSubagent)

	MoveTaskID(turn, "task-b", MessageTaskLabels("grpc")...)

	want := []string{TaskMessage, "channel:web", RoleAgent}
	if got := TaskCallLabels(WithTaskRole(turn, RoleAgent)); !slices.Equal(got, want) {
		t.Fatalf("moved turn labels = %q, want %q", got, want)
	}
	if got, wantSub := TaskCallLabels(subTurn), []string{TaskAutomation, RoleSubagent}; !slices.Equal(got, wantSub) {
		t.Fatalf("sub-turn labels = %q, want %q", got, wantSub)
	}
	if got := TaskCallLabels(t.Context()); len(got) != 0 {
		t.Fatalf("a context without a task got labels %q", got)
	}
	if id, labels := TaskCallFromContext(WithTaskRole(turn, RoleAgent)); id != "task-b" || !slices.Equal(labels, want) {
		t.Fatalf("one read of the moved turn = %q %q, want task-b %q", id, labels, want)
	}
}

// Only the vocabulary counts as a label, and a channel outside it goes unlabeled.
func TestTaskLabelVocabulary(t *testing.T) {
	if got := MessageTaskLabels("feishu"); !slices.Equal(got, []string{TaskMessage}) {
		t.Fatalf("labels for an unknown channel = %q, want only the message", got)
	}
	for _, label := range append(MessageTaskLabels("whatsapp"), TaskReaction, RoleImage) {
		if !IsTaskLabel(label) {
			t.Fatalf("%q left out of the vocabulary", label)
		}
	}
	for _, label := range []string{"task:other", "channel:feishu", "channel:grpc", "role:agent "} {
		if IsTaskLabel(label) {
			t.Fatalf("%q taken as a label", label)
		}
	}
}
