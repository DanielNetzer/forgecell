// Package safetext bounds and cleans untrusted text before Forgecell shows,
// stores or logs it. It imports only the standard library, so every package
// that handles plugin, forge or model output can share one rule set.
package safetext

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// SecretMark replaces every secret in served evidence and ledger text.
const SecretMark = "[redacted]"

// U+2028 and U+2029 end a line in JavaScript and some log readers.
const (
	lineSeparator      = 0x2028
	paragraphSeparator = 0x2029
)

// Text bounds untrusted display text: valid UTF-8, no control characters, no
// line or paragraph separators (U+2028, U+2029, which end a line in JSON
// readers and logs), no format characters (unicode.Cf, such as U+202E bidi
// overrides or zero-width joiners, which can disguise text), at most limit
// runes.
func Text(s string, limit int) string { return clean(strings.ToValidUTF8(s, "�"), limit, false) }

// Multiline is Text for bodies: it keeps newlines and tabs (a CRLF becomes a
// newline) and replaces every other control character and both separators.
func Multiline(s string, limit int) string {
	return clean(strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\r\n", "\n"), limit, true)
}

func clean(s string, limit int, lines bool) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Cf, r) {
			continue
		}
		if r == lineSeparator || r == paragraphSeparator || unicode.IsControl(r) && !(lines && (r == '\n' || r == '\t')) {
			r = ' '
		}
		if n == limit {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// RedactSecrets replaces every secret in s with SecretMark.
func RedactSecrets(s string) string { return CutRedacted(s, len(s)) }

// CutRedacted returns the first limit bytes of s (backed off to a rune start)
// with every secret replaced. Secrets are found in all of s, and one that starts
// before the cut is replaced whole, so a secret straddling the cut is never
// partly served.
func CutRedacted(s string, limit int) string {
	if limit > len(s) {
		limit = len(s)
	}
	for limit > 0 && limit < len(s) && !utf8.RuneStart(s[limit]) {
		limit--
	}
	var b strings.Builder
	last := 0
	for _, sp := range SecretSpans(s) {
		if sp[0] >= limit {
			break
		}
		b.WriteString(s[last:sp[0]])
		b.WriteString(SecretMark)
		last = sp[1]
	}
	if last < limit {
		b.WriteString(s[last:limit])
	}
	return b.String()
}
