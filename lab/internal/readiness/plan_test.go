package readiness

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func fixture() Plan {
	return Plan{SchemaVersion: "v1", MoleculeID: "mol-1", Revision: 1,
		Inputs: Inputs{Repository: "owner/repo", TargetBranch: "main", BaseCommit: strings.Repeat("a", 40), FormulaSHA256: strings.Repeat("b", 64), EvidenceSHA256: strings.Repeat("c", 64), BindingSHA256: strings.Repeat("d", 64),
			Issue: Issue{Repository: "owner/repo", Number: 1, URL: "https://github.com/owner/repo/issues/1", State: "OPEN", Title: "Fix parser", Body: "Reject trailing tokens", UpdatedAt: "today"}},
		Evidence: []Evidence{{ID: "manifest", Path: "lab/go.mod", SHA256: strings.Repeat("e", 64)}},
		Analysis: Analysis{Summary: "Reject trailing tokens in the parser", Scope: []ScopedPath{{Path: "lab/parser.go", Reason: "Parser implementation", Evidence: []string{"manifest"}}},
			Acceptance: []Criterion{{Description: "Trailing tokens return an error", Evidence: []string{"issue"}}},
			Checks:     []Check{{ID: "regression", Category: "regression", Argv: []string{"go", "test", "./..."}, Dir: "lab", TimeoutMS: 60000, Required: true, Definitions: []string{"manifest"}, Reason: "Parser regression suite", Evidence: []string{"manifest"}}},
			Impacts:    []Assessment{{Description: "Callers receive explicit errors", Evidence: []string{"issue"}}}}}
}
func TestPlanReviewAndDigest(t *testing.T) {
	p := fixture()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	digest, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	p.Inputs.Issue.UpdatedAt = "tomorrow"
	got, _ := p.Digest()
	if got != digest {
		t.Fatal("retrieval-only issue timestamp invalidated approval")
	}
	mutations := []struct {
		name   string
		mutate func(*Plan)
	}{
		{"issue", func(p *Plan) { p.Inputs.Issue.Body += " changed" }},
		{"formula", func(p *Plan) { p.Inputs.FormulaSHA256 = strings.Repeat("f", 64) }},
		{"scope", func(p *Plan) { p.Analysis.Scope[0].Path = "lab/other.go" }},
		{"command", func(p *Plan) { p.Analysis.Checks[0].Argv = append(p.Analysis.Checks[0].Argv, "-race") }},
		{"impact", func(p *Plan) { p.Analysis.Impacts[0].Description += " also public API" }},
		{"policy", func(p *Plan) {
			p.Inputs.MandatoryConstraints = []string{"github-ruleset-sha256:" + strings.Repeat("d", 64)}
		}},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			p := fixture()
			tt.mutate(&p)
			got, err := p.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if got == digest {
				t.Fatal("changed approval input retained digest")
			}
		})
	}
}
func TestRejectIncompleteOrUnsafePlan(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Plan)
	}{
		{"no acceptance", func(p *Plan) { p.Analysis.Acceptance = nil }},
		{"no checks", func(p *Plan) { p.Analysis.Checks = nil }},
		{"optional only", func(p *Plan) { p.Analysis.Checks[0].Required = false }},
		{"missing citation", func(p *Plan) { p.Analysis.Scope[0].Evidence = []string{"missing"} }},
		{"empty citation", func(p *Plan) { p.Analysis.Scope[0].Evidence = nil }},
		{"question", func(p *Plan) {
			p.Analysis.Questions = []Question{{Question: "Which parser?", Reason: "Two entrypoints", Evidence: []string{"issue"}}}
		}},
		{"closed", func(p *Plan) { p.Inputs.Issue.State = "CLOSED" }},
		{"wrong repository", func(p *Plan) { p.Inputs.Issue.Repository = "other/repo" }},
		{"wrong url", func(p *Plan) { p.Inputs.Issue.URL = "https://github.com/other/repo/issues/1" }},
		{"duplicate path", func(p *Plan) { p.Analysis.Scope = append(p.Analysis.Scope, p.Analysis.Scope[0]) }},
		{"no definitions", func(p *Plan) { p.Analysis.Checks[0].Definitions = nil }},
		{"invalid category", func(p *Plan) { p.Analysis.Checks[0].Category = "ticket-correct" }},
		{"independent provenance absent", func(p *Plan) { p.Analysis.Checks[0].Category = "independent-acceptance" }},
		{"shell string", func(p *Plan) { p.Analysis.Checks[0].Argv = []string{"go test ./..."} }},
		{"timeout", func(p *Plan) { p.Analysis.Checks[0].TimeoutMS = 0 }},
		{"first revision parent", func(p *Plan) { p.ParentDigest = strings.Repeat("a", 64) }},
		{"amendment no tree", func(p *Plan) { p.Revision = 2; p.ParentDigest = strings.Repeat("a", 64) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := fixture()
			tt.mutate(&p)
			if err := p.Validate(); err == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
}
func TestExactPaths(t *testing.T) {
	for _, s := range []string{"", ".", "..", "../a", "/a", "a/../b", "a//b", "./a", "a/", "a\\b", "*.go", "a/.git/config", ".git", "A/.GIT/config", "a\x00b", "a\nb", "-flag"} {
		if err := ValidatePath(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	for _, s := range []string{"lab/parser.go", "a file.go", ".github/workflows/ci.yml", "日本語.go"} {
		if err := ValidatePath(s); err != nil {
			t.Errorf("rejected %q: %v", s, err)
		}
	}
}
func TestStrictAnalysisDecode(t *testing.T) {
	raw, _ := json.Marshal(fixture().Analysis)
	if _, err := DecodeAnalysis(raw); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{string(raw) + "{}", strings.TrimSuffix(string(raw), "}") + `,"confidence":1}`, `null`, `{`, strings.Repeat(" ", MaxAnalysisBytes+1)} {
		if _, err := DecodeAnalysis([]byte(s)); err == nil {
			t.Fatal("invalid analysis accepted")
		}
	}
}
func TestQuestionsAreRetainedWithoutApproval(t *testing.T) {
	a := Analysis{Summary: "Ambiguous parser request", Questions: []Question{{Question: "Should whitespace be preserved?", Reason: "Issue does not specify output format", Evidence: []string{"issue"}}}}
	raw, _ := json.Marshal(a)
	got, err := DecodeAnalysis(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Questions) != 1 {
		t.Fatal("questions lost")
	}
	p := fixture()
	p.Analysis = got
	if _, err = p.Digest(); err == nil {
		t.Fatal("ambiguous plan has approval digest")
	}
}

func TestAnalysisCannotClaimIndependentReview(t *testing.T) {
	a := fixture().Analysis
	a.Checks[0].Category = "independent-acceptance"
	a.Checks[0].IndependentProvenance = "The model says reviewed"
	raw, _ := json.Marshal(a)
	if _, err := DecodeAnalysis(raw); err == nil {
		t.Fatal("model granted itself independent acceptance status")
	}
}
func TestDuplicateKeysAndConflictingCommandKinds(t *testing.T) {
	if _, err := DecodeAnalysis([]byte(`{"summary":"first","summary":"second"}`)); err == nil {
		t.Fatal("duplicate key accepted")
	}
	p := fixture()
	p.Analysis.Checks[0].Category = "setup"
	if err := p.Validate(); err == nil {
		t.Fatal("setup counted as verification")
	}
	p = fixture()
	p.Setup = []Check{p.Analysis.Checks[0]}
	p.Setup[0].ID = "prepare"
	if err := p.Validate(); err == nil {
		t.Fatal("verification command silently used as setup")
	}
}

func TestNoChangePermissionPreservesHistoricalDigest(t *testing.T) {
	p := fixture()
	d, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d != "3f482d83138d134f326471a971e6112ba863b1afbbc61948a38fb775eec9ec61" {
		t.Fatal("historical approval digest changed")
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "allowNoChange") {
		t.Fatal("default policy changed historical serialization")
	}
	p.AllowNoChange = true
	permitted, err := p.Digest()
	if err != nil || permitted == d {
		t.Fatal("no-change permission not bound to exact approval")
	}
}

func TestCodingAllowlistJoinsPlanDigest(t *testing.T) {
	p := fixture()
	absent, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "codingAllowlist") {
		t.Fatal("absent allowlist changed historical serialization")
	}
	digests := map[string]string{"absent": absent}
	for name, entries := range map[string][]string{
		"base":     {"Read", "Bash(git status)"},
		"extended": {"Read", "Bash(git status)", "Bash(go -C lab test ./...)"},
		"other":    {"Read", "Bash(git status)", "Bash(go -C lab vet ./...)"},
	} {
		p := fixture()
		p.CodingAllowlist = entries
		d, err := p.Digest()
		if err != nil {
			t.Fatal(name, err)
		}
		for seen, digest := range digests {
			if d == digest {
				t.Fatalf("%s allowlist retained the %s digest", name, seen)
			}
		}
		digests[name] = d
	}
}

func TestCodingAllowlistValidation(t *testing.T) {
	valid := [][]string{
		{"Read", "Glob", "Grep", "Edit", "Write", "Bash(git status)", "Bash(git diff)"},
		{"Bash(go -C lab test ./...)", "Bash(npm run test *)", "Bash(node --test *)", "Bash(go test ./internal/harness/ -run Allowlist -count=1)"},
	}
	for _, entries := range valid {
		p := fixture()
		p.CodingAllowlist = entries
		if err := p.Validate(); err != nil {
			t.Fatalf("%v: %v", entries, err)
		}
	}
	invalid := [][]string{
		{""}, {"Bash"}, {"Bash()"}, {"Bash(*)"}, {"Bash( go test)"}, {"Bash(go test)\n"}, {"WebFetch"}, {"Read(*)"},
		{"Bash(a,b)"}, {"Bash(a;b)"}, {"Bash(a|b)"}, {"Bash(a&b)"}, {"Bash($(x))"}, {"Bash(`x`)"}, {"Bash(a b > c)"}, {`Bash("x")`}, {"Bash(a\nb)"}, {"Bash({input:x})"},
		{"Read", "Read"},
		{"Bash(" + strings.Repeat("a", 600) + ")"},
		make([]string, 513),
	}
	for i := range invalid[len(invalid)-1] {
		invalid[len(invalid)-1][i] = fmt.Sprintf("Bash(cmd%d)", i)
	}
	for _, entries := range invalid {
		p := fixture()
		p.CodingAllowlist = entries
		if p.Validate() == nil {
			t.Fatalf("accepted %q", entries)
		}
	}
}

func reviewedRepairPlan() Plan {
	p := fixture()
	p.Analysis.Scope[0].Path = "lab/go.mod"
	input := FileEvidence{Evidence: Evidence{ID: "human", Path: "acceptance.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("true")))}, Content: "true"}
	p.Evidence = append(p.Evidence, input.Evidence)
	p.CheckInputs = []FileEvidence{input}
	p.Analysis.Checks[0].ReviewedAcceptance = "human-check"
	p.Analysis.Checks = append(p.Analysis.Checks, Check{ID: "human-check", Category: "independent-acceptance", IndependentProvenance: "human review fixture", Argv: []string{"/bin/sh", "{input:human}"}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"human"}, Evidence: []string{"human"}, Reason: "Separately reviewed acceptance"})
	return p
}
func TestDefinitionOverlapAssessmentAndReviewedRepair(t *testing.T) {
	p := fixture()
	p.Analysis.Scope[0].Path = "lab/go.mod"
	if err := p.Validate(); err != nil {
		t.Fatal("overlap conflated with structural validity", err)
	}
	overlaps := p.DefinitionOverlaps()
	if len(overlaps) != 1 || overlaps[0].Path != "lab/go.mod" || overlaps[0].Kind != "definition" || !reflect.DeepEqual(overlaps[0].CheckIDs, []string{"regression"}) {
		t.Fatalf("%+v", overlaps)
	}
	if err := p.ValidateForCoding(); err == nil || !strings.Contains(err.Error(), "lab/go.mod (regression)") {
		t.Fatal("missing exact pre-coding diagnostic", err)
	}
	p.Analysis.Scope[0].Path = "lab/parser_test.go"
	overlaps = p.DefinitionOverlaps()
	if len(overlaps) != 1 || overlaps[0].Kind != "test-source" || p.ValidateForCoding() != nil {
		t.Fatal("ordinary test edit blocked or not assessed", overlaps)
	}
	p.Analysis.Scope[0].Path = "lab/parser.go"
	if len(p.DefinitionOverlaps()) != 0 {
		t.Fatal("unaffected check reported")
	}
	p = reviewedRepairPlan()
	if err := p.ValidateForCoding(); err != nil {
		t.Fatal(err)
	}
	if p.Analysis.Checks[0].Category != "regression" || p.Evidence[0].SHA256 != strings.Repeat("e", 64) {
		t.Fatal("repair discarded protected definition")
	}
	for _, mutate := range []func(*Plan){
		func(p *Plan) { p.Analysis.Checks[1].Required = false },
		func(p *Plan) { p.Analysis.Checks[1].IndependentProvenance = "" },
		func(p *Plan) { p.Analysis.Checks[1].Category = "candidate" },
		func(p *Plan) { p.Analysis.Checks[1].Argv = []string{"/bin/true"} },
		func(p *Plan) { p.Analysis.Checks[1].Definitions = []string{"manifest"} },
		func(p *Plan) { p.Analysis.Checks[0].Category = "candidate" },
		func(p *Plan) { p.Analysis.Checks[0].ReviewedAcceptance = "missing" },
	} {
		p = reviewedRepairPlan()
		mutate(&p)
		if p.ValidateForCoding() == nil {
			t.Fatal("unreviewed repair accepted")
		}
	}
	a := reviewedRepairPlan().Analysis
	raw, _ := json.Marshal(a)
	if _, err := DecodeAnalysis(raw); err == nil {
		t.Fatal("model supplied its own repair")
	}
}
func TestOverlapNamesEveryCheckAndSetup(t *testing.T) {
	p := fixture()
	p.Analysis.Scope[0].Path = "lab/go.mod"
	other := p.Analysis.Checks[0]
	other.ID = "second"
	p.Analysis.Checks = append(p.Analysis.Checks, other)
	other.ID = "prepare"
	other.Category = "setup"
	p.Setup = []Check{other}
	got := p.DefinitionOverlaps()
	if len(got) != 1 || !reflect.DeepEqual(got[0].CheckIDs, []string{"regression", "second", "prepare"}) {
		t.Fatalf("%+v", got)
	}
	if err := p.ValidateForCoding(); err == nil || !strings.Contains(err.Error(), "prepare") {
		t.Fatal("overlapping setup not blocked", err)
	}
}

func TestFrozenTestDefinitionHasBothAssessments(t *testing.T) {
	p := fixture()
	p.Evidence[0].Path = "lab/parser_test.go"
	p.Analysis.Scope[0].Path = "lab/parser_test.go"
	got := p.DefinitionOverlaps()
	if len(got) != 2 || got[0].Kind != "definition" || got[1].Kind != "test-source" || p.Validate() != nil || p.ValidateForCoding() == nil {
		t.Fatal("frozen test definition conflated with ordinary test edit", got)
	}
}

func TestEvidenceRequestValidation(t *testing.T) {
	for _, raw := range []string{
		`{"summary":"Need content","evidenceRequests":[{"path":"../secret","reason":"Inspect","evidence":["issue"]}]}`,
		`{"summary":"Need content","evidenceRequests":[{"path":"docs/a.md","reason":"Inspect","evidence":["issue"],"extra":true}]}`,
		`{"summary":"Need content","evidenceRequests":[{"path":"docs/a.md","reason":"Inspect","evidence":["issue"]},{"path":"docs/a.md","reason":"Again","evidence":["issue"]}]}`,
	} {
		if _, err := DecodeAnalysis([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := DecodeAnalysis([]byte(`{"summary":"Historical analysis"}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEvidenceRequests([]EvidenceRequest{{Path: "docs/a.md", Reason: "Inspect", Evidence: []string{"file-999"}}}, []Evidence{}); err == nil {
		t.Fatal("invented provenance accepted")
	}
	p := fixture()
	p.Analysis.EvidenceRequests = []EvidenceRequest{{Path: "docs/a.md", Reason: "Inspect", Evidence: []string{"issue"}}}
	if p.Validate() == nil {
		t.Fatal("unresolved request approved")
	}
}

func TestPlanValidationFailureIsItsOwnPolicyClass(t *testing.T) {
	p := fixture()
	p.Analysis.Checks[0].Category = "vet"
	err := p.Validate()
	if err == nil || err.Error() != "unknown check category: vet" {
		t.Fatalf("validation: %v", err)
	}
	v := &PolicyViolation{Reason: PlanValidationMessage + ": " + err.Error()}
	if PlanValidationMessage != "the analysis produced a plan that fails validation" {
		t.Fatalf("message %q", PlanValidationMessage)
	}
	if got := PolicyClass(v); PolicyClassPlanValidation != "plan-validation" || got != PolicyClassPlanValidation {
		t.Fatalf("class %q, want plan-validation", got)
	}
	if got := PolicyClass(&PolicyViolation{Reason: "unknown check category: vet"}); got != PolicyClassOther {
		t.Fatalf("an unprefixed validator error is class %q, want other", got)
	}
}

func TestValidateConfigurationAppliesValidateRulesToSuppliedInputs(t *testing.T) {
	p := fixture()
	if err := p.ValidateConfiguration(); err != nil {
		t.Fatalf("a valid plan's configuration: %v", err)
	}
	// Analysis-owned fields are not configuration.
	p.Analysis.Summary, p.Analysis.Scope, p.Analysis.Acceptance = "", nil, nil
	if err := p.ValidateConfiguration(); err != nil {
		t.Fatalf("configuration must not depend on the analysis: %v", err)
	}
	for name, mutate := range map[string]func(*Plan){
		"category":   func(p *Plan) { p.Analysis.Checks[0].Category = "vet" },
		"constraint": func(p *Plan) { p.Inputs.MandatoryConstraints = []string{"unknown:x"} },
		"input": func(p *Plan) {
			p.CheckInputs = []FileEvidence{{Evidence: Evidence{ID: "input", Path: "input.txt", SHA256: strings.Repeat("0", 64)}, Content: "x"}}
		},
		"setup": func(p *Plan) { p.Setup = []Check{p.Analysis.Checks[0]} },
	} {
		q := fixture()
		mutate(&q)
		want := q.Validate()
		if want == nil {
			t.Fatalf("%s: fixture mutation is valid", name)
		}
		if got := q.ValidateConfiguration(); got == nil || got.Error() != want.Error() {
			t.Fatalf("%s: configuration error %v, want Validate's %v", name, got, want)
		}
	}
}

func exemptFixture() Plan {
	p := fixture()
	p.Analysis.Scope = append(p.Analysis.Scope, ScopedPath{Path: "lab/parser_test.go", Reason: "Parser tests", Evidence: []string{"manifest"}})
	return p
}

func TestProtectedExemptPathsJoinDigest(t *testing.T) {
	p := exemptFixture()
	absent, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "protectedExemptPaths") {
		t.Fatal("absent exemptions changed historical serialization")
	}
	p.ProtectedExemptPaths = []string{"lab/parser_test.go"}
	exempt, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if exempt == absent {
		t.Fatal("protected exemptions not bound to exact approval")
	}
}

func TestProtectedExemptPathsValidation(t *testing.T) {
	tooMany := make([]string, MaxProtectedExemptPaths+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("lab/p%d_test.go", i)
	}
	for name, tt := range map[string]struct {
		paths []string
		want  string
	}{
		"parent":       {[]string{"../lab/parser_test.go"}, `protected exempt path: unsafe repository path: "../lab/parser_test.go"`},
		"unclean":      {[]string{"lab/./parser_test.go"}, `protected exempt path: expected an exact normalized repository path: "lab/./parser_test.go"`},
		"absolute":     {[]string{"/lab/parser_test.go"}, `protected exempt path: expected an exact normalized repository path: "/lab/parser_test.go"`},
		"duplicate":    {[]string{"lab/parser_test.go", "lab/parser_test.go"}, `duplicate protected exempt path: "lab/parser_test.go"`},
		"out of scope": {[]string{"lab/other_test.go"}, `protected exempt path is outside the approved scope: "lab/other_test.go"`},
		"not a test":   {[]string{"lab/parser.go"}, `protected exempt path is not a conventional test path: "lab/parser.go"`},
		"too many":     {tooMany, fmt.Sprintf("at most %d protected exempt paths permitted", MaxProtectedExemptPaths)},
	} {
		p := exemptFixture()
		p.ProtectedExemptPaths = tt.paths
		if err := p.Validate(); err == nil || err.Error() != tt.want {
			t.Errorf("%s: got %v, want %q", name, err, tt.want)
		}
	}
	// A regression-check definition stays restored even when it is a conventional test path.
	defined := exemptFixture()
	defined.Evidence = append(defined.Evidence, Evidence{ID: "parser-test", Path: "lab/parser_test.go", SHA256: strings.Repeat("f", 64)})
	defined.Analysis.Checks[0].Definitions = append(defined.Analysis.Checks[0].Definitions, "parser-test")
	defined.ProtectedExemptPaths = []string{"lab/parser_test.go"}
	if err := defined.Validate(); err == nil || err.Error() != `protected exempt path defines a regression check: "lab/parser_test.go"` {
		t.Errorf("regression definition: got %v", err)
	}
	p := exemptFixture()
	p.ProtectedExemptPaths = []string{"lab/parser_test.go"}
	if err := p.Validate(); err != nil {
		t.Fatalf("in-scope test exemption refused: %v", err)
	}
}

func TestSliceBValidateEvidenceRequestsLineRanges(t *testing.T) {
	request := func(paths ...string) []EvidenceRequest {
		out := []EvidenceRequest{}
		for _, p := range paths {
			out = append(out, EvidenceRequest{Path: p, Reason: "Inspect", Evidence: []string{"issue"}})
		}
		return out
	}
	if err := ValidateEvidenceRequests(request("lab/internal/cli/cli.go#L100-200", "lab/internal/cli/cli.go#L7-7", "lab/internal/cli/cli.go", "docs/notes#todo.md"), nil); err != nil {
		t.Fatalf("valid range requests refused: %v", err)
	}
	base, start, end, ranged, err := ParseEvidenceRange("lab/internal/cli/cli.go#L100-200")
	if err != nil || !ranged || base != "lab/internal/cli/cli.go" || start != 100 || end != 200 {
		t.Fatalf("parse: %q %d %d %t %v", base, start, end, ranged, err)
	}
	if base, start, end, ranged, err := ParseEvidenceRange("a.go#L42"); err != nil || !ranged || base != "a.go" || start != 42 || end != 42 {
		t.Fatalf("single-line range: %q %d %d %t %v", base, start, end, ranged, err)
	}
	// Only an exact #L<n> or #L<start>-<end> suffix is a range; any other '#' is part
	// of an ordinary whole-file path.
	for _, whole := range []string{"docs/issue#12.md", "a.go#Lx-5", "a.go#L1-y", "a.go#100-200", "a.go#L1-2-3", "a.go#L", "a.go#", "a.go#outline"} {
		if base, _, _, ranged, err := ParseEvidenceRange(whole); err != nil || ranged || base != whole {
			t.Fatalf("%q must be a whole file: %q %t %v", whole, base, ranged, err)
		}
		if err := ValidateEvidenceRequests(request(whole), nil); err != nil {
			t.Fatalf("whole-file request %q refused: %v", whole, err)
		}
	}
	// Malformed range syntax is a retryable policy violation with its own class.
	for _, bad := range []string{"a.go#L0-5", "a.go#L5-4", "a.go#L0", "a.go#L05-6", "#L1-2", "a.go#L1234567890-1234567891"} {
		err := ValidateEvidenceRequests(request(bad), nil)
		var violation *PolicyViolation
		if !errors.As(err, &violation) || PolicyClass(violation) != PolicyClassEvidenceRange {
			t.Fatalf("%q: want an evidence-range policy violation, got %v", bad, err)
		}
	}
	for _, bad := range []string{"../a.go#L1-2", ".git/config#L1-2", "a/../b.go#L1-2"} {
		if err := ValidateEvidenceRequests(request(bad), nil); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	// Exact repeats stay plain errors (TestAnalysisCorrectiveRetryEvidenceRequestCapKeepsOtherRequestErrorsPlain).
	for _, pair := range [][2]string{{"a.go#L1-2", "A.go#L1-2"}, {"a.go", "A.GO"}} {
		if err := ValidateEvidenceRequests(request(pair[0], pair[1]), nil); err == nil || IsPolicyViolation(err) {
			t.Fatalf("%v: exact repeat must be a plain error, got %v", pair, err)
		}
	}
	for _, pair := range [][2]string{{"a.go#L5", "a.go#L5-5"}, {"a.go#L5-5", "A.go#L5"}} {
		err := ValidateEvidenceRequests(request(pair[0], pair[1]), nil)
		var violation *PolicyViolation
		if !errors.As(err, &violation) || PolicyClass(violation) != PolicyClassDuplicateRequest || !strings.Contains(violation.Reason, pair[1]) {
			t.Fatalf("%v: want a retryable duplicate-request violation, got %v", pair, err)
		}
	}
	if NormalizeEvidenceRequest("a.go#L5") != "a.go#L5-5" || NormalizeEvidenceRequest("docs/issue#12.md") != "docs/issue#12.md" {
		t.Fatal("request normalization")
	}
	if _, err := DecodeAnalysis([]byte(`{"summary":"Need content","evidenceRequests":[{"path":"a.go#L0-1","reason":"Inspect","evidence":["issue"]}]}`)); !IsPolicyViolation(err) {
		t.Fatalf("malformed range must decode to a retryable policy violation: %v", err)
	}
}

func TestSliceBCheckDefinitionsMustCiteWholeFileEvidence(t *testing.T) {
	for p, want := range map[string]bool{"a.go#L1-40": true, "a.go#L7": true, "a.go#outline": true, "a.go": false, "docs/issue#12.md": false, "a.go#Lx-5": false, "a.go#outline.md": false} {
		if IsExcerptPath(p) != want {
			t.Fatalf("IsExcerptPath(%q) = %t", p, !want)
		}
	}
	const want = WholeFileDefinitionMessage + ": regression cites excerpt (lab/go.mod#L1-40)"
	for _, excerpt := range []string{"lab/go.mod#L1-40", "lab/go.mod#outline"} {
		p := fixture()
		p.Evidence = append(p.Evidence, Evidence{ID: "excerpt", Path: excerpt, SHA256: strings.Repeat("f", 64)})
		p.Analysis.Checks[0].Definitions = []string{"manifest", "excerpt"}
		wantErr := strings.Replace(want, "lab/go.mod#L1-40", excerpt, 1)
		if err := p.Validate(); err == nil || err.Error() != wantErr {
			t.Fatalf("%s: plan validation %v, want %q", excerpt, err, wantErr)
		}
		err := ValidateWholeFileDefinitions(p.Analysis, p.Evidence)
		var violation *PolicyViolation
		if !errors.As(err, &violation) || violation.Reason != wantErr || PolicyClass(violation) != PolicyClassWholeFileDef {
			t.Fatalf("%s: intake rule %v", excerpt, err)
		}
		// Excerpt evidence remains citable as an ordinary reason.
		p.Analysis.Checks[0].Definitions = []string{"manifest"}
		p.Analysis.Checks[0].Evidence = []string{"manifest", "excerpt"}
		if err := p.Validate(); err != nil {
			t.Fatalf("%s: excerpt cited as evidence refused: %v", excerpt, err)
		}
		if err := ValidateWholeFileDefinitions(p.Analysis, p.Evidence); err != nil {
			t.Fatalf("%s: %v", excerpt, err)
		}
	}
}
