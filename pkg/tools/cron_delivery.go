package tools

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// noReplyToken is the final answer of an automation run that has nothing to
// deliver. The run's note asks for it (ExecuteJobWithResult), and
// automationDelivery takes it out before anything is published.
// (seucaranguejo fork — MST-795)
const noReplyToken = "NO_REPLY"

// noReplyPattern finds the token in any case, also with its underscore escaped
// for Markdown ("NO\_REPLY").
var noReplyPattern = regexp.MustCompile(`(?i)NO\\?_REPLY`)

// wordJoiners tie the token to the word past them in an e-mail address, a URL
// or an identifier: "no_reply@email.apple.com", "x.com/no_reply/123",
// "?from=no_reply&x=1", "no_reply_bot".
const wordJoiners = "@/.-_=&?#:+%~"

// automationDelivery is what an automation run's final answer delivers. Every
// NO_REPLY standing as a word of its own is taken out: the rune right before
// or after it does not glue it to a word (glues), so "_NO_REPLY_" counts and
// "NO_REPLYING", "no_reply@email.apple.com" and "x.com/no_reply" do not.
// When no letter or digit is left, the run delivers nothing: the token in
// quotes, code or bold is still the token. When text is left, it is
// delivered, so the token never drops news the run found; mixed reports that
// case. An answer without the token is delivered as it is.
func automationDelivery(answer string) (deliver string, mixed bool) {
	rest, found := withoutNoReply(answer)
	if !found {
		return answer, false
	}
	rest = strings.TrimSpace(rest)
	if strings.IndexFunc(rest, isLetterOrDigit) < 0 {
		return "", false
	}
	return rest, true
}

// withoutNoReply returns answer without each NO_REPLY that stands as a word of
// its own, and whether it found one.
func withoutNoReply(answer string) (string, bool) {
	var rest strings.Builder
	last, found := 0, false
	for _, loc := range noReplyPattern.FindAllStringIndex(answer, -1) {
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
