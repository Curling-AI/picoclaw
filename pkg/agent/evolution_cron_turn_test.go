package agent

import "testing"

// A spawned child turn is keyed "subturn-N"; it inherits the spawning turn's
// prompt-cache scope, which for a cron run is the job ("agent:cron-<job>").
func TestTurnStateIsCronRunFollowsTheSpawningTurn(t *testing.T) {
	cases := []struct {
		name, session, scope string
		want                 bool
	}{
		{"cron run", "agent:cron-job1-6f1c2d3e-0000-4000-8000-000000000000", "", true},
		{"cron model run", "agent:cronmodel-job1-6f1c2d3e-0000-4000-8000-000000000000", "", true},
		{"child of a cron run", "subturn-3", "agent:cron-job1", true},
		{"child of a cron model run", "subturn-4", "agent:cronmodel-job1", true},
		{"child of a chat", "subturn-5", "sk_v1_abc", false},
		{"chat", "sk_v1_abc", "", false},
	}
	for _, c := range cases {
		ts := &turnState{sessionKey: c.session, promptCacheScopeOverride: c.scope}
		if got := ts.isCronRun(); got != c.want {
			t.Errorf("%s: isCronRun = %v, want %v", c.name, got, c.want)
		}
	}
}
