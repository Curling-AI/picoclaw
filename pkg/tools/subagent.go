package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// SubTurnSpawner is an interface for spawning sub-turns.
// This avoids circular dependency between tools and agent packages.
type SubTurnSpawner interface {
	SpawnSubTurn(ctx context.Context, cfg SubTurnConfig) (*ToolResult, error)
}

// subagentPromptCore é a ÚNICA fonte do framing de sub-execução — antes havia
// 5 declarações em 3 arquivos (subagent.go, spawn.go, agent_init.go) com
// wording divergente, e editar uma não editava as outras.
const subagentPromptCore = "Complete the given task independently and report the result.\n" +
	"You have access to tools - use them as needed to complete your task.\n" +
	"After completing the task, provide a clear summary of what was done."

// SubagentSystemPrompt é o framing canônico de subagente síncrono.
func SubagentSystemPrompt(label string) string {
	if strings.TrimSpace(label) != "" {
		return fmt.Sprintf("You are a subagent labeled %q. %s", label, subagentPromptCore)
	}
	return "You are a subagent. " + subagentPromptCore
}

// SpawnedSubagentSystemPrompt é o framing canônico de subagente em background.
func SpawnedSubagentSystemPrompt(label string) string {
	if strings.TrimSpace(label) != "" {
		return fmt.Sprintf(
			"You are a spawned subagent labeled %q running in the background. %s",
			label,
			subagentPromptCore,
		)
	}
	return "You are a spawned subagent running in the background. " + subagentPromptCore
}

// SubTurnConfig holds configuration for spawning a sub-turn.
type SubTurnConfig struct {
	Model              string
	Tools              []Tool
	SystemPrompt       string
	MaxTokens          int
	Temperature        float64
	Async              bool          // true for async (spawn), false for sync (subagent)
	Critical           bool          // continue running after parent finishes gracefully
	Timeout            time.Duration // 0 = use default (5 minutes)
	MaxContextRunes    int           // 0 = auto, -1 = no limit, >0 = explicit limit
	ActualSystemPrompt string
	InitialMessages    []providers.Message
	InitialTokenBudget *atomic.Int64 // Shared token budget for team members; nil if no budget
	TargetAgentID      string        // If set, run as this agent (its workspace, model, tools)
	// OnStep, when set, hears each tool call the sub-turn starts and returns
	// what to call when it ends. (seucaranguejo fork)
	OnStep func(SubTurnStep) (done func(failed bool))
}

// SubTurnStep is a tool call a sub-turn starts, as the web shows tool calls.
type SubTurnStep struct {
	Name      string
	Arguments string
}

// ErrSubagentTimeLimit starts the error of a sub-turn stopped by its own time
// limit. It lives here so a tool can tell it apart; pkg/agent returns it.
var ErrSubagentTimeLimit = errors.New("subagent stopped by its time limit")

// maxSubagentSteps bounds the steps a task keeps: the live view shows the
// latest, and StepCount still counts them all.
const maxSubagentSteps = 20

// SubagentStep is a tool call a subagent ran, for the live view.
type SubagentStep struct {
	Name       string
	Arguments  string
	Status     string // "running" | "done" | "error" | "canceled"
	Started    int64  // unix ms
	DurationMs int64
}

type SubagentTask struct {
	ID            string
	Task          string
	Label         string
	AgentID       string
	OriginChannel string
	OriginChatID  string
	Status        string
	Result        string
	Created       int64
	// ToolCallID is the call that started the task; "" for the legacy Spawn.
	ToolCallID string
	Steps      []SubagentStep
	StepCount  int
	Finished   int64  // unix ms; 0 while running
	StopReason string // "time_limit" when its time limit stopped it
	// Sync marks a subagent call the model waited on: spawn_status, which
	// reports on spawns, leaves it out.
	Sync bool
}

