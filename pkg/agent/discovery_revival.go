package agent

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// maxRevivedDiscoveredTools matches one discovery page, so a retry never
// offers more deferred tools than a single tool_search would.
const maxRevivedDiscoveredTools = 8

const revivedToolsNudge = "[System] These tools you discovered earlier had expired and were missing from your " +
	"tools; they are available again: %s. If you meant to call one of them, call it now."

// reviveDiscoveredTools re-promotes expired tools that tool_search listed but the
// model never called (EnsureVisible only heals called ones), before a retry.
func (p *Pipeline) reviveDiscoveredTools(ts *turnState, exec *turnExecution, iteration int) {
	discovered := discoveredToolNamesFromMessages(exec.messages)
	if len(discovered) == 0 {
		return
	}
	revived := ts.agent.Tools.ReviveExpired(discovered, discoveryPromoteTTL(p.Cfg), maxRevivedDiscoveredTools)
	if len(revived) == 0 {
		return
	}
	ts.offerTools(revived)
	exec.transientTurnMessages = append(exec.transientTurnMessages, providers.Message{
		Role:    "user",
		Content: fmt.Sprintf(revivedToolsNudge, strings.Join(revived, ", ")),
	})
	logger.InfoCF("agent", "Revived expired discovered tools before retry",
		map[string]any{
			"agent_id":  ts.agent.ID,
			"iteration": iteration,
			"tools":     revived,
		})
}
