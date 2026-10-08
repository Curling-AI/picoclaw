package tools

import "testing"

type serverNamedMockTool struct {
	mockRegistryTool
	serverName string
}

func (s *serverNamedMockTool) ServerToolName() string { return s.serverName }

func hiddenServerTool(name, serverName string) *serverNamedMockTool {
	return &serverNamedMockTool{
		mockRegistryTool: mockRegistryTool{name: name, desc: "[MCP] test"},
		serverName:       serverName,
	}
}

func TestGetRegistered_ReturnsExpiredHiddenTools(t *testing.T) {
	r := newTTLTestRegistry(t)

	if _, ok := r.Get("mcp_a"); ok {
		t.Fatal("precondition: mcp_a is expired and not callable")
	}
	if tool, ok := r.GetRegistered("mcp_a"); !ok || tool.Name() != "mcp_a" {
		t.Fatalf("GetRegistered(mcp_a) = %v, %v; want the expired tool", tool, ok)
	}
	if _, ok := r.GetRegistered("unknown"); ok {
		t.Fatal("GetRegistered(unknown) found a tool")
	}
}

func TestExpiredToolAliases(t *testing.T) {
	r := NewToolRegistry()
	r.Register(&mockRegistryTool{name: "read_file", desc: "core"})
	r.RegisterHidden(hiddenServerTool("mcp_skip_skip_project_status", "skip_project_status"))
	r.RegisterHidden(hiddenServerTool("mcp_skip_skip_project_create", "skip_project_create"))
	r.RegisterHidden(hiddenServerTool("mcp_linear_get_issue", "get_issue"))
	r.RegisterHidden(hiddenServerTool("mcp_jira_get_issue", "get_issue"))
	r.RegisterHidden(hiddenServerTool("mcp_web_search", "search"))
	r.RegisterHidden(hiddenServerTool("mcp_fs_read_file", "read_file"))
	r.RegisterHidden(hiddenServerTool("mcp_crm_Get-Contact", "Get-Contact"))
	r.PromoteTools([]string{"mcp_skip_skip_project_create"}, 5)

	aliases := r.ExpiredToolAliases()

	want := map[string]string{
		"mcp_skip_skip_project_status": "mcp_skip_skip_project_status",
		"skip_project_status":          "mcp_skip_skip_project_status",
		"mcp_linear_get_issue":         "mcp_linear_get_issue",
		"mcp_jira_get_issue":           "mcp_jira_get_issue",
		"mcp_web_search":               "mcp_web_search",
		"mcp_fs_read_file":             "mcp_fs_read_file",
		"mcp_crm_get-contact":          "mcp_crm_Get-Contact",
		"get-contact":                  "mcp_crm_Get-Contact",
	}
	for alias, name := range want {
		if got := aliases[alias]; got != name {
			t.Errorf("aliases[%q] = %q, want %q", alias, got, name)
		}
	}
	for _, absent := range []string{
		"skip_project_create",          // live: nothing to heal
		"mcp_skip_skip_project_create", // live
		"get_issue",                    // two servers have it
		"search",                       // plain word
		"read_file",                    // also a core tool
	} {
		if name, ok := aliases[absent]; ok {
			t.Errorf("aliases[%q] = %q, want no entry", absent, name)
		}
	}
	if len(aliases) != len(want) {
		t.Errorf("aliases = %v, want exactly %v", aliases, want)
	}
}
