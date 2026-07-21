package agent

import "github.com/sipeed/picoclaw/pkg/tools"

// Runtime status accessors for the control plane's chat UI: "is this session
// compacting right now?" and "which background subagent tasks belong to this
// chat?". Both are read-only snapshots safe to poll.

// CompactionState reports the active context manager's in-flight compaction
// for a session key (the opaque form used by turns — derive it the same way
// ChatSend does).
func (al *AgentLoop) CompactionState(sessionKey string) CompactionState {
	if al == nil || al.contextManager == nil {
		return CompactionState{}
	}
	return al.contextManager.CompactionState(sessionKey)
}

// SubagentTasksFor lists the default agent's tracked background tasks
// originating from the given channel/chat. Empty filters match everything.
func (al *AgentLoop) SubagentTasksFor(channel, chatID string) []tools.SubagentTask {
	if al == nil || al.registry == nil {
		return nil
	}
	agent := al.registry.GetDefaultAgent()
	if agent == nil || agent.SubagentMgr == nil {
		return nil
	}
	tasks := agent.SubagentMgr.ListTaskCopies()
	out := make([]tools.SubagentTask, 0, len(tasks))
	for _, t := range tasks {
		if channel != "" && t.OriginChannel != channel {
			continue
		}
		if chatID != "" && t.OriginChatID != chatID {
			continue
		}
		out = append(out, t)
	}
	return out
}
