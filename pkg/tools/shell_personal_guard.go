package tools

import (
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// remoteURLPattern finds the URLs a command reaches.
var remoteURLPattern = regexp.MustCompile(`(?i)\b(?:https?|ftps?|sftp|wss?)://[^\s'"<>()]+`)

// personalFilePattern finds the person's profile and memory: what the assistant
// knows about them, never a deliverable of a task.
var personalFilePattern = regexp.MustCompile(
	`(?i)(?:^|[\s'"=@/(,:])(?:(?:user|soul|memory)\.md\b|(?:memory|sessions)/)`)

// scriptPathPattern finds the script files a command hands to an interpreter.
var scriptPathPattern = regexp.MustCompile(`[^\s'"<>|;&()]+\.(?:py|sh|bash|js|mjs|cjs|ts|rb|pl|php)\b`)

const (
	maxScannedScripts     = 8
	maxScannedScriptBytes = 64 << 10
)

// personalFilesSentTo returns the remote host a command, or a script it runs,
// would hand the person's profile or memory files to, or "". A skill or a file
// that asks for it is not the person asking, and some models follow a skill
// step by step.
// shortcut: it reads the command and the scripts it names, so code that builds the path or the address
// at run time, or a script reached after a cd, passes; upgrade to a network policy for skill scripts then.
func personalFilesSentTo(command, cwd string) string {
	text := withScripts(command, cwd)
	host := ""
	for _, raw := range remoteURLPattern.FindAllString(text, -1) {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Hostname() == "" {
			host = raw
			break
		}
		if !isLoopbackHost(parsed.Hostname()) {
			host = parsed.Hostname()
			break
		}
	}
	if host == "" || !personalFilePattern.MatchString(remoteURLPattern.ReplaceAllString(text, " ")) {
		return ""
	}
	return host
}

// withScripts is the command followed by the text of the regular files it
// names as scripts, so a script that names the files and the address itself
// is read like the command.
func withScripts(command, cwd string) string {
	var text strings.Builder
	text.WriteString(command)
	for _, name := range scriptPathPattern.FindAllString(command, maxScannedScripts) {
		path := name
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
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

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
