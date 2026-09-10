package openai_compat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/logger"
)

func TestChatStreamEmptyDiagnosticRecordsEachEventOnce(t *testing.T) {
	for _, terminal := range []string{"data: [DONE]\n\n", "data: [DONE]", ""} {
		t.Run(terminal, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "stream.jsonl")
			if err := logger.EnableFileLogging(logPath); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(logger.DisableFileLogging)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(
					[]byte(
						"data: {\"id\":\"gen_empty\",\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"finish_reason\":\"stop\"}]}\n\n" + terminal,
					),
				)
			}))
			t.Cleanup(server.Close)
			response, err := NewProvider(
				"test-key",
				server.URL,
				"",
			).ChatStream(t.Context(), []Message{{Role: "user", Content: "test"}}, nil, "test-model", nil, nil)
			if err != nil || response.Content != "" {
				t.Fatalf("stream result: %+v, %v", response, err)
			}
			logs, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, line := range strings.Split(strings.TrimSpace(string(logs)), "\n") {
				var entry map[string]any
				if err := json.Unmarshal([]byte(line), &entry); err != nil {
					t.Fatal(err)
				}
				frames, ok := entry["frames"].(string)
				if !ok {
					continue
				}
				found = true
				wantDone := 0
				if terminal != "" {
					wantDone = 1
				}
				if strings.Count(frames, "[DONE]") != wantDone {
					t.Fatalf("diagnostic duplicated terminal event: %q", frames)
				}
				if entry["frames_seen"] != float64(1+wantDone) || entry["frames_omitted"] != float64(0) ||
					entry["frames_truncated"] != float64(0) ||
					entry["frame_byte_limit"] != float64(frameTailFrameSize) {
					t.Fatalf("capture bounds are missing or inaccurate: %+v", entry)
				}
			}
			if !found {
				t.Fatal("empty stream diagnostic was not logged")
			}
		})
	}
}