type SpawnSubTurnFunc func(
	ctx context.Context,
	task, label, agentID string,
	tools *ToolRegistry,
	maxTokens int,
	temperature float64,
	hasMaxTokens, hasTemperature bool,
) (*ToolResult, error)

type SubagentManager struct {
	tasks          map[string]*SubagentTask
	mu             sync.RWMutex
	provider       providers.LLMProvider
	defaultModel   string
	workspace      string
	tools          *ToolRegistry
	maxIterations  int
	maxTokens      int
	temperature    float64
	hasMaxTokens   bool
	hasTemperature bool
	nextID         int
	spawner        SpawnSubTurnFunc

	// mediaResolver resolves media:// refs in tool-loop messages before
	// each LLM call in the legacy RunToolLoop fallback path.
	// This lets subagents reuse the same media handling behavior as the
	// main agent loop without importing pkg/agent and creating a cycle.
	mediaResolver func([]providers.Message) []providers.Message
}

func NewSubagentManager(
	provider providers.LLMProvider,
	defaultModel, workspace string,
) *SubagentManager {
	return &SubagentManager{
		tasks:         make(map[string]*SubagentTask),
		provider:      provider,
		defaultModel:  defaultModel,
		workspace:     workspace,
		tools:         NewToolRegistry(),
		maxIterations: 10,
		nextID:        1,
	}
}

func (sm *SubagentManager) SetSpawner(spawner SpawnSubTurnFunc) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.spawner = spawner
}

// SetMediaResolver injects a message preprocessor that resolves media:// refs
// into LLM-ready content before each tool-loop iteration.
// This is only used by the legacy RunToolLoop fallback path.
func (sm *SubagentManager) SetMediaResolver(
	resolver func([]providers.Message) []providers.Message,
) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.mediaResolver = resolver
}

// SetLLMOptions sets max tokens and temperature for subagent LLM calls.
func (sm *SubagentManager) SetLLMOptions(maxTokens int, temperature float64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.maxTokens = maxTokens
	sm.hasMaxTokens = true
	sm.temperature = temperature
	sm.hasTemperature = true
}

// SetTools sets the tool registry for subagent execution.
// If not set, subagent will have access to the provided tools.
func (sm *SubagentManager) SetTools(tools *ToolRegistry) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.tools = tools
}

// RegisterTool registers a tool for subagent execution.
func (sm *SubagentManager) RegisterTool(tool Tool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.tools.Register(tool)
}

// TrackTask registers a subagent task whose execution happens OUTSIDE this
// manager (the SpawnTool's direct SubTurn path). Without it, spawn_status reads
// an always-empty task map: in prod, agents that spawned a background task got
// "No subagents have been spawned yet" two seconds later. Returns the task ID
// so the caller can surface it (and later resolve it via ResolveTask).
func (sm *SubagentManager) TrackTask(
	task, label, agentID, originChannel, originChatID string,
) string {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	taskID := fmt.Sprintf("subagent-%d", sm.nextID)
	sm.nextID++
	sm.tasks[taskID] = &SubagentTask{
		ID:            taskID,
		Task:          task,
		Label:         label,
		AgentID:       agentID,
		OriginChannel: originChannel,
		OriginChatID:  originChatID,
		Status:        "running",
		Created:       time.Now().UnixMilli(),
	}
	return taskID
}

// ResolveTask finalizes a task registered via TrackTask with its terminal
// status ("completed", "failed" or "canceled") and result text.
func (sm *SubagentManager) ResolveTask(taskID, status, result string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.resolveLocked(taskID, status, result, "")
}

// resolveLocked is ResolveTask with sm.mu held, so a reader never sees a task
// finished without its stop reason.
func (sm *SubagentManager) resolveLocked(taskID, status, result, stopReason string) {
	task, ok := sm.tasks[taskID]
	if !ok {
		return
	}
	task.Status = status
	task.Result = result
	task.Finished = time.Now().UnixMilli()
	task.StopReason = stopReason
	// A step the turn left open (a stop cut it) will not end now.
	for i := range task.Steps {
		if task.Steps[i].Status == "running" {
			task.Steps[i].Status = "canceled"
		}
	}
}

