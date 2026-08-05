package agent

import (
	"testing"

	"github.com/sipeed/picoclaw/pkg/config"
)

// bill_partial_streams answers a question that has no universal answer: does
// the gateway charge for a stream the user interrupted?
//
// The original code assumed yes ("the gateway already billed the tokens
// generated up to the cancel"), which is true of a provider that bills as it
// streams and false of one that only charges on a complete upstream response.
// Getting it backwards charges the user every time they press stop — and the
// people who steer most are the power users.
// (seucaranguejo fork)
func TestBillsPartialStreamsDefaultsToTrue(t *testing.T) {
	// Absent config must preserve the historical behavior for every provider
	// that was working before this flag existed.
	cases := map[string]*Pipeline{
		"nil pipeline": nil,
		"nil config":   {},
		"unset flag":   {Cfg: &config.Config{}},
	}
	for name, p := range cases {
		if !p.billsPartialStreams() {
			t.Errorf("%s: got false, want the historical default (true)", name)
		}
	}
}

func TestBillsPartialStreamsHonorsExplicitFalse(t *testing.T) {
	no := false
	p := &Pipeline{Cfg: &config.Config{}}
	p.Cfg.Agents.Defaults.BillPartialStreams = &no
	if p.billsPartialStreams() {
		t.Error("explicit false ignored; interrupted streams would be charged")
	}

	yes := true
	p.Cfg.Agents.Defaults.BillPartialStreams = &yes
	if !p.billsPartialStreams() {
		t.Error("explicit true ignored")
	}
}
