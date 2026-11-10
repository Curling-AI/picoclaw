package agent

import (
	"strings"
	"testing"
)

// A ban with exceptions ("don't narrate your tool calls") and "otherwise work
// silently" both left some models' turns with no line at all, not even at the
// start; the rule opens every multi-step task and then names the moments instead.
// (seucaranguejo fork)
func TestIdentityNamesWhenToWriteProgressNotes(t *testing.T) {
	identity := NewContextBuilder(t.TempDir()).getIdentity(true)
	for _, banned := range []string{"briefly explain what you're doing", "work silently"} {
		if strings.Contains(identity, banned) {
			t.Errorf("the tool-use identity carries %q:\n%s", banned, identity)
		}
	}
	for _, moment := range []string{"Open every multi-step task", "changes the plan", "mid-task message"} {
		if !strings.Contains(identity, moment) {
			t.Errorf("the progress-notes rule lost the moment %q", moment)
		}
	}
}
