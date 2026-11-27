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

// downloadTargetPattern finds where a command writes what it downloads: a
// personal file there is written, not sent.
var downloadTargetPattern = regexp.MustCompile(`(?:>>?|(?:^|\s)(?:-o|-O|--output|--output-document)(?:=|\s+))\s*\S+`)

// scriptPathPattern finds the script files a command hands to an interpreter.
var scriptPathPattern = regexp.MustCompile(`[^\s'"<>|;&()]+\.(?:py|sh|bash|js|mjs|cjs|ts|rb|pl|php)\b`)

const (
	maxScannedScripts     = 8
	maxScannedScriptBytes = 64 << 10
)

// personalFilesSentTo returns where a command, or a script it runs, would
// hand the person's profile or memory files, or "". A skill or a file that
// asks for it is not the person asking, and some models follow a skill step
// by step.
// shortcut: it reads the command and the scripts it names, so code that builds the path or the address
// at run time, or a script reached after a cd, passes; upgrade to a network policy for skill scripts then.
func (t *ExecTool) personalFilesSentTo(command, cwd string) string {
	text := command + t.scriptsRun(command, cwd)
	destination := ""
	if raw := remoteURLPattern.FindString(text); raw != "" {
		destination = raw
		if parsed, err := url.Parse(raw); err == nil && parsed.Hostname() != "" {
			destination = parsed.Hostname()
		}
	} else if networkClientPattern.MatchString(text) {
		destination = "another host"
	}
	sent := downloadTargetPattern.ReplaceAllString(remoteURLPattern.ReplaceAllString(text, " "), " ")
	if destination == "" || !personalFilePattern.MatchString(sent) {
		return ""
	}
	return destination
}

// scriptsRun is the text of the regular files a command names as scripts, so
// a script that names the files and the address itself is read like the
// command. Under a workspace restriction it reads only scripts inside it: the
// refusal names a host, and one from a file the agent cannot read must not leak.
func (t *ExecTool) scriptsRun(command, cwd string) string {
	var text strings.Builder
	for _, name := range scriptPathPattern.FindAllString(command, maxScannedScripts) {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if t.restrictToWorkspace && t.workingDir != "" && !insideWorkspace(path, t.workingDir) {
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

func insideWorkspace(path, workspace string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	return err == nil && filepath.IsLocal(rel)
}
