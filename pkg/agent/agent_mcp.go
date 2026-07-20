// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/mcp"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type mcpRuntime struct {
	initOnce sync.Once
	mu       sync.Mutex
	manager  *mcp.Manager
	initErr  error
	// retryCancel stops the background reconnect loop for servers that failed
	// at load time; reset/takeManager cancel it so a stale loop never touches
	// a closed manager or a reloaded registry.
	retryCancel context.CancelFunc
}

func (r *mcpRuntime) reset() *mcp.Manager {
	r.mu.Lock()
	manager := r.manager
	r.manager = nil
	r.initErr = nil
	r.initOnce = sync.Once{}
	cancel := r.retryCancel
	r.retryCancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return manager
}

func (r *mcpRuntime) setRetryCancel(cancel context.CancelFunc) {
	r.mu.Lock()
	previous := r.retryCancel
	r.retryCancel = cancel
	r.mu.Unlock()
	if previous != nil {
		previous()
	}
}

func (r *mcpRuntime) setManager(manager *mcp.Manager) {
	r.mu.Lock()
	r.manager = manager
	r.initErr = nil
	r.mu.Unlock()
}

func (r *mcpRuntime) setInitErr(err error) {
	r.mu.Lock()
	r.initErr = err
	r.mu.Unlock()
}

func (r *mcpRuntime) getInitErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.initErr
}

func (r *mcpRuntime) takeManager() *mcp.Manager {
	r.mu.Lock()
	manager := r.manager
	r.manager = nil
	cancel := r.retryCancel
	r.retryCancel = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return manager
}

func (r *mcpRuntime) hasManager() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manager != nil
}

func (r *mcpRuntime) getManager() *mcp.Manager {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manager
}

// ensureMCPInitialized loads MCP servers/tools once so both Run() and direct
// agent mode share the same initialization path.
func (al *AgentLoop) ensureMCPInitialized(ctx context.Context) error {
	if !al.cfg.Tools.IsToolEnabled("mcp") {
		return nil
	}

	if al.cfg.Tools.MCP.Servers == nil || len(al.cfg.Tools.MCP.Servers) == 0 {
		logger.WarnCF("agent", "MCP is enabled but no servers are configured, skipping MCP initialization", nil)
		return nil
	}

	mcpCfg := filterMCPConfigServers(al.cfg.Tools.MCP, al.registry.allowedMCPServers())
	if mcpCfg.Servers == nil || len(mcpCfg.Servers) == 0 {
		logger.InfoCF(
			"agent",
			"No MCP servers selected after applying per-agent mcpServers allowlists",
			nil,
		)
		return nil
	}

	findValidServer := false
	for _, serverCfg := range mcpCfg.Servers {
		if serverCfg.Enabled {
			findValidServer = true
		}
	}
	if !findValidServer {
		logger.WarnCF("agent", "MCP is enabled but no valid servers are configured, skipping MCP initialization", nil)
		return nil
	}

	al.mcp.initOnce.Do(func() {
		mcpManager := mcp.NewManager(mcp.WithRuntimeEvents(al.runtimeEvents))

		defaultAgent := al.registry.GetDefaultAgent()
		workspacePath := al.cfg.WorkspacePath()
		if defaultAgent != nil && defaultAgent.Workspace != "" {
			workspacePath = defaultAgent.Workspace
		}

		if err := mcpManager.LoadFromMCPConfig(ctx, mcpCfg, workspacePath); err != nil {
			// A failed MCP connection (e.g. an unauthorized connector returning
			// 401) must NOT be fatal. ensureMCPInitialized runs at the top of
			// Run() and on every direct turn, so propagating this error would
			// kill the agent loop / abort every message — one broken connector
			// would silently brick the whole assistant. Degrade gracefully:
			// warn, keep the (empty) manager alive so the background retry loop
			// below can bring servers up when the credential is fixed, and let
			// the agent keep serving. We do NOT setInitErr here, so callers
			// proceed without MCP.
			logger.WarnCF(
				"agent",
				"Failed to load MCP servers, continuing without MCP tools while retrying in background",
				map[string]any{
					"error": err.Error(),
				},
			)
		}

		// Register MCP tools for all agents
		servers := mcpManager.GetServers()
		uniqueTools := 0
		totalRegistrations := 0
		agentIDs := al.registry.ListAgentIDs()
		agentCount := len(agentIDs)

		for serverName, conn := range servers {
			toolCount, registrations := al.registerMCPServerTools(mcpManager, mcpCfg, serverName, conn)
			uniqueTools += toolCount
			totalRegistrations += registrations
		}
		logger.InfoCF("agent", "MCP tools registered successfully",
			map[string]any{
				"server_count":        len(servers),
				"unique_tools":        uniqueTools,
				"total_registrations": totalRegistrations,
				"agent_count":         agentCount,
			})

		// Initializes Discovery Tools only if enabled by configuration
		if al.cfg.Tools.MCP.Enabled && al.cfg.Tools.MCP.Discovery.Enabled {
			useBM25 := al.cfg.Tools.MCP.Discovery.UseBM25
			useRegex := al.cfg.Tools.MCP.Discovery.UseRegex

			// Fail fast: If discovery is enabled but no search method is turned on
			if !useBM25 && !useRegex {
				al.mcp.setInitErr(fmt.Errorf(
					"tool discovery is enabled but neither 'use_bm25' nor 'use_regex' is set to true in the configuration",
				))
				if closeErr := mcpManager.Close(); closeErr != nil {
					logger.ErrorCF("agent", "Failed to close MCP manager",
						map[string]any{
							"error": closeErr.Error(),
						})
				}
				return
			}

			ttl := discoveryPromoteTTL(al.cfg)

			maxSearchResults := al.cfg.Tools.MCP.Discovery.MaxSearchResults
			if maxSearchResults <= 0 {
				// 8 by default: with several MCP servers a pod holds 40-60+
				// hidden tools and same-family siblings crowd the ranking; 5
				// slots left the wanted tool out in real sessions. Results are
				// name+description only, so the extra context cost is small.
				maxSearchResults = 8
			}

			logger.InfoCF("agent", "Initializing tool discovery", map[string]any{
				"bm25": useBM25, "regex": useRegex, "ttl": ttl, "max_results": maxSearchResults,
			})

			for _, agentID := range agentIDs {
				agent, ok := al.registry.GetAgent(agentID)
				if !ok {
					continue
				}
				if !agentHasDiscoverableMCPServers(al.cfg, agent.MCPServerAllowlist) {
					continue
				}

				if useRegex {
					agent.Tools.Register(tools.NewRegexSearchTool(agent.Tools, ttl, maxSearchResults))
				}
				if useBM25 {
					agent.Tools.Register(tools.NewBM25SearchTool(agent.Tools, ttl, maxSearchResults))
				}
			}
		}

		al.mcp.setManager(mcpManager)

		// Servers that failed to connect (expired OAuth grant, connector down)
		// keep retrying in the background and register their tools once they
		// come back — without this, a server that fails here stays without
		// tools until the next process restart even after the user fixes the
		// credential.
		if pending := pendingMCPServers(mcpCfg, mcpManager); len(pending) > 0 {
			logger.InfoCF("agent", "Scheduling background retry for MCP servers that failed to connect",
				map[string]any{"servers": pending})
			retryCtx, cancel := context.WithCancel(context.Background())
			al.mcp.setRetryCancel(cancel)
			go mcpManager.RetryPendingServers(retryCtx, mcpCfg, workspacePath, pending,
				func(serverName string, conn *mcp.ServerConnection) {
					toolCount, registrations := al.registerMCPServerTools(mcpManager, mcpCfg, serverName, conn)
					logger.InfoCF("agent", "MCP tools registered after background retry",
						map[string]any{
							"server":              serverName,
							"unique_tools":        toolCount,
							"total_registrations": registrations,
						})
				})
		}
	})

	return al.mcp.getInitErr()
}

