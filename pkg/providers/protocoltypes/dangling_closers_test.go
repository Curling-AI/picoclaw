package protocoltypes

import "testing"

// The closers below are the ones providers leave after a parsed call: DeepSeek's
// DSML tokens (fullwidth bars, sometimes doubled) and the invoke format.
func TestStripDanglingCallClosers(t *testing.T) {
	const form = "Fill in the form below and I'll book the visit.\n\n" +
		"```openui\nform(fields=[\"name\", \"date\"])\n```"
	tests := []struct {
		name string
		text string
		want string
	}{
		{"invoke closers after a form", form + "\n</parameter>\n</invoke>", form},
		{
			"invoke and function_calls closers", "Which day suits you, Monday or Tuesday?</invoke>\n</function_calls>",
			"Which day suits you, Monday or Tuesday?",
		},
		{"DSML closers", "Which day suits you, Monday or Tuesday?\n</｜DSML｜parameter>\n</｜DSML｜invoke>\n" +
			"</｜DSML｜function_calls>", "Which day suits you, Monday or Tuesday?"},
		{"DSML closer with doubled bars", "Which day suits you?\n</｜｜DSML｜｜invoke>", "Which day suits you?"},
		{"DSML closer with ASCII bars", "Which day suits you?</|DSML|invoke>  \n", "Which day suits you?"},
		// A lone </parameter> is the truncated-call guard's: the gateway ate the
		// call's head and what precedes it is an argument, not an answer.
		{"only parameter closers", "src/App.tsx</parameter>", "src/App.tsx</parameter>"},
		// XML the person asked for opens what it closes.
		{"XML the person asked for", "<process>\n<invoke name=\"approve\">\n<parameter name=\"id\">7</parameter>\n" +
			"</invoke>", "<process>\n<invoke name=\"approve\">\n<parameter name=\"id\">7</parameter>\n</invoke>"},
		{
			"a call written out whole", "<｜DSML｜invoke name=\"search\">q</｜DSML｜invoke>",
			"<｜DSML｜invoke name=\"search\">q</｜DSML｜invoke>",
		},
		{
			"inside an open fence", "Here is the format:\n```xml\n</parameter>\n</invoke>",
			"Here is the format:\n```xml\n</parameter>\n</invoke>",
		},
		{
			"closers mentioned mid-text", "The model wrote </invoke> and stopped. I fixed the parser.",
			"The model wrote </invoke> and stopped. I fixed the parser.",
		},
		{"nothing left", "</parameter>\n</invoke>", "</parameter>\n</invoke>"},
		{"plain reply", "Done: the sheet is updated.", "Done: the sheet is updated."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripDanglingCallClosers(tt.text); got != tt.want {
				t.Errorf("StripDanglingCallClosers = %q, want %q", got, tt.want)
			}
		})
	}
}
