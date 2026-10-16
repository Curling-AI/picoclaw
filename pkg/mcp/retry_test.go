package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/config"
)

func shrinkRetryBackoff(t *testing.T) {
	t.Helper()
	original := retryBackoffSchedule
	retryBackoffSchedule = []time.Duration{time.Millisecond}
	t.Cleanup(func() {
		retryBackoffSchedule = original
	})
}

func TestRetryPendingServersConnectsAfterFailures(t *testing.T) {
	shrinkRetryBackoff(t)
	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		connectServerFunc = originalConnectServerFunc
	})

	var attempts atomic.Int32
	connectServerFunc = func(
		_ context.Context,
		name string,
		cfg config.MCPServerConfig,
	) (*ServerConnection, error) {
		if attempts.Add(1) <= 2 {
			return nil, fmt.Errorf("unauthorized")
		}
		return &ServerConnection{
			Name:   name,
			Config: cfg,
			Tools:  []*sdkmcp.Tool{{Name: "echo"}},
		}, nil
	}

	mcpCfg := config.MCPConfig{
		Servers: map[string]config.MCPServerConfig{
			"skip": {Enabled: true, Type: "http", URL: "http://example.test/mcp"},
		},
	}

	mgr := NewManager()
	var connectedServer atomic.Value
	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.RetryPendingServers(
			context.Background(), mcpCfg, "/tmp/ws", []string{"skip"},
			func(name string, conn *ServerConnection) {
				connectedServer.Store(name + "/" + conn.Tools[0].Name)
			},
		)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry loop did not finish after successful connect")
	}

	if got := attempts.Load(); got != 3 {
		t.Fatalf("connect attempts = %d, want 3", got)
	}
	if got, _ := connectedServer.Load().(string); got != "skip/echo" {
		t.Fatalf("onConnected saw %q, want %q", got, "skip/echo")
	}
	if _, ok := mgr.GetServer("skip"); !ok {
		t.Fatal("server missing from manager after retry connect")
	}
}

func TestRetryPendingServersStopsOnContextCancel(t *testing.T) {
	shrinkRetryBackoff(t)
	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		connectServerFunc = originalConnectServerFunc
	})

	connectServerFunc = func(
		_ context.Context,
		_ string,
		_ config.MCPServerConfig,
	) (*ServerConnection, error) {
		return nil, fmt.Errorf("still down")
	}

	mcpCfg := config.MCPConfig{
		Servers: map[string]config.MCPServerConfig{
			"skip": {Enabled: true, Type: "http", URL: "http://example.test/mcp"},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewManager().RetryPendingServers(ctx, mcpCfg, "/tmp/ws", []string{"skip"}, nil)
	}()

	time.Sleep(20 * time.Millisecond) // let a few attempts fail
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry loop did not stop on context cancel")
	}
}

func TestRetryPendingServersSkipsAlreadyConnected(t *testing.T) {
	shrinkRetryBackoff(t)
	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		connectServerFunc = originalConnectServerFunc
	})

	var attempts atomic.Int32
	connectServerFunc = func(
		_ context.Context,
		name string,
		cfg config.MCPServerConfig,
	) (*ServerConnection, error) {
		attempts.Add(1)
		return &ServerConnection{Name: name, Config: cfg}, nil
	}

	mcpCfg := config.MCPConfig{
		Servers: map[string]config.MCPServerConfig{
			"skip": {Enabled: true, Type: "http", URL: "http://example.test/mcp"},
		},
	}

	mgr := NewManager()
	if err := mgr.ConnectServer(context.Background(), "skip", mcpCfg.Servers["skip"]); err != nil {
		t.Fatalf("seed ConnectServer failed: %v", err)
	}
	attempts.Store(0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		mgr.RetryPendingServers(context.Background(), mcpCfg, "/tmp/ws", []string{"skip"}, nil)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry loop did not return for already-connected server")
	}
	if got := attempts.Load(); got != 0 {
		t.Fatalf("connect attempts = %d, want 0 (already connected)", got)
	}
}

// The real SDK path: a handshake answered with 429 or 503 is the transport
// turning the request away, a 401 is the server answering it.
func TestRejectedByTransportRecognizesTheHandshakeStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusUnauthorized, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
		}))
		_, err := connectServer(context.Background(), "gmail", config.MCPServerConfig{
			Enabled: true, Type: "http", URL: srv.URL,
		})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: connect succeeded", tc.status)
		}
		if got := rejectedByTransport(err); got != tc.want {
			t.Errorf("status %d: rejectedByTransport(%v) = %v, want %v", tc.status, err, got, tc.want)
		}
	}
}

