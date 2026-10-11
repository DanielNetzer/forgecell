package molecule

import (
	"context"
	"crypto/sha256"
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
		{"ledger unknowns", func(r *Record) { r.Analysis.Unknowns = []string{"Is the base branch stable?"} }, "analysis-has-unknowns"},
		{"plan unknowns", func(r *Record) {
			r.Readiness.Plans[0].Plan.Analysis.Unknowns = []string{"Is the base branch stable?"}
		}, "plan-has-unknowns"},
		{"setup command", func(r *Record) {
			r.Readiness.Plans[0].Plan.Setup = []readiness.Check{{ID: "setup", Category: "setup", Argv: []string{"/bin/sh", "-c", "true"}, Dir: ".", TimeoutMS: 1000}}
		}, "plan-has-setup"},
		{"mandatory constraint", func(r *Record) {
			r.Readiness.Plans[0].Plan.Inputs.MandatoryConstraints = []string{"signed commits"}
		}, "plan-has-mandatory-constraints"},
		{"allow no change", func(r *Record) { r.Readiness.Plans[0].Plan.AllowNoChange = true }, "plan-allows-no-change"},
		{"timeout above the Formula ceiling", func(r *Record) { r.Readiness.Plans[0].Plan.HarnessTimeoutMS = 3600001 }, "harness-timeout-invalid"},
		{"credential name", func(r *Record) {
			r.Readiness.Plans[0].Plan.Policy.CredentialNames = []string{"GITHUB_TOKEN"}
		}, "policy-has-credentials"},
		{"extra environment name", func(r *Record) {
			r.Readiness.Plans[0].Plan.Policy.Environment = []string{"PATH", "HOME"}
		}, "policy-environment-not-default"},
		{"empty environment", func(r *Record) { r.Readiness.Plans[0].Plan.Policy.Environment = nil }, "policy-environment-not-default"},
		{"other environment", func(r *Record) { r.Readiness.Plans[0].Plan.Policy.Environment = []string{"HOME"} }, "policy-environment-not-default"},
		{"setup network", func(r *Record) { r.Readiness.Plans[0].Plan.Policy.SetupNetwork = "none" }, "policy-setup-network-not-default"},
		{"check network", func(r *Record) { r.Readiness.Plans[0].Plan.Policy.CheckNetwork = "none" }, "policy-check-network-not-default"},
		{"containment", func(r *Record) { r.Readiness.Plans[0].Plan.Policy.Containment = "unfiltered" }, "policy-containment-not-default"},
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

func pointerValue(doc any, pointer string) (any, bool) {
	cur := doc
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[part]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

func pointerTarget(doc any, pointer string) bool {
	_, ok := pointerValue(doc, pointer)
	return ok
}

func policyLoaded(t *testing.T, o Options) formula.Loaded {
	t.Helper()
	loaded, err := formula.Load(o.LabDir, "")
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

// approveByPolicy runs the approval transaction with the scope policy as approver.
func approveByPolicy(t *testing.T, o Options, r Record) (Record, error) {
	t.Helper()
	o.Approve = r.Readiness.Plans[0].Digest
	return resumeByPolicy(context.Background(), o, policyLoaded(t, o), r.Issue.Number, r.Issue.Repo)
}

func ledgerFiles(t *testing.T, r Record) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	md, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), string(md)
}

// assertNothingApproved fails unless the ledger still waits at the scope gate with
// no decision, no coding attempt and no coding process.
func assertNothingApproved(t *testing.T, r Record) {
	t.Helper()
	saved := loadRecord(t, r.LabDir, r.ID)
	if saved.Readiness == nil || saved.Readiness.Phase != "scope-waiting" {
		t.Fatalf("the ledger left the scope gate: %+v", saved.Readiness)
	}
	for _, e := range saved.Readiness.Events {
		if e.Kind != "proposed" {
			t.Fatalf("unexpected readiness event %q", e.Kind)
		}
	}
	if len(saved.PolicyDecisions) != 0 || len(saved.HarnessAttempts) != 0 {
		t.Fatalf("decisions %d, harness attempts %d", len(saved.PolicyDecisions), len(saved.HarnessAttempts))
	}
	if _, err := os.Stat(filepath.Join(saved.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("the coding harness ran")
	}
}

func TestGatePolicyRecordsIdentityAndEvidence(t *testing.T) {
	o, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	jsonBefore, mdBefore := ledgerFiles(t, r)
	if strings.Contains(mdBefore, "Policy decisions") {
		t.Fatal("a Markdown ledger without decisions changed")
	}
	// The projection is in memory only: it must not reach the ledger.
	pending, err := RecordScopePolicyApproval(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.PolicyDecisions) != 0 || len(pending.PolicyDecisions) != 1 {
		t.Fatalf("decisions: input %d, output %d", len(r.PolicyDecisions), len(pending.PolicyDecisions))
	}
	if _, err = RecordScopePolicyApproval(pending); err == nil {
		t.Fatal("the same policy decision was projected twice")
	}
	if jsonAfter, mdAfter := ledgerFiles(t, r); jsonAfter != jsonBefore || mdAfter != mdBefore {
		t.Fatal("the in-memory projection changed the ledger")
	}

	if _, err = approveByPolicy(t, o, r); err != nil {
		t.Fatal(err)
	}
	out := loadRecord(t, r.LabDir, r.ID)
	if len(out.PolicyDecisions) != 1 {
		t.Fatalf("persisted decisions: %d", len(out.PolicyDecisions))
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

	saved, md := ledgerFiles(t, out)
	decoded, err := ledger.Decode([]byte(saved))
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
	for _, want := range []string{"Approved by: policy:auto-when-ready@" + sha, "policy-approved", d.PlanDigest, d.Evidence[0].Pointer} {
		if !strings.Contains(md, want) {
			t.Fatalf("Markdown ledger lacks %q", want)
		}
	}
	if _, err = RecordScopePolicyApproval(out); err == nil {
		t.Fatal("the same policy decision was recorded twice")
	}
}

func TestGatePolicyAcceptsExactLabDefaultPolicy(t *testing.T) {
	_, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	p := r.Readiness.Plans[0].Plan
	if !reflect.DeepEqual(p.Policy.Environment, []string{"PATH"}) || len(p.Policy.CredentialNames) != 0 || p.Policy.SetupNetwork != "unrestricted" || p.Policy.CheckNetwork != "unrestricted" || p.Policy.Containment != "filtered-environment" {
		t.Fatalf("Run no longer writes the Lab default policy the evaluator accepts: %+v", p.Policy)
	}
	if len(p.Setup) != 0 || len(p.Inputs.MandatoryConstraints) != 0 || len(p.Analysis.Unknowns) != 0 || len(r.Analysis.Unknowns) != 0 || p.AllowNoChange || p.HarnessTimeoutMS != 0 {
		t.Fatalf("the default plan is not a plain default plan: %+v", p)
	}
	if got := EvaluateGatePolicy(r, formula.ScopeApproval); got.Outcome != PolicyApprove {
		t.Fatalf("a default Lab plan was not approved: %+v", got)
	}
}

func TestGatePolicyEvidenceHoldsObservedValues(t *testing.T) {
	_, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	observe := func(r Record) map[string]string {
		t.Helper()
		got := EvaluateGatePolicy(r, formula.ScopeApproval)
		if got.Outcome != PolicyApprove {
			t.Fatalf("%+v", got)
		}
		seen := map[string]string{}
		for _, e := range got.Evidence {
			seen[e.Pointer] = e.Observed
			if strings.Contains(e.Observed, "all exact repository paths") || strings.Contains(e.Observed, "equal to the allowlist derived") {
				t.Fatalf("evidence holds constant text: %+v", e)
			}
		}
		return seen
	}
	const plan = "/readiness/plans/0/plan"
	plain := observe(r)
	list := r.Readiness.Plans[0].Plan.CodingAllowlist
	sum := sha256.Sum256([]byte(strings.Join(list, "\n")))
	for _, tc := range []struct{ pointer, want string }{
		{"/analysis/questions", "0 questions"},
		{"/analysis/unknowns", "0 unknowns"},
		{plan + "/analysis/questions", "0 questions"},
		{plan + "/analysis/unknowns", "0 unknowns"},
		{plan + "/analysis/scope", "2 paths: code.txt, request.json"},
		{plan + "/setup", "0 setup commands"},
		{plan + "/inputs/mandatoryConstraints", "0 mandatory constraints"},
		{plan + "/policy", "environment=[PATH] credentialNames=0 setupNetwork=unrestricted checkNetwork=unrestricted containment=filtered-environment"},
		{plan, "allowNoChange=false harnessTimeoutMs=0 ceilingMs=0 expectedProtectedDeltas=0"},
		{plan + "/codingAllowlist", fmt.Sprintf("%d entries, sha256 %x", len(list), sum)},
		{plan + "/artifacts", "0 artifact roots"},
		{plan + "/checkInputs", "0 check inputs"},
		{plan + "/analysis", "sensitive-path list v1: 2 scope paths examined"},
		{plan + "/analysis/checks", "1 checks examined for scope paths cited by a check definition"},
		{"/checkoutStatus", "available"},
		{"/readiness/phase", "scope-waiting"},
		{"/readiness/plans/0/digest", r.Readiness.Plans[0].Digest},
		{"/formulaSnapshot/sha256", r.FormulaSnapshot.SHA256},
	} {
		if plain[tc.pointer] != tc.want {
			t.Fatalf("evidence %s observed %q, want %q", tc.pointer, plain[tc.pointer], tc.want)
		}
	}

	// A different plan yields different observations, so the values are not constant.
	wider := cloneRecord(t, r)
	p := &wider.Readiness.Plans[0].Plan
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "docs/extra.md", Reason: "Documentation", Evidence: []string{"issue"}})
	digest, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	wider.Readiness.Plans[0].Digest = digest
	wider.Readiness.Events[0].Digest = digest
	widerSeen := observe(wider)
	if got := widerSeen[plan+"/analysis/scope"]; got != "3 paths: code.txt, request.json, docs/extra.md" {
		t.Fatalf("scope evidence observed %q", got)
	}
	if got := widerSeen[plan+"/analysis"]; got != "sensitive-path list v1: 3 scope paths examined" {
		t.Fatalf("sensitive-path evidence observed %q", got)
	}
}

func TestGatePolicyApprovalPersistsDecisionWithApprovedEvent(t *testing.T) {
	o, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	digest := r.Readiness.Plans[0].Digest
	assertNothingApproved(t, r)

	// The coding process cannot start before the approval is persisted, so the
	// ledger as it stands when preparation runs is the earliest observable state.
	var atStart Record
	prepared := false
	o.PrepareWorkspace = func(context.Context, Workspace) error {
		prepared = true
		raw, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
		if err != nil {
			t.Error(err)
			return err
		}
		return json.Unmarshal(raw, &atStart)
	}
	pre := EvaluateGatePolicy(r, formula.ScopeApproval)
	got, err := approveByPolicy(t, o, r)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared {
		t.Fatal("approval did not reach coding")
	}
	approved := false
	for _, e := range atStart.Readiness.Events {
		approved = approved || (e.Kind == "approved" && e.Digest == digest)
	}
	if !approved || len(atStart.PolicyDecisions) != 1 || atStart.PolicyDecisions[0].PlanDigest != digest {
		t.Fatalf("approval and decision were not persisted together: events %+v, decisions %+v", atStart.Readiness.Events, atStart.PolicyDecisions)
	}
	if !reflect.DeepEqual(atStart.PolicyDecisions[0].Evidence, pre.Evidence) {
		t.Fatal("the recorded evidence differs from the evaluation of the pre-approval record")
	}
	saved := loadRecord(t, o.LabDir, r.ID)
	if got.Status != "waiting" || len(got.PolicyDecisions) != 1 || len(saved.PolicyDecisions) != 1 || len(saved.HarnessAttempts) != 1 {
		t.Fatalf("status %s, decisions %d/%d, attempts %d", got.Status, len(got.PolicyDecisions), len(saved.PolicyDecisions), len(saved.HarnessAttempts))
	}
	if saved.PolicyDecisions[0].ApprovedBy != pre.ApprovedBy() {
		t.Fatalf("approvedBy %q", saved.PolicyDecisions[0].ApprovedBy)
	}
}

func TestGatePolicyApprovalDeclinesChangedIssue(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Issue)
	}{
		{"title", func(i *Issue) { i.Title = "A different title" }},
		{"body", func(i *Issue) { i.Body = "Write something else" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
			o.ReadIssue = func(context.Context, string, int64, string) (Issue, error) {
				i := Issue{Number: 12, Repo: "acme/widgets", Title: "Change code", Body: "Write changed", URL: "https://github.com/acme/widgets/issues/12", State: "OPEN"}
				tc.edit(&i)
				return i, nil
			}
			if EvaluateGatePolicy(r, formula.ScopeApproval).Outcome != PolicyApprove {
				t.Fatal("the recorded plan is ready, so only the live issue can decline")
			}
			jsonBefore, mdBefore := ledgerFiles(t, r)
			_, err := approveByPolicy(t, o, r)
			if err == nil || !strings.Contains(err.Error(), "readiness inputs changed") {
				t.Fatalf("a changed issue was not detected: %v", err)
			}
			assertNothingApproved(t, r)
			if jsonAfter, mdAfter := ledgerFiles(t, r); jsonAfter != jsonBefore || mdAfter != mdBefore {
				t.Fatal("a declined approval changed the ledger")
			}
		})
	}
}

