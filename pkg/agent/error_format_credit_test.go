package agent

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/common"
)

const creditBody = `{"error":{"message":"You exceeded your current credit balance","type":"insufficient_quota","code":"insufficient_balance"}}`

// The out-of-credit text is the one error message that reaches an end user
// VERBATIM through the messaging channels and cron, so it has requirements the
// other branches do not.
// (seucaranguejo fork)
func TestOutOfCreditMessageIsUserFacing(t *testing.T) {
	err := &common.HTTPError{
		StatusCode:  http.StatusTooManyRequests,
		BodyPreview: creditBody,
		ErrorCode:   "insufficient_balance",
	}
	msg := formatProcessingError(err)

	// No raw error dump: a 429 JSON in a WhatsApp DM is worse than no message.
	if strings.Contains(msg, "Original error") {
		t.Errorf("raw error leaked to the user: %q", msg)
	}
	if strings.Contains(msg, "insufficient_balance") || strings.Contains(msg, "429") {
		t.Errorf("machine-readable detail leaked to the user: %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "error processing message") {
		t.Errorf("generic English prefix leaked into a user-facing string: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "crédito") {
		t.Errorf("message does not name the actual problem: %q", msg)
	}
	// There is no self-service top-up yet; promising one would send the user
	// looking for a button that does not exist.
	if strings.Contains(strings.ToLower(msg), "recarregue") {
		t.Errorf("message promises a purchase flow that does not exist: %q", msg)
	}
}

func TestOutOfCreditMessageSurvivesRetryWrapping(t *testing.T) {
	// By the time the channel formats it, the error has been through the retry
	// loop's fmt.Errorf wrapper.
	wrapped := fmt.Errorf("LLM call failed after retries: %w",
		errors.New("API request failed:\n  Status: 429\n  Body:   "+creditBody))
	msg := formatProcessingError(wrapped)
	if !strings.Contains(strings.ToLower(msg), "crédito") {
		t.Errorf("wrapped error fell through to the generic branch: %q", msg)
	}
}

func TestUnrelatedErrorsKeepTheGenericFormat(t *testing.T) {
	msg := formatProcessingError(errors.New("connection reset by peer"))
	if !strings.Contains(msg, "Error processing message") {
		t.Errorf("generic path changed: %q", msg)
	}
}

// O que os usuários receberam no Slack e no Telegram: o envelope JSON do
// gateway, cru, como se fosse resposta do assistente.
func TestUpstreamUnavailableNaoVazaJSON(t *testing.T) {
	raw := `{"error":{"message":"The model is temporarily unavailable across all configured providers","type":"server_error","code":"provider_unavailable"}}`
	err := fmt.Errorf("LLM call failed after retries: API request failed:\n  Status: 503\n  Body: %s", raw)

	got := formatProcessingError(err)

	for _, proibido := range []string{"{", "provider_unavailable", "Status:", "Original error:", "503"} {
		if strings.Contains(got, proibido) {
			t.Errorf("vazou %q para o usuário: %s", proibido, got)
		}
	}
	if !strings.Contains(got, "Tente de novo") {
		t.Errorf("não diz o que fazer: %s", got)
	}
	// Não pode culpar a conta: a falha é de capacidade do provedor.
	if strings.Contains(strings.ToLower(got), "crédito") {
		t.Errorf("confundiu indisponibilidade com falta de crédito: %s", got)
	}
}
