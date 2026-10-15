package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestTruncateToolResult_KeepsStartAndEndWithinTheLimit(t *testing.T) {
	// Multi-byte runes: the cut must count and split runes, never bytes.
	content := "início " + strings.Repeat("ação ", 10_000) + " fim"
	total := utf8.RuneCountInString(content)
	const limit = 2_000
	got := truncateToolResult(content, total, limit, "/ws/artifacts/tool-output/exec_1.txt")

	if n := utf8.RuneCountInString(got); n > limit {
		t.Fatalf("truncated to %d runes, want at most %d", n, limit)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune")
	}
	for _, want := range []string{
		fmt.Sprintf("%d characters", total),
		"/ws/artifacts/tool-output/exec_1.txt",
		"início ação",
		"ação  fim",
		"characters omitted",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("truncated result lacks %q:\n%s", want, got)
		}
	}
	if path := savedToolOutputPath(got); path != "/ws/artifacts/tool-output/exec_1.txt" {
		t.Fatalf("saved path read back as %q", path)
	}
}

func TestTruncateToolResult_SaysWhenTheOutputWasNotSaved(t *testing.T) {
	content := strings.Repeat("line of output\n", 1_000)
	got := truncateToolResult(content, len(content), 500, "")
	if !strings.Contains(got, "could not be saved") || savedToolOutputPath(got) != "" {
		t.Fatalf("notice without a saved file is wrong:\n%s", got)
	}
}

func TestSavedToolOutputPath_OnlyReadsOurNotice(t *testing.T) {
	// A tool that prints the same words is not a trimmed result.
	content := "The full output is saved at /etc/passwd: read it in parts with read_file"
	if path := savedToolOutputPath(content); path != "" {
		t.Fatalf("read %q out of a plain tool output", path)
	}
}

func TestToolsConfig_MaxResultCharsNeverGoesBelowAReadFilePage(t *testing.T) {
	for _, tc := range []struct{ set, want int }{
		{0, config.DefaultToolMaxResultChars},
		{1_000, config.MinToolMaxResultChars},
		{500_000, 500_000},
	} {
		c := config.ToolsConfig{MaxResultChars: tc.set}
		if got := c.GetMaxResultChars(); got != tc.want {
			t.Errorf("max_result_chars %d: got %d, want %d", tc.set, got, tc.want)
		}
	}
}

func TestBoundToolResultForLLM_LeavesResultsWithinTheLimitAlone(t *testing.T) {
	al := &AgentLoop{cfg: &config.Config{}}
	ts := &turnState{workspace: t.TempDir()}
	content := strings.Repeat("ç", config.DefaultToolMaxResultChars) // more bytes than the limit, as many runes
	if got := al.boundToolResultForLLM(ts, "exec", content); got != content {
		t.Fatal("a result at the limit was changed")
	}
	if entries, _ := os.ReadDir(filepath.Join(ts.workspace, "artifacts", toolOutputDir)); len(entries) != 0 {
		t.Fatalf("saved %d files for a result within the limit", len(entries))
	}
}

// The loop detector's no-progress check hashes tool results: the same output
// must come back as the same trimmed text, and be stored once.
func TestBoundToolResultForLLM_SameOutputReadsTheSameAndIsStoredOnce(t *testing.T) {
	al := &AgentLoop{cfg: &config.Config{}}
	ts := &turnState{workspace: t.TempDir()}
	content := bigOutput(500_000, 1)
	first := al.boundToolResultForLLM(ts, "browser", content)
	second := al.boundToolResultForLLM(ts, "browser", content)
	if first != second {
		t.Fatal("the same output was trimmed into different texts")
	}
	entries, _ := os.ReadDir(filepath.Join(ts.workspace, "artifacts", toolOutputDir))
	if len(entries) != 1 {
		t.Fatalf("saved %d files for one output, want 1", len(entries))
	}
}

