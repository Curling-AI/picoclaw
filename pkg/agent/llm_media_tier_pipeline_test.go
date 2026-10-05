package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// End-to-end checks of a tiered agent with image delegation, through
// processMessage: the user picked a tier whose model only reads text.

var (
	errVisionRefusedImage = errors.New(
		"API request failed:\n  Status: 400\n  Body:   {\"error\":{\"message\":\"Provided image is not valid.\"}}")
	errVisionUnavailable = errors.New(
		"API request failed:\n  Status: 503\n  Body:   {\"error\":{\"message\":\"Service Unavailable\"}}")
)

type scriptedCall struct {
	model      string
	delegation bool
	media      bool
	content    string
}

// scriptedTierProvider stands in for every model of the agent: the vision
// model answers the delegation sub-calls, "text-tier" rejects any image like a
// text-only upstream, and every other call answers with its model name.
type scriptedTierProvider struct {
	mu              sync.Mutex
	visionErrs      []error
	visionResp      string
	tierContextErrs int
	calls           []scriptedCall
}

func (p *scriptedTierProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	model string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	call := scriptedCall{
		model:      model,
		delegation: len(messages) > 0 && messages[0].Content == visionDelegationSystemPrompt,
		media:      messagesContainMedia(messages),
	}
	for _, m := range messages {
		call.content += m.Content + "\n"
	}
	p.calls = append(p.calls, call)
	switch {
	case call.delegation:
		if len(p.visionErrs) > 0 {
			err := p.visionErrs[0]
			p.visionErrs = p.visionErrs[1:]
			return nil, err
		}
		return &providers.LLMResponse{Content: p.visionResp}, nil
	case model == "text-tier" && call.media:
		return nil, errors.New("API request failed: Status: 400 Body: text-tier does not support image input")
	case model == "text-tier" && p.tierContextErrs > 0:
		p.tierContextErrs--
		return nil, errors.New("context_length_exceeded: prompt is too long")
	}
	return &providers.LLMResponse{Content: "answered by " + model}, nil
}

func (p *scriptedTierProvider) GetDefaultModel() string { return "main-model" }

func (p *scriptedTierProvider) turnCalls() []scriptedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []scriptedCall
	for _, c := range p.calls {
		if !c.delegation {
			out = append(out, c)
		}
	}
	return out
}

func (p *scriptedTierProvider) delegationCalls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.calls {
		if c.delegation {
			n++
		}
	}
	return n
}

// runTextTierImageTurn sends one image to an agent whose user picked the
// text-only tier, with every model served by provider: a media-store ref, or
// an inline data URL when inline is set.
func runTextTierImageTurn(t *testing.T, provider *scriptedTierProvider, inline bool) (string, error) {
	t.Helper()
	workspace := t.TempDir()
	cfg := &config.Config{
		Agents: config.AgentsConfig{
			Defaults: config.AgentDefaults{
				Workspace:         workspace,
				ModelName:         "main-model",
				ImageModel:        "vision-model",
				MediaDelegation:   true,
				MaxTokens:         4096,
				MaxToolIterations: 3,
				ModelTiers:        map[string]string{"flash": "main-model", "text": "text-tier"},
			},
		},
		ModelList: []*config.ModelConfig{
			{ModelName: "main-model", Model: "openai/main-model"},
			{ModelName: "vision-model", Model: "openai/vision-model"},
			{ModelName: "text-tier", Model: "openai/text-tier"},
		},
	}
	al := NewAgentLoop(cfg, bus.NewMessageBus(), provider)
	agent := al.registry.GetDefaultAgent()
	if agent == nil {
		t.Fatal("expected default agent")
	}
	candidates := append(append([]providers.FallbackCandidate(nil), agent.Candidates...), agent.ImageCandidates...)
	for _, tier := range agent.TierCandidates {
		candidates = append(candidates, tier...)
	}
	for _, c := range candidates {
		agent.CandidateProviders[candidateProviderKey(c)] = provider
	}

	// The attachment arrives as a media-store ref, as from the web channel, and
	// the pipeline resolves it into the call.
	pngPath := filepath.Join(workspace, "chart.png")
	if err := os.WriteFile(pngPath, []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x02,
		0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xDE,
	}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	store := media.NewFileMediaStore()
	al.SetMediaStore(store)
	ref, err := store.Store(pngPath, media.MediaMeta{ContentType: "image/png"}, "test:tier-image")
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	if inline {
		// Some channels hand the image over already inline; a rebuilt call then
		// carries it raw even outside the current turn.
		ref = testImageDataURL
	}

	sessionKey := "agent:main:telegram:direct:user1"
	al.SetPendingModelTier(sessionKey, "text")
	ctx, cancel := context.WithTimeout(context.Background(), responseTimeout)
	defer cancel()
	return al.processMessage(ctx, testInboundMessage(bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:   "telegram",
			ChatID:    "chat1",
			ChatType:  "direct",
			SenderID:  "user1",
			MessageID: "m1",
		},
		Content:    "what is in this image?",
		Media:      []string{ref},
		SessionKey: sessionKey,
	}))
}

