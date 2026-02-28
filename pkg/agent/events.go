// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT

package agent

import "time"

// EventType identifies the kind of agent lifecycle event.
type EventType int

const (
	EventThinking     EventType = iota // LLM call started
	EventToolStart                     // Tool execution starting
	EventToolComplete                  // Tool execution done
	EventToolError                     // Tool execution failed
	EventResponse                      // Final text response ready
	EventCompacting                    // Context compaction in progress
	EventStopped                       // User stop signal processed
)

// AgentEvent carries details about an agent lifecycle event.
type AgentEvent struct {
	Type      EventType
	ToolName  string
	ToolArgs  map[string]any
	Content   string
	IsError   bool
	Iteration int
	Duration  time.Duration
}

// EventHandler is a callback invoked for agent lifecycle events.
// Implementations must be safe for concurrent use.
type EventHandler func(event AgentEvent)
