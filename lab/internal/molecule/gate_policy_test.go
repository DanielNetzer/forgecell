package molecule

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const (
	defaultScopeAtom  = "{id: scope, type: gate, purpose: scope}"
	defaultReviewAtom = "{id: gate, type: gate, purpose: review}"
	policyScopeAtom   = "{id: scope, type: gate, purpose: scope, mode: policy, policy: auto-when-ready}"
	policyReviewAtom  = "{id: gate, type: gate, purpose: review, mode: policy, policy: deliver-when-checks-pass}"
)

func loadRecord(t *testing.T, lab, id string) Record {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(lab, "ledgers", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Record
	if err = json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func cloneRecord(t *testing.T, r Record) Record {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var out Record
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// scopeReadyRecord approves a Formula with the given gate declarations, runs
// intake to the scope gate and returns the saved record.
func scopeReadyRecord(t *testing.T, scopeAtom, reviewAtom string) (Options, Record) {
	t.Helper()
	o, _ := fixture(t)
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), defaultScopeAtom, scopeAtom, 1)
	text = strings.Replace(text, defaultReviewAtom, reviewAtom, 1)
	approveFixture(t, o.LabDir, text, "policy")
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil || r.Status != "waiting" || r.CheckoutStatus != "available" {
		t.Fatalf("fixture did not reach the scope gate: %+v", r)
	}
	return o, loadRecord(t, o.LabDir, r.ID)
}

func oneQuestion() []readiness.Question {
	return []readiness.Question{{Question: "Which behaviour is wanted?", Reason: "The issue is ambiguous", Evidence: []string{"issue"}}}
}

func TestGatePolicyScopeApprovesReadyMolecule(t *testing.T) {
	_, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	got := EvaluateGatePolicy(r, formula.ScopeApproval)
	if got.Outcome != PolicyApprove {
		t.Fatalf("%+v", got)
	}
	if got.Policy != "auto-when-ready" || got.Gate != "scope" || got.PlanDigest != r.Readiness.Plans[0].Digest || got.FormulaSHA256 != r.FormulaSnapshot.SHA256 {
		t.Fatalf("%+v", got)
	}
	if got.ApprovedBy() != "policy:auto-when-ready@"+r.FormulaSnapshot.SHA256 || len(got.Evidence) == 0 {
		t.Fatalf("%+v", got)
	}
	if !reflect.DeepEqual(got, EvaluateGatePolicy(r, formula.ScopeApproval)) {
		t.Fatal("evaluation is not deterministic")
	}
}

func TestGatePolicyScopeDeclinesEachUnreadyCondition(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	if EvaluateGatePolicy(ready, formula.ScopeApproval).Outcome != PolicyApprove {
		t.Fatal("fixture is not ready")
	}
	refrozen, err := formula.Parse([]byte(ready.FormulaSnapshot.YAML + "repositoryContext: 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Record)
		reason string
	}{
		{"plan question", func(r *Record) { r.Readiness.Plans[0].Plan.Analysis.Questions = oneQuestion() }, "plan-has-questions"},
		{"ledger question", func(r *Record) { r.Analysis.Questions = oneQuestion() }, "analysis-has-questions"},
		{"missing analysis", func(r *Record) { r.Analysis = nil }, "analysis-missing"},
		{"empty scope", func(r *Record) { r.Readiness.Plans[0].Plan.Analysis.Scope = nil }, "scope-empty"},
		{"parent path", func(r *Record) { r.Readiness.Plans[0].Plan.Analysis.Scope[0].Path = "../x" }, "scope-path-invalid"},
		{"absolute path", func(r *Record) { r.Readiness.Plans[0].Plan.Analysis.Scope[0].Path = "/abs" }, "scope-path-invalid"},
		{"protected deltas", func(r *Record) {
			r.Readiness.Plans[0].Plan.ExpectedProtectedDeltas = []readiness.ProtectedDelta{{CheckID: "regression", Package: "example.test/pkg", Test: "TestX"}}
		}, "plan-has-protected-deltas"},
		{"artifacts", func(r *Record) {
			r.Readiness.Plans[0].Plan.Artifacts = []readiness.ArtifactRoot{{Path: "out", CommandID: "check"}}
		}, "plan-has-artifacts"},
		{"check inputs", func(r *Record) {
			r.Readiness.Plans[0].Plan.CheckInputs = []readiness.FileEvidence{{Evidence: readiness.Evidence{ID: "input", Path: "input.txt"}, Content: "x"}}
		}, "plan-has-check-inputs"},
		{"sensitive scope path", func(r *Record) {
			r.Readiness.Plans[0].Plan.Analysis.Scope[0].Path = ".github/workflows/lab.yml"
		}, "scope-path-sensitive"},
		{"scope path cited as a check definition", func(r *Record) {
			p := &r.Readiness.Plans[0].Plan
			for i := range p.Evidence {
				if p.Evidence[i].ID == p.Analysis.Checks[0].Definitions[0] {
					p.Evidence[i].Path = p.Analysis.Scope[0].Path
				}
			}
		}, "scope-path-cited-by-check"},
		{"scope path cited as a definition with a reviewed repair", func(r *Record) {
			p := &r.Readiness.Plans[0].Plan
			for i := range p.Evidence {
				if p.Evidence[i].ID == p.Analysis.Checks[0].Definitions[0] {
					p.Evidence[i].Path = p.Analysis.Scope[0].Path
				}
			}
			p.Analysis.Checks[0].ReviewedAcceptance = "acceptance"
		}, "scope-path-cited-by-check"},
		{"scope path cited as a check argument", func(r *Record) {
			p := &r.Readiness.Plans[0].Plan
			p.Analysis.Checks[0].Argv = []string{"/bin/sh", "./" + p.Analysis.Scope[1].Path}
		}, "scope-path-cited-by-check"},
		{"scope path cited by a setup command", func(r *Record) {
			p := &r.Readiness.Plans[0].Plan
			p.Setup = []readiness.Check{{ID: "prepare", Category: "setup", Argv: []string{"cat", p.Analysis.Scope[0].Path}}}
		}, "scope-path-cited-by-check"},
		{"missing allowlist", func(r *Record) { r.Readiness.Plans[0].Plan.CodingAllowlist = nil }, "allowlist-missing"},
		{"extra allowlist entry", func(r *Record) {
			p := &r.Readiness.Plans[0].Plan
			p.CodingAllowlist = append(p.CodingAllowlist, "Bash(go -C lab test ./...)")
		}, "allowlist-mismatch"},
		{"short allowlist", func(r *Record) {
			p := &r.Readiness.Plans[0].Plan
			p.CodingAllowlist = p.CodingAllowlist[:len(p.CodingAllowlist)-1]
		}, "allowlist-mismatch"},
		{"underivable allowlist", func(r *Record) {
			r.FormulaSnapshot = Snapshot{YAML: refrozen.YAML, SHA256: refrozen.SHA256}
			r.Readiness.Plans[0].Plan.Inputs.FormulaSHA256 = refrozen.SHA256
		}, "allowlist-underivable"},
		{"approved phase", func(r *Record) { r.Readiness.Phase = "approved" }, "not-scope-waiting"},
		{"dismissed phase", func(r *Record) { r.Readiness.Phase = "dismissed" }, "not-scope-waiting"},
		{"prior harness attempt", func(r *Record) { r.HarnessAttempts = []HarnessAttempt{{ID: "attempt-1"}} }, "not-first-attempt"},
		{"failed checkout", func(r *Record) { r.CheckoutStatus = "failed" }, "checkout-unavailable"},
		{"allocating checkout", func(r *Record) { r.CheckoutStatus = "allocating" }, "checkout-unavailable"},
		{"absent checkout", func(r *Record) { r.CheckoutStatus = "" }, "checkout-unavailable"},
		{"unapproved Formula", func(r *Record) { r.FormulaApproved = false }, "formula-not-approved"},
		{"snapshot hash mismatch", func(r *Record) { r.FormulaSnapshot.SHA256 = strings.Repeat("0", 64) }, "formula-snapshot-invalid"},
		{"snapshot bytes changed", func(r *Record) { r.FormulaSnapshot.YAML += "# edited\n" }, "formula-snapshot-invalid"},
		{"plan continuation", func(r *Record) { r.Readiness.Plans[0].Plan.Continuation = "checks-only" }, "plan-continuation"},
		{"no plan", func(r *Record) { r.Readiness = nil }, "no-readiness-plan"},
		{"stale digest", func(r *Record) { r.Readiness.Plans[0].Digest = strings.Repeat("0", 64) }, "plan-digest-mismatch"},
		{"edited plan", func(r *Record) { r.Readiness.Plans[0].Plan.Analysis.Summary = "A different plan" }, "plan-digest-mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := cloneRecord(t, ready)
			tc.mutate(&r)
			got := EvaluateGatePolicy(r, formula.ScopeApproval)
			if got.Outcome != PolicyDecline || got.Reason != tc.reason || got.Detail == "" || got.PlanDigest != "" || len(got.Evidence) != 0 {
				t.Fatalf("want decline for %s, got %+v", tc.reason, got)
			}
			if _, err := RecordScopePolicyApproval(r); err == nil {
				t.Fatal("a declined evaluation was recorded as an approval")
			}
		})
	}
}

func TestGatePolicyHumanGateNeverApproved(t *testing.T) {
	for _, tc := range []struct{ name, scope string }{
		{"no mode", defaultScopeAtom},
		{"mode human", "{id: scope, type: gate, purpose: scope, mode: human}"},
		{"unrecognised mode", "{id: scope, type: gate, purpose: scope, mode: auto}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, r := scopeReadyRecord(t, tc.scope, defaultReviewAtom)
			got := EvaluateGatePolicy(r, formula.ScopeApproval)
			if got.Outcome != PolicyHuman || got.Reason != "gate-is-human" || got.PlanDigest != "" {
				t.Fatalf("%+v", got)
			}
			out, err := RecordScopePolicyApproval(r)
			if err == nil || len(out.PolicyDecisions) != 0 {
				t.Fatal("a human gate was approved by policy")
			}
		})
	}
	t.Run("review policy is not evaluated", func(t *testing.T) {
		_, r := scopeReadyRecord(t, policyScopeAtom, policyReviewAtom)
		got := EvaluateGatePolicy(r, formula.ReviewApproval)
		if got.Outcome != PolicyHuman || got.Policy != "deliver-when-checks-pass" || got.Reason != "policy-not-evaluated" || got.PlanDigest != "" {
			t.Fatalf("%+v", got)
		}
		if EvaluateGatePolicy(r, formula.ScopeApproval).Outcome != PolicyApprove {
			t.Fatal("a declared review policy changed the scope decision")
		}
	})
	t.Run("declared review policy with human scope", func(t *testing.T) {
		_, r := scopeReadyRecord(t, defaultScopeAtom, policyReviewAtom)
		if got := EvaluateGatePolicy(r, formula.ScopeApproval); got.Outcome != PolicyHuman {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("non-gate role", func(t *testing.T) {
		_, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
		for _, role := range []formula.ExecutionRole{formula.IntakeRole, formula.Coding, formula.Verification, formula.Publication} {
			if got := EvaluateGatePolicy(r, role); got.Outcome != PolicyHuman || got.Reason != "not-a-gate" {
				t.Fatalf("%s: %+v", role, got)
			}
		}
	})
	t.Run("default review gate", func(t *testing.T) {
		_, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
		if got := EvaluateGatePolicy(r, formula.ReviewApproval); got.Outcome != PolicyHuman || got.Reason != "gate-is-human" {
			t.Fatalf("%+v", got)
		}
	})
}

func TestGatePolicyEvaluationHasNoSideEffects(t *testing.T) {
	o, _ := fixture(t)
	calls := 0
	analyze := o.Analyze
	o.Analyze = func(c context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		calls++
		return analyze(c, b, i, s)
	}
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, strings.Replace(string(raw), defaultScopeAtom, policyScopeAtom, 1), "policy")
	ran, err := Run(context.Background(), o)
	if err != nil || ran.Readiness == nil {
		t.Fatalf("%+v %v", ran, err)
	}
	r := loadRecord(t, o.LabDir, ran.ID)
	jsonPath := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
	mdPath := filepath.Join(o.LabDir, "ledgers", r.ID+".md")
	jsonBefore, _ := os.ReadFile(jsonPath)
	mdBefore, _ := os.ReadFile(mdPath)
	recordBefore, _ := json.Marshal(r)
	callsBefore := calls

	// Any git, gh or other helper found on PATH would leave a marker behind.
	bin, markers := t.TempDir(), t.TempDir()
	for _, name := range []string{"git", "gh", "curl", "sh"} {
		script := "#!/bin/sh\ntouch " + filepath.Join(markers, name) + "\n"
		if err = os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	for _, role := range []formula.ExecutionRole{formula.ScopeApproval, formula.ReviewApproval, formula.Coding} {
		EvaluateGatePolicy(r, role)
	}

	jsonAfter, _ := os.ReadFile(jsonPath)
	mdAfter, _ := os.ReadFile(mdPath)
	recordAfter, _ := json.Marshal(r)
	if string(jsonBefore) != string(jsonAfter) || string(mdBefore) != string(mdAfter) || string(recordBefore) != string(recordAfter) {
		t.Fatal("evaluation changed the ledger or the record")
	}
	if calls != callsBefore {
		t.Fatalf("analysis ran during evaluation: %d -> %d", callsBefore, calls)
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("the coding harness ran during evaluation")
	}
	if found, _ := filepath.Glob(filepath.Join(markers, "*")); len(found) != 0 {
		t.Fatalf("evaluation started external processes: %v", found)
	}
	if len(r.HarnessAttempts) != 0 || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("evaluation advanced the Molecule: %s", r.Readiness.Phase)
	}
}

func TestGatePolicyDecisionsAreIndependentPerMolecule(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	asking := cloneRecord(t, ready)
	asking.ID = "mol-12-asking"
	asking.Analysis.Questions = oneQuestion()
	for _, order := range [][]*Record{{&ready, &asking}, {&asking, &ready}} {
		outcomes := map[string]PolicyOutcome{}
		for _, r := range order {
			outcomes[r.ID] = EvaluateGatePolicy(*r, formula.ScopeApproval).Outcome
		}
		if outcomes[ready.ID] != PolicyApprove || outcomes[asking.ID] != PolicyDecline {
			t.Fatalf("order %s first: %v", order[0].ID, outcomes)
		}
	}

	// A real intake that asks a question yields no plan, and does not affect another Molecule.
	o, _ := fixture(t)
	analyze := o.Analyze
	o.Analyze = func(c context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(c, b, i, s)
		a.Questions = oneQuestion()
		return a, err
	}
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "formulas", "sample.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, strings.Replace(string(raw), defaultScopeAtom, policyScopeAtom, 1), "policy")
	blocked, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Status != "blocked" || EvaluateGatePolicy(blocked, formula.ScopeApproval).Outcome != PolicyDecline {
		t.Fatalf("%+v", blocked)
	}
	if EvaluateGatePolicy(ready, formula.ScopeApproval).Outcome != PolicyApprove {
		t.Fatal("another Molecule's question changed this decision")
	}
}

func pointerTarget(doc any, pointer string) bool {
	cur := doc
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[part]
			if !ok {
				return false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return false
			}
			cur = v[i]
		default:
			return false
		}
	}
	return true
}