// registerMCPServerTools registers one connected server's tools (and its
// prompt contributor) on every agent that allows the server. It reads the
// registry through GetRegistry so the background retry loop registers on
// whatever registry is current. Returns the server's tool count and the number
// of registrations made across agents.
func (al *AgentLoop) registerMCPServerTools(
	mcpManager *mcp.Manager,
	mcpCfg config.MCPConfig,
	serverName string,
	conn *mcp.ServerConnection,
) (int, int) {
	registry := al.GetRegistry()
	if registry == nil || conn == nil {
		return 0, 0
	}
	agentIDs := registry.ListAgentIDs()

	// Determine whether this server's tools should be deferred (hidden).
	// Per-server "deferred" field takes precedence over the global Discovery.Enabled.
	serverCfg := mcpCfg.Servers[serverName]
	registerAsHidden := serverIsDeferred(mcpCfg.Discovery.Enabled, serverCfg)
	registeredToolsByAgent := make(map[string]map[string]struct{}, len(agentIDs))
	totalRegistrations := 0

	for _, tool := range conn.Tools {
		for _, agentID := range agentIDs {
			agent, ok := registry.GetAgent(agentID)
			if !ok {
				continue
			}
			if !agent.AllowsMCPServer(serverName) {
				logger.DebugCF("agent", "Skipped MCP tool registration by agent mcpServers allowlist",
					map[string]any{
						"agent_id": agentID,
						"server":   serverName,
						"tool":     tool.Name,
					})
				continue
			}

			mcpTool := tools.NewMCPTool(mcpManager, serverName, tool)
			toolName := mcpTool.Name()
			mcpTool.SetWorkspace(agent.Workspace)
			mcpTool.SetMaxInlineTextRunes(mcpCfg.GetMaxInlineTextChars())
			mcpTool.SetEventPublisher(al.runtimeEvents)

			if registerAsHidden {
				agent.Tools.RegisterHidden(mcpTool)
			} else {
				agent.Tools.Register(mcpTool)
			}
			if !toolRegistryIncludes(agent.Tools, toolName) {
				continue
			}

			recordRegisteredMCPTool(registeredToolsByAgent, agentID, toolName)
			totalRegistrations++
			logger.DebugCF("agent", "Registered MCP tool",
				map[string]any{
					"agent_id": agentID,
					"server":   serverName,
					"tool":     tool.Name,
					"name":     toolName,
					"deferred": registerAsHidden,
				})
		}
	}

	for _, agentID := range agentIDs {
		agent, ok := registry.GetAgent(agentID)
		if !ok {
			continue
		}
		registerMCPServerPromptContributor(
			agentID,
			agent,
			serverName,
			sortedToolNames(registeredToolsByAgent[agentID]),
			registerAsHidden,
		)
	}

	return len(conn.Tools), totalRegistrations
}

