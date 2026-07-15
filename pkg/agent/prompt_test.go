package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestPromptRegistry_RejectsRegisteredSourceWrongPlacement(t *testing.T) {
	registry := NewPromptRegistry()
	if err := registry.RegisterSource(PromptSourceDescriptor{
		ID:      "test:source",
		Owner:   "test",
		Allowed: []PromptPlacement{{Layer: PromptLayerCapability, Slot: PromptSlotTooling}},
	}); err != nil {
		t.Fatalf("RegisterSource() error = %v", err)
	}

	err := registry.ValidatePart(PromptPart{
		ID:      "wrong.placement",
		Layer:   PromptLayerContext,
		Slot:    PromptSlotRuntime,
		Source:  PromptSource{ID: "test:source"},
		Content: "runtime text",
	})
	if err == nil {
		t.Fatal("ValidatePart() error = nil, want placement error")
	}
}

func TestPromptRegistry_AllowsUnregisteredSourceInCompatibilityMode(t *testing.T) {
	registry := NewPromptRegistry()

	err := registry.ValidatePart(PromptPart{
		ID:      "unregistered.part",
		Layer:   PromptLayerCapability,
		Slot:    PromptSlotMCP,
		Source:  PromptSource{ID: "mcp:dynamic-server"},
		Content: "dynamic MCP prompt",
	})
	if err != nil {
		t.Fatalf("ValidatePart() error = %v, want nil for unregistered source", err)
	}
}

func TestRenderPromptPartsLegacy_UsesLayerAndSlotOrder(t *testing.T) {
	parts := []PromptPart{
		{
			ID:      "context.runtime",
			Layer:   PromptLayerContext,
			Slot:    PromptSlotRuntime,
			Source:  PromptSource{ID: PromptSourceRuntime},
			Content: "runtime",
		},
		{
			ID:      "kernel.identity",
			Layer:   PromptLayerKernel,
			Slot:    PromptSlotIdentity,
			Source:  PromptSource{ID: PromptSourceKernel},
			Content: "kernel",
		},
		{
			ID:      "capability.skill",
			Layer:   PromptLayerCapability,
			Slot:    PromptSlotActiveSkill,
			Source:  PromptSource{ID: "skill:test"},
			Content: "skill",
		},
		{
			ID:      "instruction.workspace",
			Layer:   PromptLayerInstruction,
			Slot:    PromptSlotWorkspace,
			Source:  PromptSource{ID: PromptSourceWorkspace},
			Content: "workspace",
		},
	}

	got := renderPromptPartsLegacy(parts)
	want := strings.Join([]string{"kernel", "workspace", "skill", "runtime"}, "\n\n---\n\n")
	if got != want {
		t.Fatalf("renderPromptPartsLegacy() = %q, want %q", got, want)
	}
}

func TestBuildMessagesFromPrompt_IncludesSystemPromptOverlay(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage: "do child task",
		Overlays: promptOverlaysForOptions(processOptions{
			SystemPromptOverride: "Use child-only system instructions.",
		}),
	})

	if len(messages) < 2 {
		t.Fatalf("messages len = %d, want at least 2", len(messages))
	}
	if messages[0].Role != "system" {
		t.Fatalf("messages[0].Role = %q, want system", messages[0].Role)
	}
	if !strings.Contains(messages[0].Content, "Use child-only system instructions.") {
		t.Fatalf("system prompt missing overlay: %q", messages[0].Content)
	}
	// The turn's precise time is prepended to the current message (turn tail),
	// not the cached system prefix — so the exact user text follows the hint.
	if messages[1].Role != "user" || !strings.HasSuffix(messages[1].Content, "do child task") {
		t.Fatalf("messages[1] = %#v, want user task", messages[1])
	}
}

