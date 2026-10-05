package mcp

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpHandshakeTimeout bounds how long one server gets to answer initialize and
// list its tools. The agent waits for every configured server before it takes
// messages, so a server that never answers (a bridge parked on a browser login,
// for one) would otherwise hold every conversation of the assistant. Giving up
// still costs closing the connection: up to mcpSessionCloseTimeout more over
// HTTP, and the stdin grace of isolatedPipeRWC.Close for a stdio process.
var mcpHandshakeTimeout = 30 * time.Second

// mcpSessionCloseTimeout bounds the DELETE that ends a streamable HTTP session.
var mcpSessionCloseTimeout = 5 * time.Second

var errConnectAbandoned = errors.New("connect abandoned after the handshake deadline")

// trackedTransport remembers the connection it opened, so a connect that
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

// closeAbandoned closes the connection of a connect that failed or was given
// up on. Client.Connect does not close it on every failure (an unsupported
// protocol version returns with it open). Both connection types close once,
// so one the SDK already closed is left as it is.
func (t *trackedTransport) closeAbandoned() {
	t.mu.Lock()
	t.abandoned = true
	conn := t.conn
	t.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

// connectWithin runs Client.Connect and stops waiting for it when ctx ends.
// Parts of Connect run on the connection's own detached context and ignore
// ctx (the standalone SSE GET it sends right after initialize, for one), so
// closing the connection is what releases them; a session that still comes
// back late is closed.
func connectWithin(ctx context.Context, client *mcp.Client, transport *trackedTransport) (*mcp.ClientSession, error) {
	type outcome struct {
		session *mcp.ClientSession
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		session, err := client.Connect(ctx, transport, nil)
		done <- outcome{session: session, err: err}
	}()

	select {
	case result := <-done:
		return result.session, result.err
	case <-ctx.Done():
		transport.closeAbandoned()
		go func() {
			if late := <-done; late.session != nil {
				_ = late.session.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// boundedDeleteTransport caps the DELETE that ends a streamable HTTP session.
// The SDK sends it from Close on a context detached from every caller, so a
// server that took the connection and never answers would hold Close, and the
// connect that already gave up on it, forever.
type boundedDeleteTransport struct {
	base http.RoundTripper
}

func (t *boundedDeleteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodDelete {
		return t.base.RoundTrip(req)
	}
	// The SDK drops the DELETE response unread, so ending the context once
	// RoundTrip returns loses nothing.
	ctx, cancel := context.WithTimeout(req.Context(), mcpSessionCloseTimeout)
	defer cancel()
	return t.base.RoundTrip(req.WithContext(ctx))
}
