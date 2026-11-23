package agent

import (
	"regexp"
	"slices"
	"strings"
)

// announcementTailWindow bounds how much of the reply the detector reads. The
// promise that never lands is always the closing sentence — "…primeiro, deixa
// eu abrir o artigo." — while a reply that merely mentions a search somewhere
// in its body is a real answer, and matching the whole text would flag it.
const announcementTailWindow = 220

// actionVerbs are the steps only a tool can carry out. The newer ones are
// infinitive or gerund only, the forms that follow "vou"/"deixa eu": "salvo"
// or "gerado" in the same sentence describe a state, not a step.
//
//nolint:misspell // Portuguese stems ("instal…"), not misspelled English
const actionVerbs = `(busc\w+|pesquis\w+|procur\w+|consult\w+|verific\w+|confirm\w+|chec\w+|execut\w+|` +
	`rod(ar|ando)\b|abr(ir|indo)\b|acess\w+|baix\w+|analis\w+|[lv](er|endo) (o|a|os|as)\b|(ler|lendo)\b|` +
	`(re)?faz(er|endo)\b|cri(ar|ando)\b|ger(ar|ando)\b|mont(ar|ando)\b|prepar(ar|ando)\b|escrev(er|endo)\b|` +
	`redig(ir|indo)\b|ajust(ar|ando)\b|corrig(ir|indo)\b|consert(ar|ando)\b|revis(ar|ando)\b|edit(ar|ando)\b|` +
	`(re)?public(ar|ando)\b|envi(ar|ando)\b|mand(ar|ando)\b|salv(ar|ando)\b|atualiz(ar|ando)\b|` +
	`calcul(ar|ando)\b|renderiz(ar|ando)\b|test(ar|ando)\b|convert(er|endo)\b|organiz(ar|ando)\b|` +
	`cancel(ar|ando)\b|agend(ar|ando)\b|instal(ar|ando)\b|aplic(ar|ando)\b|adicion(ar|ando)\b|inclu(ir|indo)\b|` +
	`remov(er|endo)\b|apag(ar|ando)\b|exclu(ir|indo)\b|import(ar|ando)\b|export(ar|ando)\b|desenh(ar|ando)\b|` +
	`traduz(ir|indo)\b|compil(ar|ando)\b|implement(ar|ando)\b|tent(ar|ando)\b|contin(uar|uando)\b|` +
	`search\w*|look up\b|fetch\w*|open\w*|browse\w*|read the\b|run the\b|fix\w*|creat\w*|generat\w*|` +
	`build\w*|writ\w*|updat\w*|send\w*|publish\w*|redo\b|check\w*|edit\w*|test\w*|render\w*|sav(e|ing)\b)`

var (
	announcementAction = regexp.MustCompile(`(?i)\b` + actionVerbs)
	// nextStepPromise is a first-person immediate-future marker with an action
	// verb at most two words later: "vou corrigir", "deixa eu só confirmar",
	// "vou te mandar". The marker alone is no promise — "Deixa eu só lembrar:
	// às 17h30…" answers —, nor is a verb further off: "vou estar por aqui se
	// precisar ajustar algo" signs off.
	nextStepPromise = regexp.MustCompile(
		`(?i)(\bvou|\birei|\bdeixa eu|\bdeixe-me|\bdeixa-me|\bj[áa] volto|\bseguindo com|\blet me|\bi['’]?ll|` +
			`\bi['’]?m going to|\bi am going to)(\s+\S+){0,2}?\s+` + actionVerbs,
	)
	// negatedIntent is a refusal, "não vou criar agendamentos sem você pedir":
	// dropped before the match, so a promise later in the sentence still counts.
	negatedIntent = regexp.MustCompile(`(?i)\b(n[ãa]o|nunca|jamais|nem)\s+(vou|irei)\b`)
	// notAPromise is what makes a "vou + verb" sentence a legitimate end of a
	// turn: an offer, a step that waits on someone else, a scheduled or standing
	// intention. Nudging any of these to "perform the action now" would run it
	// early or a second time.
	notAPromise = regexp.MustCompile(`(?i)` +
		`\bse (você |vc )?(quiser|preferir|precisar|achar melhor)|\bcaso (você |vc )?(queira|prefira|precise)|` +
		`\bquer que eu\b|\bqualquer coisa\b|(^|\s)é só |` +
		`\bme (passa|passe|manda|mande|envia|envie|diz|diga|fala|fale|confirma|confirme|avisa|avise)\b|` +
		`\b(assim que|quando|depois que) (você|(vc|eles?|elas?)\b)|` + //nolint:misspell // Portuguese "eles", not English
		`\bamanh|\bdepois de amanh|\bsemana que vem\b|\bpr[óo]xim[ao] (semana|m[êe]s)|\bm[êe]s que vem\b|` +
		`\bmais tarde\b|(^|\s)à noite\b|\btod[ao]s? (os |as )?(dia|semana|manh|tarde|noite|m[êe]s|segunda|ter[çc]a|quarta|quinta|sexta|s[áa]bado|domingo)|` +
		`\b(diariamente|semanalmente|mensalmente)\b|\ba partir de (amanh|hoje|agora|segunda|ter[çc]a|quarta|quinta|sexta|s[áa]bado|domingo|\d)|` +
		`(^|\s)às \d|\b\d{1,2}h\d{0,2}\b|\b\d{1,2}:\d{2}\b|` +
		`\bem (conta|considera)|\blembr(ar|o|ando)\b|\bavis(ar|o)\b|\bde olho\b|\bmonitor\w*|` +
		`\bif you (want|like|prefer|need)\b|\bwould you like\b|\bwhen you\b|\btomorrow\b|\btonight\b|` +
		`\bnext (week|month)\b|\bevery (day|week|morning|evening|night)\b|\bremind\w*|\bkeep (it|this|that) in mind\b`,
	)
	// sentenceEnd is where a sentence stops: its closing marks and the space
	// after them, or a line break.
	sentenceEnd = regexp.MustCompile(`[.!?…]+\s+|\n+`)
)

