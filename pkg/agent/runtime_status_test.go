package agent

import (
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/tools"
)

func TestLegacyCompactionState_TracksInFlightSummarization(t *testing.T) {
	provider := &alwaysToolCallRecorder{toolName: "search"}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	m := &legacyContextManager{al: al}

	if s := m.CompactionState("sess-1"); s.Active {
		t.Fatal("no compaction registered yet")
	}

	entry := &compactionEntry{startedAt: time.Now(), messages: 42}
	m.summarizing.Store(agent.ID+":sess-1", entry)

	s := m.CompactionState("sess-1")
	if !s.Active || s.Messages != 42 || s.StartedAtMS == 0 {
		t.Fatalf("state = %+v, want active with 42 messages", s)
	}
	if s2 := m.CompactionState("sess-2"); s2.Active {
		t.Fatal("other sessions must not report compaction")
	}

	m.summarizing.Delete(agent.ID + ":sess-1")
	if s := m.CompactionState("sess-1"); s.Active {
		t.Fatal("finished compaction must clear the state")
	}
}

func TestSubagentTasksFor_FiltersByOrigin(t *testing.T) {
	provider := &alwaysToolCallRecorder{toolName: "search"}
	al, agent, cleanup := newTurnCoordTestLoop(t, provider)
	defer cleanup()

	mgr := tools.NewSubagentManager(provider, "m", t.TempDir())
	agent.SubagentMgr = mgr

	id1 := mgr.TrackTask("tarefa A", "a", "main", "grpc", "conv-1")
	mgr.TrackTask("tarefa B", "b", "main", "grpc", "conv-2")
	mgr.TrackTask("tarefa C", "c", "main", "telegram", "conv-1")
	mgr.ResolveTask(id1, "completed", "feito")

	got := al.SubagentTasksFor("grpc", "conv-1")
	if len(got) != 1 || got[0].Label != "a" || got[0].Status != "completed" {
		t.Fatalf("filtered tasks = %+v, want only conv-1/grpc completed", got)
	}
	if all := al.SubagentTasksFor("", ""); len(all) != 3 {
		t.Fatalf("unfiltered = %d, want 3", len(all))
	}
}