// trackCall registers the task the current tool call starts, under the call's
// id, and returns the task id and the recorder of its steps.
func (sm *SubagentManager) trackCall(
	ctx context.Context,
	task, label, agentID string,
	sync bool,
) (string, func(SubTurnStep) func(bool)) {
	taskID := sm.TrackTask(task, label, agentID, ToolChannel(ctx), ToolChatID(ctx))
	sm.mu.Lock()
	sm.tasks[taskID].ToolCallID = ToolCallID(ctx)
	sm.tasks[taskID].Sync = sync
	sm.mu.Unlock()
	return taskID, sm.recordSteps(taskID)
}

// recordSteps is the SubTurnConfig.OnStep that keeps taskID's steps.
func (sm *SubagentManager) recordSteps(taskID string) func(SubTurnStep) func(bool) {
	return func(step SubTurnStep) func(bool) {
		sm.mu.Lock()
		defer sm.mu.Unlock()
		task, ok := sm.tasks[taskID]
		if !ok {
			return func(bool) {}
		}
		started := time.Now()
		task.StepCount++
		seq := task.StepCount
		task.Steps = append(task.Steps, SubagentStep{
			Name:      step.Name,
			Arguments: step.Arguments,
			Status:    "running",
			Started:   started.UnixMilli(),
		})
		if extra := len(task.Steps) - maxSubagentSteps; extra > 0 {
			task.Steps = slices.Delete(task.Steps, 0, extra)
		}
		return func(failed bool) {
			sm.mu.Lock()
			defer sm.mu.Unlock()
			// Steps are kept in start order: this one sits StepCount-seq from the end.
			i := len(task.Steps) - 1 - (task.StepCount - seq)
			if i < 0 || task.Steps[i].Status != "running" {
				return
			}
			task.Steps[i].Status = "done"
			if failed {
				task.Steps[i].Status = "error"
			}
			task.Steps[i].DurationMs = time.Since(started).Milliseconds()
		}
	}
}

