package redact

import (
	"path/filepath"
	"regexp"
	"strings"
)

// A shell word: bare characters and quoted runs, glued (`admin:'hunter 2'`).
const shellWord = `(?:[^\s'"]|'[^']*'|"[^"]*")+`

var (
	// A PEM private key, through its END line or, cut short, through the
	// end of its body.
	privateKeyRe = regexp.MustCompile(
		`-----BEGIN [A-Z ]*PRIVATE KEY-----(?:[^-]|-[^-])*(?:-----END [A-Z ]*PRIVATE KEY-----)?`)
	// `scheme://user:password@host` and `redis://:password@host` — keeps the
	// user, drops the password.
	urlUserinfoRe = regexp.MustCompile(`(://[^/\s:@"']*:)[^/\s@"']+@`)
	// `?token=x`, `&access_token=x`, `?code=x`, `&sig=x`.
	queryParamRe = regexp.MustCompile(
		`(?i)([?&](?:[a-z_]*token|password|passwd|senha|secret|client_secret|api_?key|key|sig|signature|code)=)` +
			`[^&\s"'#\\]+`)
	// An Authorization header in any spelling: `Authorization: Bearer x`,
	// `"authorization":"Basic x"`, `{"Authorization":["token x"]}`.
	authHeaderRe = regexp.MustCompile(
		`(?i)(\bauthorization\\?["']?\s*[:=]\s*[\["'\\]*(?:(?:bearer|basic|token|bot)\s+)?)([^\s"'\\,;}\]]+)`)
	// A Cookie or Set-Cookie header, to the end of its value.
	cookieHeaderRe = regexp.MustCompile(`(?i)(\b(?:set-)?cookie\\?["']?\s*[:=]\s*\\?["']?)([^\r\n"'\\]+)`)
	// A bare bearer token. `Bearer $TOKEN` stays: a shell variable is not the secret.
	bearerRe = regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{16,}`)
	// `curl -u user:password`, `-uuser:password`, `wget --user=u --http-user=u`.
	// Anchored on the HTTP clients: `docker run -u 1000:1000` is not a login.
	httpUserRe = regexp.MustCompile(
		`((?:curl|wget)\b[^\n|;&]*?\s(?:-u\s*|--user(?:\s+|=)|--http-user=))(` + shellWord + `)`)
	// `curl -b 'session=x'`, `curl --cookie session=x`.
	httpCookieRe = regexp.MustCompile(
		`((?:curl|wget)\b[^\n|;&]*?\s(?:-b\s*|--cookie(?:\s+|=)))(` + shellWord + `)`)
	// `--password x`, `--password=x`, `--pass x`, `--passwd=x`.
	passwordFlagRe = regexp.MustCompile(`(?i)(--pass(?:word|wd)?(?:=|\s+))(` + shellWord + `)`)
	// `mysql -phunter2` (glued: a bare `-p` prompts) and `sshpass -p hunter2`.
	mysqlPasswordRe = regexp.MustCompile(
		`((?:mysql|mariadb|mysqldump|mysqladmin)\b[^\n|;&]*?\s-p)(` + shellWord + `)`)
	sshpassRe = regexp.MustCompile(`(\bsshpass\s+-p\s*)(` + shellWord + `)`)
	jwtRe     = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)
	// Hulk keys keep their public part (`hulk_<kind>.key_<id>.`), which
	// identifies the key without exposing it.
	hulkKeyRe = regexp.MustCompile(`(\bhulk_[a-z]+\.key_[A-Za-z0-9]+\.)[A-Za-z0-9_-]{16,}`)
	// Well-known key formats: OpenAI/Anthropic, GitHub, Slack, AWS, Google,
	// Stripe and Telegram bot tokens (no word boundary: they follow `bot` in URLs).
	keyPrefixRe = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{20,}|` +
		`github_pat_[A-Za-z0-9_]{20,}|xox[abeprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16}|` +
		`AIza[0-9A-Za-z_-]{35}|[rs]k_live_[0-9A-Za-z]{16,})|\d{6,}:AA[A-Za-z0-9_-]{30,}`)
	// Environment variables named after a secret: `DB_PASS=x`, `MYSQL_PWD=x`,
	// `STRIPE_KEY=x`. A bare `PWD=` (the working directory) has no prefix.
	envSecretRe = regexp.MustCompile(
		`(\b[A-Z][A-Z0-9_]*_(?:PASS|PASSWD|PASSWORD|PWD|SECRET|TOKEN|KEY)=)(` + shellWord + `)`)
	// `password=x`, `"api_key": "x y"`, `senha => 'x'`, `Senha: x`. A quoted
	// value is masked to its closing quote, escaped quotes included. The name
	// is confirmed by sensitiveKey, so `max_tokens=100` or `secretName: db`
	// stay. The match starts at the keyword: a leading wildcard would cost a
	// regex attempt at every byte of every log line.
	assignmentRe = regexp.MustCompile(
		`(?i)((?:password|passwd|senha|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*)` +
			`(\\?["']?\s*(?:=>|[:=])\s*)` +
			`(\\"(?:[^"\\]|\\[^"])*|"(?:[^"\\]|\\.)*|'[^']*|[^\s"'\\&,;}]+)`)
	// An upload as the pod stores it: a random 8-hex prefix and the person's
	// file name, which UploadFile reduces to these characters. Not a credential,
	// but the person's, and every read_file would log it. Matched with or
	// without the uploads/ folder, since a command run inside it names the file
	// alone. The prefix must start the token, so a pod or job name with a hex
	// part stays. Text often arrives escaped (JSON of a command's output, a URL),
	// so an escape before the prefix also starts a token.
	uploadNameRe = regexp.MustCompile(
		`(^|[^A-Za-z0-9._-]|\\[nrt]|\\u[0-9A-Fa-f]{4}|%[0-9A-Fa-f]{2})([0-9a-f]{8}-)([A-Za-z0-9._-]+)`)
	// The rest of a UUID after its first 8 hex digits, ending the token or
	// followed by an extension or a separator. It names a run, a session or a
	// media file, and logs are searched by it.
	uuidTailRe = regexp.MustCompile(`^[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?:$|[._-])`)
)

