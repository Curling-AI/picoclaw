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
		d := retryDelay(1, 1, false)
		if d < 7500*time.Millisecond || d >= 22500*time.Millisecond {
			t.Fatalf("retryDelay(1, 1, false) = %s, want within [7.5s, 22.5s)", d)
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
		if d := retryDelay(attempt, attempt, true); d < half || d >= 3*half {
			t.Errorf("retryDelay(%d, %d, true) = %s, want within [%s, %s)", attempt, attempt, d, half, 3*half)
		}
	}
	// Past the cap a server still turned away is treated like any failure.
	if d := retryDelay(transientRetryMax+1, transientRetryMax+1, true); d < 150*time.Second {
		t.Errorf("retryDelay(%d, %d, true) = %s, want the schedule's last step (5m, spread)",
			transientRetryMax+1, transientRetryMax+1, d)
	}
	if d := retryDelay(5, 5, false); d < 150*time.Second {
		t.Errorf("retryDelay(5, 5, false) = %s, want the schedule's 5m step, spread", d)
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

// toolsListRejection is what connectServer returns when the transport turns
// tools/list away after initialize went through.
func toolsListRejection() error {
	return fmt.Errorf("%w: %w", errListingTools,
		&jsonrpc.Error{Code: codeRejectedByTransport, Message: "rejected by transport"})
}

func initializeRejection() error {
	return fmt.Errorf(`failed to connect: calling "initialize": %w`,
		&jsonrpc.Error{Code: codeRejectedByTransport, Message: "rejected by transport"})
}

func TestLoadFromMCPConfigRecordsOnlyAToolsListRejection(t *testing.T) {
	limited := rateLimitedToolsListServer(t, 1<<30)
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(refused.Close)
	healthy := rateLimitedToolsListServer(t, 0)
	mcpCfg := config.MCPConfig{
		ToolConfig: config.ToolConfig{Enabled: true},
		Servers: map[string]config.MCPServerConfig{
			"gmail":    {Enabled: true, Type: "http", URL: limited.URL},
			"calendar": {Enabled: true, Type: "http", URL: refused.URL},
			"drive":    {Enabled: true, Type: "http", URL: healthy.URL},
		},
	}
	mgr := NewManager()
	t.Cleanup(func() {
		_ = mgr.Close()
	})

	if err := mgr.LoadFromMCPConfig(context.Background(), mcpCfg, t.TempDir()); err != nil {
		t.Fatalf("LoadFromMCPConfig() error = %v", err)
	}
	for name, want := range map[string]bool{"gmail": true, "calendar": false, "drive": false} {
		if got := mgr.toolsListTurnedAway(name); got != want {
			t.Errorf("toolsListTurnedAway(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestConnectServerClearsAToolsListRejectionOnceItConnects(t *testing.T) {
	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		connectServerFunc = originalConnectServerFunc
	})
	var calls atomic.Int32
	connectServerFunc = func(_ context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		if calls.Add(1) == 1 {
			return nil, toolsListRejection()
		}
		return &ServerConnection{Name: name, Config: cfg}, nil
	}
	mgr := NewManager()
	cfg := config.MCPServerConfig{Enabled: true, Type: "http", URL: "http://example.test/mcp"}

	if err := mgr.ConnectServer(context.Background(), "gmail", cfg); err == nil {
		t.Fatal("first ConnectServer() succeeded, want the tools/list rejection")
	}
	if !mgr.toolsListTurnedAway("gmail") {
		t.Fatal("toolsListTurnedAway(gmail) = false after the rejection")
	}
	if err := mgr.ConnectServer(context.Background(), "gmail", cfg); err != nil {
		t.Fatalf("second ConnectServer() error = %v", err)
	}
	if mgr.toolsListTurnedAway("gmail") {
		t.Error("toolsListTurnedAway(gmail) = true after the server connected")
	}
}

// A server whose tools/list was turned away at load waits out the hold before
// its first retry; one whose initialize was turned away keeps the MST-325
// pace and is not held behind it.
func TestRetryPendingServersHoldsOnlyAToolsListRejection(t *testing.T) {
	originalSchedule, originalInterval := retryBackoffSchedule, transientRetryInterval
	originalHoldMin, originalHoldSpread := toolsListRetryHoldMin, toolsListRetryHoldSpread
	originalConnectServerFunc := connectServerFunc
	retryBackoffSchedule = []time.Duration{time.Millisecond}
	transientRetryInterval = time.Millisecond
	toolsListRetryHoldMin, toolsListRetryHoldSpread = 300*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		retryBackoffSchedule, transientRetryInterval = originalSchedule, originalInterval
		toolsListRetryHoldMin, toolsListRetryHoldSpread = originalHoldMin, originalHoldSpread
		connectServerFunc = originalConnectServerFunc
	})

	loading := true
	attemptedAt := map[string]time.Duration{}
	var start time.Time
	connectServerFunc = func(_ context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		if loading {
			if name == "gmail" {
				return nil, toolsListRejection()
			}
			return nil, initializeRejection()
		}
		if _, seen := attemptedAt[name]; !seen {
			attemptedAt[name] = time.Since(start)
		}
		return &ServerConnection{Name: name, Config: cfg}, nil
	}
	mgr := NewManager()
	cfg := config.MCPServerConfig{Enabled: true, Type: "http", URL: "http://example.test/mcp"}
	for _, name := range []string{"gmail", "calendar"} {
		_ = mgr.ConnectServer(context.Background(), name, cfg)
	}
	loading = false
	mcpCfg := config.MCPConfig{Servers: map[string]config.MCPServerConfig{"gmail": cfg, "calendar": cfg}}

	start = time.Now()
	runWithin(t, 5*time.Second, func() struct{} {
		mgr.RetryPendingServers(context.Background(), mcpCfg, t.TempDir(), []string{"gmail", "calendar"}, nil)
		return struct{}{}
	})
	if got := attemptedAt["calendar"]; got >= toolsListRetryHoldMin {
		t.Errorf("calendar first retried after %s, want it on the MST-325 pace, before the %s hold",
			got, toolsListRetryHoldMin)
	}
	if got := attemptedAt["gmail"]; got < toolsListRetryHoldMin {
		t.Errorf("gmail first retried after %s, want it held for at least %s", got, toolsListRetryHoldMin)
	}
}

