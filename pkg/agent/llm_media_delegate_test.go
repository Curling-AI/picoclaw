package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// recordingVisionProvider records Chat calls and returns a fixed description,
// standing in for the image model in delegation tests. Its first `failures`
// calls fail, like a vision upstream rejecting the request; errAt fails a
// given call (1-based) with a given error instead.
type recordingVisionProvider struct {
	calls        int
	failures     int
	errAt        map[int]error
	lastMessages []providers.Message
	resp         string
}

func (p *recordingVisionProvider) Chat(
	_ context.Context,
	messages []providers.Message,
	_ []providers.ToolDefinition,
	_ string,
	_ map[string]any,
) (*providers.LLMResponse, error) {
	p.calls++
	p.lastMessages = messages
	if err := p.errAt[p.calls]; err != nil {
		return nil, err
	}
	if p.calls <= p.failures {
		return nil, errors.New("vision upstream rejected the request")
	}
	return &providers.LLMResponse{Content: p.resp}, nil
}

func (p *recordingVisionProvider) GetDefaultModel() string { return "vision-model" }

const testImageDataURL = "data:image/png;base64,iVBORw0KGgoAAAANS"

func delegationFixture(delegation bool, vision *recordingVisionProvider) (*Pipeline, *turnState, *turnExecution) {
	cfg := config.DefaultConfig()
	cfg.Agents.Defaults.ImageModel = "google/gemini-2.5-flash-lite"
	p := &Pipeline{Cfg: cfg}
	agent := &AgentInstance{
		ID:              "img-agent",
		MediaDelegation: delegation,
		Provider:        vision,
		ImageCandidates: []providers.FallbackCandidate{
			{Provider: "openai", Model: "google/gemini-2.5-flash-lite"},
		},
	}
	ts := &turnState{agent: agent}
	exec := &turnExecution{
		currentTurnStart: 1,
		callMessages:     freshMediaMessages(),
	}
	return p, ts, exec
}

func freshMediaMessages() []providers.Message {
	return []providers.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "please read this receipt"},
		{Role: "tool", Content: "Image loaded: receipt.png"},
		{Role: "user", Content: toolImageFollowUpPlaceholder, Media: []string{testImageDataURL}},
	}
}

func TestDelegateMediaTurn_Disabled(t *testing.T) {
	vision := &recordingVisionProvider{resp: "a receipt"}
	p, ts, exec := delegationFixture(false, vision)

	outcome, err := p.delegateMediaTurn(context.Background(), ts, exec)
	if err != nil {
		t.Fatalf("delegateMediaTurn: %v", err)
	}
	if outcome != delegationSkipped {
		t.Fatalf("outcome = %v, want delegationSkipped when MediaDelegation is disabled", outcome)
	}
	if vision.calls != 0 {
		t.Errorf("vision calls = %d, want 0 (delegation off)", vision.calls)
	}
	// The image must be left untouched for the legacy swap path.
	if len(exec.callMessages[3].Media) != 1 {
		t.Errorf("image Media was modified while delegation is off")
	}
}

func TestDelegateMediaTurn_AnalyzesAndInjects(t *testing.T) {
	vision := &recordingVisionProvider{resp: "Total: R$ 42,00. Vendor: Padaria."}
	p, ts, exec := delegationFixture(true, vision)

	outcome, err := p.delegateMediaTurn(context.Background(), ts, exec)
	if err != nil {
		t.Fatalf("delegateMediaTurn: %v", err)
	}
	if outcome != delegationDone {
		t.Fatalf("outcome = %v, want delegationDone (an image was delegated)", outcome)
	}
	if vision.calls != 1 {
		t.Fatalf("vision calls = %d, want 1", vision.calls)
	}
	// The image was stripped and the analysis injected as text.
	img := exec.callMessages[3]
	if len(img.Media) != 0 {
		t.Errorf("image Media not stripped: %v", img.Media)
	}
	if !strings.Contains(img.Content, "Total: R$ 42,00") {
		t.Errorf("analysis not injected into content: %q", img.Content)
	}
	if strings.Contains(img.Content, toolImageFollowUpPlaceholder) {
		t.Errorf("placeholder should have been replaced: %q", img.Content)
	}
	// The vision sub-call actually received the image and the brief context.
	sawImage, sawBrief := false, false
	for _, m := range vision.lastMessages {
		for _, ref := range m.Media {
			if ref == testImageDataURL {
				sawImage = true
			}
		}
		if strings.Contains(m.Content, "please read this receipt") {
			sawBrief = true
		}
	}
	if !sawImage {
		t.Error("vision sub-call did not receive the image")
	}
	if !sawBrief {
		t.Error("vision sub-call did not receive the current-turn brief")
	}
	// The main model must NOT be swapped to the vision model.
	if exec.activeModel != "" {
		t.Errorf("activeModel = %q, want empty (no swap under delegation)", exec.activeModel)
	}
}

