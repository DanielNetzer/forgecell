package readiness

import (
	"encoding/json"
	"strings"
	"testing"
)

const newPackageTest = "lab/internal/pinned/pinned_test.go"

// newPackagePlan proposes a new package: its test file is a new scope entry, and a
// candidate check is defined by exactly that file.
func newPackagePlan() Plan {
	p := fixture()
	p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: newPackageTest, Reason: "Tests of the new package", Evidence: []string{"manifest"}})
	p.Analysis.Checks = append(p.Analysis.Checks, Check{ID: "pinned", Category: "candidate", Argv: []string{"go", "test", "./internal/pinned/"}, Dir: "lab", TimeoutMS: 60000, Required: true, Definitions: []string{newPackageTest}, Reason: "Tests of the new package", Evidence: []string{"manifest"}})
	p.NewFiles = []string{newPackageTest}
	return p
}

func TestHardening129CheckMayBeDefinedByANewScopeFile(t *testing.T) {
	p := newPackagePlan()
	if err := p.Validate(); err != nil {
		t.Fatalf("a candidate check for a new package was rejected: %v", err)
	}
	if err := p.ValidateForCoding(); err != nil {
		t.Fatalf("a new-file definition is not an overlap with a frozen definition: %v", err)
	}
	if _, err := p.Digest(); err != nil {
		t.Fatal(err)
	}
	withoutNew := fixture()
	raw, err := json.Marshal(withoutNew)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "newFiles") {
		t.Fatalf("an empty newFiles must not change the encoding, so earlier digests stay: %s", raw)
	}
	// Only newFiles differs between these two valid plans.
	withNew := newPackagePlan()
	withNew.Analysis.Checks[1].Definitions = []string{"manifest"}
	unlisted := withNew
	unlisted.NewFiles = nil
	a, errA := withNew.Digest()
	b, errB := unlisted.Digest()
	if errA != nil || errB != nil || a == b {
		t.Fatalf("newFiles must join the plan digest: %v %v", errA, errB)
	}
}

func TestHardening129NewFileDefinitionIsRejectedUnlessLabDerived(t *testing.T) {
	for name, mutate := range map[string]func(*Plan){
		"not listed as new": func(p *Plan) { p.NewFiles = nil },
		"regression check":  func(p *Plan) { p.Analysis.Checks[1].Category = "regression" },
		"independent acceptance": func(p *Plan) {
			p.Analysis.Checks[1].Category, p.Analysis.Checks[1].IndependentProvenance = "independent-acceptance", "reviewed"
		},
		"cited twice":        func(p *Plan) { p.Analysis.Checks[1].Definitions = []string{newPackageTest, newPackageTest} },
		"outside the scope":  func(p *Plan) { p.NewFiles = []string{"lab/internal/other/other_test.go"} },
		"duplicate new file": func(p *Plan) { p.NewFiles = []string{newPackageTest, newPackageTest} },
		"frozen evidence path": func(p *Plan) {
			p.NewFiles = []string{newPackageTest, "lab/go.mod"}
			p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "lab/go.mod", Reason: "Edited", Evidence: []string{"manifest"}})
		},
		"named like an evidence id": func(p *Plan) {
			p.Evidence = append(p.Evidence, Evidence{ID: "notes", Path: "docs/x.md", SHA256: strings.Repeat("e", 64)})
			p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "notes", Reason: "Collides with an evidence ID", Evidence: []string{"manifest"}})
			p.NewFiles = []string{newPackageTest, "notes"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPackagePlan()
			mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("a new-file definition that the Lab did not derive was accepted")
			}
		})
	}
}

func TestHardening129IssueAndMissingDefinitionsStillReject(t *testing.T) {
	for name, definitions := range map[string][]string{
		"issue only":       {"issue"},
		"issue and new":    {"issue", newPackageTest},
		"unknown evidence": {"nope"},
		"an existing file": {"lab/parser.go"},
		"nothing":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			p := newPackagePlan()
			p.Analysis.Checks[1].Definitions = definitions
			if err := p.Validate(); err == nil {
				t.Fatal("a check defined by the issue or by an unknown reference was accepted")
			}
		})
	}
	p := newPackagePlan()
	p.Analysis.Checks[1].Definitions = []string{"issue"}
	if err := p.Validate(); err == nil || err.Error() != DefinitionReferenceMessage {
		t.Fatalf("the plan-level message is unchanged: %v", err)
	}
}

