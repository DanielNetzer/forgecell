package readiness

import (
	"encoding/json"
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
