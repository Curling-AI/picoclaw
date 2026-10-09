package agent

import "github.com/sipeed/picoclaw/pkg/tools"
import "slices"

// ConnectorReady reports a completed MCP handshake with tools registered in
// the current default agent. Callers serialize this check with config reloads.
func (al *AgentLoop) ConnectorReady(name string) bool {
	cfg := al.GetConfig()
	if cfg == nil || !cfg.Tools.MCP.Enabled || !cfg.Tools.MCP.Servers[name].Enabled {
		return false
	}
	manager := al.mcp.getManager()
	if manager == nil {
		return false
	}
	conn, ok := manager.GetServer(name)
	if !ok || len(conn.Tools) == 0 {
		return false
	}
	registry := al.GetRegistry()
	if registry == nil {
		return false
	}
	a := registry.GetDefaultAgent()
	if a == nil || !a.AllowsMCPServer(name) {
		return false
	}
	for _, tool := range conn.Tools {
		if slices.Contains(a.Tools.List(), tools.NewMCPTool(manager, name, tool).Name()) {
			return true
		}
	}
	return false
}
