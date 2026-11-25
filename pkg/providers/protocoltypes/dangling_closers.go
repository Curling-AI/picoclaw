package protocoltypes

import (
	"regexp"
	"strings"
)

var (
	// danglingCloserRunRe is a run of closing tags at the very end of a reply,
	// in the invoke format (`</parameter></invoke>`) or DeepSeek's DSML one
	// (`</｜DSML｜invoke>`), whose bars some providers send as ASCII or doubled.
	danglingCloserRunRe = regexp.MustCompile(
		`(?:\s*</\s*(?:[|｜]+\s*DSML\s*[|｜]+\s*)?(?:parameter|invoke|function_calls)\s*>)+\s*$`)
	// danglingCloserOpenerRe opens a tag of those formats, `<parameter=` included.
	danglingCloserOpenerRe = regexp.MustCompile(
		`<\s*(?:[|｜]+\s*DSML\s*[|｜]+\s*)?(?:parameter|invoke|function_calls)\b`)
	// danglingCallCloserRe tells a run that closes a call from one of lone
	// `</parameter>`s, which LooksLikeTruncatedToolCall owns.
	danglingCallCloserRe = regexp.MustCompile(`DSML|invoke|function_calls`)
)

// StripDanglingCallClosers removes the closing tags of a tool call that end a
// reply which never opened them: a provider parsed the call and left its
// closers after an otherwise complete answer. It leaves text alone when the
// run is only `</parameter>`s, when the reply opens any of those tags (it wrote
// a call, or XML the person asked for), when the run sits inside an open ```
// fence, or when nothing would be left.
func StripDanglingCallClosers(text string) string {
	loc := danglingCloserRunRe.FindStringIndex(text)
	if loc == nil || !danglingCallCloserRe.MatchString(text[loc[0]:]) {
		return text
	}
	head := strings.TrimSpace(text[:loc[0]])
	if head == "" || danglingCloserOpenerRe.MatchString(head) || strings.Count(head, "```")%2 == 1 {
		return text
	}
	return head
}