func TestGatePolicyRecordsIdentityAndEvidence(t *testing.T) {
	_, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	mdBefore, _ := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".md"))
	if strings.Contains(string(mdBefore), "Policy decisions") {
		t.Fatal("a Markdown ledger without decisions changed")
	}
	out, err := RecordScopePolicyApproval(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.PolicyDecisions) != 0 || len(out.PolicyDecisions) != 1 {
		t.Fatalf("decisions: input %d, output %d", len(r.PolicyDecisions), len(out.PolicyDecisions))
	}
	d := out.PolicyDecisions[0]
	sha := r.FormulaSnapshot.SHA256
	if !regexp.MustCompile(`^policy:auto-when-ready@[a-f0-9]{64}$`).MatchString(d.ApprovedBy) || d.ApprovedBy != "policy:auto-when-ready@"+sha {
		t.Fatalf("approvedBy %q", d.ApprovedBy)
	}
	if d.Policy != "auto-when-ready" || d.Gate != "scope" || d.PlanDigest != r.Readiness.Plans[0].Digest || d.FormulaSHA256 != sha {
		t.Fatalf("%+v", d)
	}
	if _, err = time.Parse(time.RFC3339Nano, d.At); err != nil {
		t.Fatal(err)
	}
	if len(d.Evidence) == 0 {
		t.Fatal("no evidence recorded")
	}
	var fields map[string]any
	raw, _ := json.Marshal(d)
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"approvedBy", "at", "evidence", "formulaSha256", "gate", "planDigest", "policy"}
	if len(fields) != len(want) {
		t.Fatalf("decision fields %v, want %v", fields, want)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Fatalf("decision fields %v, want %v", fields, want)
		}
	}

	marshalled, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err = json.Unmarshal(marshalled, &doc); err != nil {
		t.Fatal(err)
	}
	for _, e := range d.Evidence {
		if !pointerTarget(doc, e.Pointer) || e.Observed == "" {
			t.Fatalf("evidence does not resolve in the record: %+v", e)
		}
	}

	if err = save(out); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ledger.Decode(saved)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := decoded.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var round Record
	if err = json.Unmarshal(encoded, &round); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round.PolicyDecisions, out.PolicyDecisions) {
		t.Fatalf("decision did not survive the ledger round trip: %+v", round.PolicyDecisions)
	}
	md, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Approved by: policy:auto-when-ready@" + sha, "policy-approved", d.PlanDigest, d.Evidence[0].Pointer} {
		if !strings.Contains(string(md), want) {
			t.Fatalf("Markdown ledger lacks %q", want)
		}
	}
	if _, err = RecordScopePolicyApproval(out); err == nil {
		t.Fatal("the same policy decision was recorded twice")
	}
}