func TestBuildMessagesFromPrompt_AttachesInternalPromptMetadata(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage: "hello",
		Summary:        "prior context",
	})
	if len(messages) != 2 {
		t.Fatalf("messages len = %d, want 2", len(messages))
	}

	system := messages[0]
	if len(system.SystemParts) < 3 {
		t.Fatalf("system parts len = %d, want at least 3", len(system.SystemParts))
	}
	if system.SystemParts[0].PromptLayer != string(PromptLayerKernel) ||
		system.SystemParts[0].PromptSlot != string(PromptSlotIdentity) ||
		system.SystemParts[0].PromptSource != string(PromptSourceKernel) {
		t.Fatalf("static system metadata = %#v, want kernel identity", system.SystemParts[0])
	}

	var hasRuntime, hasSummary bool
	for _, part := range system.SystemParts {
		switch part.PromptSource {
		case string(PromptSourceRuntime):
			hasRuntime = true
			if part.CacheControl != nil {
				t.Fatalf("runtime cache control = %#v, want nil", part.CacheControl)
			}
		case string(PromptSourceSummary):
			hasSummary = true
			if part.CacheControl != nil {
				t.Fatalf("summary cache control = %#v, want nil", part.CacheControl)
			}
		}
	}
	if !hasRuntime {
		t.Fatal("system parts missing runtime prompt metadata")
	}
	if !hasSummary {
		t.Fatal("system parts missing summary prompt metadata")
	}

	user := messages[1]
	if user.PromptLayer != string(PromptLayerTurn) ||
		user.PromptSlot != string(PromptSlotMessage) ||
		user.PromptSource != string(PromptSourceUserMessage) {
		t.Fatalf("user message metadata = %#v, want turn message", user)
	}

	data, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(data), "PromptSource") ||
		strings.Contains(string(data), "PromptLayer") ||
		strings.Contains(string(data), "PromptSlot") {
		t.Fatalf("internal prompt metadata leaked into JSON: %s", data)
	}
}

func TestContextBuilder_CollectsToolDiscoveryContributor(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir()).WithToolDiscovery(true, false)

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})
	system := messages[0]
	if !strings.Contains(system.Content, "tool_search_tool_bm25") {
		t.Fatalf("system prompt missing tool discovery rule: %q", system.Content)
	}

	var found bool
	for _, part := range system.SystemParts {
		if part.PromptSource == string(PromptSourceToolDiscovery) {
			found = true
			if part.PromptLayer != string(PromptLayerCapability) || part.PromptSlot != string(PromptSlotTooling) {
				t.Fatalf("tool discovery metadata = %#v, want capability/tooling", part)
			}
			if part.CacheControl == nil || part.CacheControl.Type != "ephemeral" {
				t.Fatalf("tool discovery cache control = %#v, want ephemeral", part.CacheControl)
			}
		}
	}
	if !found {
		t.Fatal("system parts missing tool discovery prompt metadata")
	}
}

func TestContextBuilder_SuppressesToolDiscoveryContributorWhenToolsUnavailable(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir()).WithToolDiscovery(true, false)

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage:      "hello",
		SuppressToolUseRule: true,
	})
	system := messages[0]
	if strings.Contains(system.Content, "tool_search_tool_bm25") {
		t.Fatalf("system prompt includes tool discovery despite tools being unavailable: %q", system.Content)
	}
	for _, part := range system.SystemParts {
		if part.PromptSource == string(PromptSourceToolDiscovery) {
			t.Fatalf("system parts include tool discovery despite tools being unavailable: %#v", part)
		}
	}
}

func TestContextBuilder_SuppressesToolReferencesWhenToolsUnavailable(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	writeTurnProfileSkill(
		t,
		workspace,
		"research",
		"---\ndescription: research skill\n---\n# research\n\nResearch carefully.",
	)
	cb := NewContextBuilder(workspace)

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage:      "hello",
		SuppressToolUseRule: true,
	})
	system := messages[0]
	if strings.Contains(system.Content, "When using tools") ||
		strings.Contains(system.Content, "read_file tool") ||
		strings.Contains(system.Content, "update "+workspace+"/memory/MEMORY.md") {
		t.Fatalf("system prompt includes tool references despite tools being unavailable: %q", system.Content)
	}
	if !strings.Contains(system.Content, "<name>research</name>") {
		t.Fatalf("system prompt should keep non-tool skill catalog context, got: %q", system.Content)
	}
}

