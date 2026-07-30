package evolution

import (
	"regexp"
	"strings"
)

type DraftReviewResult struct {
	Status      DraftStatus
	Findings    []string
	ReviewNotes []string
}

func ReviewDraft(draft SkillDraft) DraftReviewResult {
	findings := append([]string(nil), ValidateDraft(draft)...)
	findings = append(findings, scanDraftContent(draft)...)

	result := DraftReviewResult{
		Status:      DraftStatusCandidate,
		Findings:    findings,
		ReviewNotes: []string{"local structural validation completed"},
	}
	if len(findings) > 0 {
		result.Status = DraftStatusQuarantined
	}
	return result
}

// draftScanPattern é uma checagem no rascunho de skill ANTES de ele virar
// arquivo. Um achado não descarta o rascunho: quarentena.
type draftScanPattern struct {
	re      *regexp.Regexp
	finding string
}

// draftScanPatterns cobre o que uma skill gerada por um agente com CRM, e-mail e
// banco conectados pode acabar embutindo.
//
// A varredura era de quatro substrings de chave de API. Isso bastava quando as
// skills eram invisíveis; com Loops o usuário PASSA A VER as skills geradas — e,
// mais importante, uma skill nasce de trajetórias reais de trabalho, então o
// material que ela copia é dado de cliente, não exemplo de documentação.
//
// Falso positivo aqui custa uma quarentena; falso negativo custa PII gravada
// num arquivo que entra no prompt de todo turno seguinte. O viés é deliberado.
var draftScanPatterns = []draftScanPattern{
	{
		// Chaves de API: os prefixos conhecidos + qualquer atribuição a um campo
		// com cara de credencial.
		re: regexp.MustCompile(
			`(?i)\b(sk-live-|sk_test_|sk-proj-|ghp_|gho_|github_pat_|xox[baprs]-|AKIA[0-9A-Z]{16})` +
				`|(?i)\b(api[_-]?key|secret|password|passwd|token|authorization)\s*[:=]\s*\S{8,}`),
		finding: "secret-like token detected in body_or_patch",
	},
	{
		re:      regexp.MustCompile(`(?i)-----begin [a-z ]*private key-----`),
		finding: "private key material detected in body_or_patch",
	},
	{
		// Bearer / JWT. O JWT tem forma própria (três blocos base64url) e começa
		// sempre por "eyJ" porque o header serializado começa por '{"'.
		re:      regexp.MustCompile(`(?i)\bbearer\s+[\w-]{16,}|\beyJ[\w-]{10,}\.[\w-]{10,}\.[\w-]{10,}`),
		finding: "bearer token or JWT detected in body_or_patch",
	},
	{
		// URL com credencial embutida (?token=..., &access_token=..., user:pass@host).
		re: regexp.MustCompile(
			`(?i)[?&](access_?token|api_?key|auth|signature|sig|password)=[^&\s]{8,}` +
				`|https?://[^/\s:@]+:[^/\s@]+@`),
		finding: "URL with embedded credential detected in body_or_patch",
	},
	{
		// E-mail. Excluir domínios de exemplo evita quarentenar a própria
		// documentação que a skill cita.
		re:      regexp.MustCompile(`(?i)\b[\w.+-]+@(?:[\w-]+\.)+[a-z]{2,}\b`),
		finding: "e-mail address detected in body_or_patch",
	},
	{
		// CPF e CNPJ, com ou sem máscara. A base de usuários é brasileira e
		// isto é o dado pessoal que mais aparece num CRM.
		re:      regexp.MustCompile(`\b\d{3}\.?\d{3}\.?\d{3}-?\d{2}\b|\b\d{2}\.?\d{3}\.?\d{3}/?\d{4}-?\d{2}\b`),
		finding: "CPF/CNPJ detected in body_or_patch",
	},
	{
		// Telefone brasileiro com DDD, com ou sem +55.
		re:      regexp.MustCompile(`(?:\+55\s?)?\(?\d{2}\)?\s?9?\d{4}[-\s]?\d{4}\b`),
		finding: "phone number detected in body_or_patch",
	},
}

// exampleEmailDomains não são PII — são o que a documentação usa.
var exampleEmailDomains = regexp.MustCompile(`(?i)@(example\.(com|org|net)|test\.com|localhost|acme\.com|foo\.bar)\b`)

func scanDraftContent(draft SkillDraft) []string {
	body := draft.BodyOrPatch
	findings := make([]string, 0, len(draftScanPatterns))

	for _, p := range draftScanPatterns {
		match := p.re.FindString(body)
		if match == "" {
			continue
		}
		if strings.Contains(match, "@") && exampleEmailDomains.MatchString(match) {
			continue
		}
		findings = append(findings, p.finding)
	}

	return findings
}
