package readiness

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// markdownSnapshot is the shared repository fixture plus a tracked docs/install.md
// and a tracked Markdown file directly in .github.
func markdownSnapshot(t *testing.T) RepositorySnapshot {
	t.Helper()
	dir := sliceBFixture(t, map[string]string{"docs/install.md": "# Install\n", ".github/notes.md": "# Notes\n"})
	s, err := Collect(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMarkdownRuleAcceptsNewMarkdownNextToATrackedFile(t *testing.T) {
	s := markdownSnapshot(t)
	for _, file := range []string{"docs/new.md", "NEW.md", "lab/notes.md"} {
		scope := []ScopedPath{{Path: file, Reason: "New document", Evidence: []string{"issue"}}}
		if err := s.ValidateScope(scope); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if got := s.MarkdownScopeFiles(scope); !reflect.DeepEqual(got, []string{file}) {
			t.Fatalf("%s: Markdown-rule files %v", file, got)
		}
	}
}

func TestMarkdownRuleKeepsTheManifestRuleForEveryOtherPath(t *testing.T) {
	s := markdownSnapshot(t)
	for _, file := range []string{"docs/new.go", "docs/new.MD", "newdir/new.md", ".github/new.md", ".github/workflows/new.md", "docs/sub/new.md", "docs/.hidden/new.md"} {
		scope := []ScopedPath{{Path: file, Reason: "New file", Evidence: []string{"issue"}}}
		err := s.ValidateScope(scope)
		if err == nil || !strings.Contains(err.Error(), "needs evidence for its parent component") {
			t.Fatalf("%s: want the manifest-rule refusal, got %v", file, err)
		}
		if got := s.MarkdownScopeFiles(scope); len(got) != 0 {
			t.Fatalf("%s: listed as a Markdown-rule file: %v", file, got)
		}
	}
}

func TestMarkdownRuleKeepsTraversalAndDirectoryRefusals(t *testing.T) {
	s := markdownSnapshot(t)
	for file, want := range map[string]string{
		"linked/new.md":        "traverses a file, symlink or submodule",
		"README.md/new.md":     "traverses a file, symlink or submodule",
		"docs":                 "is a directory",
		"docs/../docs/new.md":  "",
		".git/new.md":          "",
		"docs/new.md\x00x.md":  "",
		"docs/*.md":            "",
		"docs/new.md/child.md": "needs evidence",
		"docs/install.md/x.md": "traverses a file, symlink or submodule",
	} {
		scope := []ScopedPath{{Path: file, Reason: "New file", Evidence: []string{"issue"}}}
		err := s.ValidateScope(scope)
		if err == nil || (want != "" && !strings.Contains(err.Error(), want)) {
			t.Fatalf("%q: want refusal %q, got %v", file, want, err)
		}
		if got := s.MarkdownScopeFiles(scope); len(got) != 0 {
			t.Fatalf("%q: listed as a Markdown-rule file: %v", file, got)
		}
	}
	dup := []ScopedPath{{Path: "docs/new.md", Reason: "a", Evidence: []string{"issue"}}, {Path: "docs/NEW.md", Reason: "b", Evidence: []string{"issue"}}}
	if err := s.ValidateScope(dup); err == nil || !strings.Contains(err.Error(), "case-ambiguous") {
		t.Fatalf("case-folded duplicate accepted: %v", err)
	}
}

func TestMarkdownScopeFilesListsOnlyMarkdownRulePaths(t *testing.T) {
	s := markdownSnapshot(t)
	var manifest string
	for _, e := range s.Evidence {
		if e.Path == "lab/go.mod" {
			manifest = e.ID
		}
	}
	scope := []ScopedPath{
		{Path: "README.md", Reason: "Edited", Evidence: []string{"issue"}},
		{Path: "lab/new.go", Reason: "New code", Evidence: []string{manifest}},
		{Path: "docs/new.md", Reason: "New document", Evidence: []string{"issue"}},
		{Path: "NEW.md", Reason: "New document", Evidence: []string{"issue"}},
	}
	if err := s.ValidateScope(scope); err != nil {
		t.Fatal(err)
	}
	if got := s.MarkdownScopeFiles(scope); !reflect.DeepEqual(got, []string{"docs/new.md", "NEW.md"}) {
		t.Fatalf("Markdown-rule files %v", got)
	}
}

func TestMarkdownRuleNeedsATrackedRegularFileInTheParent(t *testing.T) {
	s := RepositorySnapshot{Files: []TreeFile{{Path: "docs/link", Mode: "120000"}, {Path: "docs/sub/a.md", Mode: "100644"}, {Path: "mods/m", Mode: "160000"}}}
	for _, file := range []string{"docs/new.md", "mods/new.md", "NEW.md"} {
		scope := []ScopedPath{{Path: file, Reason: "New document", Evidence: []string{"issue"}}}
		if err := s.ValidateScope(scope); err == nil {
			t.Fatalf("%s accepted next to only a symlink, submodule or nothing", file)
		}
	}
	if err := s.ValidateScope([]ScopedPath{{Path: "docs/sub/new.md", Reason: "New document", Evidence: []string{"issue"}}}); err != nil {
		t.Fatal(err)
	}
}

func markdownPlan() Plan {
	p := fixture()
	p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "docs/new.md", Reason: "New document", Evidence: []string{"issue"}})
	p.MarkdownNewFiles = []string{"docs/new.md"}
	return p
}

func TestPlanMarkdownNewFilesJoinTheDigestAndAreOmittedWhenEmpty(t *testing.T) {
	p := markdownPlan()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	unlisted := p
	unlisted.MarkdownNewFiles = nil
	a, errA := p.Digest()
	b, errB := unlisted.Digest()
	if errA != nil || errB != nil || a == b {
		t.Fatalf("markdownNewFiles must join the plan digest: %v %v", errA, errB)
	}
	raw, err := json.Marshal(unlisted)
	if err != nil || strings.Contains(string(raw), "markdownNewFiles") {
		t.Fatalf("an empty markdownNewFiles must be omitted: %v %s", err, raw)
	}
}

func TestPlanMarkdownNewFilesRejectInvalidEntries(t *testing.T) {
	for name, mutate := range map[string]func(*Plan){
		"outside the scope": func(p *Plan) { p.MarkdownNewFiles = []string{"docs/other.md"} },
		"duplicate":         func(p *Plan) { p.MarkdownNewFiles = []string{"docs/new.md", "docs/new.md"} },
		"not markdown": func(p *Plan) {
			p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "docs/new.go", Reason: "New code", Evidence: []string{"issue"}})
			p.MarkdownNewFiles = []string{"docs/new.go"}
		},
		"upper-case extension": func(p *Plan) {
			p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "docs/new.MD", Reason: "New document", Evidence: []string{"issue"}})
			p.MarkdownNewFiles = []string{"docs/new.MD"}
		},
		"dot-directory": func(p *Plan) {
			p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: ".github/new.md", Reason: "New document", Evidence: []string{"issue"}})
			p.MarkdownNewFiles = []string{".github/new.md"}
		},
		"frozen evidence path": func(p *Plan) {
			p.Evidence = append(p.Evidence, Evidence{ID: "readme", Path: "README.md", SHA256: strings.Repeat("e", 64)})
			p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "README.md", Reason: "Edited", Evidence: []string{"readme"}})
			p.MarkdownNewFiles = []string{"README.md"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := markdownPlan()
			mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("an invalid Markdown-rule entry was accepted")
			}
		})
	}
}
