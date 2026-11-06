package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExecTimeout_RespectsWhatTheModelAsked(t *testing.T) {
	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatal(err)
	}
	tool.SetTimeout(200 * time.Millisecond)

	result := tool.Execute(context.Background(), map[string]any{"command": "sleep 1; echo done", "timeout": float64(5)})
	if result.IsError || !strings.Contains(result.ForLLM, "done") {
		t.Fatalf("a command inside the timeout it asked for must finish: %s", result.ForLLM)
	}

	result = tool.Execute(context.Background(), map[string]any{"command": "sleep 5"})
	if !result.IsError || !strings.Contains(result.ForLLM, "timed out after 200ms") {
		t.Fatalf("without timeout the default applies: %s", result.ForLLM)
	}

	result = tool.Execute(context.Background(), map[string]any{"command": "sleep 5", "timeout": int64(1)})
	if !result.IsError || !strings.Contains(result.ForLLM, "timed out after 1s") {
		t.Fatalf("the message names the timeout that applied: %s", result.ForLLM)
	}
}

func TestSecondsArg(t *testing.T) {
	cases := []struct {
		arg  any
		want time.Duration
	}{
		{float64(120), 120 * time.Second},
		{int64(330), 330 * time.Second},
		{3, 3 * time.Second},
		{float64(100000), maxExecTimeout},
		{float64(0), 0},
		{float64(-5), 0},
		{"120", 0},
		{nil, 0},
	}
	for _, c := range cases {
		if got := secondsArg(map[string]any{"timeout": c.arg}, "timeout"); got != c.want {
			t.Errorf("secondsArg(%v) = %v, want %v", c.arg, got, c.want)
		}
	}
}

func TestExecPollWait(t *testing.T) {
	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatal(err)
	}
	started := tool.Execute(context.Background(), map[string]any{"command": "sleep 1", "background": true})
	if started.IsError {
		t.Fatal(started.ForLLM)
	}
	id := strings.Split(strings.Split(started.ForLLM, `"sessionId":"`)[1], `"`)[0]

	running := tool.Execute(context.Background(), map[string]any{"action": "poll", "sessionId": id})
	if !strings.Contains(running.ForLLM, "running") {
		t.Fatalf("poll without wait answers at once: %s", running.ForLLM)
	}
	begin := time.Now()
	done := tool.Execute(context.Background(), map[string]any{"action": "poll", "sessionId": id, "wait": float64(30)})
	if !strings.Contains(done.ForLLM, "done") {
		t.Fatalf("poll with wait should see the session finish: %s", done.ForLLM)
	}
	if time.Since(begin) > 10*time.Second {
		t.Fatalf("poll should return as soon as the session finishes, took %v", time.Since(begin))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slow := tool.Execute(context.Background(), map[string]any{"command": "sleep 30", "background": true})
	slowID := strings.Split(strings.Split(slow.ForLLM, `"sessionId":"`)[1], `"`)[0]
	defer tool.Execute(context.Background(), map[string]any{"action": "kill", "sessionId": slowID})
	begin = time.Now()
	tool.Execute(ctx, map[string]any{"action": "poll", "sessionId": slowID, "wait": float64(30)})
	if time.Since(begin) > 5*time.Second {
		t.Fatal("a cancelled turn must not stay waiting")
	}
}

func TestExecExitStatus(t *testing.T) {
	tool, err := NewExecTool("", false)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		command   string
		wantError bool
	}{
		{"grep nothing /dev/null", false},
		{"test -f /nonexistent", false},
		{"echo report; exit 3", false},
		{"echo oops >&2; exit 1", true},
		{"ls /nonexistent_directory_12345", true},
		{"kill -9 $$", true},
	}
	for _, c := range cases {
		result := tool.Execute(context.Background(), map[string]any{"command": c.command})
		if result.IsError != c.wantError {
			t.Errorf("%q: IsError=%v, want %v: %s", c.command, result.IsError, c.wantError, result.ForLLM)
		}
	}
	result := tool.Execute(context.Background(), map[string]any{"command": "exit 3"})
	if !strings.Contains(result.ForLLM, "exited with code 3") {
		t.Errorf("the code must stay visible to the model: %s", result.ForLLM)
	}
}

func TestExecGuard_DotDotOnlyBlocksWhatLeavesTheWorkspace(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"work/a", "work/b", "proposta/assets"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, "work", "link")); err != nil {
		t.Fatal(err)
	}
	tool, err := NewExecTool(workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(workspace, "work", "a")

	cases := []struct {
		name    string
		command string
		cwd     string
		blocked bool
	}{
		{name: "climb inside from cwd", command: "cp ../../proposta/assets/logo.png .", cwd: subdir},
		{name: "climb inside after cd", command: "cd work/a && cp ../../proposta/assets/logo.png ."},
		{name: "absolute cd then climb", command: "cd " + subdir + " && ls ../b"},
		{name: "two cds", command: "cd work && cd a && ls ../b"},
		{name: "quoted cd", command: `cd "work/a" && ls ../b`},
		{name: "quoted path inside", command: `ls "../b"`, cwd: subdir},
		{name: "import in a heredoc", command: "cat > work/a/x.mjs <<'EOF'\nimport '../../../scripts/cad.mjs'\nEOF"},
		{name: "variable after the climb", command: "cp ../b/$f .", cwd: subdir},
		{name: "url with a climb", command: "curl https://example.com/a/../b"},
		{name: "sed expression", command: "sed 's/../x/' /dev/null"},
		{name: "git range", command: "echo HEAD..main"},
		{name: "workspace variable", command: "ls $PWD/work/../proposta"},

		{name: "from the root", command: "cat ../secret", blocked: true},
		{name: "deep escape", command: "cat ../../../etc/passwd", cwd: subdir, blocked: true},
		{name: "quoted escape", command: `cat "../../etc/passwd"`, blocked: true},
		{name: "option value", command: "tool --out=../../x", blocked: true},
		{name: "after a colon", command: "scp host:../../x .", blocked: true},
		{name: "escaped slashes", command: `cat ..\/..\/etc/passwd`, blocked: true},
		{name: "attached option", command: `gcc "-I../../x"`, blocked: true},
		{name: "dots variant", command: "ls .../.../", blocked: true},
		{name: "variable before the climb", command: "cat $DIR/../x", blocked: true},
		{name: "command substitution", command: "cat $(dirname $0)/../x", blocked: true},
		{name: "symlink out", command: "cat work/link/../x", blocked: true},
		{name: "cd leaves", command: "cd ../x && ls", blocked: true},
		{name: "cd out then climb back", command: "cd .. && ls ../x", blocked: true},
		{name: "cd not guaranteed", command: "cd work/a; cat ../../../x", blocked: true},
		{name: "cd in a subshell", command: "(cd work/a && true); cat ../../../x", blocked: true},
		{name: "file url", command: "curl file://../../etc/passwd", blocked: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := c.cwd
			if cwd == "" {
				cwd = workspace
			}
			message := tool.guardCommand(c.command, cwd)
			if c.blocked && message == "" {
				t.Errorf("%q should be blocked", c.command)
			}
			if !c.blocked && message != "" {
				t.Errorf("%q should pass: %s", c.command, message)
			}
		})
	}
}
