package evolution

import "testing"

// Uma skill nasce de trajetórias REAIS de trabalho, então o que ela copia é dado
// de cliente, não exemplo de documentação. Com Loops o usuário passa a ver essas
// skills, e elas entram no prompt de todo turno seguinte.
func TestScanDraftContent_PegaSegredoEPII(t *testing.T) {
	cases := map[string]string{
		"chave stripe":        "use a chave sk-live-abc123",
		"token github":        "export TOKEN=ghp_aaaaaaaaaaaaaaaaaaaa",
		"chave aws":           "AKIAIOSFODNN7EXAMPLE",
		"atribuição genérica": `api_key: "9f8e7d6c5b4a3210"`,
		"senha":               "password = hunter2000000",
		"chave privada":       "-----BEGIN RSA PRIVATE KEY-----",
		"bearer":              "Authorization: Bearer abcdef0123456789abcdef",
		"jwt":                 "eyJhbGciOiJIUzI1.eyJzdWIiOiIxMjM0.SflKxwRJSMeKKF2QT4",
		"url com token":       "GET https://api.crm.com/deals?access_token=abcdef123456",
		"url com senha":       "https://admin:s3nh4forte@interno.empresa.com/rel",
		"e-mail de cliente":   "falar com joao.silva@clientereal.com.br",
		"cpf":                 "cliente 123.456.789-09",
		"cnpj":                "empresa 12.345.678/0001-99",
		"telefone":            "ligar para (11) 98765-4321",
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got := scanDraftContent(SkillDraft{BodyOrPatch: body}); len(got) == 0 {
				t.Fatalf("scanDraftContent(%q) = nenhum achado, want quarentena", body)
			}
		})
	}
}

// Falso positivo custa uma quarentena, mas quarentenar a própria documentação
// que a skill cita tornaria a varredura inútil na prática.
func TestScanDraftContent_NaoPegaDocumentacao(t *testing.T) {
	clean := []string{
		"Passo 1: abrir o CRM e filtrar por estágio.",
		"Escreva para user@example.com se precisar de suporte.",
		"O campo se chama api_key na documentação.",
		"Use o endpoint /v1/deals com o header Authorization.",
	}

	for _, body := range clean {
		if got := scanDraftContent(SkillDraft{BodyOrPatch: body}); len(got) > 0 {
			t.Fatalf("scanDraftContent(%q) = %v, want nenhum achado", body, got)
		}
	}
}

// Achado quarentena, não descarta — o rascunho continua revisável.
func TestReviewDraft_AchadoViraQuarentena(t *testing.T) {
	result := ReviewDraft(SkillDraft{
		TargetSkillName: "x",
		BodyOrPatch:     "cliente 123.456.789-09",
	})
	if result.Status != DraftStatusQuarantined {
		t.Fatalf("Status = %q, want %q", result.Status, DraftStatusQuarantined)
	}
	if len(result.Findings) == 0 {
		t.Fatal("Findings vazio")
	}
}