func TestGatePolicyApprovalDeclinedByPolicyRecordsNothing(t *testing.T) {
	t.Run("plan not ready", func(t *testing.T) {
		o, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
		r.Analysis.Unknowns = []string{"Is the base branch stable?"}
		if err := save(r); err != nil {
			t.Fatal(err)
		}
		jsonBefore, mdBefore := ledgerFiles(t, r)
		_, err := approveByPolicy(t, o, r)
		if err == nil || !strings.Contains(err.Error(), "analysis-has-unknowns") {
			t.Fatalf("%v", err)
		}
		assertNothingApproved(t, r)
		if jsonAfter, mdAfter := ledgerFiles(t, r); jsonAfter != jsonBefore || mdAfter != mdBefore {
			t.Fatal("a declined approval changed the ledger")
		}
	})
	t.Run("human gate", func(t *testing.T) {
		o, r := scopeReadyRecord(t, defaultScopeAtom, defaultReviewAtom)
		_, err := approveByPolicy(t, o, r)
		if err == nil || !strings.Contains(err.Error(), "gate-is-human") {
			t.Fatalf("%v", err)
		}
		assertNothingApproved(t, r)
	})
}

func TestGatePolicyHumanApprovalRecordsNoPolicyDecision(t *testing.T) {
	o, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	if EvaluateGatePolicy(r, formula.ScopeApproval).Outcome != PolicyApprove {
		t.Fatal("the policy would approve this plan, but Run must not apply it")
	}
	assertNothingApproved(t, r)

	o.Approve = r.Readiness.Plans[0].Digest
	got, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	saved := loadRecord(t, o.LabDir, r.ID)
	if len(got.PolicyDecisions) != 0 || len(saved.PolicyDecisions) != 0 || len(saved.HarnessAttempts) != 1 {
		t.Fatalf("decisions %d/%d, attempts %d", len(got.PolicyDecisions), len(saved.PolicyDecisions), len(saved.HarnessAttempts))
	}
	for _, a := range saved.Atoms {
		if a.ID == "scope" && (a.Provenance == nil || len(a.Provenance.Actions) != 1 || a.Provenance.Actions[0].ResolvedSource != "readiness-approval") {
			t.Fatalf("the human approval was not recorded as one: %+v", a.Provenance)
		}
	}
	if _, md := ledgerFiles(t, saved); strings.Contains(md, "Policy decisions") {
		t.Fatal("a human approval rendered a policy decision")
	}
}

