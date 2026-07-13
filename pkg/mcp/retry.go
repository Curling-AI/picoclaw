// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// retryBackoffSchedule paces reconnection attempts for servers that failed to
// connect at load time; the last step repeats until the server connects. A
// variable so tests can shrink it.
var retryBackoffSchedule = []time.Duration{
	15 * time.Second,
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
}

const retryConnectTimeout = 30 * time.Second

// RetryPendingServers keeps trying to connect the named servers until each one
// connects, ctx is canceled, or the manager closes. A server that failed at
// load time (expired OAuth grant, connector briefly down) would otherwise stay
// without tools until the next process restart even after the user fixes the
// credential — this loop is what brings it back. onConnected runs after each
// successful connection so the caller can register the server's tools.
func (m *Manager) RetryPendingServers(
	ctx context.Context,
	mcpCfg config.MCPConfig,
	workspacePath string,
	pending []string,
	onConnected func(name string, conn *ServerConnection),
) {
	remaining := make([]string, 0, len(pending))
	for _, name := range pending {
		if _, ok := m.GetServer(name); !ok {
			remaining = append(remaining, name)
		}
	}

	for attempt := 1; len(remaining) > 0; attempt++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryBackoffAt(attempt)):
		}
		if m.closed.Load() {
			return
		}

		still := remaining[:0]
		for _, name := range remaining {
			serverCfg, ok := mcpCfg.Servers[name]
			if !ok {
				continue
			}
			resolved, err := resolveServerEnvFile(serverCfg, workspacePath)
			if err == nil {
				connectCtx, cancel := context.WithTimeout(ctx, retryConnectTimeout)
				err = m.ConnectServer(connectCtx, name, resolved)
				cancel()
			}
			if err != nil {
				if ctx.Err() != nil || m.closed.Load() {
					return
				}
				// WARN sparingly: at the backoff cap this loop runs for as long
				// as a credential stays broken, and one line per attempt would
				// flood the pod logs.
				fields := map[string]any{
					"server":  name,
					"attempt": attempt,
					"error":   err.Error(),
				}
				if attempt == 1 || attempt%10 == 0 {
					logger.WarnCF("mcp", "MCP server retry failed, will keep retrying", fields)
				} else {
					logger.DebugCF("mcp", "MCP server retry failed", fields)
				}
				still = append(still, name)
				continue
			}
			logger.InfoCF("mcp", "MCP server connected after retry",
				map[string]any{
					"server":   name,
					"attempts": attempt,
				})
			if conn, ok := m.GetServer(name); ok && onConnected != nil {
				onConnected(name, conn)
			}
		}
		remaining = still
	}
}

func retryBackoffAt(attempt int) time.Duration {
	if attempt <= len(retryBackoffSchedule) {
		return retryBackoffSchedule[attempt-1]
	}
	return retryBackoffSchedule[len(retryBackoffSchedule)-1]
}

// resolveServerEnvFile mirrors the envFile resolution LoadFromMCPConfig does
// before connecting: relative paths are anchored at the workspace.
func resolveServerEnvFile(
	serverCfg config.MCPServerConfig,
	workspacePath string,
) (config.MCPServerConfig, error) {
	if serverCfg.EnvFile == "" || filepath.IsAbs(serverCfg.EnvFile) {
		return serverCfg, nil
	}
	if workspacePath == "" {
		return serverCfg, fmt.Errorf(
			"workspace path is empty while resolving relative envFile %q",
			serverCfg.EnvFile,
		)
	}
	serverCfg.EnvFile = filepath.Join(workspacePath, serverCfg.EnvFile)
	return serverCfg, nil
}
