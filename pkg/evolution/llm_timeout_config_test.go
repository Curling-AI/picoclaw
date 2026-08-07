package evolution

import (
	"testing"
	"time"
)

// O default do clusterizador era 45s, e medido em produção 89 de 91 chamadas
// estouravam. O número certo depende do modelo por trás do alias e do tamanho
// do lote — nenhum dos dois estável o bastante para virar constante.
func TestPatternClusterTimeout_DefaultTemFolga(t *testing.T) {
	if got := llmPatternClusterTimeout(); got != 180*time.Second {
		t.Errorf("default = %v, want 180s", got)
	}
	if llmPatternClusterTimeout() <= 45*time.Second {
		t.Error("o default voltou a ser curto demais — era exatamente esse o corte que derrubava 13% das chamadas")
	}
}

func TestTimeoutFromEnv_LeSegundos(t *testing.T) {
	t.Setenv(envPatternClusterTimeout, "300")
	if got := llmPatternClusterTimeout(); got != 300*time.Second {
		t.Errorf("com env=300 -> %v, want 300s", got)
	}
}

// Um typo na variável não pode virar "sem timeout" num caminho de fundo que
// ninguém observa — tem de cair no default.
func TestTimeoutFromEnv_ValorInvalidoMantemDefault(t *testing.T) {
	for _, ruim := range []string{"abc", "0", "-30", " ", "30s"} {
		t.Setenv(envPatternClusterTimeout, ruim)
		if got := llmPatternClusterTimeout(); got != 180*time.Second {
			t.Errorf("env=%q -> %v, want o default de 180s", ruim, got)
		}
	}
}

// Os três são independentes: mexer num não pode arrastar os outros.
func TestTimeouts_SaoIndependentes(t *testing.T) {
	t.Setenv(envPatternClusterTimeout, "300")
	if got := llmTaskSuccessJudgeTimeout(); got != 15*time.Second {
		t.Errorf("judge = %v, want 15s", got)
	}
	if got := llmDraftGenerationTimeout(); got != 60*time.Second {
		t.Errorf("draft = %v, want 60s", got)
	}
}
