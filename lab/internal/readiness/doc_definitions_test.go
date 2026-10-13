package readiness

import (
	"strings"
	"testing"
)

func docAnalysis(scope ...string) (Analysis, []Evidence) {
	p := fixture()
	p.Evidence = append(p.Evidence,
		Evidence{ID: "readme", Path: "lab/README.md", SHA256: strings.Repeat("a", 64)},
		Evidence{ID: "guide", Path: "docs/guide.rst", SHA256: strings.Repeat("b", 64)},
		Evidence{ID: "script", Path: "scripts/check.sh", SHA256: strings.Repeat("c", 64)})
	a := p.Analysis
	for _, s := range scope {
		a.Scope = append(a.Scope, ScopedPath{Path: s, Reason: "Edited", Evidence: []string{"manifest"}})
	}
	return a, p.Evidence
}

func TestDocumentationDefinitionsAreInformational(t *testing.T) {
	for _, file := range []string{"lab/README.md", "docs/guide.rst"} {
		a, evidence := docAnalysis(file)
		a.Checks[0].Definitions = []string{"manifest", map[string]string{"lab/README.md": "readme", "docs/guide.rst": "guide"}[file]}
		if err := ValidateInScopeDefinitionsForNewFiles(a, evidence, nil); err != nil {
			t.Fatalf("a documentation citation of %s beside a frozen anchor must not reject: %v", file, err)
		}
	}
}

func TestDocumentationDefinitionsAloneReject(t *testing.T) {
	a, evidence := docAnalysis("lab/README.md")
	a.Checks[0].Definitions = []string{"readme"}
	err := ValidateInScopeDefinitionsForNewFiles(a, evidence, nil)
	if err == nil || !IsPolicyViolation(err) || !strings.Contains(err.Error(), "lab/README.md") {
		t.Fatalf("a check defined only by an in-scope documentation file must reject: %v", err)
	}
	a.Checks[0].Definitions = []string{"readme", "script"}
	a.Scope = a.Scope[:len(a.Scope)-1]
	if err := ValidateInScopeDefinitionsForNewFiles(a, evidence, nil); err != nil {
		t.Fatalf("documentation outside scope is not an in-scope definition: %v", err)
	}
}

func TestDocumentationDefinitionsStrictForAutoApproval(t *testing.T) {
	a, evidence := docAnalysis("lab/README.md")
	a.Checks[0].Definitions = []string{"manifest", "readme"}
	if err := ValidateInScopeDefinitions(a, evidence); err == nil || !IsPolicyViolation(err) {
		t.Fatalf("the auto-approval validator must still decline an in-scope documentation definition: %v", err)
	}
}

func TestDocumentationDefinitionsStillRejectExecutableAndExactArgvPaths(t *testing.T) {
	a, evidence := docAnalysis("scripts/check.sh")
	a.Checks[0].Definitions = []string{"script"}
	if err := ValidateInScopeDefinitionsForNewFiles(a, evidence, nil); err == nil || !IsPolicyViolation(err) {
		t.Fatalf("an in-scope non-documentation definition must still reject: %v", err)
	}
	a, evidence = docAnalysis("lab/README.md")
	a.Checks[0].Argv = []string{"cat", "lab/README.md"}
	a.Checks[0].Dir = "."
	if err := ValidateInScopeDefinitionsForNewFiles(a, evidence, nil); err == nil || !strings.Contains(err.Error(), "lab/README.md") {
		t.Fatalf("an in-scope exact argv path must still reject: %v", err)
	}
}

func TestDocumentationDefinitionsOverlapAgrees(t *testing.T) {
	p := fixture()
	p.Evidence = append(p.Evidence, Evidence{ID: "readme", Path: "lab/README.md", SHA256: strings.Repeat("a", 64)})
	p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "lab/README.md", Reason: "Documented", Evidence: []string{"readme"}})
	p.Analysis.Checks[0].Definitions = []string{"manifest", "readme"}
	noted := false
	for _, o := range p.DefinitionOverlaps() {
		if o.Path == "lab/README.md" {
			noted = true
			if o.Kind != "documentation" {
				t.Fatalf("an anchored documentation citation is informational: %+v", o)
			}
		}
	}
	if !noted {
		t.Fatal("a cited in-scope documentation file must still produce an informational overlap note")
	}
	if err := p.ValidateForCoding(); err != nil {
		t.Fatalf("intake and approval must agree: %v", err)
	}
	p.Analysis.Checks[0].Definitions = []string{"readme"}
	if err := p.ValidateForCoding(); err == nil {
		t.Fatal("a check defined only by an in-scope documentation file must block coding")
	}
	p.Analysis.Checks[0].Definitions = []string{"manifest", "readme"}
	p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "lab/go.mod", Reason: "Edited", Evidence: []string{"manifest"}})
	if err := p.ValidateForCoding(); err == nil {
		t.Fatal("an in-scope non-documentation definition must still block coding")
	}
}

func TestDocumentationDefinitionsPathClassification(t *testing.T) {
	for file, want := range map[string]bool{
		"README.md": true, "lab/README.MD": true, "docs/a.rst": true, "x.adoc": true, "a.markdown": true, "a.mdx": true,
		"requirements.txt": false, "lab/go.mod": false, "scripts/x.sh": false, "README": false, "md": false, "a.md.sh": false,
	} {
		if got := IsDocumentationPath(file); got != want {
			t.Fatalf("IsDocumentationPath(%q) = %t, want %t", file, got, want)
		}
	}
}
