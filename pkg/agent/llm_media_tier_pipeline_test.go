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

// tierTurn shapes the turn runTextTierTurn sends.
type tierTurn struct {
	inline     bool                // the image goes inline (data URL), not as a media-store ref
	textOnly   bool                // the turn carries no attachment
	history    []providers.Message // earlier turns of the session
	maxRetries int                 // agents.defaults.max_llm_retries; 0 keeps the default
}

// runTextTierTurn sends a turn to an agent whose user picked the text-only
// tier, with every model served by provider.
func runTextTierTurn(t *testing.T, provider *scriptedTierProvider, turn tierTurn) (string, error) {
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
				MaxLLMRetries:     turn.maxRetries,
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

	media := []string{ref}
	switch {
	case turn.textOnly:
		media = nil
	case turn.inline:
		// Some channels hand the image over already inline.
		media = []string{testImageDataURL}
	}

	sessionKey := "agent:main:telegram:direct:user1"
	if len(turn.history) > 0 {
		agent.Sessions.SetHistory(sessionKey, turn.history)
	}
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
		Media:      media,
		SessionKey: sessionKey,
	}))
}

// A vision upstream that is down for a moment does not make the image
// unreadable: the call falls back to the vision model, as before the tier
// learned about images, instead of handing the raw image to the tier.
func TestTieredImageTurn_TransientVisionFailureFallsBackToVision(t *testing.T) {
	provider := &scriptedTierProvider{visionErrs: []error{errVisionUnavailable}}
	resp, err := runTextTierTurn(t, provider, tierTurn{})
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

// The vision model refused the image: it becomes a note for the turn, and the
// picked tier answers without ever getting the raw image.
func TestTieredImageTurn_RefusedImageReachesTheTierAsANote(t *testing.T) {
	provider := &scriptedTierProvider{visionErrs: []error{errVisionRefusedImage}}
	resp, err := runTextTierTurn(t, provider, tierTurn{})
	if err != nil {
		t.Fatalf("processMessage: %v", err)
	}
	if resp != "answered by text-tier" {
		t.Fatalf("response = %q, want the picked tier to answer", resp)
	}
	for i, c := range provider.turnCalls() {
		if c.media || !strings.Contains(c.content, unreadableImageNote) {
			t.Fatalf("call %d on %q: media=%v note=%v, want the note and no image",
				i, c.model, c.media, strings.Contains(c.content, unreadableImageNote))
		}
	}
}

// A context overflow rebuilds the call from the original messages. The
// rebuilt call must carry the description again, not the raw image, whether
// the image came as a media-store ref or inline.
func TestTieredImageTurn_ContextRetryKeepsTheImageDescribed(t *testing.T) {
	for _, inline := range []bool{false, true} {
		t.Run(map[bool]string{false: "media-store ref", true: "inline"}[inline], func(t *testing.T) {
			provider := &scriptedTierProvider{visionResp: "a bar chart of monthly sales", tierContextErrs: 1}
			resp, err := runTextTierTurn(t, provider, tierTurn{inline: inline})
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
		})
	}
}

// The text-only tier refused the image, the call went on without it and then
// overflowed the context. The rebuilt call must still leave that image out.
func TestTieredImageTurn_ContextRetryKeepsTheUnreadableImageOut(t *testing.T) {
	provider := &scriptedTierProvider{visionErrs: []error{errVisionRefusedImage}, tierContextErrs: 1}
	resp, err := runTextTierTurn(t, provider, tierTurn{inline: true})
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

// An inline image from an earlier turn is outside delegation's reach and goes
// raw to the tier. A tier that cannot see images gets the call again without
// it, and that retry does not spend the budget a context overflow then needs.
func TestTieredImageTurn_OlderInlineImageIsLeftOutWithoutSpendingRetries(t *testing.T) {
	provider := &scriptedTierProvider{tierContextErrs: 1}
	resp, err := runTextTierTurn(t, provider, tierTurn{
		textOnly:   true,
		maxRetries: 1,
		history: []providers.Message{
			{Role: "user", Content: "look at this", Media: []string{testImageDataURL}},
			{Role: "assistant", Content: "a slide"},
		},
	})
	if err != nil {
		t.Fatalf("processMessage: %v", err)
	}
	if resp != "answered by text-tier" {
		t.Fatalf("response = %q, want the picked tier to answer", resp)
	}
	calls := provider.turnCalls()
	if len(calls) < 2 || !calls[0].media {
		t.Fatalf("turn calls = %+v, want the first one to carry the older image", calls)
	}
	if !strings.Contains(calls[1].content, imageNotSeenNote) {
		t.Fatal("the retry without the image lost the note")
	}
	for i, c := range calls[1:] {
		if c.media {
			t.Fatalf("call %d after the retry still carried the image", i+1)
		}
	}
}

// The error after the retry names the model that refused images, and only
// sends the operator to image_model when that is the model at fault.
func TestVisionUnsupportedModelError_NamesTheRightModel(t *testing.T) {
	cases := []struct {
		name         string
		onImageModel bool
		configured   bool
		want         string
		wantHint     bool
	}{
		{"tier, image model configured", false, true, `active model "m" does not support image input`, false},
		{"image model itself", true, true, `selected vision model "m" does not support image input`, true},
		{"no image model", false, false, `active model "m" does not support image input`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := visionUnsupportedModelError("m", tc.onImageModel, tc.configured).Error()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("error = %q, want %q", got, tc.want)
			}
			if hint := strings.Contains(got, "image_model"); hint != tc.wantHint {
				t.Fatalf("error = %q, image_model hint = %v, want %v", got, hint, tc.wantHint)
			}
		})
	}
}
