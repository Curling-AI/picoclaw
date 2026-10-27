// Package redact masks credentials before they reach a log line.
//
// Logs leave the process: they are shipped to a log backend, kept there and
// read by people who are not the user. Anything a user hands to the agent — a
// password inside a curl, a bearer token, a value saved as a secret, a note
// written to memory — must not travel with them. Everything here is pure: the
// input is never mutated, the result is a masked copy.
package redact

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Mask replaces a credential in a log line.
const Mask = "[REDACTED]"

var keyNameReplacer = strings.NewReplacer("_", "", "-", "", ".", "")

var sensitiveKeyWords = []string{
	"password", "passwd", "senha", "secret", "token", "apikey", "accesskey",
	"privatekey", "authorization", "cookie", "credential",
}

// Plural "tokens" usually counts a model's text, except in these names.
var credentialTokenNames = []string{
	"refreshtoken", "accesstoken", "apitoken", "authtoken", "sessiontoken", "idtoken", "bearertoken",
}

// Suffixes that make a name describe a secret instead of holding one:
// `secretName`, `token_count`, `token_url`, `api_key_prefix`.
var nonSecretSuffixes = []string{
	"name", "id", "ids", "ref", "path", "file", "type", "kind", "count", "limit",
	"budget", "length", "size", "usage", "percent", "details", "url", "uri",
	"endpoint", "expiresat", "expiry", "ttl", "prefix", "suffix", "hint", "required",
}

// Tool arguments whose value says what a call did without carrying what the
// user wrote. They are logged (with credentials inside still masked); every
// other string argument is logged by size only, so a password typed into an
// argument nobody listed — a stdin write, a memory note, a message body —
// stays out. Names ending in "id" or "ids" are logged too.
var loggableArgs = map[string]bool{
	"action": true, "path": true, "file": true, "files": true, "section": true, "scope": true,
	"url": true, "command": true, "session": true, "engine": true, "timeout": true,
	"query": true, "pattern": true, "glob": true, "name": true, "tool": true, "model": true,
	"mode": true, "format": true, "language": true, "channel": true, "schedule": true,
	"cwd": true, "working_dir": true, "workdir": true, "limit": true, "offset": true,
	"page": true, "recursive": true, "depth": true, "count": true, "max_results": true,
	"start_line": true, "end_line": true,
}

// Browser subcommands that carry no typed text. Any other one (fill, type,
// eval, find ... fill, keyboard, dialog) is logged by subcommand and size.
var safeBrowserCommands = map[string]bool{
	"open": true, "goto": true, "navigate": true, "click": true, "dblclick": true,
	"hover": true, "focus": true, "check": true, "uncheck": true, "select": true,
	"snapshot": true, "screenshot": true, "wait": true, "get": true, "press": true,
	"scroll": true, "scrollintoview": true, "tab": true, "close": true, "pdf": true,
	"upload": true, "back": true, "forward": true, "reload": true,
}

func sensitiveKey(key string) bool {
	k := keyNameReplacer.Replace(strings.ToLower(key))
	if hasSuffixAny(k, nonSecretSuffixes) {
		return false
	}
	// `DB_PASS`, `mysql_pwd`; a bare `pwd` is the working directory.
	if strings.HasSuffix(k, "pass") || (strings.HasSuffix(k, "pwd") && k != "pwd") {
		return true
	}
	if containsAny(k, credentialTokenNames) {
		return true
	}
	k = strings.ReplaceAll(strings.ReplaceAll(k, "tokenizer", ""), "tokens", "")
	return containsAny(k, sensitiveKeyWords)
}

// Field masks one structured log field: the whole value when the field name
// says it holds a secret, the credentials inside it otherwise.
func Field(key string, value any) any {
	if sensitiveKey(key) {
		return maskValue(value)
	}
	return Value(value)
}

// Value returns a copy of v with credentials masked: values under sensitive
// keys are replaced, strings go through Text, maps and slices are walked.
// Any other type is logged as its masked JSON.
func Value(v any) any {
	switch val := v.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	case string:
		return Text(val)
	case []byte:
		return Text(string(val))
	case error:
		return Text(val.Error())
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			out[k] = Field(k, item)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(val))
		for k, item := range val {
			out[k] = Field(k, item)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = Value(item)
		}
		return out
	case []string:
		out := make([]string, len(val))
		for i, item := range val {
			out[i] = Text(item)
		}
		return out
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return Text(fmt.Sprintf("%+v", v))
		}
		return Text(string(data))
	}
}

// maskValue hides a secret's value. Booleans and nil say nothing about the
// secret and stay, so `"password_set": true` keeps its meaning.
func maskValue(v any) any {
	switch v.(type) {
	case nil, bool:
		return v
	default:
		return Mask
	}
}

// ToolArgs returns a copy of a tool call's arguments that is safe to log.
// Argument names, numbers and booleans always stay, so the log still shows
// which call was made.
func ToolArgs(tool string, args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		out[k] = toolArg(tool, k, v)
	}
	return out
}

func toolArg(tool, key string, v any) any {
	switch {
	case sensitiveKey(key):
		return maskValue(v)
	case tool == "browser" && key == "command":
		return browserCommand(v)
	case loggableArg(key):
		return Value(v)
	default:
		return summarize(v)
	}
}

func loggableArg(key string) bool {
	k := strings.ToLower(key)
	return loggableArgs[k] || strings.HasSuffix(k, "id") || strings.HasSuffix(k, "ids")
}

func browserCommand(v any) any {
	command, ok := v.(string)
	if !ok {
		return summarize(v)
	}
	trimmed := strings.TrimSpace(command)
	sub, rest, _ := strings.Cut(trimmed, " ")
	if safeBrowserCommands[strings.ToLower(sub)] {
		return Text(command)
	}
	return sub + " " + sizeOnly(rest)
}

// summarize keeps what a value is, not what it says.
func summarize(v any) any {
	switch val := v.(type) {
	case nil, bool, int, int64, float64:
		return v
	case string:
		return sizeOnly(val)
	default:
		return Mask
	}
}

func sizeOnly(s string) string {
	return fmt.Sprintf("%s (%d chars)", Mask, utf8.RuneCountInString(s))
}

func hasSuffixAny(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