func rebindPlanDigest(t *testing.T, r *Record) {
	t.Helper()
	digest, err := r.Readiness.Plans[0].Plan.Digest()
	if err != nil {
		t.Fatal(err)
	}
	r.Readiness.Plans[0].Digest = digest
	r.Readiness.Events[0].Digest = digest
}

func assertPolicyDeclines(t *testing.T, r Record, reason string) PolicyEvaluation {
	t.Helper()
	got := EvaluateGatePolicy(r, formula.ScopeApproval)
	if got.Outcome != PolicyDecline || got.Reason != reason || got.Detail == "" || got.PlanDigest != "" || len(got.Evidence) != 0 {
		t.Fatalf("want decline for %s, got %+v", reason, got)
	}
	if _, err := RecordScopePolicyApproval(r); err == nil {
		t.Fatal("a declined evaluation was recorded as an approval")
	}
	return got
}

func assertPolicyApproves(t *testing.T, r Record) {
	t.Helper()
	if got := EvaluateGatePolicy(r, formula.ScopeApproval); got.Outcome != PolicyApprove {
		t.Fatalf("want approve, got %+v", got)
	}
}

func TestGatePolicyDeclinesArtifactAllowancesAndCheckInputs(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	assertPolicyApproves(t, ready)
	const content = "frozen human input"
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	input := readiness.FileEvidence{Evidence: readiness.Evidence{ID: "input-1", Path: "docs/input.txt", SHA256: sum}, Content: content}
	for _, tc := range []struct {
		name   string
		mutate func(*readiness.Plan)
		reason string
	}{
		{"artifact allowance", func(p *readiness.Plan) {
			p.Artifacts = []readiness.ArtifactRoot{{Path: "output", CommandID: p.Analysis.Checks[0].ID}}
		}, "plan-has-artifacts"},
		{"check input", func(p *readiness.Plan) {
			p.CheckInputs = []readiness.FileEvidence{input}
			p.Evidence = append(p.Evidence, input.Evidence)
		}, "plan-has-check-inputs"},
	} {
		// A stale digest must still show the specific reason, so these checks sit before the digest check.
		for _, rebind := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s rebound=%t", tc.name, rebind), func(t *testing.T) {
				r := cloneRecord(t, ready)
				tc.mutate(&r.Readiness.Plans[0].Plan)
				if rebind {
					rebindPlanDigest(t, &r)
				}
				assertPolicyDeclines(t, r, tc.reason)
			})
		}
	}
}

