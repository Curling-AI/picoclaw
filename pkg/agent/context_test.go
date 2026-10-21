package agent

import (
	"testing"
	"time"

	"github.com/sipeed/picoclaw/pkg/providers"
)

func msg(role, content string) providers.Message {
	return providers.Message{Role: role, Content: content}
}

func assistantWithTools(toolIDs ...string) providers.Message {
	calls := make([]providers.ToolCall, len(toolIDs))
	for i, id := range toolIDs {
		calls[i] = providers.ToolCall{ID: id, Type: "function"}
	}
	return providers.Message{Role: "assistant", ToolCalls: calls}
}

func toolResult(id string) providers.Message {
	return providers.Message{Role: "tool", Content: "result", ToolCallID: id}
}

func TestSanitizeHistoryForProvider_EmptyHistory(t *testing.T) {
	result := sanitizeHistoryForProvider(nil)
	if len(result) != 0 {
		t.Fatalf("expected empty, got %d messages", len(result))
	}

	result = sanitizeHistoryForProvider([]providers.Message{})
	if len(result) != 0 {
		t.Fatalf("expected empty, got %d messages", len(result))
	}
}

// A turn's reply followed by a mirrored automation delivery must not reach the
// provider as two assistant turns in a row.
func TestSanitizeHistoryForProvider_JoinsConsecutiveAssistantText(t *testing.T) {
	history := []providers.Message{
		msg("user", "oi"),
		msg("assistant", "Oi!"),
		msg("assistant", "Lembrete: reunião às 14h."),
		msg("user", "pode remarcar"),
		assistantWithTools("A"),
		toolResult("A"),
		{Role: "assistant", Content: "Segue a planilha.", Attachments: []providers.Attachment{{}}},
		{Role: "assistant", Content: "Lembrete das 16h.", ReasoningContent: "r"},
	}

	result := sanitizeHistoryForProvider(history)

	assertRoles(t, result, "user", "assistant", "user", "assistant", "tool", "assistant")
	if result[1].Content != "Oi!\n\nLembrete: reunião às 14h." {
		t.Fatalf("joined content = %q", result[1].Content)
	}
	if last := result[5]; last.Content != "Segue a planilha.\n\nLembrete das 16h." || len(last.Attachments) != 1 {
		t.Fatalf("reply with attachment + delivery = %+v, want one message keeping the attachment", last)
	}
	if history[1].Content != "Oi!" {
		t.Fatal("sanitizing mutated the stored history")
	}
}

// The reported case: deliveries that piled up since the user's last message
// reached the model as one block, and "remind me of that" picked the wrong
// part. Each delivery keeps its own dated line inside the joined message.
func TestSanitizeHistoryForProvider_DatesEachMirroredDelivery(t *testing.T) {
	summaryAt := time.Date(2026, 10, 19, 7, 10, 0, 0, time.UTC)
	inviteAt := time.Date(2026, 10, 19, 8, 9, 0, 0, time.UTC)
	stored := time.Date(2026, 10, 19, 8, 30, 0, 0, time.UTC)
	history := []providers.Message{
		msg("user", "oi"),
		msg("assistant", "Oi!"),
		{
			Role: "assistant", Content: "Resumo do dia.",
			Delivery: &providers.Delivery{Origin: "agent:cron-a-1", At: summaryAt},
		},
		{
			Role: "assistant", Content: "O convite expira hoje.", CreatedAt: &stored,
			Delivery: &providers.Delivery{Origin: "agent:cron-b-2", At: inviteAt},
		},
		msg("user", "me lembre disso às 17h"),
	}

	result := sanitizeHistoryForProvider(history)

	assertRoles(t, result, "user", "assistant", "user")
	want := "Oi!\n\n" +
		"[Sent to this chat by an automation at " + summaryAt.Local().Format("2006-01-02 15:04 (Monday)") + "]\n" +
		"Resumo do dia.\n\n" +
		"[Sent to this chat by an automation at " + inviteAt.Local().Format("2006-01-02 15:04 (Monday)") + "]\n" +
		"O convite expira hoje."
	if result[1].Content != want {
		t.Fatalf("joined content = %q\nwant %q", result[1].Content, want)
	}
	if history[3].Content != "O convite expira hoje." {
		t.Fatal("sanitizing mutated the stored history")
	}
}