func TestContextBuilder_CustomToolAllowListSuppressesReadFileSkillInstruction(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	writeTurnProfileSkill(
		t,
		workspace,
		"research",
		"---\ndescription: research skill\n---\n# research\n\nResearch carefully.",
	)
	cb := NewContextBuilder(workspace)

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage: "hello",
		AllowedTools:   []string{"web_search"},
	})
	system := messages[0]
	if strings.Contains(system.Content, "read_file tool") {
		t.Fatalf("system prompt includes read_file skill instruction without read_file permission: %q", system.Content)
	}
	if !strings.Contains(system.Content, "<name>research</name>") {
		t.Fatalf("system prompt should keep skill catalog context, got: %q", system.Content)
	}
}

func TestContextBuilder_CollectsMCPServerContributor(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())
	err := cb.RegisterPromptContributor(mcpServerPromptContributor{
		serverName: "GitHub Server",
		toolCount:  3,
		deferred:   true,
	})
	if err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})
	system := messages[0]
	if !strings.Contains(system.Content, "MCP server `GitHub Server` is connected") {
		t.Fatalf("system prompt missing MCP contributor content: %q", system.Content)
	}

	var found bool
	for _, part := range system.SystemParts {
		if part.PromptSource == "mcp:github_server" {
			found = true
			if part.PromptLayer != string(PromptLayerCapability) || part.PromptSlot != string(PromptSlotMCP) {
				t.Fatalf("mcp metadata = %#v, want capability/mcp", part)
			}
			if part.CacheControl == nil || part.CacheControl.Type != "ephemeral" {
				t.Fatalf("mcp cache control = %#v, want ephemeral", part.CacheControl)
			}
		}
	}
	if !found {
		t.Fatal("system parts missing MCP prompt metadata")
	}
}

// A deferred server must enumerate its tool NAMES: the bare count leaves the
// model unable to know a hidden tool exists, so it treats the last search
// result as the whole universe and wrongly refuses requests it could serve.
func TestContextBuilder_MCPServerContributorListsDeferredToolNames(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())
	err := cb.RegisterPromptContributor(mcpServerPromptContributor{
		serverName: "slack",
		toolCount:  2,
		deferred:   true,
		toolNames:  []string{"mcp_slack_read_channel", "mcp_slack_send_message"},
	})
	if err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})
	system := messages[0]
	for _, want := range []string{
		"`mcp_slack_read_channel`",
		"`mcp_slack_send_message`",
		"unlock it by searching its name",
	} {
		if !strings.Contains(system.Content, want) {
			t.Fatalf("system prompt missing %q: %q", want, system.Content)
		}
	}
}

// Native (non-deferred) servers must NOT enumerate names — their tools already
// surface full schemas, and the list would only duplicate tokens.
func TestContextBuilder_MCPServerContributorOmitsNativeToolNames(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())
	err := cb.RegisterPromptContributor(mcpServerPromptContributor{
		serverName: "slack",
		toolCount:  2,
		deferred:   false,
		toolNames:  []string{"mcp_slack_read_channel", "mcp_slack_send_message"},
	})
	if err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})
	system := messages[0]
	if strings.Contains(system.Content, "Its tool names") ||
		strings.Contains(system.Content, "mcp_slack_send_message") {
		t.Fatalf("native server should not enumerate tool names: %q", system.Content)
	}
}

