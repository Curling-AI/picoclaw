package agent

import (
	"fmt"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// connectorRejectionLimit is how many 401s in a row, in different rounds, take a
// connector out: one can be a token about to be refreshed, and the parallel calls
// of one round all see the same token.
const connectorRejectionLimit = 2

type connectorTool interface {
	MCPServerName() string
}

type connectorRejection struct {
	count     int
	iteration int
}

// connectorAccounts is what one turn learned about connector accounts, so the model
// stops spending iterations on a connector whose account does not work. It lives on
// the turn because the registry is shared with the turns of other sessions, and a
// reconnect, which does not reach the pod, only has to wait for the next turn. Only
// the turn's tool loop touches it.
//
// shortcut: every turn that uses a dead connector pays one failed call. Reading the
// account status when the config is built would avoid it, at the cost of more calls
// on Composio's shared per-minute quota; upgrade if the volume stays high.
type connectorAccounts struct {
	retired    map[string]struct{}
	rejections map[string]connectorRejection
}

// note records a call's outcome and says whether it retired the connector.
func (c *connectorAccounts) note(server string, result *tools.ToolResult, iteration int) bool {
	switch result.MCPAccount {
	case tools.MCPAccountMissing:
	case tools.MCPAccountRejected:
		rejection := c.rejections[server]
		if rejection.count > 0 && rejection.iteration == iteration {
			return false
		}
		rejection = connectorRejection{count: rejection.count + 1, iteration: iteration}
		if rejection.count < connectorRejectionLimit {
			if c.rejections == nil {
				c.rejections = map[string]connectorRejection{}
			}
			c.rejections[server] = rejection
			return false
		}
	default:
		if !result.IsError {
			delete(c.rejections, server)
		}
		return false
	}
	if c.retired == nil {
		c.retired = map[string]struct{}{}
	}
	c.retired[server] = struct{}{}
	return true
}

func (c *connectorAccounts) isRetired(server string) bool {
	_, retired := c.retired[server]
	return retired
}

func connectorRetiredNote(server string) string {
	return fmt.Sprintf("[The account of the %s connector is not usable, so its tools are off for the rest of this "+
		"turn. Do not look for another of its tools: tell the user it needs to be connected again.]", server)
}

// connectorServer is the connector a tool belongs to, or "" for any other tool.
func connectorServer(registry *tools.ToolRegistry, toolName string) string {
	if tool, ok := registry.GetRegistered(toolName); ok {
		if connector, ok := tool.(connectorTool); ok {
			return connector.MCPServerName()
		}
	}
	return ""
}

func (ts *turnState) noteConnectorAccount(toolName string, result *tools.ToolResult, iteration int) {
	server := connectorServer(ts.agent.Tools, toolName)
	if server == "" || result == nil || !ts.connectors.note(server, result, iteration) {
		return
	}
	result.ForLLM += "\n\n" + connectorRetiredNote(server)
}

func (ts *turnState) withoutRetiredConnectors(defs []providers.ToolDefinition) []providers.ToolDefinition {
	if len(ts.connectors.retired) == 0 {
		return defs
	}
	kept := make([]providers.ToolDefinition, 0, len(defs))
	for _, def := range defs {
		if !ts.connectors.isRetired(connectorServer(ts.agent.Tools, def.Function.Name)) {
			kept = append(kept, def)
		}
	}
	return kept
}
