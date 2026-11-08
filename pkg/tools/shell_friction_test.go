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
		t.Fatal("a canceled turn must not stay waiting")
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
		{"cat /dev/null | grep nothing", false},
		{"(grep nothing /dev/null)", false},
		{"cd /tmp && diff /dev/null /etc/hosts >/dev/null", false},
		{"cd /tmp && grep nothing /dev/null", false},
		{"test -f /nonexistent && grep nothing /dev/null", false},
		{"false; grep nothing /dev/null", false},
		{"false\ngrep nothing /dev/null", false},
		{"false || grep nothing /dev/null", false},
		{"false | grep nothing /dev/null", false},
		{"false && grep nothing /dev/null", true},
		{"false &&\n grep nothing /dev/null", true},
		{"cat /dev/null | false && grep nothing /dev/null", true},
		{"sh -c 'echo compile-failed; exit 1' && grep -q needle /dev/null", true},
		{"cd /nonexistent 2>/dev/null && grep nothing /dev/null", true},
		{"set -e; false; grep nothing /dev/null", true},
		{"grep nothing /dev/null && echo found", true},
		{"echo report; exit 3", true},
		{"echo failed 2>&1; exit 1", true},
		{"sh -c 'echo failed; exit 1' 2>&1", true},
		{"grep nothing /dev/null; exit 2", true},
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
	result = tool.Execute(
		context.Background(),
		map[string]any{"command": "head -c 20000 /dev/zero | tr '\\0' x; exit 3"},
	)
	if !strings.Contains(result.ForLLM, "truncated") ||
		!strings.HasSuffix(result.ForLLM, "[Command exited with code 3]") {
		t.Errorf("long output must keep the exit code: %s", result.ForLLM[max(0, len(result.ForLLM)-120):])
	}
}

func TestExecGuard_DotDotOnlyBlocksWhatLeavesTheWorkspace(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"work/a", "work/b", "proposta/assets"} {
		if err = os.MkdirAll(filepath.Join(workspace, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(workspace, "work", "link")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join("work", "a"), filepath.Join(workspace, "deep")); err != nil {
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
		{name: "url with a climb", command: "curl https://example.com/a/../b"},
		{name: "sed expression", command: "sed 's/../x/' /dev/null"},
		{name: "git range", command: "echo HEAD..main"},
		{name: "workspace variable", command: "ls $PWD/work/../proposta"},
		{name: "empty a subdir", command: "cd work && rm -rf ./*"},
		{name: "empty a sibling", command: "cd work/a && rm -rf ../b/*"},
		{name: "rm of named dirs", command: "rm -rf -- work/a work/b"},
		{name: "rm flag of another tool", command: "docker run --rm -it img ."},
		{name: "bare climb inside", command: "ls ..", cwd: subdir},

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
		{
			name:    "fallback after a failed cd",
			command: "cd work/missing && printf inside || cat ../secret",
			blocked: true,
		},
		{name: "after a semicolon the cd may have failed", command: "cd work && true; cat ../secret", blocked: true},
		{name: "on the next line the cd may have failed", command: "cd work && true\ncat ../secret", blocked: true},
		{name: "in the background the cd may have failed", command: "cd work && true & cat ../secret", blocked: true},
		{name: "fallback from the middle cd", command: "cd work && cd a && true || cat ../../../secret", blocked: true},
		{name: "fallback that stays inside from every start", command: "cd work && true || cat ./a/../x"},
		{name: "chain before the fallback stays followed", command: "cd work/a && cat ../b/x || echo missing"},
		{name: "pipe stays in the chain", command: "cd work/a && cat ../b/x | sort"},
		{name: "cd in a subshell", command: "(cd work/a && true); cat ../../../x", blocked: true},
		{name: "file url", command: "curl file://../../etc/passwd", blocked: true},
		{name: "cd home", command: "cd ~ && cat ../secret", blocked: true},
		{name: "cd other user", command: "cd ~root && cat ../etc/passwd", blocked: true},
		{name: "cd previous", command: "cd - && cat ../secret", blocked: true},
		{name: "cd end of options goes home", command: "cd -- && cat ../secret", blocked: true},
		{name: "cd physical option", command: "cd -P && cat ../secret", blocked: true},
		{name: "cd glob", command: "cd * && cat ../../secret", blocked: true},
		{name: "second cd not followed", command: "cd work && cd ..; cat ../secret", blocked: true},
		{name: "cd in a later subshell", command: "cd work && (cd ..; cat ../secret)", blocked: true},
		{name: "path split by single quotes", command: "cd work && cat ../'../secret'", blocked: true},
		{name: "path split by double quotes", command: `cd work && cat "../""../secret"`, blocked: true},
		{name: "path split by a line continuation", command: "cd work && cat ../\\\n../secret", blocked: true},
		{name: "tilde user", command: "cat ~root/../etc/passwd", blocked: true},
		{name: "heredoc operator in quotes", command: "echo '<<X'\ncat ../secret", blocked: true},
		{name: "heredoc operator in a comment", command: "# <<X\ncat ../secret", blocked: true},
		{name: "here-string", command: "cat <<<x\ncat ../secret", blocked: true},
		{name: "empty the workspace after cd", command: "cd work && rm -rf ../*", blocked: true},
		{
			name:    "empty the workspace when the cd fails",
			command: "cd work/missing && true || rm -rf ./*",
			blocked: true,
		},
		{name: "empty the workspace after a semicolon", command: "cd work && true; rm -rf ./*", blocked: true},
		{name: "empty a subdir in the chain with a fallback", command: "cd work && rm -rf ./* || echo failed"},
		{name: "empty the workspace through a dir", command: "rm -rf work/../*", blocked: true},
		{name: "empty the workspace from here", command: "rm -rf ./*", blocked: true},
		{name: "rm after end of options", command: "rm -rf -- *", blocked: true},
		{name: "rm with another operand first", command: "rm -rf x *", blocked: true},
		{name: "rm of the expanded workspace", command: "rm -rf $PWD/*", blocked: true},
		{name: "rm with escaped dots", command: `cd work && rm -rf .\./*`, blocked: true},
		{name: "rm with flags last", command: "rm * -rf", blocked: true},
		{name: "variable after the climb", command: "cp ../b/$f .", cwd: subdir, blocked: true},
		{name: "substitution after the climb", command: "cat ../`echo x`", cwd: subdir, blocked: true},
		{name: "tilde after the climb", command: "cat ../~x", cwd: subdir, blocked: true},
		{name: "env runs elsewhere", command: "cd work && env -C .. cat ../secret", blocked: true},
		{name: "git runs elsewhere", command: "cd work && git -C .. show ../secret", blocked: true},
		{name: "cd split by quotes", command: "cd work && c''d ..; cat ../secret", blocked: true},
		{name: "bare climb out", command: "ls ..", blocked: true},
		{name: "climb ending the path", command: "cp x work/../..", blocked: true},
		{name: "logical cd through a symlink", command: "cd deep/../.. && rm -rf ws", blocked: true},
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
