// PicoClaw - Ultra-lightweight personal AI agent

package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// Tool events for the channels that render tool cards for some chats
// (channels.ToolEventsCapable), besides the structured web channels
// (pico/grpc). (seucaranguejo fork — MST-957)

const (
	// metadataKeyToolIsError marks a tool_result whose call failed ("true").
	metadataKeyToolIsError = "tool_is_error"
	// maxToolResultEventLen bounds the result text the structured web
	// channels get live; the full result is in history.
	maxToolResultEventLen = 2000
)

// toolEventsWanted reports whether ts publishes the tool_calls and
// tool_result messages to a channel, other than pico/grpc, that renders them
// for its chat. A sub-turn inherits the parent's chat, but its steps are not
// the conversation's; an automation run's output waits for its gate; and a
// turn without interim output publishes none.
func (al *AgentLoop) toolEventsWanted(ts *turnState) bool {
	if ts.parentTurnID != "" || !ts.opts.AllowInterimPicoPublish || ts.isCronRunMainTurn() {
		return false
	}
	if ts.channel == "pico" || ts.channel == "grpc" || al.bus == nil {
		return false
	}
	return al.bus.ToolEventsFor(ts.channel, ts.chatID)
}

// deliverToolEvent hands a tool event of a ts.toolEvents turn to the channel
// registered for the turn, on the turn's goroutine. The streamed text reaches
// that channel the same way (GetStreamer), so cards and text keep the order in
// which they happened, and the outbound worker's rate limit does not hold the
// cards back. The channel's Send must only enqueue the event and return: the
// turn waits for it. Without a channel manager, or a channel of that name, it
// goes through the bus.
func (al *AgentLoop) deliverToolEvent(ctx context.Context, msg bus.OutboundMessage) {
	sendCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if al.channelManager != nil {
		if ch, ok := al.channelManager.GetChannel(msg.Channel); ok {
			if _, err := ch.Send(sendCtx, bus.NormalizeOutboundMessage(msg)); err != nil {
				logger.WarnCF("agent", "Failed to deliver tool event", map[string]any{
					"channel": msg.Channel,
					"chat_id": msg.ChatID,
					"kind":    msg.Context.Raw[metadataKeyMessageKind],
					"error":   err.Error(),
				})
			}
			return
		}
	}
	if al.bus != nil {
		_ = al.bus.PublishOutbound(sendCtx, msg)
	}
}

// publishToolCallsEvent delivers the tool_calls message of a turn whose
// channel renders tool cards (ts.toolEvents), each call with the progress
// title the model wrote for it (titles, by call id).
func (al *AgentLoop) publishToolCallsEvent(
	ctx context.Context,
	ts *turnState,
	modelName string,
	toolCalls []providers.ToolCall,
	titles map[string]string,
) {
	visible := utils.BuildVisibleToolCalls(toolCalls, al.cfg.Agents.Defaults.GetToolFeedbackMaxArgsLength())
	for i := range visible {
		visible[i].Title = titles[visible[i].ID]
	}
	if msg, ok := toolCallsOutbound(ts, modelName, visible); ok {
		al.deliverToolEvent(ctx, msg)
	}
}

// publishToolResultEvent surfaces a tool's result as it lands. The structured
// web channels (pico/grpc) get its text through the bus, cut to keep the live
// frame small, and only when there is text. A channel that renders tool cards
// (ts.toolEvents) gets the event without the text, since its card shows the
// status and not the output, also for an empty result, since the card waits
// for it. tool_is_error marks a failed call.
func (al *AgentLoop) publishToolResultEvent(
	ctx context.Context,
	ts *turnState,
	toolCallID, content string,
	isError bool,
) {
	if !ts.opts.AllowInterimPicoPublish || al == nil || al.bus == nil {
		return
	}
	structured := ts.channel == "pico" || ts.channel == "grpc"
	if !structured && !ts.toolEvents {
		return
	}
	text := ""
	if structured {
		text = strings.TrimSpace(content)
		if text == "" {
			return
		}
		if len(text) > maxToolResultEventLen {
			text = text[:maxToolResultEventLen] + "…"
		}
	}
	raw := map[string]string{metadataKeyToolCallID: toolCallID}
	if isError {
		raw[metadataKeyToolIsError] = "true"
	}
	msg := outboundMessageForTurnWithOptions(ts, text, outboundTurnMessageOptions{
		kind: messageKindToolResult,
		raw:  raw,
	})
	if !structured {
		al.deliverToolEvent(ctx, msg)
		return
	}
	pubCtx, pubCancel := context.WithTimeout(ctx, 3*time.Second)
	defer pubCancel()
	_ = al.bus.PublishOutbound(pubCtx, msg)
}

// takeProgressTitles takes tools.ProgressTitleParam out of each call whose
// tool got it from tools.WithProgressTitle, so validation, hooks, the repeat
// guard, the history and the tool see only the arguments the tool declares.
// It returns the calls and the titles by call id. The calls are normalized
// (providers.NormalizeToolCall sets Function); a call's arguments and
// FunctionCall are copied before they change, since the provider's response
// still shares them.
func takeProgressTitles(
	registry *tools.ToolRegistry,
	calls []providers.ToolCall,
) ([]providers.ToolCall, map[string]string) {
	var titles map[string]string
	for i, tc := range calls {
		if tool, ok := registry.GetRegistered(tc.Name); ok && tools.DeclaresProgressTitle(tool.Parameters()) {
			continue
		}
		title, rest, taken := tools.TakeProgressTitle(tc.Arguments)
		if !taken {
			continue
		}
		encoded, _ := json.Marshal(rest)
		fn := *tc.Function
		fn.Arguments = string(encoded)
		tc.Arguments, tc.Function = rest, &fn
		calls[i] = tc
		if title != "" {
			if titles == nil {
				titles = make(map[string]string)
			}
			titles[strings.TrimSpace(tc.ID)] = title
		}
	}
	return calls, titles
}