func TestHardening129ExistingInScopeFilesStillRejectButNewFilesDoNot(t *testing.T) {
	a, evidence := inScopeAnalysis("lab/go.mod", "lab/new_check.sh")
	a.Checks[0].Argv = []string{"sh", "lab/new_check.sh"}
	a.Checks[0].Definitions = []string{"manifest"}
	err := ValidateInScopeDefinitions(a, evidence)
	if !IsPolicyViolation(err) || !strings.Contains(err.Error(), "lab/new_check.sh (regression)") || !strings.Contains(err.Error(), "lab/go.mod (regression)") {
		t.Fatalf("the two-argument form must keep rejecting everything: %v", err)
	}
	err = ValidateInScopeDefinitionsForNewFiles(a, evidence, []string{"lab/new_check.sh"})
	if !IsPolicyViolation(err) || err.Error() != InScopeDefinitionMessage+": lab/go.mod (regression)" {
		t.Fatalf("only the existing in-scope file may still reject: %v", err)
	}
	if err = ValidateInScopeDefinitionsForNewFiles(a, evidence, []string{"lab/new_check.sh", "lab/go.mod"}); err != nil {
		t.Fatalf("a caller-supplied new file is exempt: %v", err)
	}
	if err = ValidateInScopeDefinitionsForNewFiles(a, evidence, nil); !IsPolicyViolation(err) {
		t.Fatalf("no new files means the old rule: %v", err)
	}
}

func TestHardening129NewScopeFilesComeFromTheFrozenTree(t *testing.T) {
	tree := []TreeFile{{Path: "lab/go.mod", Mode: "100644"}, {Path: "lab/parser.go", Mode: "100644"}}
	scope := []ScopedPath{{Path: "lab/parser.go"}, {Path: newPackageTest}, {Path: "lab/new.go"}, {Path: newPackageTest}}
	got := NewScopeFiles(scope, tree)
	if len(got) != 2 || got[0] != newPackageTest || got[1] != "lab/new.go" {
		t.Fatalf("new scope files = %v", got)
	}
	if got = NewScopeFiles(scope, nil); len(got) != 3 {
		t.Fatalf("with no tree every distinct path is new: %v", got)
	}
	checks := []Check{{ID: "a", Definitions: []string{"manifest", "lab/new.go"}}, {ID: "b", Definitions: []string{"issue"}}}
	if cited := CitedNewFiles(checks, []string{newPackageTest, "lab/new.go"}); len(cited) != 1 || cited[0] != "lab/new.go" {
		t.Fatalf("only a new file that a check cites belongs in the plan: %v", cited)
	}
}

func TestHardening129DefinitionReferenceViolationNamesTheAlternatives(t *testing.T) {
	a, _ := inScopeAnalysis()
	if err := ValidateDefinitionReferences(a); err != nil {
		t.Fatalf("a check defined by a frozen file is fine: %v", err)
	}
	a.Checks[0].Definitions = []string{"issue"}
	err := ValidateDefinitionReferences(a)
	if !IsPolicyViolation(err) || !strings.HasPrefix(err.Error(), DefinitionReferenceMessage) || !strings.Contains(err.Error(), "regression") || !strings.Contains(err.Error(), "exact scope path") {
		t.Fatalf("the retryable violation must name the check and both alternatives: %v", err)
	}
}

func TestHardening129PolicyClasses(t *testing.T) {
	_, labelling := DecodeAnalysis([]byte(`{"summary":"s","scope":[],"acceptance":[],"checks":[{"id":"c","category":"independent-acceptance","argv":["true"],"dir":".","timeoutMs":1,"required":true,"definitions":["x"],"reason":"r","evidence":["issue"],"independentProvenance":""}],"impacts":[],"unknowns":[],"questions":[]}`))
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"labelling":     {labelling, PolicyClassLabelling},
		"cap":           {ValidateEvidenceRequestCap(make([]EvidenceRequest, MaxEvidenceRequests+1)), PolicyClassEvidenceRequestCap},
		"in-scope":      {&PolicyViolation{Reason: InScopeDefinitionMessage + ": a (b)"}, PolicyClassInScopeDefinition},
		"definition":    {&PolicyViolation{Reason: DefinitionReferenceMessage + " (c cites the issue)"}, PolicyClassDefinitionRef},
		"anything else": {&PolicyViolation{Reason: "something new"}, PolicyClassOther},
	} {
		var v *PolicyViolation
		if tc.err == nil || !asViolation(tc.err, &v) {
			t.Fatalf("%s: not a policy violation: %v", name, tc.err)
		}
		if got := PolicyClass(v); got != tc.want {
			t.Fatalf("%s: class %q, want %q", name, got, tc.want)
		}
	}
	if PolicyClass(nil) != PolicyClassOther {
		t.Fatal("a missing violation is the other class")
	}
}

func asViolation(err error, out **PolicyViolation) bool {
	v, ok := err.(*PolicyViolation)
	*out = v
	return ok
}
