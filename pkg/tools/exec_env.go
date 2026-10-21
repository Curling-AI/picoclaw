package tools

import (
	"context"
	"os/exec"
	"sync/atomic"
)

// ExecEnvProvider devolve variáveis a acrescentar ao ambiente de um processo
// que o exec abre. Recebe o ctx da chamada, que traz a sessão do turno
// (ToolSessionKey): é por ela que o host sabe de que turno veio o que o
// processo fizer. (seucaranguejo fork — o pod passa por aqui um proxy local que
// vê o que os scripts mandam pela API do chat; ver agent/delivery_mirror.go)
type ExecEnvProvider func(ctx context.Context) []string

var execEnvProvider atomic.Pointer[ExecEnvProvider]

// SetExecEnvProvider vale para todo exec do processo, inclusive o dos crons de
// comando. nil volta ao ambiente herdado.
func SetExecEnvProvider(fn ExecEnvProvider) {
	if fn == nil {
		execEnvProvider.Store(nil)
		return
	}
	execEnvProvider.Store(&fn)
}

// applyExecEnv vem depois do cmd.Dir (o Environ() acerta o PWD por ele) e antes
// do isolation.Start, que parte do cmd.Environ() para sobrepor HOME e TMPDIR.
func applyExecEnv(ctx context.Context, cmd *exec.Cmd) {
	provider := execEnvProvider.Load()
	if provider == nil {
		return
	}
	extra := (*provider)(ctx)
	if len(extra) == 0 {
		return
	}
	// Chave repetida vale a última: o que o host acrescenta ganha do herdado.
	cmd.Env = append(cmd.Environ(), extra...)
}
