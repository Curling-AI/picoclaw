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

// reviveWithinTurnBudget revives the expired tools among names. Each distinct
// tool is charged once per turn: one the turn already brought back returns
// free when it expires again — a long turn must not lose its read tools and
// keep only the create, as in the incident — and new ones fill what is left
// of the page. (seucaranguejo fork)
func reviveWithinTurnBudget(ts *turnState, names []string, ttl int) []string {
	var paid, unpaid []string
	for _, name := range names {
		if _, ok := ts.revivedTools[name]; ok {
			paid = append(paid, name)
		} else {
			unpaid = append(unpaid, name)
		}
	}
	revived := ts.agent.Tools.ReviveExpired(paid, ttl, 0)
	left := maxRevivedDiscoveredTools - len(ts.revivedTools)
	if left <= 0 {
		return revived
	}
	fresh := ts.agent.Tools.ReviveExpired(unpaid, ttl, left)
	if len(fresh) > 0 && ts.revivedTools == nil {
		ts.revivedTools = make(map[string]struct{}, maxRevivedDiscoveredTools)
	}
	for _, name := range fresh {
		ts.revivedTools[name] = struct{}{}
	}
	return append(revived, fresh...)
}

const revivedToolsNudge = "[System] These tools you discovered earlier had expired and were missing from your " +
	"tools; they are available again: %s. If you meant to call one of them, call it now."

// reviveDiscoveredTools re-promotes expired tools that tool_search listed but the
// model never called (EnsureVisible only heals called ones), before a retry.
func (p *Pipeline) reviveDiscoveredTools(ts *turnState, exec *turnExecution, iteration int) {
	discovered := discoveredToolNamesFromMessages(exec.messages)
	if len(discovered) == 0 {
		return
	}
	revived := reviveWithinTurnBudget(ts, discovered, discoveryPromoteTTL(p.Cfg))
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