func rescoped(t *testing.T, r Record, scopePath string) Record {
	t.Helper()
	out := cloneRecord(t, r)
	p := &out.Readiness.Plans[0]
	p.Plan.Analysis.Scope[0].Path = scopePath
	if digest, err := p.Plan.Digest(); err == nil {
		p.Digest = digest
	}
	return out
}

func TestGatePolicyDeclinesSensitiveScopePaths(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	for _, p := range []string{
		".github/workflows/lab.yml", ".GitHub/workflows/x.yml", ".gitlab-ci.yml",
		"lab/go.mod", "lab/go.sum", "lab/GO.MOD",
		"package.json", "web/package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml",
		"Makefile", "makefile", "lab/GNUmakefile",
		".claude/settings.json", ".forgecell/x", ".githooks/pre-commit", ".husky/pre-commit",
		"scripts/check-repo-hygiene.sh", "SCRIPTS/x.sh", "tools/scripts/x.sh",
	} {
		t.Run(p, func(t *testing.T) {
			r := rescoped(t, ready, p)
			got := EvaluateGatePolicy(r, formula.ScopeApproval)
			if got.Outcome != PolicyDecline || got.Reason != "scope-path-sensitive" || got.Detail == "" || got.PlanDigest != "" || len(got.Evidence) != 0 {
				t.Fatalf("want a sensitive-scope decline, got %+v", got)
			}
			if out, err := RecordScopePolicyApproval(r); err == nil || len(out.PolicyDecisions) != 0 {
				t.Fatal("a sensitive scope was recorded as an approval")
			}
		})
	}
	for _, p := range []string{"docs/atoms.md", "lab/internal/molecule/gate_policy.go", "docs/Makefile.md", "lab/internal/githubclient/x.go", "docs/go.mod.md", "script/x.sh"} {
		t.Run("ordinary "+p, func(t *testing.T) {
			if got := EvaluateGatePolicy(rescoped(t, ready, p), formula.ScopeApproval); got.Outcome != PolicyApprove {
				t.Fatalf("%+v", got)
			}
		})
	}
	t.Run("git directory stays an invalid path", func(t *testing.T) {
		got := EvaluateGatePolicy(rescoped(t, ready, ".git/hooks/pre-commit"), formula.ScopeApproval)
		if got.Outcome != PolicyDecline || got.Reason != "scope-path-invalid" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("any sensitive path among several", func(t *testing.T) {
		r := cloneRecord(t, ready)
		p := &r.Readiness.Plans[0].Plan
		p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "lab/go.mod", Reason: "Dependency", Evidence: []string{"issue"}})
		digest, err := p.Digest()
		if err != nil {
			t.Fatal(err)
		}
		r.Readiness.Plans[0].Digest = digest
		if got := EvaluateGatePolicy(r, formula.ScopeApproval); got.Reason != "scope-path-sensitive" {
			t.Fatalf("%+v", got)
		}
	})
}