// finishCall resolves a task trackCall registered from what its sub-turn
// returned.
func (sm *SubagentManager) finishCall(taskID string, result *ToolResult, err error) {
	status, text := "completed", ""
	if result != nil {
		text = result.ForLLM
		if result.IsError {
			status = "failed"
		}
	}
	if err != nil {
		status = "failed"
	}
	stopReason := ""
	if errors.Is(err, ErrSubagentTimeLimit) {
		stopReason = "time_limit"
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.resolveLocked(taskID, status, text, stopReason)
}

func (sm *SubagentManager) Spawn(
	ctx context.Context,
	task, label, agentID, originChannel, originChatID string,
	callback AsyncCallback,
) (string, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	taskID := fmt.Sprintf("subagent-%d", sm.nextID)
	sm.nextID++

	subagentTask := &SubagentTask{
		ID:            taskID,
		Task:          task,
		Label:         label,
		AgentID:       agentID,
		OriginChannel: originChannel,
		OriginChatID:  originChatID,
		Status:        "running",
		Created:       time.Now().UnixMilli(),
	}
	sm.tasks[taskID] = subagentTask

	// Start task in background with context cancellation support
	go sm.runTask(ctx, subagentTask, callback)

	if label != "" {
		return fmt.Sprintf("Spawned subagent '%s' for task: %s", label, task), nil
	}
	return fmt.Sprintf("Spawned subagent for task: %s", task), nil
}

func (sm *SubagentManager) runTask(
	ctx context.Context,
	task *SubagentTask,
	callback AsyncCallback,
) {
	task.Status = "running"
	task.Created = time.Now().UnixMilli()
	// TODO(eventbus): once subagents are modeled as child turns inside
	// pkg/agent, emit SubTurnEnd and SubTurnResultDelivered from the parent
	// AgentLoop instead of this legacy manager.

	// Check if context is already canceled before starting
	select {
	case <-ctx.Done():
		sm.mu.Lock()
		task.Status = "canceled"
		task.Result = "Task canceled before execution"
		sm.mu.Unlock()
		return
	default:
	}

	sm.mu.RLock()
	spawner := sm.spawner
	tools := sm.tools
	maxIter := sm.maxIterations
	maxTokens := sm.maxTokens
	temperature := sm.temperature
	hasMaxTokens := sm.hasMaxTokens
	hasTemperature := sm.hasTemperature
	mediaResolver := sm.mediaResolver
	sm.mu.RUnlock()

	var result *ToolResult
	var err error

	if spawner != nil {
		result, err = spawner(
			ctx,
			task.Task,
			task.Label,
			task.AgentID,
			tools,
			maxTokens,
			temperature,
			hasMaxTokens,
			hasTemperature,
		)
	} else {
		// Fallback to legacy RunToolLoop
		systemPrompt := SubagentSystemPrompt("")

		messages := []providers.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: task.Task},
		}

		var llmOptions map[string]any
		if hasMaxTokens || hasTemperature {
			llmOptions = map[string]any{}
			if hasMaxTokens {
				llmOptions["max_tokens"] = maxTokens
			}
			if hasTemperature {
				llmOptions["temperature"] = temperature
			}
		}

		var loopResult *ToolLoopResult
		loopResult, err = RunToolLoop(ctx, ToolLoopConfig{
			Provider:      sm.provider,
			Model:         sm.defaultModel,
			Tools:         tools,
			MaxIterations: maxIter,
			LLMOptions:    llmOptions,
			MediaResolver: mediaResolver,
		}, messages, task.OriginChannel, task.OriginChatID)

		if err == nil {
			result = &ToolResult{
				ForLLM: fmt.Sprintf(
					"Subagent '%s' completed (iterations: %d): %s",
					task.Label,
					loopResult.Iterations,
					loopResult.Content,
				),
				ForUser: loopResult.Content,
				Silent:  false,
				IsError: false,
				Async:   false,
			}
		}
	}

	sm.mu.Lock()
	defer func() {
		sm.mu.Unlock()
		// Call callback if provided and result is set
		if callback != nil && result != nil {
			callback(ctx, result)
		}
	}()

	if err != nil {
		task.Status = "failed"
		task.Result = fmt.Sprintf("Error: %v", err)
		// Check if it was canceled
		if ctx.Err() != nil {
			task.Status = "canceled"
			task.Result = "Task canceled during execution"
		}
		result = &ToolResult{
			ForLLM:  task.Result,
			ForUser: "",
			Silent:  false,
			IsError: true,
			Async:   false,
			Err:     err,
		}
	} else {
		task.Status = "completed"
		task.Result = result.ForLLM
	}
}

func (sm *SubagentManager) GetTask(taskID string) (*SubagentTask, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	task, ok := sm.tasks[taskID]
	return task, ok
}

// GetTaskCopy returns a copy of the task with the given ID, taken under the
// read lock, so the caller receives a consistent snapshot with no data race.
func (sm *SubagentManager) GetTaskCopy(taskID string) (SubagentTask, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	task, ok := sm.tasks[taskID]
	if !ok {
		return SubagentTask{}, false
	}
	return copyTask(task), true
}

// copyTask is a snapshot of task that shares nothing the manager still writes.
func copyTask(task *SubagentTask) SubagentTask {
	c := *task
	c.Steps = slices.Clone(task.Steps)
	return c
}

func (sm *SubagentManager) ListTasks() []*SubagentTask {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	tasks := make([]*SubagentTask, 0, len(sm.tasks))
	for _, task := range sm.tasks {
		tasks = append(tasks, task)
	}
	return tasks
}

