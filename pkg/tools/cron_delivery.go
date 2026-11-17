package tools

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// noReplyToken is the final answer of an automation run that has nothing to
// deliver. The run's note asks for it (ExecuteJobWithResult), and
// AutomationDelivery takes it out before anything is published.
// (seucaranguejo fork — MST-795)
const noReplyToken = "NO_REPLY"

// anyCaseNoReply finds the token in any case, also with its underscore escaped
// for Markdown ("NO\_REPLY").
var anyCaseNoReply = regexp.MustCompile(`(?i)NO\\?_REPLY`)

// upperCaseNoReply finds the token as the run's note writes it, also with its
// underscore escaped. Only this form is cut out of text: "no_reply" in a
// sender or an address is the news. (seucaranguejo fork — MST-795)
var upperCaseNoReply = regexp.MustCompile(`NO\\?_REPLY`)

// wordJoiners tie the token to the word past them in an e-mail address, a URL
// or an identifier: "no_reply@email.apple.com", "x.com/no_reply/123",
// "?from=no_reply&x=1", "no_reply_bot".
const wordJoiners = "@/.-_=&?#:+%~"

// AutomationDelivery is what an automation run's final answer delivers. A
// NO_REPLY counts when it stands as a word of its own: the rune right before
// or after it does not glue it to a word (glues), so "_NO_REPLY_" counts and
// "NO_REPLYING", "no_reply@email.apple.com" and "x.com/no_reply" do not.
// When taking out every NO_REPLY, in any case, leaves no letter or digit, the
// run delivers nothing: the token in quotes, code or bold, or written
// "no_reply", is still the token. Otherwise only the upper-case NO_REPLY is
// taken out and the rest is delivered, so the token never drops news the run
// found; mixed reports that case. "no_reply" next to text is a word of that
// text ("Remetente: no_reply (Apple)"), and an answer without an upper-case
// NO_REPLY is delivered as it is.
func AutomationDelivery(answer string) (deliver string, mixed bool) {
	rest, found := withoutNoReply(answer, anyCaseNoReply)
	if !found {
		return answer, false
	}
	if strings.IndexFunc(rest, isLetterOrDigit) < 0 {
		return "", false
	}
	rest, found = withoutNoReply(answer, upperCaseNoReply)
	if !found {
		return answer, false
	}
	return strings.TrimSpace(rest), true
}

// withoutNoReply returns answer without each match of token that stands as a
// word of its own, and whether it found one.
func withoutNoReply(answer string, token *regexp.Regexp) (string, bool) {
	var rest strings.Builder
	last, found := 0, false
	for _, loc := range token.FindAllStringIndex(answer, -1) {
		if gluedBefore(answer[:loc[0]]) || gluedAfter(answer[loc[1]:]) {
			continue
		}
		rest.WriteString(answer[last:loc[0]])
		last, found = loc[1], true
	}
	rest.WriteString(answer[last:])
	return rest.String(), found
}

// gluedBefore reports whether the end of text, right before a match, glues
// the match to a word.
func gluedBefore(text string) bool {
	next, size := utf8.DecodeLastRuneInString(text)
	beyond, _ := utf8.DecodeLastRuneInString(text[:len(text)-size])
	return glues(next, beyond)
}

// gluedAfter reports whether the start of text, right after a match, glues
// the match to a word.
func gluedAfter(text string) bool {
	next, size := utf8.DecodeRuneInString(text)
	beyond, _ := utf8.DecodeRuneInString(text[size:])
	return glues(next, beyond)
}

// glues reports whether next, the rune beside a match, glues it to a word:
// next is a letter or digit, or a word joiner with a letter or digit beyond
// it, in the same direction. A joiner with no letter or digit past it is
// punctuation: "_NO_REPLY_." is the token in italics.
func glues(next, beyond rune) bool {
	return isLetterOrDigit(next) || (strings.ContainsRune(wordJoiners, next) && isLetterOrDigit(beyond))
}

func isLetterOrDigit(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}
