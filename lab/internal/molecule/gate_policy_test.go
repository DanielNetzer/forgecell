package molecule

import (
	"context"
	"encoding/json"
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
