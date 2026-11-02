package intake

import (
	"fmt"
	"strings"
)

// Bounds of a project path: a top-level group, up to 20 subgroups and the
// project. A path near MaxProjectPathBytes can still be refused later, when its
// forge.identity (host/path) exceeds formula.MaxForgeIdentity; that refusal is
// the intended fail-safe.
const (
	MinProjectSegments  = 2
	MaxProjectSegments  = 22
	MaxProjectPathBytes = 255
)

// ValidProjectPath reports why s is not a forge project path such as
// welltech-com/platform/backend, or nil when it is. Segments are split on '/',
// so a leading, trailing or doubled '/' is an empty segment and refused. Each
// segment is ASCII letters, digits, '_', '.' and '-', starts with a letter,
// digit or '_', does not end with '.', and is never "." or "..", so the path
// cannot name a traversal. The last segment does not end in .git or .atom.
func ValidProjectPath(s string) error {
	// Bounds first, so a huge input costs no further work.
	if s == "" || len(s) > MaxProjectPathBytes {
		return fmt.Errorf("project path must be non-empty and at most %d bytes", MaxProjectPathBytes)
	}
	if n := strings.Count(s, "/") + 1; n < MinProjectSegments || n > MaxProjectSegments {
		return fmt.Errorf("project path must have %d to %d '/'-separated segments", MinProjectSegments, MaxProjectSegments)
	}
	segments := strings.Split(s, "/")
	for _, seg := range segments {
		if why := projectSegment(seg); why != "" {
			return fmt.Errorf("project path segment %s", why)
		}
	}
	last := strings.ToLower(segments[len(segments)-1])
	if strings.HasSuffix(last, ".git") || strings.HasSuffix(last, ".atom") {
		return fmt.Errorf("project path must name the project without a trailing .git or .atom")
	}
	return nil
}

func projectSegment(seg string) string {
	if seg == "" {
		return "is empty"
	}
	if seg == "." || seg == ".." {
		return "must not be . or .."
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return "must hold only ASCII letters, digits, '_', '.' and '-'"
		}
	}
	if c := seg[0]; c == '.' || c == '-' {
		return "must start with a letter, digit or '_'"
	}
	if seg[len(seg)-1] == '.' {
		return "must not end with '.'"
	}
	return ""
}

// IsNestedProjectPath reports whether s has three or more '/'-separated
// segments, the only paths ValidProjectPath governs at binding time. A
// two-segment owner/repo keeps BindKeyRepository exactly, so nothing it accepted
// is newly refused.
func IsNestedProjectPath(s string) bool {
	return strings.Count(s, "/") >= 2
}

// BindKeyProject validates the project a keyed Issue is bound to. With nested
// false, or for a two-segment owner/repo, it is exactly BindKeyRepository. With
// nested true a path of three or more segments must be a ValidProjectPath, such
// as a nested group, and is returned unchanged. Callers decide nested with
// formula.Formula.NestedProjectAllowed.
func BindKeyProject(repository string, nested bool) (string, error) {
	if !nested || !IsNestedProjectPath(repository) {
		return BindKeyRepository(repository)
	}
	if err := ValidProjectPath(repository); err != nil {
		return "", fmt.Errorf("invalid repository binding: %w", err)
	}
	return repository, nil
}
