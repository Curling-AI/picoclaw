package tools

import (
	"context"
	"strings"
	"testing"
	"time"
)

// mockSpawner implements SubTurnSpawner for testing.
type mockSpawner struct {
	lastConfig SubTurnConfig
	done       chan struct{}
}

func (m *mockSpawner) SpawnSubTurn(ctx context.Context, cfg SubTurnConfig) (*ToolResult, error) {
	m.lastConfig = cfg
	if m.done != nil {
		close(m.done)
	}

	// Extract task from system prompt for response
	task := cfg.SystemPrompt
	if strings.Contains(task, "Task: ") {
		parts := strings.Split(task, "Task: ")
		if len(parts) > 1 {
			task = parts[1]
		}
	}
	return &ToolResult{
		ForLLM:  "Task completed: " + task,
		ForUser: "Task completed",
	}, nil
}

func TestSpawnTool_Execute_EmptyTask(t *testing.T) {
	provider := &MockLLMProvider{}
	manager := NewSubagentManager(provider, "test-model", "/tmp/test")
	tool := NewSpawnTool(manager)

	ctx := context.Background()

	tests := []struct {
		name string
		args map[string]any
	}{
		{"empty string", map[string]any{"task": ""}},
		{"whitespace only", map[string]any{"task": "   "}},
		{"tabs and newlines", map[string]any{"task": "\t\n  "}},
		{"missing task key", map[string]any{"label": "test"}},
		{"wrong type", map[string]any{"task": 123}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tool.Execute(ctx, tt.args)
			if result == nil {
				t.Fatal("Result should not be nil")
			}
			if !result.IsError {
				t.Error("Expected error for invalid task parameter")
			}
			if !strings.Contains(result.ForLLM, "task is required") {
				t.Errorf("Error message should mention 'task is required', got: %s", result.ForLLM)
			}
		})
	}
}

func TestSpawnTool_Execute_ValidTask(t *testing.T) {
	provider := &MockLLMProvider{}
	manager := NewSubagentManager(provider, "test-model", "/tmp/test")
	tool := NewSpawnTool(manager)
	spawner := &mockSpawner{done: make(chan struct{})}
	tool.SetSpawner(spawner)

	ctx := context.Background()
	args := map[string]any{
		"task":     "Write a haiku about coding",
		"label":    "haiku-task",
		"agent_id": "research",
	}

	result := tool.Execute(ctx, args)
	if result == nil {
		t.Fatal("Result should not be nil")
	}
	if result.IsError {
		t.Errorf("Expected success for valid task, got error: %s", result.ForLLM)
	}
	if !result.Async {
		t.Error("SpawnTool should return async result")
	}
	<-spawner.done
	if spawner.lastConfig.TargetAgentID != "research" {
		t.Errorf("TargetAgentID = %q, want research", spawner.lastConfig.TargetAgentID)
	}
	if !spawner.lastConfig.Critical {
		t.Error("SpawnTool should mark background subturns as critical")
	}
}

func TestSpawnTool_Execute_NilManager(t *testing.T) {
	tool := NewSpawnTool(nil)

	ctx := context.Background()
	args := map[string]any{"task": "test task"}

	result := tool.Execute(ctx, args)
	if !result.IsError {
		t.Error("Expected error for nil manager")
	}
	if !strings.Contains(result.ForLLM, "Subagent manager not configured") {
		t.Errorf("Error message should mention manager not configured, got: %s", result.ForLLM)
	}
}

// O spawn deve REGISTRAR a task no manager (senão o spawn_status lê um map
// sempre vazio — bug de prod: "No subagents have been spawned yet" 2s após o
// spawn) e RESOLVÊ-LA ao terminar, com o task_id visível no ack.
func TestSpawnTool_Execute_TracksTaskInManager(t *testing.T) {
	provider := &MockLLMProvider{}
	manager := NewSubagentManager(provider, "test-model", "/tmp/test")
	tool := NewSpawnTool(manager)
	spawner := &mockSpawner{done: make(chan struct{})}
	tool.SetSpawner(spawner)

	ctx := context.Background()
	result := tool.Execute(ctx, map[string]any{"task": "long research", "label": "res"})
	if result == nil || result.IsError {
		t.Fatalf("unexpected error: %+v", result)
	}
	// O ack carrega o task_id para o modelo poder consultar o status certo.
	if !strings.Contains(result.ForLLM, "task_id: subagent-") {
		t.Errorf("ack must carry the task_id, got: %s", result.ForLLM)
	}

	// A task existe imediatamente (running ou já resolvida pela goroutine).
	tasks := manager.ListTaskCopies()
	if len(tasks) != 1 {
		t.Fatalf("manager tasks = %d, want 1 (task registered at spawn time)", len(tasks))
	}

	<-spawner.done
	// Espera a resolução (a goroutine resolve após o SpawnSubTurn retornar).
	deadline := time.After(2 * time.Second)
	for {
		cpy, ok := manager.GetTaskCopy(tasks[0].ID)
		if !ok {
			t.Fatal("task disappeared from manager")
		}
		if cpy.Status == "completed" {
			if !strings.Contains(cpy.Result, "Task completed") {
				t.Errorf("task result = %q, want the subturn result", cpy.Result)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("task never resolved; status=%s", cpy.Status)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// E o spawn_status enxerga a task.
	status := NewSpawnStatusTool(manager).Execute(ctx, map[string]any{})
	if strings.Contains(status.ForLLM, "No subagents have been spawned yet") {
		t.Errorf("spawn_status still blind to spawned task: %s", status.ForLLM)
	}
	if !strings.Contains(status.ForLLM, "subagent-") {
		t.Errorf("spawn_status must list the task, got: %s", status.ForLLM)
	}
}
