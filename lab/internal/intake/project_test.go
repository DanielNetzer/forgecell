package intake

import (
	"strings"
	"testing"
)

// segments joins n single-letter segments.
func segments(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "g"
	}
	return strings.Join(parts, "/")
}

// pathOfBytes is a two-segment path of exactly n bytes.
func pathOfBytes(n int) string {
	return "a/" + strings.Repeat("b", n-2)
}

func TestValidProjectPathAccepts(t *testing.T) {
	for _, s := range []string{
		"welltech-com/platform/backend",
		"acme/widgets",
		"Acme/Widgets",
		"_group/sub_group/project.v2",
		"9group/a-b/c.d",
		"group/project-",
		"a/b",
		segments(MaxProjectSegments),
		pathOfBytes(MaxProjectPathBytes),
		"group/project.gitx",
	} {
		if err := ValidProjectPath(s); err != nil {
			t.Errorf("ValidProjectPath(%q) = %v", s, err)
		}
	}
}

func TestValidProjectPathRefuses(t *testing.T) {
	for name, s := range map[string]string{
		"empty":            "",
		"one segment":      "backend",
		"23 segments":      segments(MaxProjectSegments + 1),
		"256 bytes":        pathOfBytes(MaxProjectPathBytes + 1),
		"dot dot":          "group/../backend",
		"dot dot last":     "acme/..",
		"dot":              "group/./backend",
		"trailing dot":     "group/backend.",
		"trailing dot mid": "group./backend",
		".git":             "group/backend.git",
		".GIT":             "group/backend.GIT",
		".atom":            "group/backend.atom",
		".Atom":            "group/backend.Atom",
		"leading dash":     "group/-backend",
		"leading dot":      "group/.backend",
		"empty segment":    "group//backend",
		"leading slash":    "/group/backend",
		"trailing slash":   "group/backend/",
		"non-ASCII":        "gröup/backend",
		"kelvin sign":      "group/bacKend",
		"percent":          "group/%2e%2e",
		"space":            "group/back end",
		"backslash":        `group\sub/backend`,
		"colon":            "group:sub/backend",
		"NUL":              "group/back\x00end",
	} {
		if err := ValidProjectPath(s); err == nil {
			t.Errorf("%s: ValidProjectPath(%q) accepted", name, s)
		}
	}
}

func TestBindKeyProject(t *testing.T) {
	// With nested false it is exactly BindKeyRepository, refusals included.
	for _, repo := range []string{"Acme/widgets", "acme/-x", "acme/x.", "acme/..", "a/b/c", "", "acme"} {
		want, wantErr := BindKeyRepository(repo)
		got, err := BindKeyProject(repo, false)
		if got != want || (err == nil) != (wantErr == nil) {
			t.Errorf("BindKeyProject(%q, false) = %q, %v; BindKeyRepository = %q, %v", repo, got, err, want, wantErr)
		}
	}
	// A two-segment repository on a nested-allowed Formula is still exactly
	// BindKeyRepository: shapes it accepted that ValidProjectPath would refuse
	// (a leading '-', a trailing '.', .git) stay accepted.
	for _, repo := range []string{"Acme/widgets", "acme/-x", "acme/x.", "acme/x.git", "-acme/x", "acme/..", "acme/", "acme"} {
		want, wantErr := BindKeyRepository(repo)
		got, err := BindKeyProject(repo, true)
		if got != want || (err == nil) != (wantErr == nil) {
			t.Errorf("BindKeyProject(%q, true) = %q, %v; BindKeyRepository = %q, %v", repo, got, err, want, wantErr)
		}
	}
	for _, repo := range []string{"welltech-com/platform/backend", "Acme/Widgets", segments(MaxProjectSegments)} {
		if got, err := BindKeyProject(repo, true); err != nil || got != repo {
			t.Errorf("BindKeyProject(%q, true) = %q, %v", repo, got, err)
		}
	}
	for _, repo := range []string{"group/../backend", "group//backend", "backend", "group/sub/backend.git", "group/-sub/backend", segments(MaxProjectSegments + 1)} {
		if got, err := BindKeyProject(repo, true); err == nil {
			t.Errorf("BindKeyProject(%q, true) = %q", repo, got)
		}
	}
	// A nested path is refused unless nested is allowed.
	if _, err := BindKeyProject("welltech-com/platform/backend", false); err == nil {
		t.Error("a nested path was bound with nested false")
	}
	for s, want := range map[string]bool{"a/b": false, "a": false, "": false, "a/b/c": true, "a//b": true} {
		if got := IsNestedProjectPath(s); got != want {
			t.Errorf("IsNestedProjectPath(%q) = %v", s, got)
		}
	}
}
