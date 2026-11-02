package asr

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	hulksdk "github.com/adapta-school/hulk-sdk-go"

	"github.com/sipeed/picoclaw/pkg/config"
)

const hulkTranscriptionModel = "maestro-stt"

type HulkTranscriber struct{ user *hulksdk.GatewayUserClient }

// NewHulkTranscriber uses the assistant proxy token; a Hulk invoke key never
// enters the assistant process. The proxy assigns the authoritative owner.
func NewHulkTranscriber(model *config.ModelConfig) *HulkTranscriber {
	if model == nil || model.APIKey() == "" || !strings.Contains(model.APIBase, "/llm-proxy/") ||
		!strings.HasSuffix(strings.TrimRight(model.APIBase, "/"), "/hulk") {
		return nil
	}
	transport := &hulkProxyClient{
		token: model.APIKey(),
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	client, err := hulksdk.NewGatewayClient(
		hulksdk.GatewayClientOptions{
			BaseURL:    strings.TrimRight(model.APIBase, "/"),
			APIKey:     "hulk_invoke.proxy.scoped",
			HTTPClient: transport,
		},
	)
	if err != nil {
		return nil
	}
	user, err := client.User("assistant-proxy")
	if err != nil {
		return nil
	}
	return &HulkTranscriber{user: user}
}

type hulkProxyClient struct {
	token  string
	client *http.Client
}

func (c *hulkProxyClient) Do(request *http.Request) (*http.Response, error) {
	forwarded := request.Clone(request.Context())
	forwarded.Header.Set("Authorization", "Bearer "+c.token)
	return c.client.Do(forwarded)
}

func (t *HulkTranscriber) Transcribe(ctx context.Context, path string) (*TranscriptionResponse, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transcription audio: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect transcription audio: %w", err)
	}
	if info.Size() < 1 || info.Size() > hulksdk.MaxTranscriptionAudioBytes {
		return nil, fmt.Errorf("transcription audio must contain 1 to 50 MiB")
	}
	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	result, err := t.user.Audio.Transcriptions.CreateFromReader(
		ctx,
		hulksdk.TranscriptionReaderParams{
			Model:     hulkTranscriptionModel,
			Reader:    file,
			Filename:  filepath.Base(path),
			MediaType: mediaType,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("transcribe through assistant proxy: %w", err)
	}
	return &TranscriptionResponse{Text: result.Text, Language: result.Language, Duration: result.Duration}, nil
}

func (t *HulkTranscriber) Name() string { return "hulk" }
