package tools

import "testing"

// One case per rule of automationDelivery: taking any single rule out changes
// the outcome of at least one case. (seucaranguejo fork — MST-795)
func TestAutomationDelivery(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  string
		deliver string
		mixed   bool
	}{
		{"token alone", "NO_REPLY", "", false},
		{"lower case", "no_reply", "", false},
		{"surrounding space and newline", "  NO_REPLY\n", "", false},
		{"inline code", "`NO_REPLY`", "", false},
		{"bold with period", "**NO_REPLY.**", "", false},
		{"italic with underscores", "_NO_REPLY_", "", false},
		{"curly quotes", "“NO_REPLY”", "", false},
		{"parentheses", "(NO_REPLY)", "", false},
		{"token twice", "NO_REPLY NO_REPLY", "", false},
		{"text then token", "Nada novo. NO_REPLY", "Nada novo.", true},
		{"token then news", "NO_REPLY\n\nNova vaga: casa em Lisboa", "Nova vaga: casa em Lisboa", true},
		{"only a number left", "NO_REPLY 3", "3", true},
		{"letter right after", "NO_REPLYING is not the token", "NO_REPLYING is not the token", false},
		{"letter right before", "XNO_REPLY is not the token", "XNO_REPLY is not the token", false},
		{
			"e-mail address", "Novo e-mail de no_reply@email.apple.com: seu recibo",
			"Novo e-mail de no_reply@email.apple.com: seu recibo", false,
		},
		{"URL path", "https://x.com/no_reply/123", "https://x.com/no_reply/123", false},
		{"URL ending in it", "Responda em https://x.com/no_reply", "Responda em https://x.com/no_reply", false},
		{"query string", "?from=no_reply&x=1", "?from=no_reply&x=1", false},
		{"identifier", "no_reply_bot avisou", "no_reply_bot avisou", false},
		{"markdown-escaped underscore", "NO\\_REPLY", "", false},
		{"italic then period", "_NO_REPLY_.", "", false},
		{"no token", "Sem novidades.", "Sem novidades.", false},
		{"empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deliver, mixed := automationDelivery(tc.answer)
			if deliver != tc.deliver || mixed != tc.mixed {
				t.Fatalf("automationDelivery(%q) = (%q, %v), want (%q, %v)",
					tc.answer, deliver, mixed, tc.deliver, tc.mixed)
			}
		})
	}
}
