package agent

import "testing"

// Every "announcement" case below is a verbatim reply from the greenhouse fleet
// (2026-09-07): the turn ended there, nothing ran, and the user had to prod the
// agent to continue. The "answer" cases are replies from the same sessions that
// merely sound similar — they are what a careless matcher breaks.
// The fixtures are Portuguese, and misspell reads locale US: it takes "rela"(ção)
// for "real" and "definitivo" for "definition". Editing a verbatim prod payload
// to please a spell checker would cost the test its whole point.
//
//nolint:misspell // verbatim prod payloads, see above
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
			"prod: stops on a colon after the action",
			"Rodando — duas queries em paralelo (spend/requests/cache + active users):",
			true,
		},
		{
			"prod: stops on a colon, gerund first",
			"Executando agora — as duas queries em paralelo:",
			true,
		},
		{
			"prod: stops on a colon mid-sentence gerund",
			"Fechando — rodando a query de validação agora:",
			true,
		},
		{
			"colon inside markdown bold after the action",
			"**Executando agora as duas queries:**",
			true,
		},
		// A colon alone is not a promise: no tool action is named.
		{
			"answer that ends on a colon without an action",
			"Os números batem com o dashboard:",
			false,
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
		// Common assistant steps the first verb list did not know.
		{"promises to redo", "Entendi o problema. Vou refazer o gráfico com cores mais fortes.", true},
		{"promises to create", "Boa ideia! Deixa eu criar a planilha com essas colunas.", true},
		{"promises to send, clitic in between", "Combinado, vou te mandar o resumo por aqui.", true},
		{"promises to schedule", "Certo, vou agendar a automação agora.", true},
		// A refusal is no promise, and a verb list that knows "enviar" would
		// otherwise nudge the model to send what it just refused to send.
		{"refusal", "Não vou enviar nada sem a sua confirmação.", false},
		{"refusal, then a real promise", "Não vou apagar o arquivo antigo, mas vou gerar a versão nova agora.", true},
		{"offer", "Se quiser, vou refazer a terceira com os três sabores.", false},
		{"question to the person", "Vou publicar a versão nova agora, pode ser?", false},
		{"scheduled", "Vou enviar o resumo toda segunda às 8h.", false},
		{"verb too far from the marker", "Vou estar por aqui se precisar ajustar algo.", false},
		{"bold verb", "Vou **buscar** o artigo do DCRainmaker sobre o Fenix 9.", true},
		{
			"how-to naming a verb",
			"Vou explicar como criar uma pasta. No terminal, use mkdir seguido do nome da pasta.",
			false,
		},
		{"clitic and adverb before the verb", "Certo, vou te rapidamente mandar o arquivo.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeUndeliveredAnnouncement(tt.text); got != tt.want {
				t.Errorf("looksLikeUndeliveredAnnouncement = %v, want %v", got, tt.want)
			}
		})
	}
}

// The "true" cases are verbatim final replies from the prod fleet (2026-10-14
// to 16): each came back with stop and no tool call after tools had already
// run in the turn, so the step it introduced never happened.
func TestEndsOnDanglingColon(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"prod: spec run, step with no action verb", danglingColonReply, true},
		{"prod: announces the publish", "Agora removo o hook temporário e publico:", true},
		{"prod: announces the check", "A resposta da IA não chegou — vou checar os logs do backend:", true},
		{"prod: introduces the report", "Dedup marcado nos dois estados. Relatório pronto:", true},
		{"prod: introduces a batch", "Lote 1/30 (índices 0–19):", true},
		{"trailing newline", "Fechando:\n", true},
		{"colon inside markdown bold", "Rodada fechada.\n\n**Próximos passos:**", true},
		{"colon inside markdown italics", "Tudo publicado. _Falta só:_", true},
		{
			"colon mid-sentence, answer follows",
			"Pronto: publiquei a versão 0.0.20 em https://teste-pl-a0abe.goskip.app",
			false,
		},
		{"colon introduces content that is there", "Os números batem com o dashboard: 42 pedidos, R$ 3.100.", false},
		{"ends on a code block", "Use assim:\n\n```ts\nconst a = 1\n```", false},
		{"emoticon", "Rodou tudo :)", false},
		{"emoji shortcode", "Tudo publicado :white_check_mark:", false},
		{"full-width colon", "接下来我修改 App.tsx：", true},
		{"plain answer", "Feito. A rota /admin voltou a responder 200.", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := endsOnDanglingColon(tt.text); got != tt.want {
				t.Errorf("endsOnDanglingColon = %v, want %v", got, tt.want)
			}
		})
	}
}