func TestGatePolicySensitivePathListIsPinned(t *testing.T) {
	if sensitivePathListVersion != "v1" {
		t.Fatalf("sensitive-path list version %q: a list change needs a reviewed version bump and test edit", sensitivePathListVersion)
	}
	if want := []string{".github/", ".claude/", ".forgecell/", ".githooks/", ".husky/", "scripts/"}; !reflect.DeepEqual(sensitivePathPrefixes, want) {
		t.Fatalf("sensitive prefixes changed: %q", sensitivePathPrefixes)
	}
	if want := []string{".gitlab-ci.yml", "go.mod", "go.sum", "package.json", "package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "bun.lockb", "Makefile"}; !reflect.DeepEqual(sensitiveBaseNames, want) {
		t.Fatalf("sensitive base names changed: %q", sensitiveBaseNames)
	}
	if want := []string{".lock"}; !reflect.DeepEqual(sensitiveExtensions, want) {
		t.Fatalf("sensitive extensions changed: %q", sensitiveExtensions)
	}
	for _, prefix := range sensitivePathPrefixes {
		if !strings.HasSuffix(prefix, "/") || prefix != strings.ToLower(prefix) {
			t.Fatalf("prefix %q must be lower case and directory-bounded", prefix)
		}
	}
}

func TestGatePolicyDeclinesSensitiveScopePaths(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	assertPolicyApproves(t, ready)
	scoped := func(t *testing.T, path string) Record {
		t.Helper()
		r := cloneRecord(t, ready)
		r.Readiness.Plans[0].Plan.Analysis.Scope[0].Path = path
		rebindPlanDigest(t, &r)
		return r
	}
	for _, path := range []string{
		".github/workflows/lab.yml", ".GitHub/workflows/x.yml", ".gitlab-ci.yml", "go.mod", "lab/go.mod", "lab/go.sum",
		"web/package.json", "yarn.lock", "Makefile", "lab/MAKEFILE", ".claude/settings.json", ".forgecell/x",
		".husky/pre-commit", ".githooks/pre-push", "scripts/check-repo-hygiene.sh", "web/package-lock.json",
		"pnpm-lock.yaml", "bun.lockb", "npm-shrinkwrap.json", "Cargo.LOCK",
	} {
		t.Run("declines "+path, func(t *testing.T) {
			got := assertPolicyDeclines(t, scoped(t, path), "scope-path-sensitive")
			if !strings.Contains(got.Detail, path) || !strings.Contains(got.Detail, sensitivePathListVersion) {
				t.Fatalf("detail does not name the path and list version: %q", got.Detail)
			}
		})
	}
	for _, path := range []string{
		"docs/atoms.md", "docs/github.md", "Makefile.md", ".githubx/foo", "scriptsx/run.sh", "lab/internal/molecule/gate_policy.go", "locks/readme.md",
	} {
		t.Run("approves "+path, func(t *testing.T) { assertPolicyApproves(t, scoped(t, path)) })
	}
	t.Run("names the sensitive path among others", func(t *testing.T) {
		r := cloneRecord(t, ready)
		r.Readiness.Plans[0].Plan.Analysis.Scope[1].Path = ".claude/settings.json"
		rebindPlanDigest(t, &r)
		if got := assertPolicyDeclines(t, r, "scope-path-sensitive"); !strings.Contains(got.Detail, ".claude/settings.json") || strings.Contains(got.Detail, "code.txt") {
			t.Fatalf("%q", got.Detail)
		}
	})
	t.Run("an invalid path keeps its own reason", func(t *testing.T) {
		r := cloneRecord(t, ready)
		r.Readiness.Plans[0].Plan.Analysis.Scope[0].Path = ".github/workflows/lab.yml"
		r.Readiness.Plans[0].Plan.Analysis.Scope[1].Path = "../x"
		assertPolicyDeclines(t, r, "scope-path-invalid")
	})
	t.Run("a stale digest still shows the sensitive reason", func(t *testing.T) {
		r := cloneRecord(t, ready)
		r.Readiness.Plans[0].Plan.Analysis.Scope[0].Path = "go.mod"
		assertPolicyDeclines(t, r, "scope-path-sensitive")
	})
}

