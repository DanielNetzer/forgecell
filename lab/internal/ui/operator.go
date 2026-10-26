package ui

import (
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"strings"
	"unicode"
)

// maxOperatorRunes bounds the served operator name; maxOperatorBytes bounds the
// configured value read at all. A longer configured value is refused whole and
// served as the fallback, never partly.
const (
	maxOperatorRunes = 80
	maxOperatorBytes = 1024
)

// OperatorFallback is the neutral name served when no usable operator name is
// configured. It has no initials, so the avatar shows a generic glyph.
const OperatorFallback = "Operator"

// OperatorView is the operator avatar on GET /api/lab: a display name taken from
// the source checkout's git config user.name, and up to two initials. Set is
// false for the fallback.
type OperatorView struct {
	Name     string `json:"name"`
	Initials string `json:"initials"`
	Set      bool   `json:"set"`
}

// operatorView turns the configured name into its served form: control and
// bidirectional characters removed, every word holding an @ (an email address,
// bare or in <…>) dropped, secrets and local paths redacted, and at most
// maxOperatorRunes runes. A value that is empty, oversized or has no letter left
// is served as the fallback.
func operatorView(raw string) OperatorView {
	fallback := OperatorView{Name: OperatorFallback}
	if len(raw) > maxOperatorBytes {
		return fallback
	}
	text := harness.ReviewDisplayText(raw, maxOperatorBytes)
	var words []string
	for _, word := range strings.Fields(text) {
		if !strings.ContainsAny(word, "@＠﹫") {
			words = append(words, word)
		}
	}
	text = harness.ReviewDisplayText(scrubPaths(redactSecrets(strings.Join(words, " "))), maxOperatorRunes)
	bare := redactionMarks.Replace(text)
	if !strings.ContainsFunc(bare, unicode.IsLetter) {
		return fallback
	}
	return OperatorView{Name: text, Initials: operatorInitials(bare), Set: true}
}

// redactionMarks removes the marks redaction leaves, so they are neither
// initials nor the only letters of a name.
var redactionMarks = strings.NewReplacer(secretMark, " ", "[local path]", " ")

// operatorInitials takes the first letter of the first and the last word that
// start with a letter, upper-cased.
func operatorInitials(name string) string {
	var letters []rune
	for _, word := range strings.Fields(name) {
		r := []rune(word)[0]
		if unicode.IsLetter(r) {
			letters = append(letters, unicode.ToUpper(r))
		}
	}
	switch len(letters) {
	case 0:
		return ""
	case 1:
		return string(letters[0])
	default:
		return string([]rune{letters[0], letters[len(letters)-1]})
	}
}