// Above the cap, the list truncates with an explicit "and N more" so a huge
// catalog can't reintroduce the prompt bloat discovery exists to prevent.
func TestContextBuilder_MCPServerContributorCapsDeferredToolNames(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	names := make([]string, maxDeferredToolNamesInPrompt+3)
	for i := range names {
		names[i] = fmt.Sprintf("mcp_big_tool_%03d", i)
	}
	cb := NewContextBuilder(t.TempDir())
	err := cb.RegisterPromptContributor(mcpServerPromptContributor{
		serverName: "big",
		toolCount:  len(names),
		deferred:   true,
		toolNames:  names,
	})
	if err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})
	system := messages[0]
	if !strings.Contains(system.Content, "and 3 more (searchable)") {
		t.Fatalf("system prompt missing truncation suffix: %q", system.Content)
	}
	if strings.Contains(system.Content, names[maxDeferredToolNamesInPrompt]) {
		t.Fatalf("system prompt should not list names beyond the cap: %q", system.Content)
	}
}

func TestContextBuilder_SuppressesMCPServerContributorWhenToolsUnavailable(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())
	err := cb.RegisterPromptContributor(mcpServerPromptContributor{
		serverName: "GitHub Server",
		toolCount:  3,
		deferred:   false,
	})
	if err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage:      "hello",
		SuppressToolUseRule: true,
	})
	system := messages[0]
	if strings.Contains(system.Content, "MCP server `GitHub Server` is connected") ||
		strings.Contains(system.Content, "available as native tools") {
		t.Fatalf("system prompt includes MCP tooling despite tools being unavailable: %q", system.Content)
	}
	for _, part := range system.SystemParts {
		if part.PromptSource == "mcp:github_server" {
			t.Fatalf("system parts include MCP tooling despite tools being unavailable: %#v", part)
		}
	}
}

func TestContextBuilder_SuppressesAgentDiscoveryContributorWhenToolsUnavailable(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir()).WithAgentDiscovery(
		"main",
		func(agentID string) []AgentDescriptor {
			return []AgentDescriptor{{
				ID:          "helper",
				Name:        "Helper",
				Description: "Helps with tasks",
			}}
		},
	)

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage:      "hello",
		SuppressToolUseRule: true,
	})
	system := messages[0]
	if strings.Contains(system.Content, "Agent Discovery") ||
		strings.Contains(system.Content, "calling spawn") {
		t.Fatalf("system prompt includes agent discovery despite tools being unavailable: %q", system.Content)
	}
	for _, part := range system.SystemParts {
		if part.PromptSource == string(PromptSourceAgentDiscovery) {
			t.Fatalf("system parts include agent discovery despite tools being unavailable: %#v", part)
		}
	}
}

func TestContextBuilder_CustomToolAllowListSuppressesUnallowedToolContributors(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir()).
		WithToolDiscovery(true, true).
		WithAgentDiscovery(
			"main",
			func(agentID string) []AgentDescriptor {
				return []AgentDescriptor{{
					ID:          "helper",
					Name:        "Helper",
					Description: "Helps with tasks",
				}}
			},
		)
	err := cb.RegisterPromptContributor(mcpServerPromptContributor{
		serverName: "GitHub Server",
		toolCount:  3,
		deferred:   false,
	})
	if err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{
		CurrentMessage: "hello",
		AllowedTools:   []string{"echo_text"},
	})
	system := messages[0]
	blockedSnippets := []string{
		"tool_search_tool_bm25",
		"tool_search_tool_regex",
		"MCP server `GitHub Server` is connected",
		"Agent Discovery",
		"calling spawn",
	}
	for _, snippet := range blockedSnippets {
		if strings.Contains(system.Content, snippet) {
			t.Fatalf("system prompt includes unallowed tool contributor %q: %q", snippet, system.Content)
		}
	}
	for _, part := range system.SystemParts {
		switch part.PromptSource {
		case string(PromptSourceToolDiscovery), string(PromptSourceAgentDiscovery), "mcp:github_server":
			t.Fatalf("system parts include unallowed tool contributor: %#v", part)
		}
	}
}

