package intake

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Bounds on tracker Issue keys and the tracker's keyPattern.
const (
	MaxIssueKeyRunes   = 128
	MaxKeyPatternBytes = 256
)

// ValidateIssueKey applies the pattern-independent key rules: a non-empty, valid
// UTF-8 key of at most MaxIssueKeyRunes runes without control, format (Cf), bidi
// control or line or paragraph separator characters.
func ValidateIssueKey(key string) error {
	if key == "" {
		return fmt.Errorf("issue key is empty")
	}
	// The byte bound is checked first, so a huge input costs no further work.
	if len(key) > MaxIssueKeyRunes*utf8.UTFMax || utf8.RuneCountInString(key) > MaxIssueKeyRunes {
		return fmt.Errorf("issue key is longer than %d characters", MaxIssueKeyRunes)
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("issue key is not valid UTF-8")
	}
	if strings.ContainsFunc(key, forbiddenKeyRune) {
		return fmt.Errorf("issue key contains a control, format, bidi or line separator character")
	}
	return nil
}

func forbiddenKeyRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Bidi_Control)
}

// ParseIssueKey validates input as an Issue key of a tracker whose keyPattern is
// pattern: a Go RE2 regexp of at most MaxKeyPatternBytes bytes written with a
// leading ^ and a trailing $. The pattern is always applied to the whole key, never
// as a substring match. A key that passes is returned unchanged.
func ParseIssueKey(input, pattern string) (string, error) {
	if len(pattern) > MaxKeyPatternBytes {
		return "", fmt.Errorf("issue key pattern is longer than %d bytes", MaxKeyPatternBytes)
	}
	if !strings.HasPrefix(pattern, "^") || !strings.HasSuffix(pattern, "$") || len(pattern) < 2 {
		return "", fmt.Errorf("issue key pattern must be anchored with ^ and $")
	}
	if err := ValidateIssueKey(input); err != nil {
		return "", err
	}
	// The raw pattern must compile on its own first, so an unbalanced pattern such
	// as ^x)|(y$ is refused rather than turned into a top-level alternation by the
	// wrapper below.
	if _, err := regexp.Compile(pattern); err != nil {
		return "", fmt.Errorf("issue key pattern is not a valid regular expression")
	}
	// Wrapping keeps an alternation such as ^a|b$ from matching a substring.
	re, err := regexp.Compile(`^(?:` + pattern + `)$`)
	if err != nil {
		return "", fmt.Errorf("issue key pattern is not a valid regular expression")
	}
	if !re.MatchString(input) {
		return "", fmt.Errorf("issue key does not match the tracker's key pattern")
	}
	return input, nil
}

// BindKeyRepository validates the repository a keyed Issue is bound to and returns
// it as owner/repo, with the rules BindRepository applies.
func BindKeyRepository(repository string) (string, error) {
	m := repoPattern.FindStringSubmatch(repository)
	if m == nil || m[1] == "." || m[1] == ".." || m[2] == "." || m[2] == ".." {
		return "", fmt.Errorf("invalid repository binding")
	}
	return m[1] + "/" + m[2], nil
}