// A vision upstream that is down for a moment does not make the image
// unreadable: the call falls back to the vision model, as before the tier
// learned about images, instead of handing the raw image to the tier.
func TestTieredImageTurn_TransientVisionFailureFallsBackToVision(t *testing.T) {
	provider := &scriptedTierProvider{visionErrs: []error{errVisionUnavailable}}
	resp, err := runTextTierImageTurn(t, provider, false)
	if err != nil {
		t.Fatalf("processMessage: %v", err)
	}
	if resp != "answered by vision-model" {
		t.Fatalf("response = %q, want the vision model to answer", resp)
	}
	for _, c := range provider.turnCalls() {
		if c.model == "text-tier" {
			t.Fatalf("text-tier was called (media=%v); the transient failure belongs to the vision model", c.media)
		}
	}
}

// The vision model refused the image, so the tier takes the call; a tier that
// cannot read images must not end the turn over an image nobody could read.
func TestTieredImageTurn_RefusedImageDoesNotFailTextOnlyTier(t *testing.T) {
	provider := &scriptedTierProvider{visionErrs: []error{errVisionRefusedImage}}
	resp, err := runTextTierImageTurn(t, provider, false)
	if err != nil {
		t.Fatalf("processMessage: %v", err)
	}
	if resp != "answered by text-tier" {
		t.Fatalf("response = %q, want the picked tier to answer", resp)
	}
	calls := provider.turnCalls()
	if last := calls[len(calls)-1]; last.media {
		t.Fatal("the answering call still carried the image")
	}
}

// A context overflow rebuilds the call from the original messages. The
// rebuilt call must carry the description again, not the raw image.
func TestTieredImageTurn_ContextRetryKeepsTheImageDescribed(t *testing.T) {
	provider := &scriptedTierProvider{visionResp: "a bar chart of monthly sales", tierContextErrs: 1}
	resp, err := runTextTierImageTurn(t, provider, true)
	if err != nil {
		t.Fatalf("processMessage: %v", err)
	}
	if resp != "answered by text-tier" {
		t.Fatalf("response = %q, want the picked tier to answer", resp)
	}
	calls := provider.turnCalls()
	if len(calls) != 2 {
		t.Fatalf("turn calls = %d, want 2 (overflow + retry)", len(calls))
	}
	for i, c := range calls {
		if c.model != "text-tier" || c.media {
			t.Fatalf("call %d on %q with media=%v, want text-tier without media", i, c.model, c.media)
		}
		if !strings.Contains(c.content, "a bar chart of monthly sales") {
			t.Fatalf("call %d lost the image description", i)
		}
	}
	if n := provider.delegationCalls(); n != 1 {
		t.Fatalf("vision sub-calls = %d, want 1 (the retry reuses the description)", n)
	}
}

// The text-only tier refused the image, the call went on without it and then
// overflowed the context. The rebuilt call must still leave that image out.
func TestTieredImageTurn_ContextRetryKeepsTheUnreadableImageOut(t *testing.T) {
	provider := &scriptedTierProvider{visionErrs: []error{errVisionRefusedImage}, tierContextErrs: 1}
	resp, err := runTextTierImageTurn(t, provider, true)
	if err != nil {
		t.Fatalf("processMessage: %v", err)
	}
	if resp != "answered by text-tier" {
		t.Fatalf("response = %q, want the picked tier to answer", resp)
	}
	calls := provider.turnCalls()
	if last := calls[len(calls)-1]; last.media || !strings.Contains(last.content, unreadableImageNote) {
		t.Fatalf("answering call media=%v, note=%v; want the image left out with the note",
			last.media, strings.Contains(last.content, unreadableImageNote))
	}
}
