package redact

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestSensitiveKey(t *testing.T) {
	tests := map[string]bool{
		"password":                true,
		"Senha":                   true,
		"secretValue":             true,
		"access_token":            true,
		"refresh_tokens":          true,
		"api-key":                 true,
		"Authorization":           true,
		"client_secret":           true,
		"Cookie":                  true,
		"DB_PASS":                 true,
		"mysql_pwd":               true,
		"pwd":                     false,
		"secretName":              false,
		"max_tokens":              false,
		"tokens_before":           false,
		"input_tokens_details":    false,
		"summarize_token_percent": false,
		"tokenizer":               false,
		"token_count":             false,
		"token_url":               false,
		"api_key_hint":            false,
		"command":                 false,
		"path":                    false,
	}
	for key, want := range tests {
		if got := sensitiveKey(key); got != want {
			t.Errorf("sensitiveKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestValue_MasksNestedCopyWithoutMutatingInput(t *testing.T) {
	newInput := func() map[string]any {
		return map[string]any{
			"headers":      map[string]any{"Authorization": "Basic dXNlcjpwYXNz", "Accept": "json"},
			"items":        []any{"Bearer abcdefghijklmnop1234", 3},
			"password":     "hunter2",
			"password_set": true,
			"env":          map[string]string{"API_KEY": "abc", "REGION": "us-east-1"},
			"note":         "ok",
		}
	}
	in := newInput()

	got, ok := Value(in).(map[string]any)
	if !ok {
		t.Fatalf("Value() returned %T, want map[string]any", Value(in))
	}

	want := map[string]any{
		"headers":      map[string]any{"Authorization": Mask, "Accept": "json"},
		"items":        []any{"Bearer " + Mask, 3},
		"password":     Mask,
		"password_set": true,
		"env":          map[string]any{"API_KEY": Mask, "REGION": "us-east-1"},
		"note":         "ok",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Value()\n got  %#v\n want %#v", got, want)
	}
	if !reflect.DeepEqual(in, newInput()) {
		t.Fatalf("Value() mutated its input: %#v", in)
	}
}

func TestValue_MasksTypesItDoesNotWalk(t *testing.T) {
	type request struct {
		URL      string
		Password string `json:"password"`
	}
	tests := []struct {
		name   string
		in     any
		secret string
	}{
		{"http.Header", http.Header{"Authorization": {"Bearer abcdefghijklmnop1234"}}, "abcdefghijklmnop1234"},
		{"slice of maps", []map[string]any{{"token": "hunter2"}}, "hunter2"},
		{"struct", request{URL: "https://admin:hunter2@x", Password: "hunter3"}, "hunter"},
		{"pointer to struct", &request{Password: "hunter3"}, "hunter3"},
		{"bytes", []byte("curl -u admin:hunter2 https://x"), "hunter2"},
		{"error", errors.New("dial postgres://u:hunter2@db"), "hunter2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Value(tt.in).(string)
			if !ok {
				t.Fatalf("Value(%T) = %#v, want a masked string", tt.in, Value(tt.in))
			}
			if strings.Contains(got, tt.secret) {
				t.Fatalf("Value(%T) = %q still contains %q", tt.in, got, tt.secret)
			}
		})
	}
	if got := Value(42); got != 42 {
		t.Errorf("Value(42) = %#v, want 42", got)
	}
}

func TestField_MasksBySensitiveName(t *testing.T) {
	if got := Field("token", "abc"); got != Mask {
		t.Errorf("Field(token) = %v, want %q", got, Mask)
	}
	if got := Field("error", "auth failed for https://u:hunter2@x"); got != "auth failed for https://u:"+Mask+"@x" {
		t.Errorf("Field(error) = %v", got)
	}
	if got := Field("iteration", 3); got != 3 {
		t.Errorf("Field(iteration) = %v, want 3", got)
	}
}

func TestToolArgs(t *testing.T) {
	tests := []struct {
		name string
		tool string
		in   map[string]any
		want map[string]any
	}{
		{
			name: "memory note is logged by size",
			tool: "memory",
			in: map[string]any{
				"action":  "update_section",
				"section": "Backoffice",
				"content": "Login: ana@example.com / hunter2",
				"reason":  "senha antiga hunter3",
			},
			want: map[string]any{
				"action":  "update_section",
				"section": "Backoffice",
				"content": Mask + " (32 chars)",
				"reason":  Mask + " (20 chars)",
			},
		},
		{
			name: "edit keeps the path and hides both texts",
			tool: "edit_file",
			in:   map[string]any{"path": "a.env", "old_text": "PASS=x", "new_text": "PASS=y"},
			want: map[string]any{"path": "a.env", "old_text": Mask + " (6 chars)", "new_text": Mask + " (6 chars)"},
		},
		{
			name: "exec command keeps its shape",
			tool: "exec",
			in:   map[string]any{"command": "curl -u admin:hunter2 https://x", "timeout": 30},
			want: map[string]any{"command": "curl -u admin:" + Mask + " https://x", "timeout": 30},
		},
		{
			name: "exec stdin write is logged by size",
			tool: "exec",
			in:   map[string]any{"action": "write", "data": "hunter2\n", "session_id": "s1"},
			want: map[string]any{"action": "write", "data": Mask + " (8 chars)", "session_id": "s1"},
		},
		{
			name: "env var under any tool name",
			tool: "mcp_x_set_env",
			in:   map[string]any{"projectId": 42, "name": "DB_PASSWORD", "value": "hunter2"},
			want: map[string]any{"projectId": 42, "name": "DB_PASSWORD", "value": Mask + " (7 chars)"},
		},
		{
			name: "set_secret keeps the name",
			tool: "mcp_skip_skip_cloud_set_secret",
			in:   map[string]any{"projectId": 42, "name": "SMTP", "secretValue": "abc"},
			want: map[string]any{"projectId": 42, "name": "SMTP", "secretValue": Mask},
		},
		{
			name: "list_secrets keeps its project",
			tool: "mcp_skip_skip_cloud_list_secrets",
			in:   map[string]any{"projectId": 42},
			want: map[string]any{"projectId": 42},
		},
		{
			name: "browser fill hides the typed text",
			tool: "browser",
			in:   map[string]any{"command": "fill @e5 correct horse battery", "session": ""},
			want: map[string]any{"command": "fill " + Mask + " (25 chars)", "session": ""},
		},
		{
			name: "browser eval hides the script",
			tool: "browser",
			in:   map[string]any{"command": `eval "document.querySelector('#p').value='hunter2'"`},
			want: map[string]any{"command": "eval " + Mask + " (46 chars)"},
		},
		{
			name: "browser find ... fill hides the typed text",
			tool: "browser",
			in:   map[string]any{"command": "find label Password fill hunter2"},
			want: map[string]any{"command": "find " + Mask + " (27 chars)"},
		},
		{
			name: "browser open keeps the URL without its token",
			tool: "browser",
			in:   map[string]any{"command": "open https://x/reset?token=hunter2"},
			want: map[string]any{"command": "open https://x/reset?token=" + Mask},
		},
		{
			name: "browser click is untouched",
			tool: "browser",
			in:   map[string]any{"command": "click @e3"},
			want: map[string]any{"command": "click @e3"},
		},
		{
			name: "arguments nobody listed are logged by size",
			tool: "spawn",
			in:   map[string]any{"task": "log in with hunter2", "raw": `{"cmd":"x`},
			want: map[string]any{"task": Mask + " (19 chars)", "raw": Mask + " (9 chars)"},
		},
		{
			name: "sensitive argument name",
			tool: "web_fetch",
			in:   map[string]any{"url": "https://x", "api_key": "abc"},
			want: map[string]any{"url": "https://x", "api_key": Mask},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToolArgs(tt.tool, tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ToolArgs(%q)\n got  %#v\n want %#v", tt.tool, got, tt.want)
			}
		})
	}
}

func TestToolArgs_NilAndNoMutation(t *testing.T) {
	if got := ToolArgs("exec", nil); got != nil {
		t.Fatalf("ToolArgs(nil) = %#v, want nil", got)
	}
	in := map[string]any{"content": "hunter2"}
	_ = ToolArgs("write_file", in)
	if in["content"] != "hunter2" {
		t.Fatalf("ToolArgs mutated its input: %#v", in)
	}
}