// The "true" cases are final replies that came back with stop and no tool call
// after tools had run, and ended the turn on a promise: the first mirrors one seen
// in production, the others come from eval transcripts. The "false" cases are
// finals from the same sources that also say "vou" and are the right end of a
// turn: a refusal, a question or an offer to the person, something that waits
// on someone else, a scheduled or standing intention.
//
//nolint:misspell // verbatim Portuguese replies: "calcular" is not misspelled English
func TestEndsOnUndeliveredPromise(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"prod: promises to redo the visual", promiseAfterTools, true},
		{
			"eval: next step after creating the project",
			"Projeto criado no Skip. Agora vou ver a estrutura dele para montar a página do cardápio.",
			true,
		},
		{"eval: promises to build the page", "Vou montar a página do cardápio agora.", true},
		{"eval: promises to publish", "Página montada — agora vou publicar a versão no Skip para gerar o link.", true},
		{
			"eval: promises to fix and rerun",
			"Achei um erro de digitação nessa linha — vou corrigir e rodar o script.",
			true,
		},
		{"eval: promises to read files", "Vou ler os arquivos existentes para copiar o formato exato.", true},
		{
			"eval: promise, then the reason",
			"Vou revisar o PDF gerado e corrigir a ordem das linhas — o detalhe do desconto precisa ficar no lugar certo para manter clareza.",
			true,
		},
		{"promise inside markdown bold", "Rodou sem erro. **Agora vou gerar o PDF final.**", true},
		{"english promise", "The build passed. Let me fix the last failing test.", true},

		{"reminder scheduled for later", "Pronto! Vou te lembrar amanhã às 9h para pagar o boleto do contador.", false},
		{
			"refusal after a preference",
			"Anotado que prefere rodar por conta própria — não vou criar agendamentos sem você pedir.",
			false,
		},
		{"refusal to keep data", "Apagado. Não vou guardar seu CPF.", false},
		{
			"delegation that did happen",
			"Perguntei pro Theo — ele vai calcular o total de outubro e me responde em seguida. Assim que ele responder, te passo os números.",
			false,
		},
		{"offer", "Se quiser, posso refazer a terceira com os três sabores.", false},
		{"offer with vou", "Se quiser, vou refazer a terceira com os três sabores.", false},
		{"asks the person for data", "Me passa o número com DDI e DDD que eu coloco e republico em seguida.", false},
		{"standing note", "Alérgica a amendoim — vou levar isso em conta em receitas e festas.", false},
		{"standing task from tomorrow", "Vou cuidar do seu e-mail a partir de amanhã às 8h.", false},
		{"scheduled send", "Automação criada. Vou enviar o resumo toda segunda às 8h.", false},
		{"waits on the person", "Rascunho pronto. Vou publicar assim que você aprovar.", false},
		{"waits on someone named", "Rascunho pronto. Vou publicar assim que a Ana aprovar.", false},
		{"waits on someone's material", "Página montada. Vou publicar assim que o Pedro mandar as fotos.", false},
		{"waits on someone's green light", "Rascunho salvo. Vou publicar quando a Ana der o sinal verde.", false},
		{"schedules with programar", "Fuso conferido. Vou programar o lembrete para amanhã.", true},
		{"schedules in English", "Timezone checked. I'll schedule the reminder for tomorrow.", true},
		{"waits on an approval", "Deixei o post no rascunho; vou publicar depois da aprovação do cliente.", false},
		{"schedules now what happens tomorrow", "Entendi. Vou agendar agora o lembrete para amanhã.", true},
		{"schedules a send for later", "Combinado, vou agendar o envio para segunda às 8h.", true},
		{"bold verb", "Achei o link. Vou **buscar** o artigo.", true},
		{"question", "Gerei as duas versões. Vou publicar a primeira, pode ser?", false},
		{"sign-off", "Feito! Qualquer coisa, é só chamar que eu vou ajustar.", false},
		{
			"promise, then the report",
			"Vou publicar a versão nova. Publicado: o link é https://exemplo.goskip.app",
			false,
		},
		{
			"report of work already done",
			"Corrigi o erro de digitação e rodei o script: as 42 linhas foram importadas.",
			false,
		},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := endsOnUndeliveredPromise(tt.text); got != tt.want {
				t.Errorf("endsOnUndeliveredPromise = %v, want %v", got, tt.want)
			}
		})
	}
}

// longAnswerBody stands in for a real answer that follows the promise in the
// same reply — the agent said it would look, then looked, then reported. Only
// the tail is read, so the opening promise must not flag it.
const longAnswerBody = "O resultado: a conta de agosto fechou em R$ 12.400. " +
	"O maior item foi o plano de 12 meses renovado no dia 3, R$ 7.900, e o restante se " +
	"divide entre licenças avulsas (R$ 3.100) e um ajuste retroativo de R$ 1.400 " +
	"lançado pelo financeiro na virada do mês. Nada fora do previsto no orçamento."
