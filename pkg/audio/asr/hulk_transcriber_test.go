package asr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

func TestHulkTranscriberUsesScopedProxyReaderAndLanguage(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/llm-proxy/assistant/hulk/v1/audio/transcriptions" ||
			r.Header.Get("Authorization") != "Bearer assistant-proxy-token" {
			t.Errorf("unexpected proxy identity: %s", r.URL.Path)
		}
		parts, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			return
		}
		fields := map[string]string{}
		for {
			part, err := parts.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(part)
			fields[part.FormName()] = string(data)
		}
		if fields["file"] != "ogg audio" || fields["model"] != "maestro-stt" {
			t.Errorf("invalid STT upload: %v", fields)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"text":"olá","language":"pt","duration":1.5}`)
	}))
	defer server.Close()
	model := &config.ModelConfig{
		Model:   "openai/maestro-stt",
		APIBase: server.URL + "/llm-proxy/assistant/hulk",
		APIKeys: config.SimpleSecureStrings("assistant-proxy-token"),
	}
	transcriber := transcriberFromModelConfig(model)
	if transcriber == nil || transcriber.Name() != "hulk" {
		t.Fatal("did not select Hulk STT")
	}
	path := filepath.Join(t.TempDir(), "audio.ogg")
	if err := os.WriteFile(path, []byte("ogg audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := transcriber.Transcribe(context.Background(), path)
	if err != nil || result.Text != "olá" || result.Language != "pt" || result.Duration != 1.5 {
		t.Fatal(result, err)
	}
	oversized := filepath.Join(t.TempDir(), "large.wav")
	file, err := os.Create(oversized)
	if err != nil {
		t.Fatal(err)
	}
	file.Truncate((50 << 20) + 1)
	file.Close()
	if _, err := transcriber.Transcribe(context.Background(), oversized); err == nil {
		t.Fatal("accepted oversized file")
	}
	if requests != 1 {
		t.Fatalf("invalid upload reached provider: %d requests", requests)
	}
	model.APIBase = server.URL + "/v1"
	if transcriberFromModelConfig(model) != nil {
		t.Fatal("maestro-stt fell back to direct credentials/chat")
	}
}
