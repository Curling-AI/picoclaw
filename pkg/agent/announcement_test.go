package agent

import "testing"

// Every "announcement" case below is a verbatim reply from the greenhouse fleet
// (2026-09-07): the turn ended there, nothing ran, and the user had to prod the
// agent to continue. The "answer" cases are replies from the same sessions that
// merely sound similar — they are what a careless matcher breaks.
func TestLooksLikeUndeliveredAnnouncement(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{
			"prod: promises to open the article",
			"Vou buscar o artigo do DCRainmaker sobre o Fenix 9 e resumir, depois procurar relação com o seu relógio 970.\n\nPrimeiro, deixa eu abrir o artigo.",
			true,
		},
		{
			"prod: promises after the user said continue",
			"Seguindo com a pesquisa. Deixa eu buscar os dados dos dois modelos no Brasil.",
			true,
		},
		{
			"prod: promises to confirm current data",
			"Um ponto rápido antes da pesquisa: são veículos de marcas chinesas diferentes (Denza é da BYD, Jetour é do grupo Chery), então a comparação vai envolver preço, tamanho, motorização e posicionamento no Brasil. Deixa eu confirmar os dados atuais.",
			true,
		},
		{
			"prod: promises to run in stages",
			"Entendido, Dr. Vou executar. Começando pelo estudo definitivo na internet sobre envio em massa via WhatsApp Web. Vou pesquisar.",
			true,
		},
		{
			"prod: promises to read the second file",
			"Achei! O prompt aparece em sessões de smoke test. Deixa eu ver o segundo arquivo e confirmar o contexto",
			true,
		},
		{
			"english promise",
			"Sure — let me open the changelog and pull the exact version.",
			true,
		},
		// The marker alone is not a promise: this one answered, then added a
		// reminder. Flagging it would retry a turn that already delivered.
		{
			"answer that opens with the marker",
			"Anotado! ✅ Manual mesmo então.\n\nDeixa eu só lembrar: às 17h30 hoje tem o Resumo Diário Pendências — SKIPPERS & CX.",
			false,
		},
		// The action verb alone is not a promise either: past tense reports what
		// already ran.
		{
			"report of work already done",
			"Busquei os três relatórios e consolidei: a fatura de agosto fechou em R$ 12.400, com 8% acima do mês anterior.",
			false,
		},
		{
			"promise buried far from the end still answers",
			"Vou pesquisar isso pra você. " + longAnswerBody,
			false,
		},
		{"empty", "", false},
		{"whitespace only", "   \n\t ", false},
		{
			"plain answer",
			"A capital do Brasil é Brasília.",
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeUndeliveredAnnouncement(tt.text); got != tt.want {
				t.Errorf("looksLikeUndeliveredAnnouncement = %v, want %v", got, tt.want)
			}
		})
	}
}

// longAnswerBody stands in for a real answer that follows the promise in the
// same reply — the agent said it would look, then looked, then reported. Only
// the tail is read, so the opening promise must not flag it.
const longAnswerBody = "O resultado: a conta de agosto fechou em R$ 12.400. " +
	"O maior item foi o plano anual renovado no dia 3, R$ 7.900, e o restante se " +
	"divide entre licenças avulsas (R$ 3.100) e um ajuste retroativo de R$ 1.400 " +
	"lançado pelo financeiro na virada do mês. Nada fora do previsto no orçamento."