// ListTaskCopies returns value copies of all tasks, taken under the read lock,
// so callers receive consistent snapshots with no data race.
func (sm *SubagentManager) ListTaskCopies() []SubagentTask {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	copies := make([]SubagentTask, 0, len(sm.tasks))
	for _, task := range sm.tasks {
		copies = append(copies, copyTask(task))
	}
	return copies
}

// SubagentTool executes a subagent task synchronously and returns the result.
// It directly calls SubTurnSpawner with Async=false for synchronous execution.
type SubagentTool struct {
	spawner      SubTurnSpawner
	manager      *SubagentManager
	defaultModel string
	maxTokens    int
	temperature  float64
}

func NewSubagentTool(manager *SubagentManager) *SubagentTool {
	if manager == nil {
		return &SubagentTool{}
	}
	return &SubagentTool{
		manager:      manager,
		defaultModel: manager.defaultModel,
		maxTokens:    manager.maxTokens,
		temperature:  manager.temperature,
	}
}

// SetSpawner sets the SubTurnSpawner for direct sub-turn execution.
func (t *SubagentTool) SetSpawner(spawner SubTurnSpawner) {
	t.spawner = spawner
}

func (t *SubagentTool) Name() string {
	return "subagent"
}

func (t *SubagentTool) Description() string {
	return "Execute a subagent task synchronously and return the result. Use this for delegating specific tasks to an independent agent instance. Returns execution summary to user and full details to LLM."
}

func (t *SubagentTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "The task for subagent to complete",
			},
			"label": map[string]any{
				"type":        "string",
				"description": "Optional short label for the task (for display)",
			},
		},
		"required": []string{"task"},
	}
}

func (t *SubagentTool) Execute(ctx context.Context, args map[string]any) *ToolResult {
	task, ok := args["task"].(string)
	if !ok {
		return ErrorResult("task is required").WithError(fmt.Errorf("task parameter is required"))
	}

	label, ok := args["label"].(string)
	if !ok {
		label = ""
	}

	// Build system prompt for subagent (framing canônico compartilhado)
	systemPrompt := SubagentSystemPrompt(label) + "\n\nTask: " + task

	// Use spawner if available (direct SpawnSubTurn call)
	if t.spawner != nil {
		// Tracked like a spawn: the web shows both the same way.
		taskID, onStep := "", func(SubTurnStep) func(bool) { return func(bool) {} }
		if t.manager != nil {
			taskID, onStep = t.manager.trackCall(ctx, task, label, "", true)
		}
		result, err := t.spawner.SpawnSubTurn(ctx, SubTurnConfig{
			Model:        t.defaultModel,
			Tools:        nil, // Will inherit from parent via context
			SystemPrompt: systemPrompt,
			MaxTokens:    t.maxTokens,
			Temperature:  t.temperature,
			Async:        false, // Synchronous execution
			OnStep:       onStep,
		})
		if t.manager != nil {
			t.manager.finishCall(taskID, result, err)
		}
		if err != nil {
			return ErrorResult(fmt.Sprintf("Subagent execution failed: %v", err)).WithError(err)
		}

		// Format result for display
		userContent := result.ForLLM
		if result.ForUser != "" {
			userContent = result.ForUser
		}
		maxUserLen := 500
		if len(userContent) > maxUserLen {
			userContent = userContent[:maxUserLen] + "..."
		}

		labelStr := label
		if labelStr == "" {
			labelStr = "(unnamed)"
		}
		llmContent := fmt.Sprintf("Subagent task completed:\nLabel: %s\nResult: %s",
			labelStr, result.ForLLM)

		return &ToolResult{
			ForLLM:  llmContent,
			ForUser: userContent,
			Silent:  false,
			IsError: result.IsError,
			Async:   false,
		}
	}

	// Fallback: spawner not configured
	return ErrorResult("Subagent manager not configured").WithError(fmt.Errorf("spawner not set"))
}
