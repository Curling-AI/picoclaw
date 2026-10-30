package integrationtools

// MCPServerName exposes the canonical identity rather than parsing a display
// name whose sanitization can be lossy or collide with another server prefix.
func (t *MCPTool) MCPServerName() string { return t.serverName }
