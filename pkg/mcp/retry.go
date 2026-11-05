// PicoClaw - Ultra-lightweight personal AI agent
// Inspired by and based on nanobot: https://github.com/HKUDS/nanobot
// License: MIT
//
// Copyright (c) 2026 PicoClaw contributors

package mcp

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

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

// transientRetryInterval paces the retries while the HTTP transport turns
// servers away: 429 or 5xx (the upstream is saturated or briefly down) or a
// failure to reach it at all. That clears by itself once the burst passes, and
// the schedule above is sized for a credential someone has to fix: stepping a
// rate-limited server up to minutes kept its tools away long after the
// upstream had capacity again. The pace is per round: while any pending server
// is being turned away, the others on its track are retried on it too. A variable so tests
// can shrink it.
var transientRetryInterval = 30 * time.Second

// transientRetryMax caps those fast rounds at about ten minutes' worth; after
// that the schedule applies whatever the failures are.
const transientRetryMax = 20

// toolsListRetryHoldMin and toolsListRetryHoldSpread bound the wait before the
// first retry of a server whose tools/list the transport turned away. That
// happens to the assistants woken together two to four minutes ahead of a
// crowded cron minute, all on Composio's per-minute quota for the whole
// account: retried on the pace above, thousands of them kept that quota taken
// through the first minutes of the cron run, when its own tool calls need it.
// Held for at least six minutes, they come back after the run's first calls,
// spread over six more instead of all at once. Variables so tests can shrink
// them.
var (
	toolsListRetryHoldMin    = 6 * time.Minute
	toolsListRetryHoldSpread = 6 * time.Minute
)

// toolsListRetryHold draws a hold from the window, so pods woken for the same
// minute spread over it.
func toolsListRetryHold() time.Duration {
	return toolsListRetryHoldMin + rand.N(toolsListRetryHoldSpread)
}

// holdRelease reports whether name starts on the held track and when its hold
// ends. A server whose tools/list was turned away is held once until it
// connects again: a loop restarted during the hold (a reload that keeps the
// connections) keeps its release, and one restarted after it leaves the
// server on the transient pace. Servers held at the same time share the
// release drawn for the first of them.
func (m *Manager) holdRelease(name string, now time.Time) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.listTurnedAway[name] {
		return time.Time{}, false
	}
	if until, ok := m.listHeldUntil[name]; ok {
		return until, until.After(now)
	}
	if !m.listReleaseAt.After(now) {
		m.listReleaseAt = now.Add(toolsListRetryHold())
	}
	m.listHeldUntil[name] = m.listReleaseAt
	return m.listReleaseAt, true
}

// codeRejectedByTransport is the JSON-RPC code the SDK puts on a request its
// HTTP transport could not get answered: an HTTP 429/5xx, or no response.
const codeRejectedByTransport = -32005

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
	// Servers whose tools/list was turned away start on their own track at the
	// release; the others keep the schedule from now. Each track counts its own
	// rounds, so neither moves the other's deadline or fast rounds.
	var ordinary, held retryTrack
	now := time.Now()
	for _, name := range pending {
		if _, ok := m.GetServer(name); ok {
			continue
		}
		if release, ok := m.holdRelease(name, now); ok {
			held.names = append(held.names, name)
			held.next = release
		} else {
			ordinary.names = append(ordinary.names, name)
		}
	}
	ordinary.next = now.Add(retryDelay(1, false))
	if len(held.names) > 0 {
		logger.InfoCF("mcp", "Holding MCP servers whose tools/list was turned away before retrying",
			map[string]any{
				"servers": slices.Sorted(slices.Values(held.names)),
				"hold":    held.next.Sub(now).String(),
			})
	}

	for {
		track := nextTrack(&ordinary, &held)
		if track == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(max(time.Until(track.next), 0)):
		}
		if m.closed.Load() || !m.retryRound(ctx, mcpCfg, workspacePath, track, onConnected) {
			return
		}
	}
}

// retryTrack is a set of pending servers retried together, on its own round
// count and pace.
type retryTrack struct {
	names   []string
	attempt int
	next    time.Time
}

// nextTrack is the track with servers left whose round comes first.
func nextTrack(tracks ...*retryTrack) *retryTrack {
	var first *retryTrack
	for _, track := range tracks {
		if len(track.names) > 0 && (first == nil || track.next.Before(first.next)) {
			first = track
		}
	}
	return first
}

// retryRound tries each server of track once and schedules the track's next
// round. It reports false when ctx was canceled or the manager closed meanwhile.
func (m *Manager) retryRound(
	ctx context.Context,
	mcpCfg config.MCPConfig,
	workspacePath string,
	track *retryTrack,
	onConnected func(name string, conn *ServerConnection),
) bool {
	track.attempt++
	attempt := track.attempt
	transient := false
	still := track.names[:0]
	for _, name := range track.names {
		serverCfg, ok := mcpCfg.Servers[name]
		if !ok {
			continue
		}
		resolved, err := resolveServerEnvFile(serverCfg, workspacePath)
		if err == nil {
			// connectServer bounds the handshake with mcpHandshakeTimeout.
			err = m.ConnectServer(ctx, name, resolved)
		}
		if err != nil {
			if ctx.Err() != nil || m.closed.Load() {
				return false
			}
			transient = transient || rejectedByTransport(err)
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
	track.names = still
	track.next = time.Now().Add(retryDelay(attempt+1, transient))
	return true
}

// retryDelay is the wait before attempt. Every delay is spread over [d/2, 3d/2):
// assistants woken for the same cron minute fail together, and without the
// spread they retried together into the same exhausted quota.
func retryDelay(attempt int, transient bool) time.Duration {
	d := retryBackoffAt(attempt)
	if transient && attempt <= transientRetryMax {
		d = transientRetryInterval
	}
	if d <= 0 {
		return d
	}
	return d/2 + rand.N(d)
}

// rejectedByTransport reports whether err is the HTTP transport failing to get
// the request answered (429/5xx, or no response), as opposed to the server
// answering it.
func rejectedByTransport(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == codeRejectedByTransport
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
