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
			{Name: "exec", Success: true, Argument: "python3 import.py", Background: true},
		},
	)
	if len(got) != 2 || got[0].Name != "exec" || got[0].Success || got[0].ErrorSummary != "exit 1" ||
		got[0].Argument != "python3 ranking.py" || got[0].Background ||
		len(got[0].SkillNames) != 1 || !got[1].Background {
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

// A relative command keeps the directory it ran in, and a step that only
// launched or queried a background command is marked so: its success is not
// the command's.
func TestToolStepKeepsTheDirectoryAndMarksBackgroundSteps(t *testing.T) {
	cases := map[string]struct {
		args       map[string]any
		argument   string
		background bool
	}{
		"run with cwd": {
			map[string]any{"command": "python3 ranking.py", "cwd": "scripts"},
			"python3 ranking.py (cwd: scripts)",
			false,
		},
		"run in workspace": {
			map[string]any{"command": "python3 scripts/ranking.py", "cwd": "."},
			"python3 scripts/ranking.py",
			false,
		},
		"background launch": {
			map[string]any{"command": "python3 import.py", "background": true},
			"python3 import.py",
			true,
		},
		"poll of a session": {map[string]any{"action": "poll", "sessionId": "s-1"}, "(poll)", true},
		"explicit run":      {map[string]any{"action": "run", "command": "ls artifacts"}, "ls artifacts", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := toolStepArgument("exec", c.args); got != c.argument {
				t.Errorf("toolStepArgument = %q, want %q", got, c.argument)
			}
			if got := toolStepInBackground("exec", c.args); got != c.background {
				t.Errorf("toolStepInBackground = %v, want %v", got, c.background)
			}
		})
	}
	if toolStepInBackground("read_file", map[string]any{"path": "a", "background": true}) {
		t.Error("a read_file step is never a background command")
	}
}