func TestGatePolicyDeclinesScopeCitedByCheckDefinition(t *testing.T) {
	_, ready := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	assertPolicyApproves(t, ready)
	const defined = "tools/check.sh"
	// bind adds the file to the plan's evidence and lets mutate make a check cite it,
	// without putting the file in scope.
	bind := func(t *testing.T, mutate func(*readiness.Plan)) Record {
		t.Helper()
		r := cloneRecord(t, ready)
		p := &r.Readiness.Plans[0].Plan
		p.Evidence = append(p.Evidence, readiness.Evidence{ID: "file-90", Path: defined, SHA256: strings.Repeat("b", 64)})
		mutate(p)
		rebindPlanDigest(t, &r)
		return r
	}
	addToScope := func(r Record, file string) Record {
		p := &r.Readiness.Plans[0].Plan
		p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: file, Reason: "Edit the checker", Evidence: []string{"issue"}})
		return r
	}
	for _, tc := range []struct {
		name   string
		mutate func(*readiness.Plan)
	}{
		{"definition evidence", func(p *readiness.Plan) {
			p.Analysis.Checks[0].Definitions = append(p.Analysis.Checks[0].Definitions, "file-90")
		}},
		{"argv path", func(p *readiness.Plan) {
			p.Analysis.Checks[0].Argv = []string{"/bin/sh", defined}
		}},
		{"argv path relative to the check directory", func(p *readiness.Plan) {
			p.Analysis.Checks[0].Dir = "tools"
			p.Analysis.Checks[0].Argv = []string{"/bin/sh", "check.sh"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The same plan is ready until the cited file joins the scope, so the scope is the only cause.
			r := bind(t, tc.mutate)
			assertPolicyApproves(t, r)
			r = addToScope(r, defined)
			rebindPlanDigest(t, &r)
			got := assertPolicyDeclines(t, r, "scope-path-check-definition")
			if !strings.Contains(got.Detail, defined) {
				t.Fatalf("detail does not name the path: %q", got.Detail)
			}
			// A stale digest shows the specific reason, so the check sits before the digest check.
			stale := cloneRecord(t, r)
			stale.Readiness.Plans[0].Digest = strings.Repeat("0", 64)
			assertPolicyDeclines(t, stale, "scope-path-check-definition")
		})
	}
	t.Run("a reviewed repair does not excuse the overlap", func(t *testing.T) {
		r := bind(t, func(p *readiness.Plan) {
			p.Analysis.Checks[0].Definitions = append(p.Analysis.Checks[0].Definitions, "file-90")
		})
		r = addToScope(r, defined)
		r.Readiness.Plans[0].Plan.Analysis.Checks[0].ReviewedAcceptance = "accept-1"
		assertPolicyDeclines(t, r, "scope-path-check-definition")
	})
	t.Run("a test path named in argv is exempt", func(t *testing.T) {
		r := bind(t, func(p *readiness.Plan) {
			p.Analysis.Checks[0].Argv = []string{"/bin/sh", "tools/check_test.go"}
		})
		r = addToScope(r, "tools/check_test.go")
		rebindPlanDigest(t, &r)
		assertPolicyApproves(t, r)
	})
}

