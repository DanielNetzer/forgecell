package readiness

import (
	"crypto/sha256"
	"encoding/json"
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
