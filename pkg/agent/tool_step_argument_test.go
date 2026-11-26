package agent

import (
	"strings"
	"testing"
)

// A step keeps what it did and nothing it wrote or sent: the command, the
// path, the query, clipped.
func TestToolStepArgumentKeepsOnlyWhatTheStepDid(t *testing.T) {
	long := strings.Repeat("x", toolStepMaxRunes+50)
	for name, test := range map[string]struct {
		tool string
		args map[string]any
		want string
	}{
		"a command":          {"exec", map[string]any{"command": " python3 ranking.py vendas.csv ", "timeout": 30}, "python3 ranking.py vendas.csv"},
		"a file written":     {"write_file", map[string]any{"path": "artifacts/ranking.md", "content": "Vestuário R$ 3.250,50"}, "artifacts/ranking.md"},
		"a message":          {"message", map[string]any{"content": "oi Ana"}, ""},
		"a search":           {"web_search", map[string]any{"query": "ptax hoje"}, "ptax hoje"},
		"a missing key":      {"exec", map[string]any{"cmd": "ls"}, ""},
		"a non-string":       {"read_file", map[string]any{"path": 42}, ""},
		"a too long command": {"exec", map[string]any{"command": long}, strings.Repeat("x", toolStepMaxRunes) + "…"},
	} {
		if got := toolStepArgument(test.tool, test.args); got != test.want {
			t.Errorf("%s: %q, want %q", name, got, test.want)
		}
	}
}

// The step's argument crosses to the evolution with the rest of the record.
func TestEvolutionGetsEachStepWithItsArgument(t *testing.T) {
	got := toEvolutionToolExecutions(
		[]ToolExecutionRecord{
			{
				Name:         "exec",
				Success:      false,
				ErrorSummary: "exit 1",
				SkillNames:   []string{"ranking"},
				Argument:     "python3 ranking.py",
			},
		},
	)
	if len(got) != 1 || got[0].Name != "exec" || got[0].Success || got[0].ErrorSummary != "exit 1" ||
		got[0].Argument != "python3 ranking.py" ||
		len(got[0].SkillNames) != 1 {
		t.Errorf("crossed as %+v", got)
	}
}

// What the turn recorded of a step reaches the turn's end whole, its argument
// included: the snapshot handed to the evolution copies the record.
func TestTheTurnSnapshotKeepsEachStepsArgument(t *testing.T) {
	ts := &turnState{}
	ts.recordToolExecution(ToolExecutionRecord{
		Name: "exec", Success: true, SkillNames: []string{"ranking"},
		Argument: toolStepArgument("exec", map[string]any{"command": "python3 ranking.py vendas.csv"}),
	})
	got := ts.toolExecutionsSnapshot()
	if len(got) != 1 || got[0].Argument != "python3 ranking.py vendas.csv" || !got[0].Success ||
		len(got[0].SkillNames) != 1 {
		t.Fatalf("snapshot %+v, want the exec step with its command", got)
	}
	got[0].SkillNames[0] = "changed"
	if again := ts.toolExecutionsSnapshot(); again[0].SkillNames[0] != "ranking" {
		t.Error("the snapshot shares the turn's skill names")
	}
}
