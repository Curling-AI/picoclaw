//go:build !windows

package tools

import (
	"context"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newOwnerTestTool(t *testing.T) (*ExecTool, *SessionManager) {
	t.Helper()
	tool, err := NewExecTool("", false)
	require.NoError(t, err)
	sm := NewSessionManager()
	t.Cleanup(sm.Stop)
	tool.sessionManager = sm
	return tool, sm
}

func startOwned(t *testing.T, tool *ExecTool, sm *SessionManager, owner, command string) *ProcessSession {
	t.Helper()
	ctx := WithToolSessionContext(context.Background(), "main", owner, nil)
	result := tool.Execute(ctx, map[string]any{"action": "run", "command": command, "background": "true"})
	require.False(t, result.IsError, result.ForLLM)
	for _, info := range sm.List() {
		if s, err := sm.Get(info.ID); err == nil && s.Owner == owner && s.Command == command {
			return s
		}
	}
	t.Fatalf("no session recorded for owner %q", owner)
	return nil
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// nickgs1337 on #112: the session is done as soon as its sh exits, but what the
// sh left in the background (nohup script &) keeps running in its group.
func TestKillOwnedBy_StopsWhatTheShellLeftBehind(t *testing.T) {
	tool, sm := newOwnerTestTool(t)
	s := startOwned(t, tool, sm, "subturn-7", "nohup sleep 30 >/dev/null 2>&1 & echo $!")
	var pid int
	require.Eventually(t, func() bool {
		if !s.IsDone() {
			return false
		}
		n, err := strconv.Atoi(strings.TrimSpace(s.Read()))
		pid = n
		return err == nil
	}, 5*time.Second, 20*time.Millisecond, "the shell never printed the pid it left behind")
	require.True(t, processAlive(pid), "the left-behind process is not running")
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	killed := sm.KillOwnedBy("subturn-7")

	require.Equal(t, []string{s.ID}, killed)
	require.Eventually(t, func() bool { return !processAlive(pid) }, 5*time.Second, 20*time.Millisecond,
		"the process the shell left behind kept running")
}

// The returned ids are processes actually stopped, not attempts.
func TestKillOwnedBy_ListsOnlyWhatItStopped(t *testing.T) {
	tool, sm := newOwnerTestTool(t)
	s := startOwned(t, tool, sm, "subturn-7", "true")
	require.Eventually(t, s.IsDone, 5*time.Second, 20*time.Millisecond)

	require.Empty(t, sm.KillOwnedBy("subturn-7"), "a finished process with nothing left was reported as stopped")
}

// A sub-turn that finished hands what it left running to the turn that
// launched it, so that turn's failure stops it.
func TestHandOver_MovesTheProcessesToTheNewOwner(t *testing.T) {
	tool, sm := newOwnerTestTool(t)
	s := startOwned(t, tool, sm, "subturn-9", "sleep 30")
	t.Cleanup(func() { _ = s.Kill() })

	sm.HandOver("subturn-9", "subturn-8")

	require.Empty(t, sm.KillOwnedBy("subturn-9"))
	require.Equal(t, []string{s.ID}, sm.KillOwnedBy("subturn-8"))
}
