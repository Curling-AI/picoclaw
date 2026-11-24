package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// announcedToolCall is verbatim from greenhouse (2026-09-07). The turn ended
// right here: no markup for the truncation guard to find, no call on the wire,
// and the user had to type "Siga" to get anything else.
//
//nolint:misspell // verbatim prod payload: misspell reads "rela"(ção) as "real"
const announcedToolCall = "Vou buscar o artigo do DCRainmaker sobre o Fenix 9 e resumir, depois procurar " +
	"relação com o seu relógio 970.\n\nPrimeiro, deixa eu abrir o artigo."

const promiseNudgeMarker = "announcing a step it did not take"

func TestPromiseBeforeAnyToolIsRetriedWithANudge(t *testing.T) {
	al, provider := newJudgedLoop(t, []judgeAnswer{judgedYes, judgedNo},
		providers.LLMResponse{Content: announcedToolCall},
		readFileCall("call_1"),
		providers.LLMResponse{Content: "o Fenix 9 traz mapas offline e ECG"},
	)

	resp, err := al.ProcessDirect(context.Background(), "resuma esse artigo pra mim", "announce-retry-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "o Fenix 9 traz mapas offline e ECG" {
		t.Errorf("response = %q, want the answer after the retry", resp)
	}
	if got := provider.calls(); got != 3 {
		t.Fatalf("provider calls = %d, want 3 (promise, retry with a call, answer)", got)
	}
	if !provider.callCarried(2, promiseNudgeMarker, "deixa eu abrir o artigo") {
		t.Error("the retry went out without the nudge quoting the discarded promise")
	}
	if provider.callCarried(3, promiseNudgeMarker) {
		t.Error("the nudge kept telling the final call to take a step that already ran")
	}
	if calls := provider.judgeCalls(); len(calls) == 0 || !strings.Contains(calls[0].input, "used in this turn: none") {
		t.Error("the judge was not told that no tool had run yet")
	}
	for _, m := range directSessionHistory(t, al) {
		if strings.Contains(m.Content, promiseNudgeMarker) || strings.Contains(m.Content, "deixa eu abrir") {
			t.Errorf("the nudge or the discarded promise was persisted: %q", m.Content)
		}
	}
}

// Out of retries, the reply goes out and stays in the session. Dropped, as it
// was, the person got the empty-response fallback, or nothing, in place of an
// answer the guard had taken for a promise.
func TestPromiseBeforeAnyToolOutOfRetriesIsDelivered(t *testing.T) {
	const again = "Vou abrir o artigo agora mesmo."
	readLog := captureAgentLog(t)
	al, provider := newJudgedLoop(t, []judgeAnswer{judgedYes, judgedYes},
		providers.LLMResponse{Content: announcedToolCall},
		providers.LLMResponse{Content: again},
	)

	resp, err := al.ProcessDirect(context.Background(), "resuma esse artigo pra mim", "announce-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != again {
		t.Errorf("response = %q, want the retried reply delivered", resp)
	}
	if got := provider.calls(); got != 1+maxPromiseRetries {
		t.Errorf("provider calls = %d, want %d (original + capped retries)", got, 1+maxPromiseRetries)
	}
	if strings.Count(readLog(), deliveredPromiseLine) != 1 {
		t.Error("the delivered promise was not logged once")
	}
	kept := false
	for _, m := range directSessionHistory(t, al) {
		kept = kept || m.Role == "assistant" && m.Content == again
		if strings.Contains(m.Content, promiseNudgeMarker) {
			t.Errorf("the corrective nudge was persisted: %q", m.Content)
		}
	}
	if !kept {
		t.Error("the delivered reply is not in the session: a reload would lose it")
	}
}

// A complete answer that quotes a line for the person to use ("vou verificar
// agora") is not a promise: the judge says so and it goes out on the first
// call. The regex guard retried it and, when the model repeated it, dropped it.
func TestCompleteAnswerQuotingAPromiseGoesOutOnTheFirstCall(t *testing.T) {
	const answer = "1. \"Isso não é comigo.\" Motivo: empurra o problema.\n\n" +
		"Em vez de “não sei”, diga “boa pergunta, vou verificar agora e já te retorno”."
	al, provider := newJudgedLoop(t, []judgeAnswer{judgedNo},
		providers.LLMResponse{Content: answer},
	)

	resp, err := al.ProcessDirect(
		context.Background(),
		"frases que o atendente não deve dizer",
		"quoted-promise-session",
	)
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != answer {
		t.Errorf("response = %q, want the answer untouched", resp)
	}
	if got := provider.calls(); got != 1 {
		t.Errorf("provider calls = %d, want 1 (no retry)", got)
	}
}