// uploadExtensions are the extensions kept after a masked upload name: they say
// what kind of file failed. Anything else after a dot may be part of the name,
// such as an invoice number.
var uploadExtensions = map[string]bool{
	".pdf": true, ".doc": true, ".docx": true, ".odt": true, ".rtf": true, ".epub": true,
	".xls": true, ".xlsx": true, ".ods": true, ".csv": true, ".ppt": true, ".pptx": true, ".odp": true,
	".txt": true, ".md": true, ".json": true, ".xml": true, ".html": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".heic": true, ".svg": true,
	".ogg": true, ".oga": true, ".opus": true, ".mp3": true, ".m4a": true, ".wav": true, ".aac": true,
	".amr": true, ".flac": true, ".wma": true,
	".mp4": true, ".mov": true, ".webm": true,
	".zip": true, ".gz": true, ".tar": true, ".rar": true, ".7z": true,
}

// textRule runs mask only when the lowercased text holds one of its hints.
// Text runs on every log line and most lines carry no credential, so a few
// substring checks spare the regexes almost always. The upload rule's hint, a
// dash, is in most lines; its mask scans for the 8-hex prefix before the regex.
type textRule struct {
	hints []string
	mask  func(string) string
}

var textRules = []textRule{
	{[]string{"private key-----"}, replaceWith(privateKeyRe, Mask)},
	{[]string{"://"}, replaceWith(urlUserinfoRe, "${1}"+Mask+"@")},
	{[]string{"?", "&"}, replaceWith(queryParamRe, "${1}"+Mask)},
	{[]string{"authorization"}, maskTailOf(authHeaderRe)},
	{[]string{"cookie"}, maskTailOf(cookieHeaderRe)},
	{[]string{"bearer"}, replaceWith(bearerRe, "${1}"+Mask)},
	{
		[]string{"curl", "wget"},
		func(s string) string { return maskTail(httpCookieRe, maskCredentialWord(httpUserRe, s)) },
	},
	{[]string{"--pass"}, maskTailOf(passwordFlagRe)},
	{[]string{"mysql", "mariadb"}, maskTailOf(mysqlPasswordRe)},
	{[]string{"sshpass"}, maskTailOf(sshpassRe)},
	{[]string{"eyj"}, replaceWith(jwtRe, Mask)},
	{[]string{"hulk_"}, replaceWith(hulkKeyRe, "${1}"+Mask)},
	{
		[]string{"sk-", "ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "xox", "akia", "aiza", "k_live_", ":aa"},
		replaceWith(keyPrefixRe, Mask),
	},
	{[]string{"_pass", "_pwd", "_secret", "_token", "_key"}, maskTailOf(envSecretRe)},
	{
		[]string{
			"password", "passwd", "senha", "secret", "token", "api_key", "api-key", "apikey",
			"access_key", "access-key", "accesskey", "private_key", "private-key", "privatekey",
		},
		func(s string) string { return assignmentRe.ReplaceAllStringFunc(s, maskAssignment) },
	},
	{[]string{"-"}, maskUploadNames},
}

// Text masks the credentials found in free text, and the file name in the path
// of an upload.
func Text(s string) string {
	if s == "" {
		return s
	}
	lower := strings.ToLower(s)
	for _, rule := range textRules {
		if containsAny(lower, rule.hints) {
			s = rule.mask(s)
		}
	}
	return s
}

func replaceWith(re *regexp.Regexp, replacement string) func(string) string {
	return func(s string) string { return re.ReplaceAllString(s, replacement) }
}

func maskTailOf(re *regexp.Regexp) func(string) string {
	return func(s string) string { return maskTail(re, s) }
}

// maskTail masks the second group of each match of re, whose first group is
// the prefix to keep, unless the value only refers to a secret.
func maskTail(re *regexp.Regexp, s string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		parts := re.FindStringSubmatch(match)
		if isPlaceholder(parts[2]) {
			return match
		}
		return parts[1] + Mask
	})
}

