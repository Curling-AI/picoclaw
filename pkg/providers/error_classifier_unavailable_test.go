package providers

import (
	"fmt"
	"net/http"
	"testing"
)

// Corpo REAL, capturado do gateway sob carga — é o que apareceu verbatim no
// Slack e no Telegram dos usuários.
const unavailableBody = `{"error":{"message":"The model is temporarily unavailable across all configured providers","type":"server_error","code":"provider_unavailable"}}`

func TestUpstreamUnavailableIsRecognized(t *testing.T) {
	err := httpErr(http.StatusServiceUnavailable, unavailableBody, "provider_unavailable")
	if !IsUpstreamUnavailableError(err) {
		t.Fatal("503 provider_unavailable não reconhecido")
	}
}

// O erro chega ao formatador do canal já embrulhado pelo laço de retry, e é
// nessa forma que ele precisa ser reconhecido — foi assim que vazou.
func TestUpstreamUnavailableThroughWrappedMessage(t *testing.T) {
	wrapped := fmt.Errorf(
		"LLM call failed after retries: API request failed:\n  Status: 503\n  Body: %s",
		unavailableBody,
	)
	if !IsUpstreamUnavailableError(wrapped) {
		t.Fatal("mensagem embrulhada não reconhecida")
	}
}

func TestGatewayTimeoutCounts(t *testing.T) {
	err := httpErr(
		http.StatusGatewayTimeout,
		`{"error":{"message":"upstream timed out","code":"upstream_unavailable"}}`,
		"upstream_unavailable",
	)
	if !IsUpstreamUnavailableError(err) {
		t.Fatal("504 não reconhecido")
	}
}

// Os casos negativos são o ponto: confundir indisponibilidade com falta de
// crédito trocaria "tente de novo" por "sua conta está sem créditos", que
// manda a pessoa procurar um problema que não existe.
func TestUnavailableDoesNotSwallowOtherFailures(t *testing.T) {
	casos := []struct {
		nome string
		err  error
	}{
		{"sem saldo", httpErr(http.StatusTooManyRequests,
			`{"error":{"type":"insufficient_quota","code":"insufficient_balance"}}`, "insufficient_balance")},
		{"rate limit comum", httpErr(http.StatusTooManyRequests,
			`{"error":{"message":"Rate limit reached","code":"rate_limit_exceeded"}}`, "rate_limit_exceeded")},
		{"modelo inexistente", httpErr(http.StatusNotFound,
			`{"error":{"message":"model not found","code":"model_not_found"}}`, "model_not_found")},
		{"erro interno sem código de upstream", httpErr(http.StatusInternalServerError,
			`{"error":{"message":"boom","code":"internal_error"}}`, "internal_error")},
	}
	for _, c := range casos {
		if IsUpstreamUnavailableError(c.err) {
			t.Errorf("%s foi lido como indisponibilidade de upstream", c.nome)
		}
	}
}

func TestUnavailableNilIsFalse(t *testing.T) {
	if IsUpstreamUnavailableError(nil) {
		t.Error("nil não é indisponibilidade")
	}
}
