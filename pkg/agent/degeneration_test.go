package agent

import (
	"strings"
	"testing"
)

// prodLoop reproduces the greenhouse report of 2026-09-07: the model latched
// onto one phrase and spent the rest of its budget on it, so the user got a
// wall of it instead of an answer.
var prodLoop = "Vou fazer uma busca ampla em todos os registros, memórias e skills para encontrar tudo sobre " +
	strings.Repeat("princípios de interação com IA / \"", 22)

func TestLooksDegenerate(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"prod loop", prodLoop, true},
		{"short clause repeated many times", strings.Repeat("nao consegui acessar. ", 14), true},
		{"empty", "", false},
		{"plain answer", "A fatura de agosto fechou em R$ 12.400, 8% acima de julho.", false},
		// A separator row repeats hard but carries no letters — flagging it would
		// throw away every reply that ends in a table.
		{"markdown table separator", "| item | valor |\n" + strings.Repeat("| --- | --- |\n", 9), false},
		// Distinct list items look repetitive without being a loop.
		{
			"numbered list",
			"1. revisar o contrato\n2. revisar a fatura\n3. revisar o aditivo\n4. revisar o anexo\n5. revisar o laudo\n",
			false,
		},
		// Twice is emphasis, not a loop.
		{"phrase repeated twice", strings.Repeat("isso precisa ser revisado com cuidado. ", 2), false},
		// Long enough block, but only four repeats.
		{"four repeats is under the bar", strings.Repeat("preciso confirmar esse dado antes. ", 4), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksDegenerate(tt.text); got != tt.want {
				t.Errorf("looksDegenerate = %v, want %v", got, tt.want)
			}
		})
	}
}
