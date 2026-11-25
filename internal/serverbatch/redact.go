package serverbatch

import (
	"regexp"
	"strings"
)

// Redaction of credential-shaped substrings (§10.4, §21).
//
// Redact masks values that look like credentials while keeping the rest
// of the text intact, so exported commands and outputs stay useful:
//   - password/passwd/pwd/token/secret/api-key assignments, in
//     `key=value`, `key: value` and `--key value` / `--key=value` forms
//     (case-insensitive, quoted values included);
//   - Authorization: Bearer/Basic headers;
//   - a bare `key=value` assignment.
//
// It is pattern-based, not a guarantee: a credential in an unfamiliar
// spelling can survive, and a non-secret value next to one of these
// words can be masked. Exporters run every command, output and error
// through Redact before writing; nothing else in this package writes
// credential-shaped text anywhere.

var redactPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Authorization: Bearer xxx / Basic xxx
	{regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|basic)\s+)[^\s]+`), `${1}***`},
	// --password xxx, --password=xxx, --api-key xxx. The dashes are
	// mandatory: a bare "token is ..." in prose must not be masked.
	{regexp.MustCompile(`(?i)(--?(?:password|passwd|pwd|token|secret|api[-_]?key|access[-_]?key|private[-_]?key)(?:\s+|=))("[^"]*"|'[^']*'|[^\s]+)`), `${1}***`},
	// password=xxx, password: xxx, token=xxx, api_key: xxx (quoted or
	// bare). A word prefix such as db_ is allowed (db_password=...),
	// but the keyword must be followed by : or = so prose such as
	// "the token is ..." and "passwords are ..." survive.
	{regexp.MustCompile(`(?i)([A-Za-z0-9_-]*(?:password|passwd|pwd|token|secret|api[-_]?key|access[-_]?key|private[-_]?key))\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`), `$1=***`},
	// a bare key=xxx assignment (the word "key" alone, not "monkey")
	{regexp.MustCompile(`(?i)\bkey\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`), `key=***`},
}

// Redact masks credential-shaped substrings in s, preserving the rest.
func Redact(s string) string {
	if s == "" {
		return s
	}
	out := s
	for _, p := range redactPatterns {
		out = p.re.ReplaceAllString(out, p.repl)
	}
	return out
}

// redactLines applies Redact line by line (kept as a helper for
// exporters that stream output).
func redactLines(s string) string {
	if !strings.Contains(s, "\n") {
		return Redact(s)
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = Redact(line)
	}
	return strings.Join(lines, "\n")
}
