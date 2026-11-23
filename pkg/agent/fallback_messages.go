package agent

import (
	"cmp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/config"
)

// The texts a turn writes in place of a reply the model did not give come from
// config.MessagesConfig, so a deployment whose people do not read English
// can word them; the English constants stay the default. (seucaranguejo fork)

func (al *AgentLoop) messagesConfig() config.MessagesConfig {
	if cfg := al.GetConfig(); cfg != nil {
		return cfg.Messages
	}
	return config.MessagesConfig{}
}

func (al *AgentLoop) emptyResponseText() string {
	return cmp.Or(al.messagesConfig().EmptyResponse, defaultResponse)
}

func (al *AgentLoop) toolLimitText() string {
	return cmp.Or(al.messagesConfig().ToolLimit, toolLimitResponse)
}

// repeatStopText is the reply of a turn the repeat guard ended. toolName is
// the name the user knows the tool by.
func (al *AgentLoop) repeatStopText(toolName string) string {
	if text := al.messagesConfig().RepeatStop; text != "" {
		return strings.ReplaceAll(text, "{tool}", toolName)
	}
	return repeatStopSummary(toolName)
}
