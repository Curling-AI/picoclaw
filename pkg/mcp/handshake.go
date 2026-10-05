package mcp

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/logger"
)

// mcpHandshakeTimeout bounds how long one server gets to answer initialize and
// list its tools. The agent waits for every configured server before it takes
// messages, so a server that never answers (a bridge parked on a browser login,
// for one) would otherwise hold every conversation of the assistant. Giving up
// adds at most mcpSessionCloseTimeout for closing what the server left open.
var mcpHandshakeTimeout = 30 * time.Second

// mcpSessionCloseTimeout bounds the DELETE that ends a streamable HTTP session
// and how long an abandoned handshake waits for its connection to close.
var mcpSessionCloseTimeout = 5 * time.Second

var errConnectAbandoned = errors.New("connect abandoned after the handshake deadline")

// trackedTransport remembers the connection it opened, so a handshake that
// failed or ran out of time can be closed from outside the SDK.
type trackedTransport struct {
	mcp.Transport

	mu        sync.Mutex
	conn      mcp.Connection
	abandoned bool
}

func (t *trackedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	abandoned := t.abandoned
	t.conn = conn
	t.mu.Unlock()
	if abandoned {
		_ = conn.Close()
		return nil, errConnectAbandoned
	}
	return conn, nil
}

// closeAbandoned closes the connection of a handshake that failed or was given
// up on. Client.Connect does not close it on every failure (an unsupported
// protocol version returns with it open). Both connection types close once,
// so one the SDK already closed is left as it is.
func (t *trackedTransport) closeAbandoned() error {
	t.mu.Lock()
	t.abandoned = true
	conn := t.conn
	t.mu.Unlock()
	if conn == nil {
		return nil
	}
	return conn.Close()
}

type handshakeResult struct {
	session *mcp.ClientSession
	tools   []*mcp.Tool
	err     error
}

// handshakeWithin runs initialize and the tools listing as one unit and stops
// waiting for it when ctx ends. Parts of it run on the connection's own
// detached context and ignore ctx (the standalone SSE GET the SDK opens inside
// Connect, its replies to requests from the server, a write to a stdio server
// that stopped reading), so abandonConnection is what releases them. A session
// that still comes back late is closed.
func handshakeWithin(
	ctx context.Context,
	name string,
	client *mcp.Client,
	transport *trackedTransport,
) handshakeResult {
	done := make(chan handshakeResult, 1)
	go func() {
		done <- runHandshake(ctx, name, client, transport)
	}()

	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		go func() {
			if late := <-done; late.session != nil {
				_ = late.session.Close()
			}
		}()
		return handshakeResult{err: ctx.Err()}
	}
}

func runHandshake(ctx context.Context, name string, client *mcp.Client, transport *trackedTransport) handshakeResult {
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return handshakeResult{err: err}
	}

	initResult := session.InitializeResult()
	logger.InfoCF("mcp", "Connected to MCP server",
		map[string]any{
			"server":        name,
			"serverName":    initResult.ServerInfo.Name,
			"serverVersion": initResult.ServerInfo.Version,
			"protocol":      initResult.ProtocolVersion,
		})

	tools, err := listServerTools(ctx, name, session, initResult)
	if err != nil {
		return handshakeResult{session: session, err: err}
	}
	return handshakeResult{session: session, tools: tools}
}

// abandonConnection closes what a failed handshake left open, waiting at most
// mcpSessionCloseTimeout: the close can stall on the server itself (a DELETE,
// a stdio process that ignores its stdin), and then it carries on in the
// background. conns, for an HTTP server, is cut either way.
func abandonConnection(name string, transport *trackedTransport, session *mcp.ClientSession, conns *connTracker) {
	closed := make(chan error, 1)
	go func() {
		var err error
		if session != nil {
			err = session.Close()
		}
		if closeErr := transport.closeAbandoned(); err == nil {
			err = closeErr
		}
		closed <- err
	}()

	select {
	case err := <-closed:
		if err != nil {
			logger.WarnCF("mcp", "Failed to close abandoned MCP connection",
				map[string]any{"server": name, "error": err.Error()})
		}
	case <-time.After(mcpSessionCloseTimeout):
		logger.WarnCF("mcp", "Abandoned MCP connection is still closing; continuing in the background",
			map[string]any{"server": name})
	}
	if conns != nil {
		conns.closeAll()
	}
}