// A failed sub-call is not the same as having nothing to delegate, and a
// refusal is not the same as an upstream failing for a moment: routeTurnModel
// sends the first to the tier and keeps the second on the vision model.
func TestDelegateMediaTurn_ReportsHowTheSubCallFailed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want delegationOutcome
	}{
		{"refused image", errVisionRefusedImage, delegationRefused},
		{"generic rejection", errors.New("vision upstream rejected the request"), delegationRefused},
		{"upstream unavailable", errVisionUnavailable, delegationFailed},
		{
			"timeout",
			errors.New("Post \"http://gateway/v1/chat/completions\": context deadline exceeded"),
			delegationFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ts, exec := delegationFixture(true, &recordingVisionProvider{errAt: map[int]error{1: tc.err}})
			outcome, err := p.delegateMediaTurn(context.Background(), ts, exec)
			if err != nil {
				t.Fatalf("delegateMediaTurn: %v", err)
			}
			if outcome != tc.want {
				t.Fatalf("outcome = %v, want %v", outcome, tc.want)
			}
			if len(exec.callMessages[3].Media) != 1 {
				t.Errorf("image Media was modified although nothing was analyzed")
			}
		})
	}
}

// twoImageMessages is a turn with an attached image and one a tool loaded.
func twoImageMessages() []providers.Message {
	return []providers.Message{
		{Role: "system", Content: "system prompt"},
		{Role: "user", Content: "compare these", Media: []string{testImageDataURL}},
		{Role: "tool", Content: "Image loaded: second.png"},
		{Role: "user", Content: toolImageFollowUpPlaceholder, Media: []string{testImageDataURL + "Zm9v"}},
	}
}

// One image described, the other not: the described one stays text, the other
// stays raw, and the outcome says why it is raw.
func TestDelegateMediaTurn_PartialKeepsWhatWasDescribed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want delegationOutcome
	}{
		{"second refused", errVisionRefusedImage, delegationRefused},
		{"second unavailable", errVisionUnavailable, delegationFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vision := &recordingVisionProvider{resp: "a receipt", errAt: map[int]error{2: tc.err}}
			p, ts, exec := delegationFixture(true, vision)
			exec.callMessages = twoImageMessages()
			outcome, err := p.delegateMediaTurn(context.Background(), ts, exec)
			if err != nil {
				t.Fatalf("delegateMediaTurn: %v", err)
			}
			if outcome != tc.want {
				t.Fatalf("outcome = %v, want %v", outcome, tc.want)
			}
			first, second := exec.callMessages[1], exec.callMessages[3]
			if len(first.Media) != 0 || !strings.Contains(first.Content, "a receipt") {
				t.Errorf("described image not rewritten: media=%v content=%q", first.Media, first.Content)
			}
			if len(second.Media) != 1 {
				t.Errorf("undescribed image media = %v, want it kept raw", second.Media)
			}
		})
	}
}

func TestDelegateMediaTurn_MemoizesAcrossIterations(t *testing.T) {
	vision := &recordingVisionProvider{resp: "cached description"}
	p, ts, exec := delegationFixture(true, vision)

	if _, err := p.delegateMediaTurn(context.Background(), ts, exec); err != nil {
		t.Fatalf("first delegateMediaTurn: %v", err)
	}
	// Simulate the next agentic iteration: resolveMediaRefs rebuilds the image
	// message from the untouched working set. The memo cache must prevent a
	// second vision call.
	exec.callMessages = freshMediaMessages()
	if _, err := p.delegateMediaTurn(context.Background(), ts, exec); err != nil {
		t.Fatalf("second delegateMediaTurn: %v", err)
	}
	if vision.calls != 1 {
		t.Errorf("vision calls = %d, want 1 (memoized across iterations)", vision.calls)
	}
}

func TestStripDataImages(t *testing.T) {
	got := stripDataImages([]string{testImageDataURL, "media://keepme", testImageDataURL})
	if len(got) != 1 || got[0] != "media://keepme" {
		t.Errorf("stripDataImages = %v, want [media://keepme]", got)
	}
	if stripDataImages([]string{testImageDataURL}) != nil {
		t.Error("stripDataImages of only-images should be nil")
	}
}

func TestInjectVisionAnalysis(t *testing.T) {
	// Placeholder is replaced wholesale.
	got := injectVisionAnalysis(toolImageFollowUpPlaceholder, "desc", "gemini")
	if strings.Contains(got, toolImageFollowUpPlaceholder) {
		t.Errorf("placeholder not replaced: %q", got)
	}
	if !strings.Contains(got, "desc") || !strings.Contains(got, "gemini") {
		t.Errorf("analysis/model missing: %q", got)
	}
	// Real content is preserved and appended to.
	got = injectVisionAnalysis("user asked X", "desc", "")
	if !strings.Contains(got, "user asked X") || !strings.Contains(got, "desc") {
		t.Errorf("content not preserved with analysis: %q", got)
	}
}

func TestHashImages_StableAndDistinct(t *testing.T) {
	a := hashImages([]string{"data:image/png;base64,AAA"})
	b := hashImages([]string{"data:image/png;base64,AAA"})
	c := hashImages([]string{"data:image/png;base64,BBB"})
	if a != b {
		t.Error("hashImages not stable for identical input")
	}
	if a == c {
		t.Error("hashImages collided for different images")
	}
}

func TestDelegationBrief_ExcludesImagesAndBounds(t *testing.T) {
	msgs := freshMediaMessages()
	brief := delegationBrief(msgs, 1, []mediaImageTarget{{idx: 3, images: []string{testImageDataURL}}})
	if !strings.Contains(brief, "please read this receipt") {
		t.Errorf("brief missing current-turn text: %q", brief)
	}
	if strings.Contains(brief, toolImageFollowUpPlaceholder) {
		t.Errorf("brief should exclude the image message: %q", brief)
	}
	if strings.Contains(brief, "system prompt") {
		t.Errorf("brief should start at currentTurnStart, not include history: %q", brief)
	}
}