// maskCredentialWord masks what follows the first colon of a `user:password`
// word, keeping the user and the closing quote of a quoted word.
func maskCredentialWord(re *regexp.Regexp, s string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		parts := re.FindStringSubmatch(match)
		word := parts[2]
		user, password, found := strings.Cut(word, ":")
		if !found || isPlaceholder(password) {
			return match
		}
		closing := ""
		if quote := word[:1]; (quote == "'" || quote == `"`) && strings.HasSuffix(word, quote) {
			closing = quote
		}
		return parts[1] + user + ":" + Mask + closing
	})
}

// maskUploadNames keeps the prefix, which tells two uploads apart, and a known
// extension. A UUID stays whole.
func maskUploadNames(s string) string {
	if !hasHexBeforeDash(s) {
		return s
	}
	return uploadNameRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := uploadNameRe.FindStringSubmatch(match)
		name := parts[3]
		if uuidTailRe.MatchString(name) {
			return match
		}
		return parts[1] + parts[2] + Mask + uploadExtension(name)
	})
}

// hasHexBeforeDash says whether eight lowercase hex digits come right before a
// dash: uploadNameRe has no literal to anchor on, and most lines fail this.
func hasHexBeforeDash(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '-' && run >= 8:
			return true
		case '0' <= c && c <= '9', 'a' <= c && c <= 'f':
			run++
		default:
			run = 0
		}
	}
	return false
}

func uploadExtension(name string) string {
	if ext := filepath.Ext(name); uploadExtensions[strings.ToLower(ext)] {
		return ext
	}
	return ""
}

func maskAssignment(match string) string {
	parts := assignmentRe.FindStringSubmatch(match)
	name, sep, value := parts[1], parts[2], parts[3]
	if !sensitiveKey(name) || isPlaceholder(value) || isLiteral(value) {
		return match
	}
	quote := value[:len(value)-len(strings.TrimLeft(value, `\"'`))]
	return name + sep + quote + Mask
}

// A reference to a secret is not the secret: `$TOKEN`, `${TOKEN}`,
// `{{ .token }}`, `<your-token>`.
func isPlaceholder(value string) bool {
	value = strings.TrimLeft(value, `\"'`)
	return value == "" || strings.HasPrefix(value, Mask) || strings.HasPrefix(value, "$") ||
		strings.HasPrefix(value, "{{") || strings.HasPrefix(value, "<")
}

// `password_changed: true` says nothing about the password.
func isLiteral(value string) bool {
	switch strings.ToLower(value) {
	case "true", "false", "null", "nil":
		return true
	}
	return false
}