func TestGatePolicySensitiveScopeListIsVersionedAndPinned(t *testing.T) {
	const hint = "the sensitive scope list changed: bump sensitiveScopeListVersion and have the list reviewed"
	if sensitiveScopeListVersion != "v1" {
		t.Fatal(hint)
	}
	if !reflect.DeepEqual(sensitiveScopeSegments, []string{".github", ".claude", ".forgecell", ".githooks", ".husky", "scripts"}) {
		t.Fatal(hint)
	}
	if !reflect.DeepEqual(sensitiveScopeBasenames, []string{
		".gitlab-ci.yml", "go.mod", "go.sum",
		"package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml",
		"Makefile", "GNUmakefile",
	}) {
		t.Fatal(hint)
	}

	_, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	hasVersion := func(evidence []PolicyEvidence) bool {
		for _, e := range evidence {
			if strings.Contains(e.Observed, "sensitive scope list v1") {
				return true
			}
		}
		return false
	}
	if got := EvaluateGatePolicy(r, formula.ScopeApproval); got.Outcome != PolicyApprove || !hasVersion(got.Evidence) {
		t.Fatalf("the evaluation does not record the sensitive scope list version: %+v", got)
	}
	out, err := RecordScopePolicyApproval(r)
	if err != nil {
		t.Fatal(err)
	}
	if !hasVersion(out.PolicyDecisions[0].Evidence) {
		t.Fatalf("the recorded decision lacks the sensitive scope list version: %+v", out.PolicyDecisions[0])
	}
}

