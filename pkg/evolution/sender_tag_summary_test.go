package evolution_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/evolution"
)

// ownerTag is the shape the Ethos server prepends to every owner message (internal/picoclaw/server.go).
const ownerTag = `[SENDER id="user_test_owner_01" name="Pessoa Teste" channel="web" role="owner"]` + "\n"

func TestRuntime_FinalizeTurnSummaryDropsSenderTag(t *testing.T) {
	workspace := t.TempDir()
	now := time.Unix(1700000000, 0).UTC()
	rt, err := evolution.NewRuntime(evolution.RuntimeOptions{
		Config: config.EvolutionConfig{Enabled: true, Mode: "observe"},
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	tasks := []string{
		"resuma os emails de hoje",
		"agende uma reunião com o time amanhã às 10h",
		"traduza este parágrafo para o inglês",
	}
	for i, task := range tasks {
		if finalizeErr := rt.FinalizeTurn(context.Background(), evolution.TurnCaseInput{
			Workspace:    workspace,
			TurnID:       fmt.Sprintf("turn-%d", i),
			SessionKey:   "agent:main:web:direct:user_test_owner_01",
			Status:       "completed",
			UserMessage:  ownerTag + task,
			FinalContent: "Pronto.",
		}); finalizeErr != nil {
			t.Fatalf("FinalizeTurn %d: %v", i, finalizeErr)
		}
	}

	raw, err := os.ReadFile(evolution.NewPaths(workspace, "").TaskRecords)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "SENDER") || strings.Contains(string(raw), "Pessoa Teste") {
		t.Fatalf("task records kept the sender tag:\n%s", raw)
	}
	records, err := evolution.NewStore(evolution.NewPaths(workspace, "")).LoadTaskRecords()
	if err != nil {
		t.Fatalf("LoadTaskRecords: %v", err)
	}
	for i, record := range records {
		if record.Summary != tasks[i] {
			t.Fatalf("record %d Summary = %q, want %q", i, record.Summary, tasks[i])
		}
	}

	// Three unrelated asks from the same owner are not one pattern.
	clusterer := evolution.NewHeuristicPatternClusterer(3, func() time.Time { return now })
	patterns, _, err := clusterer.BuildPatterns(context.Background(), workspace, records, nil)
	if err != nil {
		t.Fatalf("BuildPatterns: %v", err)
	}
	if len(patterns) != 0 {
		t.Fatalf("unrelated tasks of one owner clustered together: %+v", patterns[0].Label)
	}
}

func TestStore_LoadTaskRecordsDropsSenderTagOfOlderRecords(t *testing.T) {
	paths := evolution.NewPaths(t.TempDir(), "")
	if err := os.MkdirAll(filepath.Dir(paths.TaskRecords), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// A record as the previous version wrote it: the tag inside the summary.
	line := `{"id":"task-old","kind":"task","workspace_id":"ws","summary":` +
		fmt.Sprintf("%q", ownerTag+"resuma os emails de hoje") + "}\n"
	if err := os.WriteFile(paths.TaskRecords, []byte(line), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	records, err := evolution.NewStore(paths).LoadTaskRecords()
	if err != nil {
		t.Fatalf("LoadTaskRecords: %v", err)
	}
	if len(records) != 1 || records[0].Summary != "resuma os emails de hoje" {
		t.Fatalf("records = %+v, want the summary without the sender tag", records)
	}
}
