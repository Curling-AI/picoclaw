package mcp

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

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
