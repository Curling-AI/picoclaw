package agent

import "context"

type toolTurnOriginKey struct{}

// ToolTurnOrigin identifies the immutable user turn that authorized tool calls,
// including work delegated to independent or nested sub-turns. It is runtime
// metadata, never derived from a subagent's model-generated prompt.
type ToolTurnOrigin struct {
	SessionKey  string
	ChatID      string
	UserMessage string
	SubTurn     bool
	Resolved    bool
}

// ToolOrigin returns the trusted origin captured by the tool pipeline. Missing
// ancestry stays unresolved so authorization hooks can fail closed.
func ToolOrigin(ctx context.Context) ToolTurnOrigin {
	origin, _ := ctx.Value(toolTurnOriginKey{}).(ToolTurnOrigin)
	return origin
}

// RootTurnUserMessage returns the root user message even after the parent turn
// has finished. A subagent's own prompt cannot replace the user's decisions.
func RootTurnUserMessage(ctx context.Context) (string, bool) {
	origin := ToolOrigin(ctx)
	return origin.UserMessage, origin.Resolved
}

// IsSubTurn reports whether this tool call belongs to delegated work.
func IsSubTurn(ctx context.Context) bool { return ToolOrigin(ctx).SubTurn }

func withToolTurnOrigin(ctx context.Context, ts *turnState) context.Context {
	origin := ToolTurnOrigin{SubTurn: ts.parentTurnID != "" || ts.depth > 0}
	root := ts
	for root.parentTurnID != "" || root.depth > 0 {
		if root.parentTurnState == nil {
			return context.WithValue(ctx, toolTurnOriginKey{}, origin)
		}
		root = root.parentTurnState
	}
	origin.SessionKey, origin.ChatID, origin.UserMessage = root.sessionKey, root.chatID, root.userMessage
	origin.Resolved = true
	return context.WithValue(ctx, toolTurnOriginKey{}, origin)
}
