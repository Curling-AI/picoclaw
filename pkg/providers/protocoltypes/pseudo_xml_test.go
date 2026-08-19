package protocoltypes

import (
	"strings"
	"testing"
)

// realLeak is a verbatim payload from prod (2026-08-19): the model wrote the
// call out as markup and the gateway forwarded it as text.
const realLeak = "<<<<<<< SEARCH\n  const minhaAvaliacao = (c: Contribuicao) =>\n" +
	"    avaliacoes.find((a) => a.contribuicao === c.id)\n=======\n>>>>>>> REPLACE</parameter>\n" +
	"<parameter=path>src/pages/Gestao.tsx</parameter>\n<parameter=projectId>50503</parameter>\n" +
	"</function>\n</tool_call>"

func TestExtractPseudoXMLToolCall(t *testing.T) {
	text := "<tool_call>\n<function=mcp_skip_skip_file_patch>\n<parameter=patch>\n" +
		"<<<<<<< SEARCH\n    keep me indented\n=======\n>>>>>>> REPLACE\n</parameter>\n" +
		"<parameter=path>src/pages/Gestao.tsx</parameter>\n" +
		"<parameter=projectId>50503</parameter>\n</function>\n</tool_call>"

	calls := ExtractToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Name != "mcp_skip_skip_file_patch" {
		t.Errorf("name = %q", calls[0].Name)
	}

	patch, ok := calls[0].Arguments["patch"].(string)
	if !ok {
		t.Fatalf("patch = %T, want string", calls[0].Arguments["patch"])
	}
	// The leading indentation of the value's own lines must survive: a patch
	// whose whitespace shifted no longer matches the file it targets.
	if !strings.Contains(patch, "\n    keep me indented\n") {
		t.Errorf("patch lost its indentation: %q", patch)
	}
	if strings.HasPrefix(patch, "\n") || strings.HasSuffix(patch, "\n") {
		t.Errorf("patch kept the format's framing newlines: %q", patch)
	}

	if got := calls[0].Arguments["path"]; got != "src/pages/Gestao.tsx" {
		t.Errorf("path = %v", got)
	}
	// Numbers must arrive as numbers — a tool typed on projectId rejects "50503".
	if got, ok := calls[0].Arguments["projectId"].(float64); !ok || got != 50503 {
		t.Errorf("projectId = %#v, want 50503 as a number", calls[0].Arguments["projectId"])
	}
}

func TestExtractPseudoXMLWithoutToolCallWrapper(t *testing.T) {
	text := "<function=read_file>\n<parameter=path>a.txt</parameter>\n</function>"
	calls := ExtractToolCallsFromText(text)
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("calls = %+v, want one read_file call", calls)
	}
}

// A patch body can itself contain a {"name":…,"arguments":…} object. The bare
// scanner would lift that inner object out as THE call, so pseudo-XML has to
// be tried first.
func TestPseudoXMLWinsOverBareJSONInsideAnArgument(t *testing.T) {
	text := "<function=mcp_skip_skip_file_write>\n<parameter=content>\n" +
		"{\"name\":\"not_a_call\",\"arguments\":{\"x\":1}}\n</parameter>\n</function>"
	calls := ExtractToolCallsFromText(text)
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Name != "mcp_skip_skip_file_write" {
		t.Errorf("name = %q, want the outer call, not the JSON inside the argument", calls[0].Name)
	}
}

func TestStripPseudoXMLToolCalls(t *testing.T) {
	text := "Vou aplicar o patch.\n<tool_call>\n<function=read_file>\n" +
		"<parameter=path>a.txt</parameter>\n</function>\n</tool_call>"
	if got := StripToolCallsFromText(text); got != "Vou aplicar o patch." {
		t.Errorf("stripped = %q", got)
	}
}

func TestLooksLikeTruncatedToolCall(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{"prod leak", realLeak, true},
		{"ends with function", "…</parameter>\n</function>", true},
		{"ends with parameter", "…value</parameter>", true},
		{"trailing whitespace still counts", "x</tool_call>\n\n  ", true},
		{"empty", "", false},
		{"plain prose", "Apliquei o patch em Gestao.tsx.", false},
		// The moment anyone debugs this bug, the assistant explains the markup.
		// Explaining it must not be mistaken for emitting it.
		{"explaining the markup", "O modelo emitiu </tool_call> no meio do texto e o turno morreu.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeTruncatedToolCall(tt.text); got != tt.want {
				t.Errorf("LooksLikeTruncatedToolCall = %v, want %v", got, tt.want)
			}
		})
	}
}

// The truncated tail carries no <function=NAME>, so there is nothing to call —
// the guard in the agent loop is what handles it, not the extractor.
func TestTruncatedLeakIsNotExtractable(t *testing.T) {
	if calls := ExtractToolCallsFromText(realLeak); len(calls) != 0 {
		t.Errorf("calls = %+v, want none (no function name survives the truncation)", calls)
	}
}
