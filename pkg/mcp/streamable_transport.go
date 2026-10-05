package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
)

// newStreamableTransport builds the SSE/HTTP transport of one server. It gets
// an HTTP client of its own, so the connections it dials can be cut when its
// handshake is abandoned.
func newStreamableTransport(
	name string,
	cfg config.MCPServerConfig,
	transportType string,
) (*mcp.StreamableClientTransport, *connTracker) {
	// Configure DisableStandaloneSSE based on transport type.
	// - "http": Streamable HTTP request-response mode. Disable the standalone
	//   SSE stream to avoid compatibility issues with servers that don't
	//   support the optional GET listener.
	// - "sse": Bidirectional mode. Enable the standalone SSE stream to receive
	//   server-initiated notifications (e.g., ToolListChangedNotification).
	// - Empty or auto-detected: Defaults to "sse" behavior (standalone SSE enabled).
	disableStandaloneSSE := transportType == "http"

	logger.DebugCF("mcp", "Using SSE/HTTP transport",
		map[string]any{
			"server":               name,
			"url":                  cfg.URL,
			"disableStandaloneSSE": disableStandaloneSSE,
		})

	conns := &connTracker{}
	var roundTripper http.RoundTripper = newTrackedHTTPTransport(conns)
	if len(cfg.Headers) > 0 {
		roundTripper = &headerTransport{
			base:    roundTripper,
			headers: cfg.Headers,
		}
		logger.DebugCF("mcp", "Added custom HTTP headers",
			map[string]any{
				"server":       name,
				"header_count": len(cfg.Headers),
			})
	}

	return &mcp.StreamableClientTransport{
		Endpoint:             cfg.URL,
		DisableStandaloneSSE: disableStandaloneSSE,
		HTTPClient: &http.Client{
			Transport:     &boundedDeleteTransport{base: roundTripper},
			CheckRedirect: refuseDeleteRedirect,
		},
	}, conns
}

func newTrackedHTTPTransport(conns *connTracker) *http.Transport {
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	dial := transport.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	transport.DialContext = conns.track(dial)
	return transport
}

// connTracker records the connections one server's HTTP client dials. The SDK
// sends some requests on contexts nothing cancels (its replies to requests from
// the server, for one), and cutting their connections is the only way to stop
// them once the handshake is given up.
type connTracker struct {
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (t *connTracker) track(dial dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.closed {
			_ = conn.Close()
			return nil, net.ErrClosed
		}
		if t.conns == nil {
			t.conns = make(map[net.Conn]struct{})
		}
		t.conns[conn] = struct{}{}
		return &trackedConn{Conn: conn, tracker: t}, nil
	}
}

func (t *connTracker) forget(conn net.Conn) {
	t.mu.Lock()
	delete(t.conns, conn)
	t.mu.Unlock()
}

// closeAll cuts every connection and refuses new ones.
func (t *connTracker) closeAll() {
	t.mu.Lock()
	conns := t.conns
	t.conns = nil
	t.closed = true
	t.mu.Unlock()
	for conn := range conns {
		_ = conn.Close()
	}
}

type trackedConn struct {
	net.Conn
	tracker *connTracker
}

func (c *trackedConn) Close() error {
	c.tracker.forget(c.Conn)
	return c.Conn.Close()
}

// refuseDeleteRedirect keeps the session-ending DELETE from following a
// redirect: the client would continue it as a GET on the SDK's detached
// context, out of boundedDeleteTransport's reach. The SDK ignores the reply.
func refuseDeleteRedirect(req *http.Request, via []*http.Request) error {
	if via[0].Method == http.MethodDelete {
		return http.ErrUseLastResponse
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// boundedDeleteTransport caps the DELETE that ends a streamable HTTP session.
// The SDK sends it from Close on a context detached from every caller, so a
// server that took the connection and never answers would hold Close forever.
type boundedDeleteTransport struct {
	base http.RoundTripper
}

func (t *boundedDeleteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodDelete {
		return t.base.RoundTrip(req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), mcpSessionCloseTimeout)
	defer cancel()
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	// The SDK drops the reply without closing it; close it here, while the
	// deadline still holds, so the connection is released.
	_ = resp.Body.Close()
	resp.Body = http.NoBody
	return resp, nil
}