// looksLikeUndeliveredAnnouncement reports whether a reply promises an action
// instead of performing it. The caller pairs it with the structural facts that
// make the promise a bug — no tool ran yet this turn, no tool call, tools were
// offered — so this only has to judge the prose.
func looksLikeUndeliveredAnnouncement(text string) bool {
	tail := replyTail(text)
	if tail == "" {
		return false
	}
	// A reply that stops on a colon after naming an action promised a step
	// that never followed.
	if endsOnDanglingColon(tail) && announcementAction.MatchString(tail) {
		return true
	}
	return slices.ContainsFunc(sentences(tail), promisesNextStep)
}

// endsOnUndeliveredPromise reports whether a reply's closing sentence promises
// the next step: "Página montada — agora vou publicar a versão no Skip." The
// caller only asks once a tool has run in the turn, where the reply is the
// final report. Only the last sentence counts there: a report that says what
// it was going to do and then what it did ("Vou publicar. Publicado: o link…")
// delivered, and a retry would run the step a second time.
func endsOnUndeliveredPromise(text string) bool {
	all := sentences(strings.TrimRight(text, trailingEmphasis))
	return promisesNextStep(all[len(all)-1])
}

// endsOnUnfinishedStep is the after-the-first-tool check: the reply introduces
// a step, on a colon or as a promise, instead of reporting one.
func endsOnUnfinishedStep(text string) bool {
	return endsOnDanglingColon(text) || endsOnUndeliveredPromise(text)
}

// promisesNextStep judges one sentence. A question hands the turn to the
// person, so it is a legitimate stop whatever it announces.
func promisesNextStep(sentence string) bool {
	s := negatedIntent.ReplaceAllString(sentence, "")
	return nextStepPromise.MatchString(s) && !strings.HasSuffix(s, "?") && !notAPromise.MatchString(s)
}

// sentences splits text where sentences end, each keeping its closing mark.
func sentences(text string) []string {
	ends := sentenceEnd.FindAllStringIndex(text, -1)
	all := make([]string, 0, len(ends)+1)
	start := 0
	for _, loc := range ends {
		all = append(all, strings.TrimSpace(text[start:loc[1]]))
		start = loc[1]
	}
	return append(all, strings.TrimSpace(text[start:]))
}

// replyTail is the closing stretch of a reply, where an undelivered promise
// lives.
func replyTail(text string) string {
	tail := strings.TrimSpace(text)
	if runes := []rune(tail); len(runes) > announcementTailWindow {
		tail = string(runes[len(runes)-announcementTailWindow:])
	}
	return tail
}

// trailingEmphasis is what markdown can leave after a closing colon:
// "**Próximos passos:**" still ends on the colon for whoever reads it.
const trailingEmphasis = " \t\r\n*_"

// endsOnDanglingColon reports whether a reply stops right after a colon: it
// introduced a step or a block and nothing followed. Unlike
// looksLikeUndeliveredAnnouncement it needs no verb. The caller only asks once
// a tool has run in the turn, where a reply without a call is the final report,
// and a report does not end on an introduction — "…agora as edições cirúrgicas:
// rota no App.tsx e botão no Balcão:" names no action the verb list knows, and
// it is still a step that never ran.
func endsOnDanglingColon(text string) bool {
	trimmed := strings.TrimRight(text, trailingEmphasis)
	// An emoji shortcode (":white_check_mark:") closes on a colon and still
	// ends the sentence, as it renders on Slack and Discord.
	if emojiShortcodeTail.MatchString(trimmed) {
		return false
	}
	// GLM drifting into Chinese closes the same introduction with the
	// full-width colon.
	return strings.HasSuffix(trimmed, ":") || strings.HasSuffix(trimmed, "：")
}

var emojiShortcodeTail = regexp.MustCompile(`:[a-z0-9_+\-]+:$`)
