package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// promiseAfterTools mirrors the final reply of a real turn: after
// web_search, exec, write_file, exec and load_image it came back with stop and
// no tool call, went out as the answer, and the turn ended there.
const promiseAfterTools = "Você tem razão, o gráfico ficou confuso. Vou refazer com uma ideia visual mais clara: " +
	"comparar as duas curvas lado a lado e marcar onde elas se separam — ou seja, o que muda na prática."

const deliveredPromiseLine = "Final reply still promises a step that never ran"

// The turn did real work: when the retry promises again, the reply goes out as
// today, and the delivery is logged for counting without its text.
func TestPromiseAfterAToolOutOfRetriesIsDeliveredAndLogged(t *testing.T) {
	readLog := captureAgentLog(t)
	al, provider := newJudgedLoop(t, []judgeAnswer{judgedYes, judgedYes},
		readFileCall("call_1"),
		providers.LLMResponse{Content: promiseAfterTools},
		providers.LLMResponse{Content: "Vou montar a página do cardápio agora."},
	)

	resp, err := al.ProcessDirect(context.Background(), "o visual ficou fraco", "promise-cap-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != "Vou montar a página do cardápio agora." {
		t.Errorf("response = %q, want the retried reply delivered as is", resp)
	}
	if got := provider.calls(); got != 2+maxPromiseRetries {
		t.Errorf("provider calls = %d, want %d (call + original + capped retries)", got, 2+maxPromiseRetries)
	}
	log := readLog()
	if strings.Count(log, deliveredPromiseLine) != 1 {
		t.Fatalf("want one %q line, log:\n%s", deliveredPromiseLine, log)
	}
	for _, field := range []string{
		"tool_ran_this_turn", "request_id", "model", "content_chars", "judge_p", "judge_model", "judge_ms",
	} {
		if !strings.Contains(log, field) {
			t.Errorf("the delivery line has no %s field", field)
		}
	}
	if strings.Contains(log, "cardápio") || strings.Contains(log, "Vou refazer") {
		t.Error("the reply text reached the log")
	}
}

func TestLegitimateFinalAfterAToolIsNeitherRetriedNorLogged(t *testing.T) {
	const final = "Pronto! Vou te lembrar amanhã às 9h para pagar o boleto do contador."
	readLog := captureAgentLog(t)
	al, provider := newJudgedLoop(t, []judgeAnswer{judgedNo},
		readFileCall("call_1"),
		providers.LLMResponse{Content: final},
	)

	resp, err := al.ProcessDirect(context.Background(), "me lembra do boleto amanhã às 9h", "promise-legit-session")
	if err != nil {
		t.Fatalf("ProcessDirect: %v", err)
	}
	if resp != final {
		t.Errorf("response = %q, want the reply untouched", resp)
	}
	if got := provider.calls(); got != 2 {
		t.Errorf("provider calls = %d, want 2 (no retry)", got)
	}
	if strings.Contains(readLog(), deliveredPromiseLine) {
		t.Error("a legitimate final was logged as an undelivered promise")
	}
}