func TestSaveToolOutput(t *testing.T) {
	t.Run("needs a workspace", func(t *testing.T) {
		if _, err := saveToolOutput("", "exec", "output"); err == nil {
			t.Fatal("saved without a workspace: the file would land in the process's working directory")
		}
	})

	t.Run("names stay short and inside the directory", func(t *testing.T) {
		ws := t.TempDir()
		for _, name := range []string{strings.Repeat("mcp_server_tool_", 30), "../../etc/passwd"} {
			path, err := saveToolOutput(ws, name, "output of "+name)
			if err != nil {
				t.Fatalf("save %q: %v", name, err)
			}
			if filepath.Dir(path) != filepath.Join(ws, "artifacts", toolOutputDir) {
				t.Fatalf("saved %q at %s, outside the output directory", name, path)
			}
			if base := filepath.Base(path); len(base) > maxToolOutputNameLen+32 {
				t.Fatalf("file name %q is %d bytes long", base, len(base))
			}
		}
	})

	t.Run("keeps only the most recent outputs", func(t *testing.T) {
		ws := t.TempDir()
		paths := make([]string, 0, maxSavedToolOutputs+3)
		for i := range maxSavedToolOutputs + 3 {
			path, err := saveToolOutput(ws, "exec", fmt.Sprintf("output %d", i))
			if err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(time.Duration(i-100) * time.Minute)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, path)
		}
		entries, _ := os.ReadDir(filepath.Join(ws, "artifacts", toolOutputDir))
		if len(entries) > maxSavedToolOutputs {
			t.Fatalf("%d files kept, want at most %d", len(entries), maxSavedToolOutputs)
		}
		if _, err := os.Stat(paths[len(paths)-1]); err != nil {
			t.Fatalf("the newest output was pruned: %v", err)
		}
		if _, err := os.Stat(paths[0]); err == nil {
			t.Fatal("the oldest output survived the pruning")
		}
	})
}

func TestWithElidedToolResults(t *testing.T) {
	messages := []providers.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "go"}, // the active turn starts here
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "xml_call_0"}}},
		{Role: "tool", ToolCallID: "xml_call_0", Content: "first result"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "xml_call_0"}}},
		{Role: "tool", ToolCallID: "xml_call_0", Content: "second result"},
	}
	elided := turnElisions{2: {toolCallID: "xml_call_0", chars: 12}}

	got := withElidedToolResults(messages, 1, elided)
	if !strings.HasPrefix(got[3].Content, elidedToolResultMarker) ||
		!strings.Contains(got[3].Content, "12 characters") {
		t.Fatalf("first result = %q, want its elision note", got[3].Content)
	}
	if got[3].ToolCallID != "xml_call_0" {
		t.Fatal("the note lost its tool call id; the provider would reject the orphan call")
	}
	// Text-lifted calls reuse ids across responses: only the elided position
	// changes, never another result with the same id.
	if got[5].Content != "second result" {
		t.Fatalf("second result = %q, want it untouched", got[5].Content)
	}
	if messages[3].Content != "first result" {
		t.Fatal("the turn's own messages were modified")
	}
	if same := withElidedToolResults(messages, 1, nil); &same[0] != &messages[0] {
		t.Fatal("without elisions the messages should pass through without a copy")
	}
	// The id guards a moved turn: an offset now pointing elsewhere is skipped.
	if moved := withElidedToolResults(
		messages,
		0,
		elided,
	); moved[2].Content != "" ||
		moved[3].Content != "first result" {
		t.Fatal("an elision applied at an offset that no longer holds its result")
	}
}

func TestElidedToolResultNote_PointsAtTheSavedOutput(t *testing.T) {
	note := elidedToolResultNote(elidedToolResult{chars: 131072, savedPath: "/ws/artifacts/tool-output/browser_ab.txt"})
	if !strings.Contains(note, "/ws/artifacts/tool-output/browser_ab.txt") ||
		strings.Contains(note, "Run the tool again") {
		t.Fatalf("note for a saved output should send the model to the file:\n%s", note)
	}
}

func elisionTestTurn() []providers.Message {
	result := func(id string, size int) providers.Message {
		return providers.Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("r", size)}
	}
	call := func(ids ...string) providers.Message {
		msg := providers.Message{Role: "assistant"}
		for _, id := range ids {
			msg.ToolCalls = append(msg.ToolCalls, providers.ToolCall{ID: id})
		}
		return msg
	}
	return []providers.Message{
		{Role: "user", Content: strings.Repeat("u", 5_000)}, // 0
		call("a", "b", "c"), // 1
		result("a", 500),    // 2: too small to be worth a note
		result("b", 10_000), // 3: oldest large result
		result("c", 10_000), // 4
		call("d"),           // 5
		result("d", 10_000), // 6: newest batch
	}
}

// measureWithWindow builds the call with a set of elisions and says how far
// past window it is.
func measureWithWindow(turn []providers.Message, window int) func(turnElisions) ([]providers.Message, int) {
	return func(elided turnElisions) ([]providers.Message, int) {
		msgs := withElidedToolResults(turn, 0, elided)
		return msgs, contextBudgetOverflow(window, msgs, nil, 0)
	}
}

