package agent

import (
	"fmt"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/providers"
)

// maxRevivedDiscoveredTools matches one discovery page: a turn never brings
// back more distinct uncalled deferred tools than a single tool_search would
// list, across retries and the tools the model names.
const maxRevivedDiscoveredTools = 8

// takeHealBudget returns the part of missing this turn may still bring back,
// and charges it: one page of distinct tools per turn, shared by the retry
// revival and the mention heal. The turn's set only grows, so a tool brought
// back stays offered and is never charged twice. (seucaranguejo fork)
func takeHealBudget(ts *turnState, missing []string) []string {
	left := maxRevivedDiscoveredTools - len(ts.revivedTools)
	if left <= 0 || len(missing) == 0 {
		return nil
	}
	if len(missing) > left {
		missing = missing[:left]
	}
	if ts.revivedTools == nil {
		ts.revivedTools = make(map[string]struct{}, maxRevivedDiscoveredTools)
	}
	for _, name := range missing {
		ts.revivedTools[name] = struct{}{}
	}
	return missing
}

const revivedToolsNudge = "[System] These tools you discovered earlier were missing from your tools; " +
	"they are available again: %s. If you meant to call one of them, call it now."

// reviveDiscoveredTools offers, before a retry, the tools a tool_search listed
// that this turn does not offer (calling one heals it, listing does not). The
// turn's own set decides, not the registry's TTL: a tool another session
// promoted after this turn started is live in the registry and still missing
// from this turn's tools.
func (p *Pipeline) reviveDiscoveredTools(ts *turnState, exec *turnExecution, iteration int) {
	discovered := discoveredToolNamesFromMessages(exec.messages)
	if len(discovered) == 0 {
		return
	}
	offered := ts.offeredToolSet()
	var missing []string
	for _, name := range ts.agent.Tools.HiddenNames(discovered) {
		if _, ok := offered[name]; !ok {
			missing = append(missing, name)
		}
	}
	missing = takeHealBudget(ts, missing)
	if len(missing) == 0 {
		return
	}
	// Keeps the registry in step for what reads it (the executor, other turns).
	ts.agent.Tools.EnsureVisible(missing, discoveryPromoteTTL(p.Cfg))
	ts.offerTools(missing)
	exec.transientTurnMessages = append(exec.transientTurnMessages, providers.Message{
		Role:    "user",
		Content: fmt.Sprintf(revivedToolsNudge, strings.Join(missing, ", ")),
	})
	logger.InfoCF("agent", "Offered discovered tools the turn lacked before retry",
		map[string]any{
			"agent_id":  ts.agent.ID,
			"iteration": iteration,
			"tools":     missing,
		})
}
