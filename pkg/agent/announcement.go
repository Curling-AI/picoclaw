package agent

import (
	"regexp"
	"strings"
)

// announcementTailWindow bounds how much of the reply the detector reads. The
// promise that never lands is always the closing sentence — "…primeiro, deixa
// eu abrir o artigo." — while a reply that merely mentions a search somewhere
// in its body is a real answer, and matching the whole text would flag it.
const announcementTailWindow = 220

// An announcement needs both halves: a first-person immediate-future marker AND
// a verb that only a tool can carry out. "Deixa eu só lembrar que às 17h30…" is
// a real answer that happens to open with the marker; requiring the action verb
// is what keeps it out.
var (
	announcementIntent = regexp.MustCompile(
		`(?i)(\bvou\b|\birei\b|\bdeixa eu\b|\bdeixe-me\b|\bdeixa-me\b|\bj[áa] volto\b|\bseguindo com\b|\blet me\b|\bi'?ll\b|\bi'?m going to\b|\bi am going to\b)`,
	)
	announcementAction = regexp.MustCompile(
		`(?i)(\bbusc\w+|\bpesquis\w+|\bprocur\w+|\bconsult\w+|\bverific\w+|\bconfirm\w+|\bchec\w+|\bexecut\w+|\brod(ar|ando)\b|\babr(ir|indo)\b|\bacess\w+|\bbaix\w+|\banalis\w+|\b[lv](er|endo) (o|a|os|as)\b|\bsearch\w*|\blook up\b|\bfetch\w*|\bopen\w*|\bbrowse\w*|\bread the\b|\brun the\b)`,
	)
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
	// A reply that stops on a colon promised something that never followed.
	intent := announcementIntent.MatchString(tail) || endsOnDanglingColon(tail)
	return intent && announcementAction.MatchString(tail)
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
