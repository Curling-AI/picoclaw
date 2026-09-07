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
	announcementIntent = regexp.MustCompile(`(?i)(\bvou\b|\birei\b|\bdeixa eu\b|\bdeixe-me\b|\bdeixa-me\b|\bj[áa] volto\b|\bseguindo com\b|\blet me\b|\bi'?ll\b|\bi'?m going to\b|\bi am going to\b)`)
	announcementAction = regexp.MustCompile(`(?i)(\bbusc\w+|\bpesquis\w+|\bprocur\w+|\bconsult\w+|\bverific\w+|\bconfirm\w+|\bchec\w+|\bexecut\w+|\brod(ar|ando)\b|\babr(ir|indo)\b|\bacess\w+|\bbaix\w+|\banalis\w+|\b[lv](er|endo) (o|a|os|as)\b|\bsearch\w*|\blook up\b|\bfetch\w*|\bopen\w*|\bbrowse\w*|\bread the\b|\brun the\b)`)
)

// looksLikeUndeliveredAnnouncement reports whether a reply promises an action
// instead of performing it. The caller pairs it with the structural facts that
// make the promise a bug — first iteration, no tool call, tools were offered —
// so this only has to judge the prose.
func looksLikeUndeliveredAnnouncement(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	tail := trimmed
	if runes := []rune(tail); len(runes) > announcementTailWindow {
		tail = string(runes[len(runes)-announcementTailWindow:])
	}
	return announcementIntent.MatchString(tail) && announcementAction.MatchString(tail)
}
