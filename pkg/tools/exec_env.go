package tools

import (
	"context"
	"os/exec"
	"sync/atomic"
)

// ExecEnvProvider returns variables to add to the environment of a process exec
// starts. It gets the call's ctx, which carries the turn's session
// (ToolSessionKey): that is how the host knows which turn whatever the process
// does came from. (seucaranguejo fork — the pod routes a local proxy through
// here that sees what scripts send through the chat's API; see
// agent/delivery_mirror.go)
type ExecEnvProvider func(ctx context.Context) []string

var execEnvProvider atomic.Pointer[ExecEnvProvider]

// SetExecEnvProvider applies to every exec of the process, including the one
// command cron jobs run. nil goes back to the inherited environment.
func SetExecEnvProvider(fn ExecEnvProvider) {
	if fn == nil {
		execEnvProvider.Store(nil)
		return
	}
	execEnvProvider.Store(&fn)
}

// applyExecEnv runs after cmd.Dir is set (Environ() fixes PWD from it) and
// before isolation.Start, which starts from cmd.Environ() to override HOME and
// TMPDIR.
func applyExecEnv(ctx context.Context, cmd *exec.Cmd) {
	provider := execEnvProvider.Load()
	if provider == nil {
		return
	}
	extra := (*provider)(ctx)
	if len(extra) == 0 {
		return
	}
	// A repeated key takes the last value: what the host adds wins.
	cmd.Env = append(cmd.Environ(), extra...)
}
