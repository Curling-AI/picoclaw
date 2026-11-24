package providers

import (
	"context"

	"github.com/sipeed/picoclaw/pkg/providers/common"
)

type toolSchemaTransformProvider struct {
	delegate  LLMProvider
	transform string
}

type toolSchemaStreamingProvider struct {
	*toolSchemaTransformProvider
}

// The deciding variants carry the delegate's Decisions API, and only a delegate
// that has one gets them: callers pick the decision or the chat path by asking
// for Decider, so a wrapper that always answered yes sent chat-only providers
// down a path that always fails. (seucaranguejo fork)
type toolSchemaDecidingProvider struct {
	*toolSchemaTransformProvider
	Decider
}

type toolSchemaStreamingDecidingProvider struct {
	*toolSchemaStreamingProvider
	Decider
}

func wrapProviderWithToolSchemaTransform(delegate LLMProvider, transform string) (LLMProvider, error) {
	transform, err := common.NormalizeToolSchemaTransform(transform)
	if err != nil {
		return nil, err
	}
	if transform == common.ToolSchemaTransformOff || delegate == nil {
		return delegate, nil
	}
	base := &toolSchemaTransformProvider{
		delegate:  delegate,
		transform: transform,
	}
	decider, decides := delegate.(Decider)
	if _, ok := delegate.(StreamingProvider); ok {
		streaming := &toolSchemaStreamingProvider{toolSchemaTransformProvider: base}
		if decides {
			return &toolSchemaStreamingDecidingProvider{toolSchemaStreamingProvider: streaming, Decider: decider}, nil
		}
		return streaming, nil
	}
	if decides {
		return &toolSchemaDecidingProvider{toolSchemaTransformProvider: base, Decider: decider}, nil
	}
	return base, nil
}

func (p *toolSchemaTransformProvider) Chat(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
) (*LLMResponse, error) {
	transformed, err := common.TransformToolDefinitions(tools, p.transform)
	if err != nil {
		return nil, err
	}
	return p.delegate.Chat(ctx, messages, transformed, model, options)
}

func (p *toolSchemaTransformProvider) GetDefaultModel() string {
	return p.delegate.GetDefaultModel()
}

func (p *toolSchemaStreamingProvider) ChatStream(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(accumulated string),
) (*LLMResponse, error) {
	streaming := p.delegate.(StreamingProvider)
	transformed, err := common.TransformToolDefinitions(tools, p.transform)
	if err != nil {
		return nil, err
	}
	return streaming.ChatStream(ctx, messages, transformed, model, options, onChunk)
}

func (p *toolSchemaStreamingProvider) ChatStreamEvents(
	ctx context.Context,
	messages []Message,
	tools []ToolDefinition,
	model string,
	options map[string]any,
	onChunk func(StreamChunk),
) (*LLMResponse, error) {
	streaming, ok := p.delegate.(StreamingEventProvider)
	if !ok {
		return p.ChatStream(ctx, messages, tools, model, options, func(accumulated string) {
			if onChunk != nil {
				onChunk(StreamChunk{Content: accumulated})
			}
		})
	}
	transformed, err := common.TransformToolDefinitions(tools, p.transform)
	if err != nil {
		return nil, err
	}
	return streaming.ChatStreamEvents(ctx, messages, transformed, model, options, onChunk)
}

func (p *toolSchemaTransformProvider) SupportsThinking() bool {
	tc, ok := p.delegate.(ThinkingCapable)
	return ok && tc.SupportsThinking()
}

func (p *toolSchemaTransformProvider) SupportsNativeSearch() bool {
	ns, ok := p.delegate.(NativeSearchCapable)
	return ok && ns.SupportsNativeSearch()
}

func (p *toolSchemaTransformProvider) Close() {
	if stateful, ok := p.delegate.(StatefulProvider); ok {
		stateful.Close()
	}
}
