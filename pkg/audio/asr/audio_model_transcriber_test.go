package asr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

var _ Transcriber = (*AudioModelTranscriber)(nil)

type fakeLLMProvider struct {
	chatFunc func(
		ctx context.Context,
		messages []providers.Message,
		tools []providers.ToolDefinition,
		model string,
		options map[string]any,
	) (*providers.LLMResponse, error)
}

func (p *fakeLLMProvider) Chat(
	ctx context.Context,
	messages []providers.Message,
	tools []providers.ToolDefinition,
	model string,
	options map[string]any,
) (*providers.LLMResponse, error) {
	if p.chatFunc == nil {
		return nil, nil
	}
	return p.chatFunc(ctx, messages, tools, model, options)
}

func (p *fakeLLMProvider) GetDefaultModel() string {
	return ""
}

func TestAudioModelTranscriberName(t *testing.T) {
	tr := &AudioModelTranscriber{}
	if got := tr.Name(); got != "audio-model" {
		t.Errorf("Name() = %q, want %q", got, "audio-model")
	}
}

func TestNewAudioModelTranscriberInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.ModelConfig
	}{
		{
			name: "nil config",
			cfg:  nil,
		},
		{
			name: "missing api key",
			cfg: &config.ModelConfig{
				Model: "gemini/gemini-2.5-flash",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tr := NewAudioModelTranscriber(tt.cfg); tr != nil {
				t.Fatalf("NewAudioModelTranscriber() = %#v, want nil", tr)
			}
		})
	}
}

func TestAudioModelTranscriberTranscribe(t *testing.T) {
	tmpDir := t.TempDir()
	audioPath := filepath.Join(tmpDir, "clip.ogg")
	audioData := []byte("fake-audio-data")
	if err := os.WriteFile(audioPath, audioData, 0o644); err != nil {
		t.Fatalf("failed to write fake audio file: %v", err)
	}

	t.Run("success", func(t *testing.T) {
		tr := &AudioModelTranscriber{
			provider: &fakeLLMProvider{
				chatFunc: func(
					ctx context.Context,
					messages []providers.Message,
					tools []providers.ToolDefinition,
					model string,
					options map[string]any,
				) (*providers.LLMResponse, error) {
					if ctx == nil {
						t.Fatal("context should not be nil")
					}
					if tools != nil {
						t.Fatalf("tools = %#v, want nil", tools)
					}
					if model != "gemini-2.5-flash" {
						t.Fatalf("model = %q, want %q", model, "gemini-2.5-flash")
					}
					if len(messages) != 1 {
						t.Fatalf("len(messages) = %d, want 1", len(messages))
					}
					msg := messages[0]
					if msg.Role != "user" {
						t.Fatalf("role = %q, want %q", msg.Role, "user")
					}
					if msg.Content != defaultTranscriptionPrompt {
						t.Fatalf("prompt = %q, want %q", msg.Content, defaultTranscriptionPrompt)
					}
					if len(msg.Media) != 1 {
						t.Fatalf("len(media) = %d, want 1", len(msg.Media))
					}
					wantMedia := "data:audio/ogg;base64," + base64.StdEncoding.EncodeToString(audioData)
					if msg.Media[0] != wantMedia {
						t.Fatalf("media = %q, want %q", msg.Media[0], wantMedia)
					}
					if len(options) != 1 {
						t.Fatalf("options = %#v, want only temperature", options)
					}
					if got := options["temperature"]; got != 0 {
						t.Fatalf("temperature = %#v, want 0", got)
					}

					return &providers.LLMResponse{Content: "  hello from gemini \n"}, nil
				},
			},
			modelID: "gemini-2.5-flash",
			prompt:  defaultTranscriptionPrompt,
		}

		resp, err := tr.Transcribe(context.Background(), audioPath)
		if err != nil {
			t.Fatalf("Transcribe() error: %v", err)
		}
		if resp.Text != "hello from gemini" {
			t.Fatalf("Text = %q, want %q", resp.Text, "hello from gemini")
		}
	})

	t.Run("provider error", func(t *testing.T) {
		tr := &AudioModelTranscriber{
			provider: &fakeLLMProvider{
				chatFunc: func(
					ctx context.Context,
					messages []providers.Message,
					tools []providers.ToolDefinition,
					model string,
					options map[string]any,
				) (*providers.LLMResponse, error) {
					return nil, errors.New("upstream failure")
				},
			},
			modelID: "gemini-2.5-flash",
			prompt:  defaultTranscriptionPrompt,
		}

		_, err := tr.Transcribe(context.Background(), audioPath)
		if err == nil {
			t.Fatal("expected error for provider failure, got nil")
		}
		if got := err.Error(); got != "transcription request failed: upstream failure" {
			t.Fatalf("error = %q, want %q", got, "transcription request failed: upstream failure")
		}
	})

	t.Run("missing file", func(t *testing.T) {
		tr := &AudioModelTranscriber{
			provider: &fakeLLMProvider{},
			modelID:  "gemini-2.5-flash",
			prompt:   defaultTranscriptionPrompt,
		}

		_, err := tr.Transcribe(context.Background(), filepath.Join(tmpDir, "nonexistent.ogg"))
		if err == nil {
			t.Fatal("expected error for missing file, got nil")
		}
	})

	t.Run("unsupported audio format", func(t *testing.T) {
		badPath := filepath.Join(tmpDir, "clip.txt")
		if err := os.WriteFile(badPath, []byte("not-audio"), 0o644); err != nil {
			t.Fatalf("failed to write fake file: %v", err)
		}

		tr := &AudioModelTranscriber{
			provider: &fakeLLMProvider{},
			modelID:  "gemini-2.5-flash",
			prompt:   defaultTranscriptionPrompt,
		}

		_, err := tr.Transcribe(context.Background(), badPath)
		if err == nil {
			t.Fatal("expected error for unsupported audio format, got nil")
		}
		if got := err.Error(); got != `unsupported audio format "`+filepath.Ext(badPath)+`"` {
			t.Fatalf("error = %q, want unsupported format error", got)
		}
	})
}

