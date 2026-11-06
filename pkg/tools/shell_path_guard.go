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

// laterDirChangePattern finds a directory change the guard does not follow,
// including a tool told to run elsewhere (`env -C ..`, `git -C ..`).
var laterDirChangePattern = regexp.MustCompile(`\b(cd|pushd|popd)\b|\s-C\b|--chdir\b|--directory\b`)

// chainSeparatorPattern finds what ends a command; `&&` comes first so that a
// lone `&` means the background operator.
var chainSeparatorPattern = regexp.MustCompile(`&&|\|\||[;&\n]`)

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
	starts, rest := skipLeadingCDs(stripHeredocBodies(cmd), cwdPath, root)
	rest = shellJoinReplacer.Replace(rest)
	if laterDirChangePattern.MatchString(rest) {
		starts = []string{root}
	}
	guaranteed, fallback := splitAfterAndChain(rest)
	if path := t.firstEscapingPath(guaranteed, starts[len(starts)-1:], root); path != "" {
		return path
	}
	return t.firstEscapingPath(fallback, starts, root)
}

// splitAfterAndChain separates what only runs if every leading cd worked (the
// `&&` chain) from what also runs when one failed: `cd a && x || cat ../y` and
// `cd a && x; cat ../y` read ../y from wherever the shell stayed.
func splitAfterAndChain(text string) (guaranteed, fallback string) {
	for _, loc := range chainSeparatorPattern.FindAllStringIndex(text, -1) {
		if text[loc[0]:loc[1]] != "&&" {
			return text[:loc[0]], text[loc[0]:]
		}
	}
	return text, ""
}

// firstEscapingPath returns the first path in text that leaves root from any of
// the directories text may run in.
func (t *ExecTool) firstEscapingPath(text string, starts []string, root string) string {
	for _, token := range strings.FieldsFunc(text, isPathTokenBoundary) {
		if isWebURL(token) {
			continue
		}
		for _, path := range strings.Split(token, ":") {
			for _, start := range starts {
				if t.dotDotPathEscapes(path, start, root) {
					return path
				}
			}
		}
	}
	return ""
}

// skipLeadingCDs returns the directories the rest of text may run in, the one
// where every cd worked last. A cd it cannot resolve, or one that leaves root,
// stays in the text to be checked as a path.
func skipLeadingCDs(text, cwdPath, root string) ([]string, string) {
	starts := []string{cwdPath}
	for {
		base := starts[len(starts)-1]
		match := leadingCDPattern.FindStringSubmatch(text)
		if match == nil {
			return starts, text
		}
		target := strings.Trim(match[1], `"'`)
		// Anything the shell expands lands somewhere this cannot tell.
		if target == "-" || strings.ContainsAny(target, "$`~*?[\\") {
			return starts, text
		}
		// The shell's cd is logical: `a/..` is base even when a is a symlink.
		next := logicalPath(base, target)
		if !isInsideDir(next, root) || !isInsideDir(realPath(next), root) {
			return starts, text
		}
		starts, text = append(starts, next), text[len(match[0]):]
	}
}

func (t *ExecTool) dotDotPathEscapes(path, base, root string) bool {
	// The shell drops the backslash of `..\/`, so it climbs all the same.
	path = strings.ReplaceAll(path, `\`, "/")
	if !strings.Contains(path, "../") && path != ".." && !strings.HasSuffix(path, "/..") {
		return false
	}
	// A variable, a substitution or ~user can land anywhere.
	if strings.ContainsAny(path, "$`~") {
		return true
	}
	for segment := range strings.SplitSeq(path, "/") {
		// `.../`, `-I../x`, `HEAD../x`: nothing to resolve, so as strict as a climb.
		if segment != ".." && strings.HasSuffix(segment, "..") {
			return true
		}
	}
	// A cd follows the logical path and everything else the physical one.
	for _, resolved := range []string{climbPath(base, path), logicalPath(base, path)} {
		allowed := safePaths[resolved] || isAllowedPath(resolved, t.allowedPathPatterns)
		if !allowed && !isInsideDir(resolved, root) {
			return true
		}
	}
	return false
}

func logicalPath(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(base, path))
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
	return strings.ContainsRune(" \t\r\n'\"=,()[]{};&|<>", r)
}
