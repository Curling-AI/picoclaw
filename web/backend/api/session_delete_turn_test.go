package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/agent"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type deletedSessionProvider struct{}

func (deletedSessionProvider) Chat(
	context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
) (*providers.LLMResponse, error) {
	return &providers.LLMResponse{Content: "fixture reply", FinishReason: "stop"}, nil
}

func (deletedSessionProvider) GetDefaultModel() string { return "test-model" }

func TestHandleDeleteSession_AllowsNextRealAgentTurn(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(fmt.Sprint(marked), func(t *testing.T) { testRealTurnAfterLauncherDelete(t, marked) })
	}
}

func testRealTurnAfterLauncherDelete(t *testing.T, marked bool) {
	t.Helper()
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	loaded, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Agents: config.AgentsConfig{Defaults: config.AgentDefaults{
		Workspace: loaded.Agents.Defaults.Workspace, ModelName: "test-model", MaxTokens: 4096, MaxToolIterations: 2,
	}}}
	loop := agent.NewAgentLoop(cfg, bus.NewMessageBus(), deletedSessionProvider{})
	t.Cleanup(loop.Close)
	instance := loop.GetRegistry().GetDefaultAgent()
	if _, ok := instance.Provider.(deletedSessionProvider); !ok {
		t.Fatalf("fixture replaced its injected provider: %T", instance.Provider)
	}
	for _, candidate := range instance.CandidateProviders {
		if _, ok := candidate.(deletedSessionProvider); !ok {
			t.Fatalf("fixture installed a non-fake candidate: %T", candidate)
		}
	}
	key := legacyPicoSessionPrefix + "warm-delete-turn"
	if _, operationErr := loop.ProcessDirect(t.Context(), "old prompt", key); operationErr != nil {
		t.Fatal(operationErr)
	}
	sessions := loop.GetRegistry().GetDefaultAgent().Sessions
	sessions.SetSummary(key, "deleted private summary")
	reader, ok := sessions.(interface {
		ReadHistoryPage(ctx context.Context, request memory.HistoryPageRequest) (memory.HistoryPage, error)
	})
	if !ok {
		t.Fatal("real turn backend has no bounded reader")
	}
	before, err := reader.ReadHistoryPage(t.Context(), memory.HistoryPageRequest{Key: key})
	if err != nil || len(before.Entries) != 2 {
		t.Fatal("first turn did not persist both messages", before, err)
	}
	dir := sessionsTestDir(t, configPath)
	if marked {
		if operationErr := os.WriteFile(
			filepath.Join(dir, sanitizeSessionKey(key))+".jsonl.recovery-required",
			nil,
			0o600,
		); operationErr != nil {
			t.Fatal(operationErr)
		}
		if _, operationErr := reader.ReadHistoryPage(
			t.Context(),
			memory.HistoryPageRequest{Key: key},
		); !errors.Is(
			operationErr,
			memory.ErrHistoryRecoveryRequired,
		) {
			t.Fatal("fixture did not warm the recovery latch", operationErr)
		}
	}
	mux := http.NewServeMux()
	NewHandler(configPath).RegisterRoutes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/sessions/warm-delete-turn", nil))
	if response.Code != http.StatusNoContent {
		t.Fatal("launcher did not confirm complete deletion", response.Code, response.Body.String())
	}
	// ProcessDirect executes runAgentLoopResult's metadata precursor before its
	// first append. Direct AddMessage-only tests bypass that production path.
	for _, prompt := range []string{"fresh prompt", "next prompt"} {
		if content, operationErr := loop.ProcessDirect(
			t.Context(),
			prompt,
			key,
		); operationErr != nil ||
			content != "fixture reply" {
			t.Fatal("metadata before append poisoned the turn after DELETE", content, operationErr)
		}
	}
	history := sessions.GetHistory(key)
	if len(history) != 4 || history[0].Content != "fresh prompt" || history[2].Content != "next prompt" ||
		sessions.GetSummary(key) != "" {
		t.Fatal("new turns retained deleted content or failed to persist", history)
	}
	reopened, err := memory.NewJSONLStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.ReadHistoryPage(t.Context(), memory.HistoryPageRequest{Key: key})
	if err != nil || len(after.Entries) != 4 || after.Generation == before.Generation || after.ParentGeneration != "" {
		t.Fatal("reopen retained a deleted generation or recovery fence", after, err)
	}
	if _, operationErr := reader.ReadHistoryPage(
		t.Context(),
		memory.HistoryPageRequest{Key: key, Cursor: before.ResumeCursor},
	); !errors.Is(
		operationErr,
		memory.ErrHistoryCursor,
	) {
		t.Fatal("deleted capture cursor remained live", operationErr)
	}
}
