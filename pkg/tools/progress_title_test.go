package tools

import (
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers"
)

// (seucaranguejo fork — MST-957)

func toolDef(name string, params map[string]any) providers.ToolDefinition {
	return providers.ToolDefinition{
		Type:     "function",
		Function: providers.ToolFunctionDefinition{Name: name, Parameters: params},
	}
}

func propertiesOf(t *testing.T, def providers.ToolDefinition) map[string]any {
	t.Helper()
	properties, ok := def.Function.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s has no properties: %v", def.Function.Name, def.Function.Parameters)
	}
	return properties
}

func TestWithProgressTitleAddsTheArgumentToEveryTool(t *testing.T) {
	ownValue := map[string]any{"type": "string"}
	withArgs := map[string]any{
		"type":       "object",
		"properties": map[string]any{"value": ownValue},
		"required":   []string{"value"},
	}
	defs := []providers.ToolDefinition{
		toolDef("with_args", withArgs),
		toolDef("no_properties", map[string]any{"type": "object"}),
		toolDef("no_parameters", nil),
	}

	got := WithProgressTitle(defs)

	for _, def := range got {
		title, ok := propertiesOf(t, def)[ProgressTitleParam].(map[string]any)
		if !ok {
			t.Fatalf("%s has no %s property", def.Function.Name, ProgressTitleParam)
		}
		if title["type"] != "string" || title["maxLength"] != ProgressTitleMaxRunes ||
			!strings.Contains(title["description"].(string), "language") {
			t.Fatalf("%s %s schema = %v", def.Function.Name, ProgressTitleParam, title)
		}
	}
	if props := propertiesOf(t, got[0]); !reflect.DeepEqual(props["value"], ownValue) {
		t.Fatalf("with_args lost its own property: %v", props)
	}
	if !reflect.DeepEqual(got[0].Function.Parameters["required"], []string{"value"}) {
		t.Fatalf("with_args required = %v, want [value]", got[0].Function.Parameters["required"])
	}
	if got[2].Function.Parameters["type"] != "object" {
		t.Fatalf("no_parameters type = %v, want object", got[2].Function.Parameters["type"])
	}
}

func TestWithProgressTitleLeavesTheToolsOwnSchemaAlone(t *testing.T) {
	properties := map[string]any{"value": map[string]any{"type": "string"}}
	params := map[string]any{"type": "object", "properties": properties}
	defs := []providers.ToolDefinition{toolDef("shared", params)}

	WithProgressTitle(defs)

	if _, added := properties[ProgressTitleParam]; added {
		t.Fatal("the tool's own properties map got the argument")
	}
	if _, added := defs[0].Function.Parameters["properties"].(map[string]any)[ProgressTitleParam]; added {
		t.Fatal("the caller's definition got the argument")
	}
}

func TestWithProgressTitleKeepsAToolsOwnProgressTitle(t *testing.T) {
	own := map[string]any{"type": "string", "description": "the tool's own"}
	defs := []providers.ToolDefinition{
		toolDef("declares_it", map[string]any{"type": "object", "properties": map[string]any{ProgressTitleParam: own}}),
		toolDef("not_an_object", map[string]any{"type": "string"}),
	}

	got := WithProgressTitle(defs)

	if !reflect.DeepEqual(propertiesOf(t, got[0])[ProgressTitleParam], own) {
		t.Fatalf(
			"declares_it %s = %v, want the tool's own",
			ProgressTitleParam,
			propertiesOf(t, got[0])[ProgressTitleParam],
		)
	}
	if !reflect.DeepEqual(got[1].Function.Parameters, map[string]any{"type": "string"}) {
		t.Fatalf("not_an_object parameters = %v, want unchanged", got[1].Function.Parameters)
	}
}

func TestDeclaresProgressTitle(t *testing.T) {
	cases := map[string]struct {
		params map[string]any
		want   bool
	}{
		"declared":      {map[string]any{"properties": map[string]any{ProgressTitleParam: map[string]any{}}}, true},
		"other args":    {map[string]any{"properties": map[string]any{"value": map[string]any{}}}, false},
		"no properties": {map[string]any{"type": "object"}, false},
		"nil":           {nil, false},
	}
	for name, tc := range cases {
		if got := DeclaresProgressTitle(tc.params); got != tc.want {
			t.Errorf("%s: DeclaresProgressTitle = %v, want %v", name, got, tc.want)
		}
	}
}

func TestTakeProgressTitle(t *testing.T) {
	args := map[string]any{"value": "x", ProgressTitleParam: "  Checking the weather  "}
	original := maps.Clone(args)

	title, rest, taken := TakeProgressTitle(args)

	if !taken || title != "Checking the weather" {
		t.Fatalf("TakeProgressTitle = (%q, taken %v), want (Checking the weather, true)", title, taken)
	}
	if !reflect.DeepEqual(rest, map[string]any{"value": "x"}) {
		t.Fatalf("rest = %v, want only value", rest)
	}
	if !reflect.DeepEqual(args, original) {
		t.Fatalf("args changed to %v", args)
	}
}

func TestTakeProgressTitleCutsALongTitleByRunes(t *testing.T) {
	long := strings.Repeat("ã", ProgressTitleMaxRunes+20)

	title, _, _ := TakeProgressTitle(map[string]any{ProgressTitleParam: long})

	if title != strings.Repeat("ã", ProgressTitleMaxRunes) {
		t.Fatalf("title = %q (%d runes), want %d runes", title, len([]rune(title)), ProgressTitleMaxRunes)
	}
}

func TestTakeProgressTitleWithoutATextTitle(t *testing.T) {
	title, rest, taken := TakeProgressTitle(map[string]any{"value": "x", ProgressTitleParam: 42})
	if !taken || title != "" || !reflect.DeepEqual(rest, map[string]any{"value": "x"}) {
		t.Fatalf("non-text title: (%q, %v, %v), want (\"\", [value], true)", title, rest, taken)
	}

	args := map[string]any{"value": "x"}
	title, rest, taken = TakeProgressTitle(args)
	if taken || title != "" || !reflect.DeepEqual(rest, args) {
		t.Fatalf("no title: (%q, %v, %v), want (\"\", args, false)", title, rest, taken)
	}
}

func TestWithProgressTitleOnOneToolAndOnNone(t *testing.T) {
	if got := WithProgressTitle(nil); got != nil {
		t.Fatalf("WithProgressTitle(nil) = %v, want nil: a call without tools sends none", got)
	}
	got := WithProgressTitle([]providers.ToolDefinition{toolDef("only", map[string]any{"type": "object"})})
	if _, ok := propertiesOf(t, got[0])[ProgressTitleParam]; !ok {
		t.Fatalf("only tool has no %s: %v", ProgressTitleParam, got[0].Function.Parameters)
	}
}

func TestTakeProgressTitleDropsTheSpaceWhereItIsCut(t *testing.T) {
	text := strings.Repeat("a", ProgressTitleMaxRunes-1) + " and more"

	title, _, _ := TakeProgressTitle(map[string]any{ProgressTitleParam: text})

	if title != strings.Repeat("a", ProgressTitleMaxRunes-1) {
		t.Fatalf("title = %q, want %d runes without the trailing space", title, ProgressTitleMaxRunes-1)
	}
}
