package evolution

import "testing"

// TestParseSkillFrontmatterFields_LenientRecovery covers the fallback that keeps
// an LLM-drafted skill from being discarded when its frontmatter isn't strict
// YAML (typically an unquoted description containing a colon).
func TestParseSkillFrontmatterFields_LenientRecovery(t *testing.T) {
	// Strict-valid frontmatter parses as before.
	fields, err := parseSkillFrontmatterFields("name: my-skill\ndescription: does a thing", false)
	if err != nil || fields["name"] != "my-skill" || fields["description"] != "does a thing" {
		t.Fatalf("valid frontmatter: fields=%v err=%v", fields, err)
	}

	// Unquoted colon in the description breaks strict YAML; lenient recovers both
	// fields, taking everything after the first colon as the description value.
	fm := "name: influencer-eval\ndescription: Use when: the user asks to rank influencers"
	fields, err = parseSkillFrontmatterFields(fm, false)
	if err != nil {
		t.Fatalf("lenient recovery should succeed, got err=%v", err)
	}
	if fields["name"] != "influencer-eval" {
		t.Errorf("name = %q, want influencer-eval", fields["name"])
	}
	if fields["description"] != "Use when: the user asks to rank influencers" {
		t.Errorf("description = %q", fields["description"])
	}

	// Invalid YAML that lenient also can't complete (no name) still errors, so a
	// genuinely broken draft is not applied.
	if _, err := parseSkillFrontmatterFields("description: has: colon", false); err == nil {
		t.Errorf("invalid frontmatter without a name should still error")
	}
}
