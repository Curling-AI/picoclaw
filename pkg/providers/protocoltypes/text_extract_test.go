package protocoltypes

import (
	"testing"
)

func TestExtractToolCallsFromText_JSONWrapper(t *testing.T) {
	text := `Let me check that. {"tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]} Done.`

	calls := ExtractToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(calls))
	}
	if calls[0].Name != "get_weather" {
		t.Fatalf("Name = %q, want %q", calls[0].Name, "get_weather")
	}
	if calls[0].Arguments["city"] != "SF" {
		t.Fatalf("Arguments[city] = %v, want SF", calls[0].Arguments["city"])
	}
	if calls[0].Function == nil || calls[0].Function.Name != "get_weather" {
		t.Fatalf("Function.Name not set correctly")
	}
}

func TestExtractToolCallsFromText_XMLTag(t *testing.T) {
	text := `I'll read the file for you.
<tool_call>{"name":"read_file","arguments":{"path":"/tmp/test.txt"}}</tool_call>
`

	calls := ExtractToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("len(calls) = %d, want 1", len(calls))
	}
	if calls[0].Name != "read_file" {
		t.Fatalf("Name = %q, want %q", calls[0].Name, "read_file")
	}
	if calls[0].Arguments["path"] != "/tmp/test.txt" {
		t.Fatalf("Arguments[path] = %v, want /tmp/test.txt", calls[0].Arguments["path"])
	}
}

func TestExtractToolCallsFromText_MultipleXMLTags(t *testing.T) {
	text := `<tool_call>{"name":"a","arguments":{"x":1}}</tool_call>
<tool_call>{"name":"b","arguments":{"y":2}}</tool_call>`

	calls := ExtractToolCallsFromText(text)
	if len(calls) != 2 {
		t.Fatalf("len(calls) = %d, want 2", len(calls))
	}
	if calls[0].Name != "a" {
		t.Fatalf("calls[0].Name = %q, want %q", calls[0].Name, "a")
	}
	if calls[1].Name != "b" {
		t.Fatalf("calls[1].Name = %q, want %q", calls[1].Name, "b")
	}
}

func TestExtractToolCallsFromText_PlainText(t *testing.T) {
	calls := ExtractToolCallsFromText("Just regular text, no tool calls here.")
	if calls != nil {
		t.Fatalf("expected nil, got %v", calls)
	}
}

func TestExtractToolCallsFromText_InvalidJSON(t *testing.T) {
	text := `{"tool_calls":[{invalid json}]}`
	calls := ExtractToolCallsFromText(text)
	if calls != nil {
		t.Fatalf("expected nil for invalid JSON, got %v", calls)
	}
}

func TestExtractToolCallsFromText_InvalidXML(t *testing.T) {
	text := `<tool_call>{not valid json}</tool_call>`
	calls := ExtractToolCallsFromText(text)
	if calls != nil {
		t.Fatalf("expected nil for invalid XML tool call JSON, got %v", calls)
	}
}

func TestStripToolCallsFromText_JSONWrapper(t *testing.T) {
	text := `Let me check. {"tool_calls":[{"id":"c1","type":"function","function":{"name":"fn","arguments":"{}"}}]} Done.`
	got := StripToolCallsFromText(text)
	want := "Let me check.  Done."
	if got != want {
		t.Fatalf("StripToolCallsFromText = %q, want %q", got, want)
	}
}

func TestStripToolCallsFromText_XMLTag(t *testing.T) {
	text := "Here you go.\n<tool_call>{\"name\":\"fn\",\"arguments\":{}}</tool_call>\nDone."
	got := StripToolCallsFromText(text)
	if got != "Here you go.\n\nDone." {
		t.Fatalf("StripToolCallsFromText = %q, want %q", got, "Here you go.\n\nDone.")
	}
}

func TestStripToolCallsFromText_NoToolCalls(t *testing.T) {
	text := "Just regular text."
	got := StripToolCallsFromText(text)
	if got != text {
		t.Fatalf("StripToolCallsFromText = %q, want %q", got, text)
	}
}

func TestFindMatchingBrace(t *testing.T) {
	tests := []struct {
		text string
		pos  int
		want int
	}{
		{`{"a":1}`, 0, 7},
		{`{"a":{"b":2}}`, 0, 13},
		{`text {"a":1} more`, 5, 12},
		{`{unclosed`, 0, 0},
		{`{}`, 0, 2},
	}
	for _, tt := range tests {
		got := FindMatchingBrace(tt.text, tt.pos)
		if got != tt.want {
			t.Errorf("FindMatchingBrace(%q, %d) = %d, want %d", tt.text, tt.pos, got, tt.want)
		}
	}
}
