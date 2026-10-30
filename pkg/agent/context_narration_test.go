package agent

import (
	"strings"
	"testing"
)

// The identity once asked to explain each tool call, and the web chat filled
// with one announcement per step. (seucaranguejo fork)
func TestIdentityDoesNotAskToNarrateToolCalls(t *testing.T) {
	identity := NewContextBuilder(t.TempDir()).getIdentity(true)
	if strings.Contains(identity, "briefly explain what you're doing") {
		t.Fatalf("the tool-use identity asks to narrate the calls:\n%s", identity)
	}
	if !strings.Contains(identity, "Don't narrate your tool calls") {
		t.Fatalf("the tool-use identity lost its rule:\n%s", identity)
	}
}
