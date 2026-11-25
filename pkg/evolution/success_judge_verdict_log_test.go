package evolution_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/evolution"
	"github.com/sipeed/picoclaw/pkg/logger"
)

const successVerdictLine = "Success judge verdict"

func captureEvolutionLog(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evolution.log")
	prevLevel := logger.GetLevel()
	logger.DisableFileLogging()
	logger.SetLevel(logger.INFO)
	if err := logger.EnableFileLogging(path); err != nil {
		t.Fatalf("EnableFileLogging() error = %v", err)
	}
	t.Cleanup(func() {
		logger.DisableFileLogging()
		logger.SetLevel(prevLevel)
	})
	return func() string {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q) error = %v", path, err)
		}
		return string(data)
	}
}

// Every record the cold path judges leaves one line with who decided, the
// decision model's probability and the record's shape, never its text: the
// only way to measure the judge in production.
func TestColdPath_LogsEachSuccessVerdictWithoutText(t *testing.T) {
	const (
		asked  = "monte a planilha de despesas da viagem a Recife"
		output = "Planilha pronta: 14 despesas, total de R$ 3.280, com a hospedagem como maior gasto."
	)
	long := output + " " + strings.Repeat("Detalhe por dia e categoria. ", 60)
	cases := []struct {
		name    string
		answer  decision
		content string
		want    []string
		wantNot []string
	}{
		{
			name:    "doubt falls back to the heuristic",
			answer:  decision{p: 0.5},
			content: output,
			want: []string{
				`"source":"doubt_fallback"`, `"judge_p":0.5`, `"verdict":"success"`,
				`"output_chars":83`, `"output_clipped":false`,
			},
		},
		{
			name:    "a clear no is the decision model's",
			answer:  decision{p: 0.05},
			content: output,
			want:    []string{`"source":"decision"`, `"judge_p":0.05`, `"verdict":"rejected"`},
		},
		{
			name:    "a failed call falls back without a probability",
			answer:  decision{err: errGatewayDown},
			content: output,
			want:    []string{`"source":"error_fallback"`, `"verdict":"success"`},
			wantNot: []string{`"judge_p"`},
		},
		{
			name:    "an output cut by the record is flagged",
			answer:  decision{p: 0.93},
			content: long,
			want: []string{
				`"source":"decision"`, `"verdict":"success"`,
				`"output_chars":1200`, `"output_clipped":true`,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readLog := captureEvolutionLog(t)
			root := t.TempDir()
			rt, err := evolution.NewRuntime(evolution.RuntimeOptions{
				Config: config.EvolutionConfig{Enabled: true, Mode: "draft"},
				SuccessJudge: evolution.NewDecisionTaskSuccessJudge(
					&scriptedDecider{answers: []decision{tc.answer}},
					"typesafe-ai/jev-1.14.0",
					&evolution.HeuristicSuccessJudge{},
				),
			})
			if err != nil {
				t.Fatalf("NewRuntime: %v", err)
			}
			if err := rt.FinalizeTurn(context.Background(), evolution.TurnCaseInput{
				Workspace:    root,
				TurnID:       "turn-1",
				SessionKey:   "sk_v1_" + strings.Repeat("a", 64),
				AgentID:      "main",
				Status:       "completed",
				UserMessage:  `[SENDER id="u-1" name="Ana" channel="web" role="owner"] ` + asked,
				FinalContent: tc.content,
			}); err != nil {
				t.Fatalf("FinalizeTurn: %v", err)
			}
			if err := rt.RunColdPathOnce(context.Background(), root); err != nil {
				t.Fatalf("RunColdPathOnce: %v", err)
			}

			log := readLog()
			if strings.Count(log, successVerdictLine) != 1 {
				t.Fatalf("want one %q line, log:\n%s", successVerdictLine, log)
			}
			want := append([]string{`"session_kind":"opaque"`, `"asked_chars":47`}, tc.want...)
			for _, field := range want {
				if !strings.Contains(log, field) {
					t.Errorf("log lacks %s:\n%s", field, log)
				}
			}
			for _, field := range append([]string{asked, "Planilha pronta", "Ana"}, tc.wantNot...) {
				if strings.Contains(log, field) {
					t.Errorf("log carries %q:\n%s", field, log)
				}
			}
		})
	}
}