func TestGatePolicyPointerTargetRequiresPresentKey(t *testing.T) {
	var doc any
	if err := json.Unmarshal([]byte(`{"a":{"empty":[],"null":null,"list":[1]},"b":"text"}`), &doc); err != nil {
		t.Fatal(err)
	}
	for pointer, want := range map[string]bool{
		"/a": true, "/a/empty": true, "/a/null": true, "/a/list/0": true, "/b": true,
		"/a/missing": false, "/a/list/1": false, "/a/list/x": false, "/a/list/-1": false, "/b/deeper": false, "/missing": false,
	} {
		if got := pointerTarget(doc, pointer); got != want {
			t.Fatalf("pointerTarget(%q) = %t, want %t", pointer, got, want)
		}
	}
}

func TestGatePolicyRecordedPointersResolveInPersistedLedger(t *testing.T) {
	o, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	if _, err := approveByPolicy(t, o, r); err != nil {
		t.Fatal(err)
	}
	saved, _ := ledgerFiles(t, r)
	decoded, err := ledger.Decode([]byte(saved))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := decoded.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err = json.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}
	var round Record
	if err = json.Unmarshal(encoded, &round); err != nil {
		t.Fatal(err)
	}
	if len(round.PolicyDecisions) != 1 {
		t.Fatalf("decisions after the round trip: %d", len(round.PolicyDecisions))
	}
	recorded := map[string]bool{}
	for _, e := range round.PolicyDecisions[0].Evidence {
		recorded[e.Pointer] = true
		value, ok := pointerValue(doc, e.Pointer)
		if !ok {
			t.Fatalf("evidence pointer does not resolve in the persisted ledger: %s", e.Pointer)
		}
		if list, isList := value.([]any); isList && len(list) == 0 {
			t.Fatalf("evidence pointer targets an empty slice: %s", e.Pointer)
		}
	}
	// omitempty slices are absent from the JSON when empty, so none may be recorded then.
	const plan = "/readiness/plans/0/plan"
	p := round.Readiness.Plans[0].Plan
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"codingAllowlist", len(p.CodingAllowlist)},
		{"expectedProtectedDeltas", len(p.ExpectedProtectedDeltas)},
		{"formulaBaseline", len(p.FormulaBaseline)},
	} {
		pointer := plan + "/" + tc.name
		switch {
		case tc.n == 0 && (pointerTarget(doc, pointer) || recorded[pointer]):
			t.Fatalf("empty omitempty slice %s is present or recorded", pointer)
		case tc.n > 0 && !pointerTarget(doc, pointer):
			t.Fatalf("non-empty slice %s is absent from the persisted ledger", pointer)
		}
	}
	if len(p.CodingAllowlist) == 0 || !recorded[plan+"/codingAllowlist"] {
		t.Fatal("the derived allowlist is not recorded against a non-empty slice")
	}
}