func TestHistoryContent_DeliveryTime(t *testing.T) {
	written := time.Date(2026, 10, 19, 8, 9, 0, 0, time.UTC)
	cases := []struct {
		name string
		msg  providers.Message
		want string
	}{
		{"plain message", msg("assistant", "oi"), "oi"},
		{
			"delivery without its own time uses the write time",
			providers.Message{
				Role: "assistant", Content: "oi", CreatedAt: &written,
				Delivery: &providers.Delivery{Origin: "agent:cron-job-1"},
			},
			"[Sent to this chat by an automation at " + written.Local().Format("2006-01-02 15:04 (Monday)") + "]\noi",
		},
		{
			"delivery with no time at all",
			providers.Message{
				Role:     "assistant",
				Content:  "oi",
				Delivery: &providers.Delivery{Origin: "agent:cronmodel-job-1"},
			},
			"[Sent to this chat by an automation]\noi",
		},
		{
			"delivery from a web conversation is not an automation",
			providers.Message{
				Role: "assistant", Content: "oi",
				Delivery: &providers.Delivery{Origin: "sk_v1_webconversation", At: written},
			},
			"[Sent to this chat from another conversation at " + written.Local().
				Format("2006-01-02 15:04 (Monday)") +
				"]\noi",
		},
		{
			"the chat's own background script",
			providers.Message{
				Role: "assistant", Content: "oi",
				Delivery: &providers.Delivery{Origin: "sk_v1_chat", Self: true},
			},
			"[Sent to this chat by a script this conversation started]\noi",
		},
		{
			"heartbeat is an automation",
			providers.Message{Role: "assistant", Content: "oi", Delivery: &providers.Delivery{Origin: "heartbeat"}},
			"[Sent to this chat by an automation]\noi",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := historyContent(tc.msg); got != tc.want {
				t.Fatalf("historyContent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizeHistoryForProvider_SingleToolCall(t *testing.T) {
	history := []providers.Message{
		msg("user", "hello"),
		assistantWithTools("A"),
		toolResult("A"),
		msg("assistant", "done"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}
	assertRoles(t, result, "user", "assistant", "tool", "assistant")
}

func TestSanitizeHistoryForProvider_MultiToolCalls(t *testing.T) {
	history := []providers.Message{
		msg("user", "do two things"),
		assistantWithTools("A", "B"),
		toolResult("A"),
		toolResult("B"),
		msg("assistant", "both done"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 5 {
		t.Fatalf("expected 5 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "assistant", "tool", "tool", "assistant")
}

func TestSanitizeHistoryForProvider_AssistantToolCallAfterPlainAssistant(t *testing.T) {
	history := []providers.Message{
		msg("user", "hi"),
		msg("assistant", "thinking"),
		assistantWithTools("A"),
		toolResult("A"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "assistant")
}

func TestSanitizeHistoryForProvider_OrphanedLeadingTool(t *testing.T) {
	history := []providers.Message{
		toolResult("A"),
		msg("user", "hello"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user")
}

func TestSanitizeHistoryForProvider_ToolAfterUserDropped(t *testing.T) {
	history := []providers.Message{
		msg("user", "hello"),
		toolResult("A"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user")
}

func TestSanitizeHistoryForProvider_ToolAfterAssistantNoToolCalls(t *testing.T) {
	history := []providers.Message{
		msg("user", "hello"),
		msg("assistant", "hi"),
		toolResult("A"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "assistant")
}

func TestSanitizeHistoryForProvider_AssistantToolCallAtStart(t *testing.T) {
	history := []providers.Message{
		assistantWithTools("A"),
		toolResult("A"),
		msg("user", "hello"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 1 {
		t.Fatalf("expected 1 message, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user")
}

func TestSanitizeHistoryForProvider_MultiToolCallsThenNewRound(t *testing.T) {
	history := []providers.Message{
		msg("user", "do two things"),
		assistantWithTools("A", "B"),
		toolResult("A"),
		toolResult("B"),
		msg("assistant", "done"),
		msg("user", "hi"),
		assistantWithTools("C"),
		toolResult("C"),
		msg("assistant", "done again"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 9 {
		t.Fatalf("expected 9 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "assistant", "tool", "tool", "assistant", "user", "assistant", "tool", "assistant")
}

func TestSanitizeHistoryForProvider_ConsecutiveMultiToolRounds(t *testing.T) {
	history := []providers.Message{
		msg("user", "start"),
		assistantWithTools("A", "B"),
		toolResult("A"),
		toolResult("B"),
		assistantWithTools("C", "D"),
		toolResult("C"),
		toolResult("D"),
		msg("assistant", "all done"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 8 {
		t.Fatalf("expected 8 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "assistant", "tool", "tool", "assistant", "tool", "tool", "assistant")
}

func TestSanitizeHistoryForProvider_PlainConversation(t *testing.T) {
	history := []providers.Message{
		msg("user", "hello"),
		msg("assistant", "hi"),
		msg("user", "how are you"),
		msg("assistant", "fine"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}
	assertRoles(t, result, "user", "assistant", "user", "assistant")
}

func TestSanitizeHistoryForProvider_DuplicateToolResults(t *testing.T) {
	history := []providers.Message{
		msg("user", "do something"),
		assistantWithTools("A", "B"),
		toolResult("A"),
		toolResult("B"),
		toolResult("A"), // duplicate
		toolResult("B"), // duplicate
		msg("assistant", "done"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 5 {
		t.Fatalf("expected 5 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "assistant", "tool", "tool", "assistant")
	// Verify the kept tool results have the correct IDs
	if result[2].ToolCallID != "A" {
		t.Errorf("expected tool result A, got %q", result[2].ToolCallID)
	}
	if result[3].ToolCallID != "B" {
		t.Errorf("expected tool result B, got %q", result[3].ToolCallID)
	}
}

func TestSanitizeHistoryForProvider_ReusedToolCallIDAcrossRounds(t *testing.T) {
	history := []providers.Message{
		msg("user", "first"),
		assistantWithTools("call_0"),
		toolResult("call_0"),
		msg("assistant", "first done"),
		msg("user", "second"),
		assistantWithTools("call_0"),
		toolResult("call_0"),
		msg("assistant", "second done"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 8 {
		t.Fatalf("expected 8 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "assistant", "tool", "assistant", "user", "assistant", "tool", "assistant")
	if result[2].ToolCallID != "call_0" || result[6].ToolCallID != "call_0" {
		t.Fatalf(
			"expected both tool results to be preserved, got IDs %q and %q",
			result[2].ToolCallID,
			result[6].ToolCallID,
		)
	}
}

func TestSanitizeHistoryForProvider_DropsAssistantWithEmptyToolCallID(t *testing.T) {
	history := []providers.Message{
		msg("user", "do something"),
		assistantWithTools(""),
		toolResult(""),
		msg("assistant", "done"),
	}

	result := sanitizeHistoryForProvider(history)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "assistant")
}

func roles(msgs []providers.Message) []string {
	r := make([]string, len(msgs))
	for i, m := range msgs {
		r[i] = m.Role
	}
	return r
}

func assertRoles(t *testing.T, msgs []providers.Message, expected ...string) {
	t.Helper()
	if len(msgs) != len(expected) {
		t.Fatalf("role count mismatch: got %v, want %v", roles(msgs), expected)
	}
	for i, exp := range expected {
		if msgs[i].Role != exp {
			t.Errorf("message[%d]: got role %q, want %q", i, msgs[i].Role, exp)
		}
	}
}

// TestSanitizeHistoryForProvider_IncompleteToolResults tests the forward validation
// that ensures assistant messages with tool_calls have ALL matching tool results.
// This fixes the DeepSeek error: "An assistant message with 'tool_calls' must be
// followed by tool messages responding to each 'tool_call_id'."
func TestSanitizeHistoryForProvider_IncompleteToolResults(t *testing.T) {
	// Assistant expects tool results for both A and B, but only A is present
	history := []providers.Message{
		msg("user", "do two things"),
		assistantWithTools("A", "B"),
		toolResult("A"),
		// toolResult("B") is missing - this would cause DeepSeek to fail
		msg("user", "next question"),
		msg("assistant", "answer"),
	}

	result := sanitizeHistoryForProvider(history)
	// The assistant message with incomplete tool results should be dropped,
	// along with its partial tool result. The remaining messages are:
	// user ("do two things"), user ("next question"), assistant ("answer")
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "user", "assistant")
}

// TestSanitizeHistoryForProvider_MissingAllToolResults tests the case where
// an assistant message has tool_calls but no tool results follow at all.
func TestSanitizeHistoryForProvider_MissingAllToolResults(t *testing.T) {
	history := []providers.Message{
		msg("user", "do something"),
		assistantWithTools("A"),
		// No tool results at all
		msg("user", "hello"),
		msg("assistant", "hi"),
	}

	result := sanitizeHistoryForProvider(history)
	// The assistant message with no tool results should be dropped.
	// Remaining: user ("do something"), user ("hello"), assistant ("hi")
	if len(result) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "user", "assistant")
}

// TestSanitizeHistoryForProvider_PartialToolResultsInMiddle tests that
// incomplete tool results in the middle of a conversation are properly handled.
func TestSanitizeHistoryForProvider_PartialToolResultsInMiddle(t *testing.T) {
	history := []providers.Message{
		msg("user", "first"),
		assistantWithTools("A"),
		toolResult("A"),
		msg("assistant", "done"),
		msg("user", "second"),
		assistantWithTools("B", "C"),
		toolResult("B"),
		// toolResult("C") is missing
		msg("user", "third"),
		assistantWithTools("D"),
		toolResult("D"),
		msg("assistant", "all done"),
	}

	result := sanitizeHistoryForProvider(history)
	// First round is complete (user, assistant+tools, tool, assistant),
	// second round is incomplete and dropped (assistant+tools, partial tool),
	// third round is complete (user, assistant+tools, tool, assistant).
	// Remaining: user, assistant, tool, assistant, user, user, assistant, tool, assistant
	if len(result) != 9 {
		t.Fatalf("expected 9 messages, got %d: %+v", len(result), roles(result))
	}
	assertRoles(t, result, "user", "assistant", "tool", "assistant", "user", "user", "assistant", "tool", "assistant")
}
