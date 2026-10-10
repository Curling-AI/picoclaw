package agent

import (
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/tools"
)

// mentionScanAssistantMessages bounds how far back mentions are read: the reply
// being continued plus enough of the previous turn to see past a turn that
// ended on a synthetic notice (max_tool_iterations). Only messages with text
// count.
const mentionScanAssistantMessages = 3

var identifierToken = regexp.MustCompile(`[A-Za-z0-9_.-]+`)

// mentionedExpiredToolNames returns, most recent first, the tools a tool_search
// listed earlier whose promotion expired and that the latest assistant
// messages name in their text or reasoning. (seucaranguejo fork)
//
// A discovery promotion expires after a few idle rounds, so a tool the model
// searched for and planned around, but hasn't called yet, can leave the tools
// array while the model still means to call it. Some models then don't name the
// missing tool: they emit the closest one they are offered. In prod a plan to
// "read the project with skip_project_status" came out as skip_project_create
// with empty arguments, one blank project per round. Naming a tool is as clear
// a sign of intent as calling it, so it gets the same heal as a call in history.
//
// A mention can also be a decision not to call the tool ("I won't use X"); that
// costs one extra definition in the request, never a call.
func mentionedExpiredToolNames(messages []providers.Message, registry *tools.ToolRegistry) []string {
	aliases := registry.ExpiredToolAliases()
	if len(aliases) == 0 {
		return nil
	}
	discovered := map[string]struct{}{}
	for _, name := range discoveredToolNamesFromMessages(messages) {
		discovered[name] = struct{}{}
	}
	found := &mentionedTools{aliases: aliases, discovered: discovered, seen: map[string]struct{}{}}
	scanned := 0
	for i := len(messages) - 1; i >= 0 && scanned < mentionScanAssistantMessages; i-- {
		msg := messages[i]
		if msg.Role != "assistant" || (msg.Content == "" && msg.ReasoningContent == "") {
			continue
		}
		scanned++
		found.scan(msg.ReasoningContent)
		found.scan(msg.Content)
	}
	return found.names
}

// mentionedTools collects, in order and once each, the discovered tools a
// text names. aliases only holds names with a '_', '-' or '.', so a plain
// word never matches. A qualified name (functions.skip_project_status, the
// way OpenAI- and Gemini-style models write tools) matches by its last part
// when the whole token isn't an alias (files.get is one).
type mentionedTools struct {
	aliases    map[string]string
	discovered map[string]struct{}
	seen       map[string]struct{}
	names      []string
}

func (m *mentionedTools) scan(text string) {
	for _, token := range identifierToken.FindAllString(text, -1) {
		name, ok := m.lookup(strings.ToLower(strings.TrimRight(token, ".")))
		if !ok {
			continue
		}
		_, listed := m.discovered[name]
		_, dup := m.seen[name]
		if !listed || dup {
			continue
		}
		m.seen[name] = struct{}{}
		m.names = append(m.names, name)
	}
}

func (m *mentionedTools) lookup(token string) (string, bool) {
	if name, ok := m.aliases[token]; ok {
		return name, true
	}
	if dot := strings.LastIndexByte(token, '.'); dot >= 0 {
		name, ok := m.aliases[token[dot+1:]]
		return name, ok
	}
	return "", false
}
