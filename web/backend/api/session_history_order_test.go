package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/memory"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestPublicSessionDetailDoesNotExposeHistoryOrderProof(t *testing.T) {
	configPath, cleanup := setupOAuthTestEnv(t)
	defer cleanup()
	store, err := memory.NewJSONLStore(sessionsTestDir(t, configPath))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := store.AppendHistoryReceipt(t.Context(), picoSessionPrefix+"private-history-order", providers.Message{Role: "assistant", Content: "public content"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewHandler(configPath).RegisterRoutes(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/sessions/private-history-order", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "public content") {
		t.Fatal("public detail did not return the saved content", response.Code, response.Body.String())
	}
	for _, private := range []string{receipt.Order.StreamID, "history_order", "history_stream_id", "history_sequence", "HistorySequence"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatal("public detail exposed private ordering evidence", private, response.Body.String())
		}
	}
}