func TestToolsListRetryHoldFallsBetweenSixAndTwelveMinutes(t *testing.T) {
	lowest, highest := 12*time.Minute, time.Duration(0)
	for range 1000 {
		d := toolsListRetryHold()
		if d < 6*time.Minute || d >= 12*time.Minute {
			t.Fatalf("toolsListRetryHold() = %s, want [6m, 12m)", d)
		}
		lowest, highest = min(lowest, d), max(highest, d)
	}
	// Pods woken for the same minute must not all retry at once.
	if highest-lowest < 4*time.Minute {
		t.Errorf("1000 holds spanned %s..%s, want them spread over the window", lowest, highest)
	}
}

// Only a tools/list the transport turned away is held: the server answering
// tools/list with an error of its own is not a quota running out.
func TestConnectServerMarksOnlyAToolsListTheTransportTurnedAway(t *testing.T) {
	originalConnectServerFunc := connectServerFunc
	t.Cleanup(func() {
		connectServerFunc = originalConnectServerFunc
	})
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"tools/list turned away", toolsListRejection(), true},
		{"initialize turned away", initializeRejection(), false},
		{"tools/list answered with an error", fmt.Errorf("%w: %w", errListingTools,
			&jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "Method not found"}), false},
	} {
		connectServerFunc = func(context.Context, string, config.MCPServerConfig) (*ServerConnection, error) {
			return nil, tc.err
		}
		mgr := NewManager()
		_ = mgr.ConnectServer(context.Background(), "gmail", config.MCPServerConfig{Enabled: true, Type: "http"})
		if got := mgr.toolsListTurnedAway("gmail"); got != tc.want {
			t.Errorf("%s: toolsListTurnedAway = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestToolsListRetryHoldWithoutSpreadIsTheMinimum(t *testing.T) {
	originalHoldMin, originalHoldSpread := toolsListRetryHoldMin, toolsListRetryHoldSpread
	toolsListRetryHoldMin, toolsListRetryHoldSpread = time.Second, 0
	t.Cleanup(func() {
		toolsListRetryHoldMin, toolsListRetryHoldSpread = originalHoldMin, originalHoldSpread
	})
	if got := toolsListRetryHold(); got != time.Second {
		t.Errorf("toolsListRetryHold() = %s, want 1s", got)
	}
}

// After the hold, a server turned away again keeps the MST-325 transient pace:
// waiting out the hold must not use up the fast rounds.
func TestRetryPendingServersKeepsTheTransientPaceAfterTheHold(t *testing.T) {
	originalSchedule, originalInterval := retryBackoffSchedule, transientRetryInterval
	originalHoldMin, originalHoldSpread := toolsListRetryHoldMin, toolsListRetryHoldSpread
	originalConnectServerFunc := connectServerFunc
	retryBackoffSchedule = []time.Duration{time.Millisecond}
	transientRetryInterval = 100 * time.Millisecond
	toolsListRetryHoldMin, toolsListRetryHoldSpread = 200*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		retryBackoffSchedule, transientRetryInterval = originalSchedule, originalInterval
		toolsListRetryHoldMin, toolsListRetryHoldSpread = originalHoldMin, originalHoldSpread
		connectServerFunc = originalConnectServerFunc
	})

	var attempts []time.Time
	connectServerFunc = func(_ context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		attempts = append(attempts, time.Now())
		if len(attempts) <= 2 {
			return nil, toolsListRejection()
		}
		return &ServerConnection{Name: name, Config: cfg}, nil
	}
	mgr := NewManager()
	cfg := config.MCPServerConfig{Enabled: true, Type: "http", URL: "http://example.test/mcp"}
	_ = mgr.ConnectServer(context.Background(), "gmail", cfg)

	runWithin(t, 5*time.Second, func() struct{} {
		mgr.RetryPendingServers(context.Background(),
			config.MCPConfig{Servers: map[string]config.MCPServerConfig{"gmail": cfg}},
			t.TempDir(), []string{"gmail"}, nil)
		return struct{}{}
	})
	if len(attempts) != 3 {
		t.Fatalf("connect attempts = %d, want 3 (load, held retry, transient retry)", len(attempts))
	}
	if gap := attempts[2].Sub(attempts[1]); gap < transientRetryInterval/2 {
		t.Errorf("retry after the hold came %s later, want the transient pace (>= %s)", gap, transientRetryInterval/2)
	}
}

