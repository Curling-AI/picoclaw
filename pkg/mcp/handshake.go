package mcp

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpHandshakeTimeout bounds how long one server gets to answer initialize and
// list its tools. The agent waits for every configured server before it takes
// messages, so a server that never answers (a bridge parked on a browser login,
// for one) would otherwise hold every conversation of the assistant. Giving up
// still costs the close: up to mcpSessionCloseTimeout more over HTTP, and the
// stdin grace of isolatedPipeRWC.Close for a stdio process.
var mcpHandshakeTimeout = 30 * time.Second

// mcpSessionCloseTimeout bounds the DELETE that ends a streamable HTTP session.
var mcpSessionCloseTimeout = 5 * time.Second

// trackedTransport remembers the connection it opened. Client.Connect does not
// close it on every failure (an unsupported protocol version returns with the
// connection open), and a stdio server left that way would run until the agent
// exits, one more on every retry.
type trackedTransport struct {
	mcp.Transport
	conn mcp.Connection
}

func (t *trackedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.Transport.Connect(ctx)
	t.conn = conn
	return conn, err
}

// closeAbandoned closes the connection of a failed connect. Both connection
// types close once, so one the SDK already closed is left as it is.
func (t *trackedTransport) closeAbandoned() {
	if t.conn != nil {
		_ = t.conn.Close()
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
	ctx, cancel := context.WithTimeout(req.Context(), mcpSessionCloseTimeout)
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
