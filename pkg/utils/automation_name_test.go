package utils

import (
	"strings"
	"testing"
)

func TestAutomationName(t *testing.T) {
	for _, tc := range []struct{ name, task, want string }{
		{"Resumo diário", "Outra tarefa", "Resumo diário"},
		{"Rodada da automação: Resumo diário", "Outra tarefa", "Resumo diário"},
		{" RODADA DA AUTOMACAO — Resumo diário ", "", "Resumo diário"},
		{"Rodada da automação. Resumo diário", "", "Resumo diário"},
		{"Rodada da automação: Rodada da automação: Notícias", "", "Notícias"},
		{"Rodada da automação: Resu...", "Rodada da automação: Resumo diário de notícias", "Resumo diário de notícias"},
		{"Rodada da automação…", "Resumo de notícias", "Resumo de notícias"},
		{"", "Rodada da automação\nResumo diário de notícias\nEnviar por e-mail", "Resumo diário de notícias"},
		{"Rodada da automação", "", "Automação agendada"},
		{"Como funciona a Rodada da automação", "", "Como funciona a Rodada da automação"},
		{"Rodada da automaçãonova", "", "Rodada da automaçãonova"},
	} {
		t.Run(tc.name+tc.task, func(t *testing.T) {
			got := AutomationName(tc.name, tc.task)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if again := AutomationName(got, tc.task); again != got {
				t.Fatalf("not idempotent: %q", again)
			}
		})
	}
	if got := AutomationName(strings.Repeat("á", 100), ""); len([]rune(got)) != maxAutomationNameRunes {
		t.Fatalf("incorrect Unicode length: %q", got)
	}
	if got := AutomationName(
		"",
		strings.Repeat("界", 40)+"private task details",
	); len([]rune(got)) > maxAutomationFallbackRunes ||
		strings.Contains(got, "private task details") {
		t.Fatalf("fallback exposed task details: %q", got)
	}
}