// With another pending server on a long step of the schedule, the held one is
// still released when its hold ends.
func TestRetryPendingServersReleasesTheHoldBetweenScheduleSteps(t *testing.T) {
	originalSchedule := retryBackoffSchedule
	originalHoldMin, originalHoldSpread := toolsListRetryHoldMin, toolsListRetryHoldSpread
	originalConnectServerFunc := connectServerFunc
	retryBackoffSchedule = []time.Duration{time.Millisecond, 10 * time.Second}
	toolsListRetryHoldMin, toolsListRetryHoldSpread = 100*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		retryBackoffSchedule = originalSchedule
		toolsListRetryHoldMin, toolsListRetryHoldSpread = originalHoldMin, originalHoldSpread
		connectServerFunc = originalConnectServerFunc
	})

	loading := true
	gmailBack := make(chan time.Duration, 1)
	var start time.Time
	connectServerFunc = func(_ context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		if name == "supabase" {
			return nil, fmt.Errorf(`failed to connect: calling "initialize": sending "initialize": Unauthorized`)
		}
		if loading {
			return nil, toolsListRejection()
		}
		gmailBack <- time.Since(start)
		return &ServerConnection{Name: name, Config: cfg}, nil
	}
	mgr := NewManager()
	cfg := config.MCPServerConfig{Enabled: true, Type: "http", URL: "http://example.test/mcp"}
	_ = mgr.ConnectServer(context.Background(), "gmail", cfg)
	loading = false

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	workspace := t.TempDir()
	start = time.Now()
	go func() {
		defer close(done)
		mgr.RetryPendingServers(ctx,
			config.MCPConfig{Servers: map[string]config.MCPServerConfig{"gmail": cfg, "supabase": cfg}},
			workspace, []string{"gmail", "supabase"}, nil)
	}()
	// Registered last, so it runs first: the loop is gone before the globals
	// it reads are put back.
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case got := <-gmailBack:
		if got < toolsListRetryHoldMin {
			t.Errorf("gmail retried after %s, before its %s hold", got, toolsListRetryHoldMin)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gmail still held after 2s: the release waited for the schedule's 10s step")
	}
}