func TestGatePolicyEvidenceResolvesWithoutOmittedSlices(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	for _, tc := range []struct {
		name  string
		shape func(*readiness.Plan)
	}{
		{"nil slices", func(p *readiness.Plan) {
			p.ExpectedProtectedDeltas, p.FormulaBaseline, p.Artifacts, p.CheckInputs = nil, nil, nil, nil
		}},
		{"empty slices", func(p *readiness.Plan) {
			p.ExpectedProtectedDeltas, p.FormulaBaseline = []readiness.ProtectedDelta{}, []string{}
			p.Artifacts, p.CheckInputs = []readiness.ArtifactRoot{}, []readiness.FileEvidence{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := cloneRecord(t, ready)
			p := &r.Readiness.Plans[0]
			tc.shape(&p.Plan)
			digest, err := p.Plan.Digest()
			if err != nil {
				t.Fatal(err)
			}
			p.Digest = digest
			out, err := RecordScopePolicyApproval(r)
			if err != nil {
				t.Fatal(err)
			}

			planJSON, err := json.Marshal(out.Readiness.Plans[0].Plan)
			if err != nil {
				t.Fatal(err)
			}
			var keys map[string]json.RawMessage
			if err = json.Unmarshal(planJSON, &keys); err != nil {
				t.Fatal(err)
			}
			for _, omitted := range []string{"expectedProtectedDeltas", "formulaBaseline"} {
				if _, present := keys[omitted]; present {
					t.Fatalf("the plan serialises %s, so the fixture no longer covers an omitted slice", omitted)
				}
			}
			resolvesIn := func(label string, doc any) {
				t.Helper()
				for _, e := range out.PolicyDecisions[0].Evidence {
					if strings.HasSuffix(e.Pointer, "/expectedProtectedDeltas") || strings.HasSuffix(e.Pointer, "/formulaBaseline") {
						t.Fatalf("%s: evidence points at an omitempty slice: %+v", label, e)
					}
					if !pointerTarget(doc, e.Pointer) || e.Observed == "" {
						t.Fatalf("%s: evidence does not resolve: %+v", label, e)
					}
				}
			}
			var doc any
			marshalled, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(marshalled, &doc); err != nil {
				t.Fatal(err)
			}
			resolvesIn("marshalled record", doc)

			if err = save(out); err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(filepath.Join(out.LabDir, "ledgers", out.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := ledger.Decode(saved)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := decoded.Encode()
			if err != nil {
				t.Fatal(err)
			}
			var roundTripped any
			if err = json.Unmarshal(encoded, &roundTripped); err != nil {
				t.Fatal(err)
			}
			resolvesIn("ledger round trip", roundTripped)
		})
	}
}

// approveScopeByPolicy stands in for the apply step a runner adds later: the
// evaluator judges only the stored record, so drift is caught by approving
// with freshly observed inputs before any decision is recorded.
func approveScopeByPolicy(r Record, current readiness.Inputs) (Record, error) {
	eval := EvaluateGatePolicy(r, formula.ScopeApproval)
	if eval.Outcome != PolicyApprove {
		return r, fmt.Errorf("scope policy did not approve: %s", eval.Reason)
	}
	state, err := r.Readiness.Approve(eval.PlanDigest, current, stamp())
	if err != nil {
		return r, err
	}
	out, err := RecordScopePolicyApproval(r)
	if err != nil {
		return r, err
	}
	out.Readiness = &state
	return out, nil
}

func TestGatePolicyApprovalRefusesInputDrift(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	planned := ready.Readiness.Plans[0].Plan.Inputs
	if EvaluateGatePolicy(ready, formula.ScopeApproval).Outcome != PolicyApprove {
		t.Fatal("fixture is not ready")
	}
	for _, tc := range []struct {
		name    string
		drift   func(*readiness.Inputs)
		refused bool
	}{
		{"base commit", func(in *readiness.Inputs) { in.BaseCommit = strings.Repeat("a", 40) }, true},
		{"origin repository", func(in *readiness.Inputs) { in.Repository = "acme/other" }, true},
		{"target branch", func(in *readiness.Inputs) { in.TargetBranch = "release" }, true},
		{"repository evidence", func(in *readiness.Inputs) { in.EvidenceSHA256 = strings.Repeat("0", 64) }, true},
		{"Formula hash", func(in *readiness.Inputs) { in.FormulaSHA256 = strings.Repeat("0", 64) }, true},
		{"binding hash", func(in *readiness.Inputs) { in.BindingSHA256 = strings.Repeat("0", 64) }, true},
		{"issue title", func(in *readiness.Inputs) { in.Issue.Title += " (edited)" }, true},
		{"issue body", func(in *readiness.Inputs) { in.Issue.Body += "\nMore" }, true},
		{"mandatory constraints", func(in *readiness.Inputs) {
			in.MandatoryConstraints = []string{"github-ruleset-sha256:" + strings.Repeat("0", 64)}
		}, true},
		{"issue update time only", func(in *readiness.Inputs) { in.Issue.UpdatedAt = "2099-01-01T00:00:00Z" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := cloneRecord(t, ready)
			current := planned
			tc.drift(&current)
			// The evaluator reads only the stored record, so it cannot see drift.
			if EvaluateGatePolicy(r, formula.ScopeApproval).Outcome != PolicyApprove {
				t.Fatal("the evaluator no longer approves the stored record")
			}
			events := len(r.Readiness.Events)
			out, err := approveScopeByPolicy(r, current)
			if tc.refused {
				if err == nil {
					t.Fatal("drifted inputs were approved")
				}
				if len(out.PolicyDecisions) != 0 || len(r.PolicyDecisions) != 0 || out.Readiness.Phase != "scope-waiting" || len(out.Readiness.Events) != events {
					t.Fatalf("a refused approval changed the record: %+v", out.Readiness)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(out.PolicyDecisions) != 1 || out.Readiness.Phase != "approved" || out.Readiness.Events[len(out.Readiness.Events)-1].Kind != "approved" {
				t.Fatalf("%+v", out.Readiness)
			}
		})
	}
}

func TestGatePolicyUnusualGateModeStaysHuman(t *testing.T) {
	for _, tc := range []struct{ name, mode string }{
		{"unrecognised string", "manual"},
		{"mapping", "{a: b}"},
		{"sequence", "[policy]"},
		{"number", "7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			atom := "{id: scope, type: gate, purpose: scope, mode: " + tc.mode + "}"
			_, r := scopeReadyRecord(t, atom, defaultReviewAtom)
			got := EvaluateGatePolicy(r, formula.ScopeApproval)
			if got.Outcome != PolicyHuman || got.Reason != "gate-is-human" || got.PlanDigest != "" {
				t.Fatalf("%+v", got)
			}
			out, err := RecordScopePolicyApproval(r)
			if err == nil || len(out.PolicyDecisions) != 0 {
				t.Fatal("a gate with an unusual mode was approved by policy")
			}
		})
	}
}