func TestGatePolicyApprovalDeclinesChangedLiveInputs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		drift  func(t *testing.T, o *Options, r Record)
		reason string
	}{
		{"base commit", func(t *testing.T, o *Options, r Record) {
			testGit(t, o.SourceRoot, "commit", "--allow-empty", "-qm", "moved")
		}, "source base changed"},
		{"base ref", func(t *testing.T, o *Options, r Record) {
			testGit(t, o.SourceRoot, "commit", "--allow-empty", "-qm", "elsewhere")
			testGit(t, o.SourceRoot, "branch", "elsewhere")
			testGit(t, o.SourceRoot, "reset", "-q", "--hard", "HEAD~1")
			o.Base = "elsewhere"
		}, "source base changed"},
		{"workspace origin", func(t *testing.T, o *Options, r Record) {
			testGit(t, r.Workspace.Path, "remote", "set-url", "origin", "https://github.com/other/repository.git")
		}, "repository origin changed"},
		{"source origin", func(t *testing.T, o *Options, r Record) {
			testGit(t, o.SourceRoot, "remote", "set-url", "origin", "https://github.com/other/repository.git")
		}, "repository origin changed"},
		{"workspace branch", func(t *testing.T, o *Options, r Record) {
			testGit(t, r.Workspace.Path, "checkout", "-q", "-b", "other-branch")
		}, "workspace branch changed"},
		{"target branch", func(t *testing.T, o *Options, r Record) { o.TargetBranch = "release" }, "target branch changed"},
		{"frozen evidence", func(t *testing.T, o *Options, r Record) {
			// The stored plan froze different evidence than the repository now yields, with a consistent digest.
			rewriteStoredPlan(t, r, func(p *readiness.Plan) { p.Inputs.EvidenceSHA256 = strings.Repeat("0", 64) })
		}, "readiness inputs changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
			tc.drift(t, &o, r)
			if got := EvaluateGatePolicy(r, formula.ScopeApproval); got.Outcome != PolicyApprove {
				t.Fatalf("the recorded plan is ready, so only a live input can decline: %+v", got)
			}
			jsonBefore, mdBefore := ledgerFiles(t, r)
			_, err := approveByPolicy(t, o, r)
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("want %q, got %v", tc.reason, err)
			}
			assertNothingApproved(t, r)
			if jsonAfter, mdAfter := ledgerFiles(t, r); jsonAfter != jsonBefore || mdAfter != mdBefore {
				t.Fatal("a declined approval changed the ledger")
			}
		})
	}
}

