package messageutil

import "testing"

func TestProjectLegacyPublicPreservesRawSenderProvenance(t *testing.T) {
	tests := []struct {
		name, role, content string
		tagged              bool
		included            bool
		wantContent         string
		wantTagged          bool
		reason              LegacyPublicExclusionReason
	}{
		{
			"human typed System",
			"user",
			"[SENDER id=1]\n[System] my own words",
			false,
			true,
			"[System] my own words",
			true,
			"",
		},
		{"already projected human", "user", "[System] my own words", true, true, "[System] my own words", true, ""},
		{
			"human typed nested sender",
			"user",
			"[SENDER id=1]\n[SENDER] my own words",
			false,
			true,
			"[SENDER] my own words",
			true,
			"",
		},
		{
			"private steering",
			"user",
			" \n[System] internal budget note",
			false,
			false,
			"",
			false,
			LegacyPublicInternalSteering,
		},
		{
			"private uppercase steering",
			"assistant",
			"[SYSTEM] internal note",
			false,
			false,
			"",
			false,
			LegacyPublicInternalSteering,
		},
		{
			"public runtime card",
			"user",
			"[System: async:spawn] result",
			false,
			true,
			"[System: async:spawn] result",
			false,
			"",
		},
		{"ordinary assistant", "assistant", "reply", false, true, "reply", false, ""},
		{"public tool", "tool", "result", false, true, "result", false, ""},
		{"unsupported role", "system", "private instructions", false, false, "", false, LegacyPublicUnsupportedRole},
		{
			"unsupported tagged role",
			"system",
			"[SENDER id=1]\nhello",
			true,
			false,
			"",
			false,
			LegacyPublicUnsupportedRole,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectLegacyPublic(test.role, test.content, test.tagged)
			if got.Included != test.included || got.Content != test.wantContent ||
				got.SenderTagged != test.wantTagged ||
				got.Reason != test.reason {
				t.Fatal("legacy projection changed provenance or inclusion", got)
			}
			if got.Included {
				again := ProjectLegacyPublic(test.role, got.Content, got.SenderTagged)
				if again != got {
					t.Fatal("projection lost the person's provenance on its second pass", got, again)
				}
			}
		})
	}
}