func TestElideTurnToolResultsToFit(t *testing.T) {
	turn := elisionTestTurn()
	full := contextBudgetOverflow(0, turn, nil, 0)
	resultTokens := EstimateMessageTokens(turn[3])

	t.Run("oldest first, only as many as needed", func(t *testing.T) {
		elided, _, fits := elideTurnToolResultsToFit(turn, nil, 0, measureWithWindow(turn, full-resultTokens/2))
		if !fits || len(elided) != 1 || elided[3].toolCallID != "b" {
			t.Fatalf("elided = %v (fits %v), want only the oldest large result", elided, fits)
		}
	})

	t.Run("headroom elides earlier results past the bare fit", func(t *testing.T) {
		elided, _, fits := elideTurnToolResultsToFit(
			turn,
			nil,
			resultTokens,
			measureWithWindow(turn, full-resultTokens/2),
		)
		if !fits || len(elided) != 2 || elided[4].toolCallID != "c" {
			t.Fatalf("elided = %v (fits %v), want b and c", elided, fits)
		}
	})

	t.Run("the newest batch goes only for the bare fit", func(t *testing.T) {
		// Out-of-reach headroom: every earlier result goes, the newest stays.
		elided, _, fits := elideTurnToolResultsToFit(turn, nil, full, measureWithWindow(turn, full-resultTokens/2))
		if !fits || len(elided) != 2 {
			t.Fatalf("elided = %v (fits %v), want the earlier results and the newest kept", elided, fits)
		}
		// Too small even with every earlier result elided: the newest goes too.
		elided, _, fits = elideTurnToolResultsToFit(turn, nil, 0, measureWithWindow(turn, full-resultTokens*5/2))
		if !fits || elided[6].toolCallID != "d" {
			t.Fatalf("elided = %v (fits %v), want the newest result elided as a last resort", elided, fits)
		}
	})

	t.Run("keeps what an earlier retry elided", func(t *testing.T) {
		before := turnElisions{3: {toolCallID: "b", chars: 10_000}}
		elided, _, fits := elideTurnToolResultsToFit(turn, before, 0, measureWithWindow(turn, full-resultTokens*3/2))
		if !fits || len(elided) != 2 || elided[4].toolCallID != "c" {
			t.Fatalf("elided = %v (fits %v), want b kept and c added", elided, fits)
		}
		if len(before) != 1 {
			t.Fatal("the caller's set was modified")
		}
	})

	t.Run("gives up when even every result elided does not fit", func(t *testing.T) {
		before := turnElisions{}
		elided, _, fits := elideTurnToolResultsToFit(turn, before, 0, measureWithWindow(turn, 100))
		if fits || len(elided) != 0 {
			t.Fatalf("elided = %v (fits %v), want no fit and the caller's set back", elided, fits)
		}
	})
}

// After eliding, the call gets back as much earlier history as the headroom
// leaves room for: the model keeps the conversation that led to the turn.
func TestFitTurnByElision_PutsBackTheEarlierHistoryThatFits(t *testing.T) {
	turn := elisionTestTurn()
	stable := []providers.Message{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}
	build := func(history []providers.Message, elided turnElisions) []providers.Message {
		msgs := append(append([]providers.Message(nil), history...), turn...)
		return withElidedToolResults(msgs, len(history), elided)
	}
	turnTokens := contextBudgetOverflow(0, turn, nil, 0)
	resultTokens := EstimateMessageTokens(turn[3])
	budget := callBudget{window: turnTokens - resultTokens/2}

	history, call, elided, ok := fitTurnByElision(stable, turn, nil, budget, build)
	if !ok {
		t.Fatal("did not fit")
	}
	if len(history) != len(stable) || call[0].Content != "earlier question" {
		t.Fatalf("history kept = %d messages, want the whole earlier conversation back", len(history))
	}
	if over := budget.overflow(call); over > -budget.window/elisionHeadroomDivisor {
		t.Fatalf("the call leaves %d tokens, want the headroom of %d kept", -over, budget.window/elisionHeadroomDivisor)
	}
	if len(elided) == 0 {
		t.Fatal("nothing was elided")
	}
}

func TestRetryCompactionSkipReason(t *testing.T) {
	turn := []providers.Message{{Role: "user", Content: "go"}, {Role: "tool", ToolCallID: "a", Content: "result"}}
	build := func(history []providers.Message) []providers.Message {
		return append(append([]providers.Message(nil), history...), turn...)
	}
	roomy := callBudget{window: 1_000_000}
	earlier := []providers.Message{{Role: "user", Content: "earlier question"}}

	// Even when the turn alone would fit, there is nothing before it to drop:
	// compacting could only cut into the turn.
	if reason := retryCompactionSkipReason(nil, build, roomy); reason == "" {
		t.Fatal("allowed a compaction with no history before the active turn")
	}
	if reason := retryCompactionSkipReason(earlier, build, callBudget{window: 1}); reason == "" {
		t.Fatal("allowed a compaction that cannot make the turn fit")
	}
	if reason := retryCompactionSkipReason(earlier, build, roomy); reason != "" {
		t.Fatalf("refused a compaction that can help: %s", reason)
	}
}