func TestRetryDelaySpreadsEveryStep(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 200 {
		d := retryDelay(1, false)
		if d < 7500*time.Millisecond || d >= 22500*time.Millisecond {
			t.Fatalf("retryDelay(1, false) = %s, want within [7.5s, 22.5s)", d)
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Errorf("only %d distinct delays in 200 draws: pods would still retry together", len(seen))
	}
}

func TestRetryDelayKeepsTransientFailuresOnTheShortInterval(t *testing.T) {
	half := transientRetryInterval / 2
	for _, attempt := range []int{2, 5, transientRetryMax} {
		if d := retryDelay(attempt, true); d < half || d >= 3*half {
			t.Errorf("retryDelay(%d, true) = %s, want within [%s, %s)", attempt, d, half, 3*half)
		}
	}
	// Past the cap a server still turned away is treated like any failure.
	if d := retryDelay(transientRetryMax+1, true); d < 150*time.Second {
		t.Errorf("retryDelay(%d, true) = %s, want the schedule's last step (5m, spread)", transientRetryMax+1, d)
	}
	if d := retryDelay(5, false); d < 150*time.Second {
		t.Errorf("retryDelay(5, false) = %s, want the schedule's 5m step, spread", d)
	}
}

// A rate-limited server comes back on the short interval instead of climbing
// the schedule: with the schedule at an hour after the first step, the loop
// only finishes in time if the transient path is taken.
func TestRetryPendingServersRetriesTransientRejectionsFast(t *testing.T) {
	originalSchedule, originalInterval := retryBackoffSchedule, transientRetryInterval
	retryBackoffSchedule = []time.Duration{time.Millisecond, time.Hour}
	transientRetryInterval = time.Millisecond
	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		retryBackoffSchedule, transientRetryInterval = originalSchedule, originalInterval
		connectServerFunc = originalConnectServerFunc
	})

	var attempts atomic.Int32
	connectServerFunc = func(_ context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		if attempts.Add(1) <= 3 {
			return nil, fmt.Errorf(`failed to connect: calling "initialize": %w`,
				&jsonrpc.Error{Code: codeRejectedByTransport, Message: "rejected by transport"})
		}
		return &ServerConnection{Name: name, Config: cfg}, nil
	}
	mcpCfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"gmail": {Enabled: true, Type: "http", URL: "http://example.test/mcp"},
	}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		NewManager().RetryPendingServers(context.Background(), mcpCfg, "/tmp/ws", []string{"gmail"}, nil)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("still retrying after %d attempts: transient rejections climbed the schedule", attempts.Load())
	}
	if got := attempts.Load(); got != 4 {
		t.Errorf("connect attempts = %d, want 4", got)
	}
}

// The pace is per round: a server failing for good is retried on the short
// interval while another one in the same pod is being turned away, and the
// schedule takes over once only it is left.
func TestRetryPendingServersPacesTheRoundByAnyTransientFailure(t *testing.T) {
	originalSchedule, originalInterval := retryBackoffSchedule, transientRetryInterval
	retryBackoffSchedule = []time.Duration{time.Millisecond, time.Hour}
	transientRetryInterval = time.Millisecond
	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		retryBackoffSchedule, transientRetryInterval = originalSchedule, originalInterval
		connectServerFunc = originalConnectServerFunc
	})

	var gmailAttempts, brokenAttempts atomic.Int32
	connectServerFunc = func(_ context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		if name == "broken" {
			brokenAttempts.Add(1)
			return nil, fmt.Errorf(`failed to connect: calling "initialize": Unauthorized`)
		}
		if gmailAttempts.Add(1) <= 3 {
			return nil, fmt.Errorf(`failed to connect: calling "initialize": %w`,
				&jsonrpc.Error{Code: codeRejectedByTransport, Message: "rejected by transport"})
		}
		return &ServerConnection{Name: name, Config: cfg}, nil
	}
	mcpCfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{
		"gmail":  {Enabled: true, Type: "http", URL: "http://example.test/gmail"},
		"broken": {Enabled: true, Type: "http", URL: "http://example.test/broken"},
	}}

	ctx, cancel := context.WithCancel(context.Background())
	connected := make(chan string, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		NewManager().RetryPendingServers(ctx, mcpCfg, "/tmp/ws", []string{"gmail", "broken"},
			func(name string, _ *ServerConnection) { connected <- name })
	}()
	// The loop must be gone before the cleanup restores the globals it reads.
	defer func() {
		cancel()
		<-done
	}()

	select {
	case name := <-connected:
		if name != "gmail" {
			t.Fatalf("connected %q, want gmail", name)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("gmail still pending after %d attempts", gmailAttempts.Load())
	}
	// broken rode the transient rounds: tried once per round until gmail came
	// back, then left on the schedule's hour step.
	time.Sleep(50 * time.Millisecond)
	if got := brokenAttempts.Load(); got != 4 {
		t.Errorf("broken attempts = %d, want 4 (the rounds gmail needed)", got)
	}
}
