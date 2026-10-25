package ui

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// secretMark replaces every secret in served evidence and ledger text.
const secretMark = "[redacted]"

// secretRule finds one kind of secret; group is the submatch that holds the
// secret itself, so a name such as OPENAI_API_KEY= or Authorization: stays
// readable and only its value is replaced.
type secretRule struct {
	pattern *regexp.Regexp
	group   int
}

// secretRules cover token patterns, *_KEY style assignments and Authorization
// headers, in plain text and inside JSON strings. They over-redact rather than
// leave part of a secret: a value runs to the end of its line or quote.
var secretRules = []secretRule{
	// Authorization and Proxy-Authorization header values.
	{regexp.MustCompile(`(?i)\b((?:proxy-)?authorization\\?["']?\s*[:=]\s*(?:\[\s*)?\\?["']?)([^\r\n"']+)`), 2},
	// x-api-key and similar headers.
	{regexp.MustCompile(`(?i)\b((?:x-)?api-key\\?["']?\s*[:=]\s*\\?["']?)([^\s"',;&]+)`), 2},
	// *_KEY, *_TOKEN, *_SECRET and *_PASSWORD assignments (env, YAML, JSON).
	{regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*_(?:KEY|KEY_ID|TOKEN|SECRET|PASSWORD|PASSWD)\\?["']?\s*[:=]\s*\\?["']?)([^\s"',;&]+)`), 2},
	// Bearer credentials wherever they appear; 16 characters keeps prose such as
	// "bearer tokens" readable.
	{regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=-]{16,})`), 2},
	// GitHub tokens.
	{regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`), 1},
	// Anthropic and OpenAI style keys (sk-ant-…, sk-proj-…, sk-…).
	{regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{16,})`), 1},
	// Slack tokens.
	{regexp.MustCompile(`\b(xox[abposr]-[A-Za-z0-9-]{10,})`), 1},
	// AWS access key ids.
	{regexp.MustCompile(`\b((?:AKIA|ASIA)[A-Z0-9]{16})\b`), 1},
	// Google API keys.
	{regexp.MustCompile(`\b(AIza[0-9A-Za-z_-]{35})`), 1},
	// Credentials in a URL: scheme://user:password@host.
	{regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)([^/\s:@"']+:[^/\s@"']+)@`), 2},
	// PEM private keys, to their end marker or the end of the text.
	{regexp.MustCompile(`(?s)(-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----|\z))`), 1},
	// Quoted values may contain spaces: redact up to the matching quote.
	{regexp.MustCompile(`(?i)\b(?:[A-Za-z0-9_]*_(?:KEY|KEY_ID|TOKEN|SECRET|PASSWORD|PASSWD)|password|passwd|secret|token|api[_-]?key|auth[_-]?token|access[_-]?token|client[_-]?secret)\s*[:=]\s*"([^"\r\n]*)"`), 1},
	{regexp.MustCompile(`(?i)\b(?:[A-Za-z0-9_]*_(?:KEY|KEY_ID|TOKEN|SECRET|PASSWORD|PASSWD)|password|passwd|secret|token|api[_-]?key|auth[_-]?token|access[_-]?token|client[_-]?secret)\s*[:=]\s*'([^'\r\n]*)'`), 1},
	// Bare and camelCase secret keys, quoted or not ("password": "…", apiKey=…).
	{regexp.MustCompile(`(?i)\b((?:password|passwd|secret|token|api[_-]?key|auth[_-]?token|access[_-]?token|client[_-]?secret)\\?["']?\s*[:=]\s*\\?["']?)([^\s"',;&]+)`), 2},
	{regexp.MustCompile(`(?i)(_authtoken\s*=\s*)(\S+)`), 2},
	{regexp.MustCompile(`(?i)\b((?:set-)?cookie\\?["']?\s*[:=]\s*(?:\[\s*)?\\?["']?)([^\r\n"']+)`), 2},
	{regexp.MustCompile(`\b(npm_[A-Za-z0-9]{36})\b`), 1},
	{regexp.MustCompile(`\b(glpat-[A-Za-z0-9_-]{20,})`), 1},
	{regexp.MustCompile(`\b(eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`), 1},
}

// secretSpans returns the byte ranges of every secret in s, sorted and merged.
func secretSpans(s string) [][2]int {
	var spans [][2]int
	for _, rule := range secretRules {
		for _, m := range rule.pattern.FindAllStringSubmatchIndex(s, -1) {
			if start, end := m[2*rule.group], m[2*rule.group+1]; start >= 0 && end > start {
				spans = append(spans, [2]int{start, end})
			}
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	merged := spans[:0]
	for _, sp := range spans {
		if n := len(merged); n > 0 && sp[0] <= merged[n-1][1] {
			merged[n-1][1] = max(merged[n-1][1], sp[1])
			continue
		}
		merged = append(merged, sp)
	}
	return merged
}

// redactSecrets replaces every secret in s with secretMark.
func redactSecrets(s string) string { return cutRedacted(s, len(s)) }

// cutRedacted returns the first limit bytes of s (backed off to a rune start)
// with every secret replaced. Secrets are found in all of s, and one that starts
// before the cut is replaced whole, so a secret straddling the cut is never
// partly served.
func cutRedacted(s string, limit int) string {
	if limit > len(s) {
		limit = len(s)
	}
	for limit > 0 && limit < len(s) && !utf8.RuneStart(s[limit]) {
		limit--
	}
	var b strings.Builder
	last := 0
	for _, sp := range secretSpans(s) {
		if sp[0] >= limit {
			break
		}
		b.WriteString(s[last:sp[0]])
		b.WriteString(secretMark)
		last = sp[1]
	}
	if last < limit {
		b.WriteString(s[last:limit])
	}
	return b.String()
}