type testPromptContributor struct {
	desc PromptSourceDescriptor
	part PromptPart
}

func (c testPromptContributor) PromptSource() PromptSourceDescriptor {
	return c.desc
}

func (c testPromptContributor) ContributePrompt(_ context.Context, _ PromptBuildRequest) ([]PromptPart, error) {
	return []PromptPart{c.part}, nil
}

func TestContextBuilder_CollectsRegisteredPromptContributors(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())

	sourceID := PromptSourceID("test:contributor")
	err := cb.RegisterPromptContributor(testPromptContributor{
		desc: PromptSourceDescriptor{
			ID:      sourceID,
			Owner:   "test",
			Allowed: []PromptPlacement{{Layer: PromptLayerCapability, Slot: PromptSlotMCP}},
		},
		part: PromptPart{
			ID:      "capability.mcp.test",
			Layer:   PromptLayerCapability,
			Slot:    PromptSlotMCP,
			Source:  PromptSource{ID: sourceID, Name: "test"},
			Content: "registered contributor prompt",
		},
	})
	if err != nil {
		t.Fatalf("RegisterPromptContributor() error = %v", err)
	}

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})
	if !strings.Contains(messages[0].Content, "registered contributor prompt") {
		t.Fatalf("system prompt missing contributor content: %q", messages[0].Content)
	}
}

func TestBuildMessages_TimeInTurnTailNotSystemPrefix(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	cb := NewContextBuilder(t.TempDir())

	messages := cb.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hello"})
	if len(messages) < 2 {
		t.Fatalf("messages len = %d, want >= 2", len(messages))
	}
	system := messages[0]
	user := messages[len(messages)-1]

	// The cacheable system prefix carries only the date (day granularity), never
	// a minute-precision clock — a minute change must not invalidate the prefix.
	if !strings.Contains(system.Content, "## Current Date") {
		t.Errorf("system prompt missing Current Date block: %q", system.Content)
	}
	if strings.Contains(system.Content, "## Current Time") {
		t.Errorf("system prompt still carries minute-precision time (breaks cache): %q", system.Content)
	}
	// The precise time rides in the turn tail (current user message).
	if !strings.HasPrefix(user.Content, "[Current time:") {
		t.Errorf("current message missing time hint: %q", user.Content)
	}
	if !strings.HasSuffix(user.Content, "hello") {
		t.Errorf("current message lost user text: %q", user.Content)
	}
}

func TestSkillDiscovery_DefersCatalogToHint(t *testing.T) {
	t.Setenv("PICOCLAW_BUILTIN_SKILLS", t.TempDir())
	ws := t.TempDir()
	writeTurnProfileSkill(t, ws, "pdf-extract",
		"---\nname: pdf-extract\ndescription: Extract text from PDF files\n---\n# pdf")

	// Default (discovery off): full catalog inlined.
	full := NewContextBuilder(ws)
	m1 := full.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hi"})
	if !strings.Contains(m1[0].Content, "<skills>") || !strings.Contains(m1[0].Content, "Extract text from PDF") {
		t.Fatalf("discovery off should inline the full catalog: %q", m1[0].Content)
	}

	// Discovery on: only a one-line hint, no per-skill <skills> block.
	lean := NewContextBuilder(ws).WithSkillDiscovery(true)
	m2 := lean.BuildMessagesFromPrompt(PromptBuildRequest{CurrentMessage: "hi"})
	sys := m2[0].Content
	if strings.Contains(sys, "<skills>") || strings.Contains(sys, "Extract text from PDF") {
		t.Fatalf("discovery on should NOT inline the catalog: %q", sys)
	}
	if !strings.Contains(sys, "find_installed_skills") || !strings.Contains(sys, "1 installed skill") {
		t.Fatalf("discovery on should show a find_installed_skills hint with the count: %q", sys)
	}
}