// Another server turned away on initialize keeps the round on the transient
// pace while the held one waits. The held server still gets the fast rounds
// after its release, instead of falling to the schedule's long step on its
// next 429.
func TestRetryPendingServersRestartsTheFastRoundsAtTheRelease(t *testing.T) {
	originalSchedule, originalInterval := retryBackoffSchedule, transientRetryInterval
	originalHoldMin, originalHoldSpread := toolsListRetryHoldMin, toolsListRetryHoldSpread
	originalConnectServerFunc := connectServerFunc
	retryBackoffSchedule = []time.Duration{time.Millisecond, time.Hour}
	transientRetryInterval = 4 * time.Millisecond
	toolsListRetryHoldMin, toolsListRetryHoldSpread = 300*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		retryBackoffSchedule, transientRetryInterval = originalSchedule, originalInterval
		toolsListRetryHoldMin, toolsListRetryHoldSpread = originalHoldMin, originalHoldSpread
		connectServerFunc = originalConnectServerFunc
	})

	var gmailAttempts atomic.Int32
	gmailBack := make(chan struct{})
	connectServerFunc = func(_ context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		if name == "calendar" {
			return nil, initializeRejection()
		}
		// Load and the first retry after the hold are turned away; the next one
		// connects.
		if gmailAttempts.Add(1) <= 2 {
			return nil, toolsListRejection()
		}
		close(gmailBack)
		return &ServerConnection{Name: name, Config: cfg}, nil
	}
	mgr := NewManager()
	cfg := config.MCPServerConfig{Enabled: true, Type: "http", URL: "http://example.test/mcp"}
	for _, name := range []string{"gmail", "calendar"} {
		_ = mgr.ConnectServer(context.Background(), name, cfg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	workspace := t.TempDir()
	go func() {
		defer close(done)
		mgr.RetryPendingServers(ctx,
			config.MCPConfig{Servers: map[string]config.MCPServerConfig{"gmail": cfg, "calendar": cfg}},
			workspace, []string{"gmail", "calendar"}, nil)
	}()
	// Registered last, so it runs first: the loop is gone before the globals
	// it reads are put back.
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case <-gmailBack:
	case <-time.After(3 * time.Second):
		t.Fatalf("gmail not back 3s in (%d attempts): its retry after the hold fell to the schedule's 1h step",
			gmailAttempts.Load())
	}
}

// Waiting out the hold does not advance the schedule: a held server that then
// fails for another reason retries on the schedule's first steps.
func TestRetryPendingServersDoesNotAdvanceTheScheduleDuringTheHold(t *testing.T) {
	originalSchedule := retryBackoffSchedule
	originalHoldMin, originalHoldSpread := toolsListRetryHoldMin, toolsListRetryHoldSpread
	originalConnectServerFunc := connectServerFunc
	retryBackoffSchedule = []time.Duration{time.Millisecond, 20 * time.Millisecond, time.Hour}
	toolsListRetryHoldMin, toolsListRetryHoldSpread = 300*time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		retryBackoffSchedule = originalSchedule
		toolsListRetryHoldMin, toolsListRetryHoldSpread = originalHoldMin, originalHoldSpread
		connectServerFunc = originalConnectServerFunc
	})

	var attempts atomic.Int32
	back := make(chan struct{})
	connectServerFunc = func(_ context.Context, name string, cfg config.MCPServerConfig) (*ServerConnection, error) {
		switch attempts.Add(1) {
		case 1:
			return nil, toolsListRejection()
		case 2:
			return nil, fmt.Errorf(`failed to connect: calling "initialize": sending "initialize": Unauthorized`)
		default:
			close(back)
			return &ServerConnection{Name: name, Config: cfg}, nil
		}
	}
	mgr := NewManager()
	cfg := config.MCPServerConfig{Enabled: true, Type: "http", URL: "http://example.test/mcp"}
	_ = mgr.ConnectServer(context.Background(), "gmail", cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	workspace := t.TempDir()
	go func() {
		defer close(done)
		mgr.RetryPendingServers(ctx, config.MCPConfig{Servers: map[string]config.MCPServerConfig{"gmail": cfg}},
			workspace, []string{"gmail"}, nil)
	}()
	// Registered last, so it runs first: the loop is gone before the globals
	// it reads are put back.
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case <-back:
	case <-time.After(3 * time.Second):
		t.Fatalf("gmail not back 3s in (%d attempts): the hold advanced the schedule to its 1h step", attempts.Load())
	}
}
