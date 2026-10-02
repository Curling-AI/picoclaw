package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sipeed/picoclaw/pkg/config"
)

// stdioHelperEnv makes the re-executed test binary act as a stdio MCP server,
// so the stdio lifecycle is exercised against a real child process.
const stdioHelperEnv = "PICOCLAW_TEST_MCP_STDIO_HELPER"

// stdioHelperPIDFileEnv is where a helper writes the PID the test watches.
const stdioHelperPIDFileEnv = "PICOCLAW_TEST_MCP_STDIO_HELPER_PID_FILE"

const (
	stdioHelperServe  = "serve"
	stdioHelperSilent = "silent"
	// stdioHelperLauncher mimics `npx mcp-remote`: it starts the real process
	// and leaves on its own, with that process still running.
	stdioHelperLauncher = "launcher"
	stdioHelperLinger   = "linger"
	// stdioHelperOldProtocol answers initialize with a protocol version the
	// SDK refuses, and then waits for its stdin to close.
	stdioHelperOldProtocol = "old-protocol"
)

func TestMain(m *testing.M) {
	switch os.Getenv(stdioHelperEnv) {
	case stdioHelperServe:
		runStdioHelperServer()
		os.Exit(0)
	case stdioHelperSilent:
		// Never answers initialize, like a bridge parked on a browser login.
		// It leaves once its stdin closes, as stdio servers do.
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case stdioHelperLauncher:
		runStdioHelperLauncher()
		os.Exit(0)
	case stdioHelperLinger:
		// Ignores stdin; bounded so a failed test does not leak it for long.
		time.Sleep(time.Minute)
		os.Exit(0)
	case stdioHelperOldProtocol:
		runStdioHelperOldProtocol()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func writeHelperPID(pid int) {
	if err := os.WriteFile(os.Getenv(stdioHelperPIDFileEnv), []byte(strconv.Itoa(pid)), 0o600); err != nil {
		os.Exit(1)
	}
}

func runStdioHelperLauncher() {
	executable, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	child := exec.Command(executable)
	child.Env = append(os.Environ(), stdioHelperEnv+"="+stdioHelperLinger)
	if err := child.Start(); err != nil {
		os.Exit(1)
	}
	writeHelperPID(child.Process.Pid)
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func runStdioHelperOldProtocol() {
	writeHelperPID(os.Getpid())
	var initialize struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.NewDecoder(os.Stdin).Decode(&initialize); err != nil {
		return
	}
	fmt.Printf(`{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"1999-01-01",`+
		`"capabilities":{},"serverInfo":{"name":"stdio-helper","version":"1.0.0"}}}`+"\n", initialize.ID)
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func runStdioHelperServer() {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "stdio-helper", Version: "1.0.0"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "ping", Description: "Answers pong"},
		func(context.Context, *sdkmcp.CallToolRequest, map[string]any) (*sdkmcp.CallToolResult, any, error) {
			return &sdkmcp.CallToolResult{
				Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "pong"}},
			}, nil, nil
		})
	_ = server.Run(context.Background(), &sdkmcp.StdioTransport{})
}

func stdioHelperConfig(t *testing.T, mode string) config.MCPServerConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}
	return config.MCPServerConfig{
		Enabled: true,
		Type:    "stdio",
		Command: executable,
		Env:     map[string]string{stdioHelperEnv: mode},
	}
}

func shrinkHandshakeTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	original := mcpHandshakeTimeout
	mcpHandshakeTimeout = timeout
	t.Cleanup(func() {
		mcpHandshakeTimeout = original
	})
}

// runWithin fails the test when fn is still running after limit, so a hung
// handshake fails one test instead of stalling the whole binary.
func runWithin[T any](t *testing.T, limit time.Duration, fn func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() {
		done <- fn()
	}()
	select {
	case result := <-done:
		return result
	case <-time.After(limit):
		t.Fatalf("still blocked after %s", limit)
		var zero T
		return zero
	}
}

func assertPingAnswers(t *testing.T, mgr *Manager, server string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := mgr.CallTool(ctx, server, "ping", map[string]any{})
	if err != nil {
		t.Fatalf("CallTool(%s, ping) error = %v", server, err)
	}
	if len(result.Content) != 1 {
		t.Fatalf("CallTool(%s, ping) content = %#v, want one item", server, result.Content)
	}
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok || text.Text != "pong" {
		t.Fatalf("CallTool(%s, ping) content = %#v, want pong", server, result.Content[0])
	}
}

