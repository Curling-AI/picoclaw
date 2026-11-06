package tools

import (
	"path/filepath"
	"regexp"
	"strings"
)

// webSchemes are URL schemes whose path components (starting with //) are exempt
// from the workspace checks. file: is left out so file:// URIs still are.
var webSchemes = []string{
	"http:", "https:", "ftp:", "ftps:", "sftp:", "ssh:", "git:",
	// Network protocols curl and other tools reach over //host — not
	// filesystem paths. (seucaranguejo fork)
	"smtp:", "smtps:", "imap:", "imaps:", "pop3:", "pop3s:", "ws:", "wss:",
}

// leadingCDPattern is `cd <dir> &&` at the start of a command: what follows only
// runs if the cd worked, so it is known to run there. Any later cd is not
// followed: `(cd a); cat ../x` runs back where it started.
var leadingCDPattern = regexp.MustCompile(`^\s*cd\s+("[^"]*"|'[^']*'|[^\s;&|<>()]+)\s*&&`)

// laterDirChangePattern finds a directory change the guard does not follow.
var laterDirChangePattern = regexp.MustCompile(`\b(cd|pushd|popd)\b`)

// shellJoinReplacer removes what the shell removes when it joins one word, so a
// path split across quotes is checked whole: `../'../x'` climbs twice.
var shellJoinReplacer = strings.NewReplacer(`'`, "", `"`, "", "\\\n", "")

// escapingDotDotPath returns the first path in cmd that climbs out of the
// workspace, or "". Heredoc bodies are data (an import there is relative to the
// file being written); quoted text is checked, because `cat "../../x"` is a real
// argument. This is a guard against slips, like the absolute-path scan beside
// it, not a sandbox: inline code can always build a path at run time.
func (t *ExecTool) escapingDotDotPath(cmd, cwdPath string) string {
	root := cwdPath
	if t.workingDir != "" {
		if abs, err := filepath.Abs(t.workingDir); err == nil {
			root = abs
		}
	}
	base, rest := skipLeadingCDs(stripHeredocBodies(cmd), cwdPath, root)
	if laterDirChangePattern.MatchString(rest) {
		base = root
	}
	for _, token := range strings.FieldsFunc(shellJoinReplacer.Replace(rest), isPathTokenBoundary) {
		if isWebURL(token) {
			continue
		}
		for _, path := range strings.Split(token, ":") {
			if t.dotDotPathEscapes(path, base, root) {
				return path
			}
		}
	}
	return ""
}

// skipLeadingCDs returns the directory the rest of text runs in. A cd it cannot
// resolve, or one that leaves root, stays in the text to be checked as a path.
func skipLeadingCDs(text, cwdPath, root string) (string, string) {
	base := cwdPath
	for {
		match := leadingCDPattern.FindStringSubmatch(text)
		if match == nil {
			return base, text
		}
		target := strings.Trim(match[1], `"'`)
		// Anything the shell expands lands somewhere this cannot tell.
		if target == "-" || strings.ContainsAny(target, "$`~*?[\\") {
			return base, text
		}
		next := climbPath(base, target)
		if !isInsideDir(next, root) {
			return base, text
		}
		base, text = next, text[len(match[0]):]
	}
}

func (t *ExecTool) dotDotPathEscapes(path, base, root string) bool {
	// The shell drops the backslash of `..\/`, so it climbs all the same.
	path = strings.ReplaceAll(path, `\`, "/")
	if !strings.Contains(path, "../") {
		return false
	}
	segments := strings.Split(path, "/")
	lastClimb := -1
	for i, segment := range segments {
		switch {
		case segment == "..":
			lastClimb = i
		case strings.HasSuffix(segment, ".."):
			// `.../`, `-I../x`, `HEAD../x`: nothing to resolve, so as strict as a climb.
			return true
		}
	}
	// A variable or ~user before the climb can be anywhere.
	if lastClimb > 0 && strings.ContainsAny(strings.Join(segments[:lastClimb], "/"), "$~") {
		return true
	}
	resolved := climbPath(base, path)
	if safePaths[resolved] || isAllowedPath(resolved, t.allowedPathPatterns) {
		return false
	}
	return !isInsideDir(resolved, root)
}

// climbPath walks path from base the way the kernel does: `..` leaves the real
// directory, so a symlink in the way counts, which filepath.Clean would miss.
func climbPath(base, path string) string {
	current := base
	if strings.HasPrefix(path, "/") {
		current = "/"
	}
	for _, segment := range strings.Split(path, "/") {
		switch segment {
		case "", ".":
		case "..":
			current = filepath.Dir(realPath(current))
		default:
			current = filepath.Join(current, segment)
		}
	}
	return realPath(current)
}

func realPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

func isInsideDir(path, dir string) bool {
	for _, root := range []string{dir, realPath(dir)} {
		if rel, err := filepath.Rel(root, path); err == nil && filepath.IsLocal(rel) {
			return true
		}
	}
	return false
}

func isWebURL(token string) bool {
	for _, scheme := range webSchemes {
		if strings.HasPrefix(token, scheme+"//") {
			return true
		}
	}
	return false
}

func isPathTokenBoundary(r rune) bool {
	return strings.ContainsRune(" \t\r\n'\"`=,()[]{};&|<>", r)
}
