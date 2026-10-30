package asr

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/utils"
)

type AudioModelTranscriber struct {
	provider providers.LLMProvider
	modelID  string
	prompt   string
}

const (
	defaultTranscriptionPrompt = "Transcribe this audio."
)

func NewAudioModelTranscriber(modelCfg *config.ModelConfig) *AudioModelTranscriber {
	if modelCfg == nil {
		return nil
	}

	logger.DebugCF("voice", "Creating audio model transcriber", map[string]any{
		"has_api_key": modelCfg.APIKey() != "",
		"api_base":    modelCfg.APIBase,
		"model":       modelCfg.Model,
	})

	provider, modelID, err := providers.CreateProviderFromConfig(modelCfg)
	if err != nil {
		logger.ErrorCF("voice", "Failed to create audio model provider", map[string]any{"error": err})
		return nil
	}

	return &AudioModelTranscriber{
		provider: provider,
		modelID:  modelID,
		prompt:   defaultTranscriptionPrompt,
	}
}

func (t *AudioModelTranscriber) Transcribe(ctx context.Context, audioFilePath string) (*TranscriptionResponse, error) {
	// (seucaranguejo fork) Nenhum log deste arquivo leva a fala nem o nome do
	// arquivo: os logs INFO do pod chegam ao Datadog. Só tamanho, formato e modelo.
	logger.InfoCF("voice", "Starting audio model transcription", map[string]any{
		"model": t.modelID,
	})

	audioBytes, err := os.ReadFile(audioFilePath)
	if err != nil {
		// (seucaranguejo fork) *fs.PathError carrega o caminho inteiro, que em
		// upload web inclui o nome original do arquivo do usuário. Log e erro
		// devolvido (logado de novo em agent_transcribe) levam só op e causa.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			logger.ErrorCF("voice", "Failed to read audio file", map[string]any{
				"error": pathErr.Op + ": " + pathErr.Err.Error(),
			})
			return nil, fmt.Errorf("failed to read audio file: %w", pathErr.Err)
		}
		logger.ErrorCF("voice", "Failed to read audio file", map[string]any{"error": "unknown read error"})
		return nil, errors.New("failed to read audio file")
	}

	// (seucaranguejo fork) Arquivo vazio vira data URL sem dados, que o
	// serializador descarta; o modelo inventaria a resposta e a cobraria.
	if len(audioBytes) == 0 {
		logger.ErrorCF("voice", "Audio file is empty", map[string]any{"model": t.modelID})
		return nil, fmt.Errorf("audio file is empty")
	}

	format, err := utils.AudioFormat(audioFilePath)
	if err != nil {
		// (seucaranguejo fork) O erro de utils.AudioFormat cita o caminho
		// inteiro; aqui só a extensão vai ao log e ao chamador.
		ext := filepath.Ext(audioFilePath)
		logger.ErrorCF("voice", "Unsupported audio format", map[string]any{"extension": ext})
		return nil, fmt.Errorf("unsupported audio format %q", ext)
	}
	logger.InfoCF("voice", "Audio format detected", map[string]any{"format": format})

	resp, err := t.provider.Chat(ctx, []providers.Message{
		{
			Role:    "user",
			Content: t.prompt,
			Media: []string{
				fmt.Sprintf(
					"data:%s;base64,%s",
					utils.AudioMIMEType(format),
					base64.StdEncoding.EncodeToString(audioBytes),
				),
			},
		},
	}, nil, t.modelID, map[string]any{
		"temperature": 0,
	})
	if err != nil {
		logger.ErrorCF("voice", "Audio model transcription request failed", map[string]any{"error": err})
		return nil, fmt.Errorf("transcription request failed: %w", err)
	}

	text := strings.TrimSpace(resp.Content)
	logger.InfoCF("voice", "Audio model transcription completed successfully", map[string]any{
		"text_length": len(text),
	})

	return &TranscriptionResponse{Text: text}, nil
}

func (t *AudioModelTranscriber) Name() string {
	return "audio-model"
}