func newClosingManager(t *testing.T) *Manager {
	t.Helper()
	mgr := NewManager()
	t.Cleanup(func() {
		_ = mgr.Close()
	})
	return mgr
}

func TestConnectServerGivesUpOnStdioServerThatNeverAnswers(t *testing.T) {
	shrinkHandshakeTimeout(t, 300*time.Millisecond)
	silent := stdioHelperConfig(t, stdioHelperSilent)

	err := runWithin(t, 10*time.Second, func() error {
		conn, err := connectServer(context.Background(), "silent", silent)
		if conn != nil {
			_ = conn.Session.Close()
		}
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connectServer() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestConnectServerGivesUpOnHTTPServerThatNeverAnswers(t *testing.T) {
	shrinkHandshakeTimeout(t, 300*time.Millisecond)
	originalCloseTimeout := mcpSessionCloseTimeout
	mcpSessionCloseTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		mcpSessionCloseTimeout = originalCloseTimeout
	})

	// Takes every request and answers none, the session-ending DELETE included.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		close(release)
	})

	// The SDK spends up to 5s notifying the server that initialize was canceled.
	err := runWithin(t, 20*time.Second, func() error {
		conn, err := connectServer(context.Background(), "unresponsive", config.MCPServerConfig{
			Enabled: true,
			Type:    "http",
			URL:     server.URL,
		})
		if conn != nil {
			_ = conn.Session.Close()
		}
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connectServer() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestLoadFromMCPConfigDoesNotWaitOnStdioServerThatNeverAnswers(t *testing.T) {
	shrinkHandshakeTimeout(t, 300*time.Millisecond)
	mgr := newClosingManager(t)
	mcpCfg := config.MCPConfig{
		ToolConfig: config.ToolConfig{Enabled: true},
		Servers: map[string]config.MCPServerConfig{
			"healthy": stdioHelperConfig(t, stdioHelperServe),
			"silent":  stdioHelperConfig(t, stdioHelperSilent),
		},
	}
	workspace := t.TempDir()

	err := runWithin(t, 10*time.Second, func() error {
		return mgr.LoadFromMCPConfig(context.Background(), mcpCfg, workspace)
	})
	if err != nil {
		t.Fatalf("LoadFromMCPConfig() error = %v", err)
	}
	if _, ok := mgr.GetServer("silent"); ok {
		t.Fatal("silent server registered as connected")
	}
	assertPingAnswers(t, mgr, "healthy")
}

// The background retry and the CallTool reconnect dial with contexts that end
// right after the connect; the stdio process must not end with them.
func TestStdioServerOutlivesTheContextThatConnectedIt(t *testing.T) {
	mgr := newClosingManager(t)
	ctx, cancel := context.WithCancel(context.Background())
	if err := mgr.ConnectServer(ctx, "healthy", stdioHelperConfig(t, stdioHelperServe)); err != nil {
		t.Fatalf("ConnectServer() error = %v", err)
	}
	cancel()
	// A process bound to ctx is killed asynchronously; give that kill time to land.
	time.Sleep(300 * time.Millisecond)

	assertPingAnswers(t, mgr, "healthy")
}

func TestRetryPendingServersKeepsStdioServerRunning(t *testing.T) {
	shrinkRetryBackoff(t)
	mgr := newClosingManager(t)
	mcpCfg := config.MCPConfig{
		ToolConfig: config.ToolConfig{Enabled: true},
		Servers: map[string]config.MCPServerConfig{
			"healthy": stdioHelperConfig(t, stdioHelperServe),
		},
	}
	workspace := t.TempDir()

	runWithin(t, 10*time.Second, func() struct{} {
		mgr.RetryPendingServers(context.Background(), mcpCfg, workspace, []string{"healthy"}, nil)
		return struct{}{}
	})
	time.Sleep(300 * time.Millisecond)

	assertPingAnswers(t, mgr, "healthy")
}

func TestListServerToolsFailsOnceItsDeadlinePasses(t *testing.T) {
	conn, err := connectServer(context.Background(), "healthy", stdioHelperConfig(t, stdioHelperServe))
	if err != nil {
		t.Fatalf("connectServer() error = %v", err)
	}
	t.Cleanup(func() {
		_ = conn.Session.Close()
	})
	expired, cancel := context.WithCancel(context.Background())
	cancel()

	tools, err := listServerTools(expired, "healthy", conn.Session, conn.Session.InitializeResult())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("listServerTools() = (%d tools, %v), want context.Canceled", len(tools), err)
	}
}
