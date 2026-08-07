package evolution

import (
	"context"
	"os"
	"strconv"
	"time"
)

// Timeouts das chamadas de LLM do caminho frio.
//
// O de clusterização era 45s fixos, e isso saía caro de um jeito invisível:
// medido em produção em 07/08/2026, 89 de 91 chamadas do clusterizador
// estouravam esse limite. O contexto cancelava no meio da geração, a conexão
// com o gateway caía — 13% de todas as chamadas de LLM da plataforma, a maior
// fonte de erro do gateway — e o código caía no clusterizador heurístico SEM
// REGISTRAR NADA. Ou seja: pagávamos a geração, jogávamos fora, usávamos a
// heurística, e ninguém sabia que a clusterização por LLM não funcionava.
//
// O penhasco era limpo: quase nada terminava entre 46s e 50s, então essas
// chamadas não estavam "quase lá" — são muito mais longas que 45s. Subir para
// 60 ou 90 só moveria o corte de lugar.
//
// Por isso os três viraram configuráveis: o valor certo depende do modelo por
// trás do alias e do tamanho do lote, e nenhum dos dois é estável o bastante
// para virar constante. O default novo dá folga real; quem observar a cauda em
// produção ajusta sem release do fork.
const (
	defaultTaskSuccessJudgeTimeout = 15 * time.Second
	defaultPatternClusterTimeout   = 180 * time.Second
	defaultDraftGenerationTimeout  = 60 * time.Second

	envTaskSuccessJudgeTimeout = "PICOCLAW_EVOLUTION_JUDGE_TIMEOUT_SECONDS"
	envPatternClusterTimeout   = "PICOCLAW_EVOLUTION_CLUSTER_TIMEOUT_SECONDS"
	envDraftGenerationTimeout  = "PICOCLAW_EVOLUTION_DRAFT_TIMEOUT_SECONDS"
)

func llmTaskSuccessJudgeTimeout() time.Duration {
	return timeoutFromEnv(envTaskSuccessJudgeTimeout, defaultTaskSuccessJudgeTimeout)
}

func llmPatternClusterTimeout() time.Duration {
	return timeoutFromEnv(envPatternClusterTimeout, defaultPatternClusterTimeout)
}

func llmDraftGenerationTimeout() time.Duration {
	return timeoutFromEnv(envDraftGenerationTimeout, defaultDraftGenerationTimeout)
}

// timeoutFromEnv lê segundos da env. Valor ausente, ilegível ou <= 0 mantém o
// default: um typo na variável não pode virar "sem timeout" num caminho de
// fundo que ninguém observa.
func timeoutFromEnv(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		return fallback
	}
	return time.Duration(secs) * time.Second
}

func withLLMCallTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= timeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}