// pendingMCPServers lists enabled servers the manager has no live connection
// for — the candidates for the background retry loop.
func pendingMCPServers(mcpCfg config.MCPConfig, mcpManager *mcp.Manager) []string {
	var pending []string
	for name, serverCfg := range mcpCfg.Servers {
		if !serverCfg.Enabled {
			continue
		}
		if _, ok := mcpManager.GetServer(name); !ok {
			pending = append(pending, name)
		}
	}
	return pending
}

// sortedToolNames flattens a registered-tools set into a sorted slice —
// deterministic order keeps the contributed prompt part stable across
// restarts (it is cacheable).
func sortedToolNames(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func registerMCPServerPromptContributor(
	agentID string,
	agent *AgentInstance,
	serverName string,
	toolNames []string,
	registerAsHidden bool,
) {
	if agent == nil || agent.ContextBuilder == nil || len(toolNames) == 0 {
		return
	}
	if err := agent.ContextBuilder.RegisterPromptContributor(mcpServerPromptContributor{
		serverName: serverName,
		toolCount:  len(toolNames),
		deferred:   registerAsHidden,
		toolNames:  toolNames,
	}); err != nil {
		logger.WarnCF("agent", "Failed to register MCP prompt contributor",
			map[string]any{
				"agent_id": agentID,
				"server":   serverName,
				"error":    err.Error(),
			})
	}
}

func recordRegisteredMCPTool(
	registeredToolsByAgent map[string]map[string]struct{},
	agentID, toolName string,
) {
	if registeredToolsByAgent[agentID] == nil {
		registeredToolsByAgent[agentID] = make(map[string]struct{})
	}
	registeredToolsByAgent[agentID][toolName] = struct{}{}
}

func toolRegistryIncludes(registry *tools.ToolRegistry, name string) bool {
	if registry == nil {
		return false
	}
	return registry.HasRegistered(name)
}

func filterMCPConfigServers(
	mcpCfg config.MCPConfig,
	allowed map[string]struct{},
) config.MCPConfig {
	if allowed == nil {
		return mcpCfg
	}

	filtered := mcpCfg
	filtered.Servers = make(map[string]config.MCPServerConfig)
	normalizedAllowed := make(map[string]struct{}, len(allowed))
	for serverName := range allowed {
		name := normalizeMCPServerName(serverName)
		if name == "" {
			continue
		}
		normalizedAllowed[name] = struct{}{}
	}
	for serverName, serverCfg := range mcpCfg.Servers {
		if _, ok := normalizedAllowed[normalizeMCPServerName(serverName)]; ok {
			filtered.Servers[serverName] = serverCfg
		}
	}

	return filtered
}

func agentHasDiscoverableMCPServers(cfg *config.Config, allowed map[string]struct{}) bool {
	if cfg == nil || !cfg.Tools.MCP.Enabled || !cfg.Tools.MCP.Discovery.Enabled {
		return false
	}

	filtered := filterMCPConfigServers(cfg.Tools.MCP, allowed)
	for _, serverCfg := range filtered.Servers {
		if serverCfg.Enabled && serverIsDeferred(cfg.Tools.MCP.Discovery.Enabled, serverCfg) {
			return true
		}
	}

	return false
}

// serverIsDeferred reports whether an MCP server's tools should be registered
// as hidden (deferred/discovery mode).
//
// The per-server Deferred field takes precedence over the global discoveryEnabled
// default. When Deferred is nil, discoveryEnabled is used as the fallback.
func serverIsDeferred(discoveryEnabled bool, serverCfg config.MCPServerConfig) bool {
	if !discoveryEnabled {
		return false
	}
	if serverCfg.Deferred != nil {
		return *serverCfg.Deferred
	}
	return true
}

// discoveryPromoteTTL returns the configured tool-discovery promotion TTL (in
// tool rounds), with the same default the discovery search tools use.
func discoveryPromoteTTL(cfg *config.Config) int {
	ttl := cfg.Tools.MCP.Discovery.TTL
	if ttl <= 0 {
		ttl = 5
	}
	return ttl
}
