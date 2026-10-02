//go:build !windows

package mcp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
)

func TestClosedStdioSessionStopsWhatTheServerLaunched(t *testing.T) {
	shrinkHandshakeTimeout(t, 300*time.Millisecond)
	launcher, pidFile := stdioHelperConfigWithPIDFile(t, stdioHelperLauncher)

	runWithin(t, 10*time.Second, func() error {
		_, err := connectServer(context.Background(), "launcher", launcher)
		return err
	})

	assertProcessEnds(t, readHelperPID(t, pidFile))
}

func TestConnectServerStopsServerWithUnsupportedProtocol(t *testing.T) {
	server, pidFile := stdioHelperConfigWithPIDFile(t, stdioHelperOldProtocol)

	err := runWithin(t, 10*time.Second, func() error {
		_, err := connectServer(context.Background(), "old-protocol", server)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol version") {
		t.Fatalf("connectServer() error = %v, want unsupported protocol version", err)
	}

	assertProcessEnds(t, readHelperPID(t, pidFile))
}

func stdioHelperConfigWithPIDFile(t *testing.T, mode string) (config.MCPServerConfig, string) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	cfg := stdioHelperConfig(t, mode)
	cfg.Env[stdioHelperPIDFileEnv] = pidFile
	return cfg, pidFile
}

func readHelperPID(t *testing.T, pidFile string) int {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("reading helper PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parsing helper PID %q: %v", raw, err)
	}
	return pid
}

// assertProcessEnds waits for an orphan to be killed and reaped; a failure
// leaves the process alone, since its PID may already belong to someone else.
func assertProcessEnds(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for processRunning(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d is still running after its session closed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processRunning(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	// A killed orphan stays a zombie until init reaps it, and kill(pid, 0)
	// still answers for zombies; where PID 1 never reaps (a bare container),
	// that would look like a live process.
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	end := bytes.LastIndexByte(stat, ')')
	return end < 0 || end+2 >= len(stat) || stat[end+2] != 'Z'
}