// O data URL leva o MIME real: mp3 é audio/mpeg (MST-351).
func TestAudioModelTranscriberSendsTheMIMEType(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "clip.mp3")
	if err := os.WriteFile(audioPath, []byte("fake-mp3"), 0o644); err != nil {
		t.Fatal(err)
	}
	var media string
	tr := &AudioModelTranscriber{
		provider: &fakeLLMProvider{chatFunc: func(
			_ context.Context, messages []providers.Message, _ []providers.ToolDefinition, _ string, _ map[string]any,
		) (*providers.LLMResponse, error) {
			media = messages[0].Media[0]
			return &providers.LLMResponse{Content: "ok"}, nil
		}},
		modelID: "maestro-transcribe",
		prompt:  defaultTranscriptionPrompt,
	}
	if _, err := tr.Transcribe(context.Background(), audioPath); err != nil {
		t.Fatal(err)
	}
	if want := "data:audio/mpeg;base64," + base64.StdEncoding.EncodeToString([]byte("fake-mp3")); media != want {
		t.Fatalf("media = %q, want %q", media, want)
	}
}

// O caminho que o ASR usa de verdade: entrada da model_list até o corpo enviado.
func TestTranscriberFromModelConfigHonorsAudioFileParts(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "clip.ogg")
	if err := os.WriteFile(audioPath, []byte("fake-ogg"), 0o600); err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("fake-ogg"))

	for _, fileParts := range []bool{false, true} {
		t.Run(fmt.Sprintf("audio_file_parts=%v", fileParts), func(t *testing.T) {
			var body struct {
				Messages []struct {
					Content []map[string]any `json:"content"`
				} `json:"messages"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()

			cfg := &config.ModelConfig{
				ModelName:      "maestro-transcribe",
				Model:          "litellm/maestro-transcribe",
				APIBase:        server.URL,
				AudioFileParts: fileParts,
			}
			cfg.SetAPIKey("test-key")

			tr := transcriberFromModelConfig(cfg)
			if tr == nil {
				t.Fatal("transcriberFromModelConfig() = nil")
			}
			if _, err := tr.Transcribe(context.Background(), audioPath); err != nil {
				t.Fatalf("Transcribe() error = %v", err)
			}

			if len(body.Messages) != 1 || len(body.Messages[0].Content) != 2 {
				t.Fatalf("messages = %#v, want one message with text and audio", body.Messages)
			}
			part := body.Messages[0].Content[1]
			if fileParts {
				file, _ := part["file"].(map[string]any)
				if part["type"] != "file" || file["filename"] != "audio.ogg" ||
					file["file_data"] != "data:audio/ogg;base64,"+b64 {
					t.Fatalf("audio part = %#v, want a file part with the data URL", part)
				}
				return
			}
			audio, _ := part["input_audio"].(map[string]any)
			if part["type"] != "input_audio" || audio["format"] != "ogg" || audio["data"] != b64 {
				t.Fatalf("audio part = %#v, want input_audio with format and data", part)
			}
		})
	}
}

func TestAudioModelTranscriberRejectsEmptyFile(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "empty.ogg")
	if err := os.WriteFile(audioPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	tr := &AudioModelTranscriber{
		provider: &fakeLLMProvider{chatFunc: func(
			context.Context, []providers.Message, []providers.ToolDefinition, string, map[string]any,
		) (*providers.LLMResponse, error) {
			called = true
			return &providers.LLMResponse{Content: "invented"}, nil
		}},
		modelID: "m",
		prompt:  defaultTranscriptionPrompt,
	}
	if _, err := tr.Transcribe(context.Background(), audioPath); err == nil {
		t.Fatal("Transcribe() error = nil, want error for empty file")
	}
	if called {
		t.Fatal("provider was called for an empty file")
	}
}

func TestAudioModelTranscriberErrorsOmitFilePath(t *testing.T) {
	tr := &AudioModelTranscriber{
		provider: &fakeLLMProvider{},
		modelID:  "m",
		prompt:   defaultTranscriptionPrompt,
	}

	t.Run("missing file", func(t *testing.T) {
		audioPath := filepath.Join(t.TempDir(), "consulta-fulana.ogg")
		_, err := tr.Transcribe(context.Background(), audioPath)
		if err == nil {
			t.Fatal("Transcribe() error = nil, want error")
		}
		if strings.Contains(err.Error(), "consulta-fulana") {
			t.Fatalf("error leaks the file name: %q", err.Error())
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("errors.Is(err, fs.ErrNotExist) = false for %v", err)
		}
	})

	t.Run("unsupported format", func(t *testing.T) {
		audioPath := filepath.Join(t.TempDir(), "consulta-fulana.xyz")
		if err := os.WriteFile(audioPath, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := tr.Transcribe(context.Background(), audioPath)
		if err == nil {
			t.Fatal("Transcribe() error = nil, want error")
		}
		if strings.Contains(err.Error(), "consulta-fulana") {
			t.Fatalf("error leaks the file name: %q", err.Error())
		}
	})
}
