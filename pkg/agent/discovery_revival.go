package agent

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// maxRevivedDiscoveredTools matches one discovery page: a turn never offers
// more uncalled deferred tools again than a single tool_search would, across
// retries and the tools the model names (turnState.discoveryRevivals).
const maxRevivedDiscoveredTools = 8

const revivedToolsNudge = "[System] These tools you discovered earlier had expired and were missing from your " +
	"tools; they are available again: %s. If you meant to call one of them, call it now."

// reviveDiscoveredTools re-promotes expired tools that tool_search listed but the
// model never called (EnsureVisible only heals called ones), before a retry.
func (p *Pipeline) reviveDiscoveredTools(ts *turnState, exec *turnExecution, iteration int) {
	discovered := discoveredToolNamesFromMessages(exec.messages)
	budget := maxRevivedDiscoveredTools - ts.discoveryRevivals
	if len(discovered) == 0 || budget <= 0 {
		return
	}
	revived := ts.agent.Tools.ReviveExpired(discovered, discoveryPromoteTTL(p.Cfg), budget)
	ts.discoveryRevivals += len(revived)
	if len(revived) == 0 {
		return
	}
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