func TestGatePolicyUnusualModeNeverApproves(t *testing.T) {
	for _, tc := range []struct{ name, mode string }{
		{"integer", "7"},
		{"boolean", "true"},
		{"null", "null"},
		{"tilde", "~"},
		{"empty string", `""`},
		{"sequence", "[policy]"},
		{"capitalised", "Policy"},
		{"upper case", "POLICY"},
		{"padded", `"policy "`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, r := scopeReadyRecord(t, "{id: scope, type: gate, purpose: scope, mode: "+tc.mode+"}", defaultReviewAtom)
			got := EvaluateGatePolicy(r, formula.ScopeApproval)
			if got.Outcome != PolicyHuman || got.Reason != "gate-is-human" || got.PlanDigest != "" || len(got.Evidence) != 0 {
				t.Fatalf("%+v", got)
			}
			out, err := RecordScopePolicyApproval(r)
			if err == nil || len(out.PolicyDecisions) != 0 {
				t.Fatal("a gate with an unusual mode was approved by policy")
			}
			if _, err = approveByPolicy(t, o, r); err == nil || !strings.Contains(err.Error(), "gate-is-human") {
				t.Fatalf("%v", err)
			}
			assertNothingApproved(t, r)
		})
	}
}

func TestGatePolicyDecisionIsStampedByItsRecorder(t *testing.T) {
	o, r := scopeReadyRecord(t, policyScopeAtom, defaultReviewAtom)
	built, err := scopePolicyDecision(r)
	if err != nil {
		t.Fatal(err)
	}
	if built.At != "" {
		t.Fatalf("the decision builder stamped %q; its recorder stamps it once", built.At)
	}
	projected, err := RecordScopePolicyApproval(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = time.Parse(time.RFC3339Nano, projected.PolicyDecisions[0].At); err != nil {
		t.Fatalf("the in-memory projection has no valid time: %v", err)
	}
	if _, err = approveByPolicy(t, o, r); err != nil {
		t.Fatal(err)
	}
	if _, err = time.Parse(time.RFC3339Nano, loadRecord(t, r.LabDir, r.ID).PolicyDecisions[0].At); err != nil {
		t.Fatalf("the persisted decision has no valid time: %v", err)
	}
}
