package readiness

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func inScopeAnalysis(scope ...string) (Analysis, []Evidence) {
	p := fixture()
	p.Evidence = append(p.Evidence, Evidence{ID: "doc", Path: "docs/atoms.md", SHA256: strings.Repeat("f", 64)})
	a := p.Analysis
	for _, s := range scope {
		a.Scope = append(a.Scope, ScopedPath{Path: s, Reason: "Edited", Evidence: []string{"manifest"}})
	}
	return a, p.Evidence
}

func TestInScopeDefinitionRejectsDefinitionPathInScope(t *testing.T) {
	a, evidence := inScopeAnalysis("lab/go.mod")
	err := ValidateInScopeDefinitions(a, evidence)
	var violation *PolicyViolation
	if !errors.As(err, &violation) || !IsPolicyViolation(err) {
		t.Fatalf("not a policy violation: %v", err)
	}
	if want := InScopeDefinitionMessage + ": lab/go.mod (regression)"; err.Error() != want || InScopeDefinitionMessage != "drop or replace checks defined by in-scope files" {
		t.Fatalf("message %q, want %q", err.Error(), want)
	}
}

func TestInScopeDefinitionRejectsTestPathDefinition(t *testing.T) {
	a, evidence := inScopeAnalysis("docs/atoms.md", "lab/parser_test.go")
	evidence = append(evidence, Evidence{ID: "pinned", Path: "lab/parser_test.go", SHA256: strings.Repeat("a", 64)})
	a.Checks[0].Definitions = []string{"pinned"}
	if err := ValidateInScopeDefinitions(a, evidence); err == nil || !strings.Contains(err.Error(), "lab/parser_test.go (regression)") {
		t.Fatalf("the run-time guard blocks every definition overlap, test path or not: %v", err)
	}
}

func TestInScopeDefinitionPassesWhenNothingOverlaps(t *testing.T) {
	a, evidence := inScopeAnalysis("docs/atoms.md")
	a.Checks[0].Definitions = []string{"manifest"}
	if err := ValidateInScopeDefinitions(a, evidence); err != nil {
		t.Fatalf("scope %v does not overlap a check definition: %v", a.Scope, err)
	}
}

func TestInScopeDefinitionSkipsUnknownDefinitionIDs(t *testing.T) {
	a, evidence := inScopeAnalysis("lab/go.mod")
	a.Checks[0].Definitions = []string{"issue", "missing-evidence"}
	if err := ValidateInScopeDefinitions(a, evidence); err != nil {
		t.Fatalf("unknown IDs are rejected later by Plan.Validate, not here: %v", err)
	}
}

func TestInScopeDefinitionRejectsExactArgvPath(t *testing.T) {
	for name, tc := range map[string]struct {
		dir  string
		argv []string
		file string
	}{
		"docs presence":   {".", []string{"test", "-f", "docs/atoms.md"}, "docs/atoms.md"},
		"dot slash":       {".", []string{"sh", "./docs/atoms.md"}, "docs/atoms.md"},
		"relative to dir": {"docs", []string{"cat", "atoms.md"}, "docs/atoms.md"},
		"parent of dir":   {"lab", []string{"cat", "../docs/atoms.md"}, "docs/atoms.md"},
	} {
		t.Run(name, func(t *testing.T) {
			a, evidence := inScopeAnalysis("docs/atoms.md")
			a.Checks[0].Dir, a.Checks[0].Argv = tc.dir, tc.argv
			err := ValidateInScopeDefinitions(a, evidence)
			if !IsPolicyViolation(err) || err.Error() != InScopeDefinitionMessage+": "+tc.file+" (regression)" {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestInScopeDefinitionIgnoresTokensThatAreNotExactInScopePaths(t *testing.T) {
	for name, argv := range map[string][]string{
		"flag":             {"go", "test", "-run=docs/atoms.md"},
		"absolute":         {"cat", "/docs/atoms.md"},
		"empty":            {"echo", ""},
		"substring":        {"sh", "-c", "test -f docs/atoms.md"},
		"parent directory": {"ls", "docs"},
		"other file":       {"cat", "docs/other.md"},
	} {
		t.Run(name, func(t *testing.T) {
			a, evidence := inScopeAnalysis("docs/atoms.md")
			a.Checks[0].Argv = argv
			if err := ValidateInScopeDefinitions(a, evidence); err != nil {
				t.Fatalf("argv %q is not an exact in-scope path: %v", argv, err)
			}
		})
	}
}

func TestInScopeDefinitionDoesNotRejectTestPathsNamedInArgv(t *testing.T) {
	a, evidence := inScopeAnalysis("lab/parser_test.go", "lab/testdata/case.json")
	a.Checks[0].Argv = []string{"go", "test", "lab/parser_test.go", "lab/testdata/case.json"}
	if err := ValidateInScopeDefinitions(a, evidence); err != nil {
		t.Fatalf("a conventional test path is only a test-source assessment: %v", err)
	}
	if o := (Plan{Analysis: a}).DefinitionOverlaps(); len(o) == 0 || o[0].Kind != "test-source" {
		t.Fatalf("the guard assesses the same paths as test sources: %+v", o)
	}
}

func TestInScopeDefinitionNamesEveryOffendingPathAndCheckOnce(t *testing.T) {
	a, evidence := inScopeAnalysis("lab/go.mod", "docs/atoms.md", "scripts/install.sh")
	second := a.Checks[0]
	second.ID = "docs-present"
	second.Definitions = []string{"doc", "manifest"}
	second.Argv = []string{"test", "-f", "docs/atoms.md", "scripts/install.sh", "scripts/install.sh"}
	third := a.Checks[0]
	third.ID = "clean"
	third.Definitions = []string{"issue"}
	third.Argv = []string{"true"}
	a.Checks = append(a.Checks, second, third)
	err := ValidateInScopeDefinitions(a, evidence)
	want := InScopeDefinitionMessage + ": lab/go.mod (regression, docs-present); docs/atoms.md (docs-present); scripts/install.sh (docs-present)"
	if !IsPolicyViolation(err) || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
}

func TestInScopeDefinitionDecodeAndRunTimeGuardAreUnchanged(t *testing.T) {
	a, evidence := inScopeAnalysis("lab/go.mod")
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = DecodeAnalysis(raw); err != nil {
		t.Fatalf("the decoder has no evidence context and is unchanged: %v", err)
	}
	p := fixture()
	p.Analysis = a
	p.Evidence = evidence
	if err = p.Validate(); err != nil {
		t.Fatalf("an overlapping plan stays digestible and reviewable: %v", err)
	}
	if err = p.ValidateForCoding(); err == nil || !strings.Contains(err.Error(), "frozen definition overlaps proposed scope") {
		t.Fatalf("the run-time guard must remain the backstop: %v", err)
	}
}
