package ui

import (
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/safetext"
)

// secretMark replaces every secret in served evidence and ledger text.
const secretMark = safetext.SecretMark

// maxReasonRunes bounds a served refusal reason.
const maxReasonRunes = 300

// reason makes a refusal reason safe to serve: secrets are replaced, then the
// record's directories and any other local path, and the result is one line of
// at most maxReasonRunes runes with control characters stripped.
func (x redactor) reason(s string) string {
	return harness.ReviewDisplayText(x.text(redactSecrets(s)), maxReasonRunes)
}

// secretSpans returns the byte ranges of every secret in s, sorted and merged.
// The rule set lives in safetext so plugin diagnostics share it.
func secretSpans(s string) [][2]int { return safetext.SecretSpans(s) }

// redactSecrets replaces every secret in s with secretMark.
func redactSecrets(s string) string { return safetext.RedactSecrets(s) }

// cutRedacted returns the first limit bytes of s (backed off to a rune start)
// with every secret replaced. Secrets are found in all of s, and one that starts
// before the cut is replaced whole, so a secret straddling the cut is never
// partly served.
func cutRedacted(s string, limit int) string { return safetext.CutRedacted(s, limit) }
