package molecule

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const gofmtGoFile = "lab/internal/x/format.go"

// gofmtFixture approves the sample Formula with a required baseline check that
// has the exact shape CI enforces for gofmt, and adds the given Go files to the
// scope the fixture analysis proposes.
func gofmtFixture(t *testing.T, scopeAtom string, goFiles ...string) Options {
	t.Helper()
	o, repo := fixture(t)
	if err := os.MkdirAll(filepath.Join(repo, ".github", "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(baselineWorkflow)), []byte("name: CI\non: pull_request\njobs: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "-qm", "workflow")
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "formulas", "sample.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := json.Marshal(map[string]any{"id": "baseline-gofmt-lab", "argv": []string{"sh", "-c", `test -z "$(gofmt -l internal cmd)"`}, "dir": "lab", "timeoutMs": 5000, "workflow": baselineWorkflow, "reason": "CI enforces gofmt."})
	text := strings.Replace(string(raw), defaultScopeAtom, scopeAtom, 1)
	approveFixture(t, o.LabDir, text+"requiredChecks:\n  - "+string(entry)+"\n", "gofmt")
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(ctx, b, i, s)
		// The default fixture check reads code.txt, a scope file, which auto-when-ready
		// declines because a check names it. Read a file outside the scope instead.
		a.Checks[0].Argv = []string{"/bin/sh", "-c", "test -f package.json"}
		manifest := a.Scope[1].Evidence
		for _, p := range goFiles {
			a.Scope = append(a.Scope, readiness.ScopedPath{Path: p, Reason: "Implementation", Evidence: append([]string{}, manifest...)})
		}
		return a, err
	}
	return o
}

func hasEntry(entries []string, want string) bool {
	for _, entry := range entries {
		if entry == want {
			return true
		}
	}
	return false
}

func TestPlanCreationStoresFormatterEntriesForARequiredGofmtCheck(t *testing.T) {
	o := gofmtFixture(t, defaultScopeAtom, gofmtGoFile)
	r, plan := baselinePlan(t, o)
	want, err := harness.DeriveCodingAllowlist(r.FormulaSnapshot.YAML, plan.Analysis.Checks, plan.Analysis.Scope...)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.CodingAllowlist, want) {
		t.Fatalf("stored allowlist %q, want %q", plan.CodingAllowlist, want)
	}
	for _, entry := range []string{"Bash(gofmt -l lab/internal lab/cmd)", "Bash(gofmt -w " + gofmtGoFile + ")"} {
		if !hasEntry(plan.CodingAllowlist, entry) {
			t.Fatalf("missing %q in %q", entry, plan.CodingAllowlist)
		}
	}
	for _, entry := range plan.CodingAllowlist {
		if strings.HasPrefix(entry, "Bash(gofmt -w ") && entry != "Bash(gofmt -w "+gofmtGoFile+")" {
			t.Fatalf("a formatter entry outside the approved .go scope: %q", entry)
		}
		if strings.Contains(entry, "*") {
			t.Fatalf("pattern entry %q", entry)
		}
	}
	legacy, err := harness.DerivePreFormatterAllowlist(r.FormulaSnapshot.YAML, plan.Analysis.Checks)
	if err != nil || reflect.DeepEqual(legacy, plan.CodingAllowlist) {
		t.Fatalf("the stored list must differ from the pre-change derivation: %q %v", legacy, err)
	}
	digest, err := plan.Digest()
	if err != nil || digest != r.Readiness.Plans[0].Digest {
		t.Fatalf("the allowlist must join the plan digest: %v", err)
	}
}

func TestPlanCreationWithoutAGofmtCheckKeepsTheAllowlist(t *testing.T) {
	o, _ := fixture(t)
	r, plan := baselinePlan(t, o)
	want, err := harness.DerivePreFormatterAllowlist(r.FormulaSnapshot.YAML, plan.Analysis.Checks)
	if err != nil || !reflect.DeepEqual(plan.CodingAllowlist, want) {
		t.Fatalf("a plan without a gofmt check changed its allowlist: %q want %q %v", plan.CodingAllowlist, want, err)
	}
}

func TestApprovedCodingAllowlistAcceptsCurrentAndEarlierPlansOnly(t *testing.T) {
	o := gofmtFixture(t, defaultScopeAtom, gofmtGoFile)
	r, plan := baselinePlan(t, o)
	f := formula.Formula{YAML: r.FormulaSnapshot.YAML}
	legacy, err := harness.DerivePreFormatterAllowlist(f.YAML, plan.Analysis.Checks)
	if err != nil {
		t.Fatal(err)
	}
	current := append([]string{}, plan.CodingAllowlist...)
	for name, stored := range map[string][]string{"current": current, "earlier approval": legacy} {
		p := copyPlan(t, plan)
		p.CodingAllowlist = stored
		got, err := approvedCodingAllowlist(f, p)
		if err != nil || !reflect.DeepEqual(got, stored) {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	none := copyPlan(t, plan)
	none.CodingAllowlist = nil
	wantLegacy, err := harness.LegacyCodingAllowlist()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := approvedCodingAllowlist(f, none); err != nil || !reflect.DeepEqual(got, wantLegacy) {
		t.Fatalf("a plan with no stored list lost its legacy allowance: %q %v", got, err)
	}
	moreScope := copyPlan(t, plan)
	moreScope.Analysis.Scope = append(moreScope.Analysis.Scope, readiness.ScopedPath{Path: "lab/internal/x/other.go", Reason: "Implementation", Evidence: []string{"issue"}})
	if _, err = approvedCodingAllowlist(f, moreScope); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("a scope that grew without a new list was accepted: %v", err)
	}
	outOfScope := append(append([]string{}, current...), "Bash(gofmt -w lab/internal/x/other.go)")
	dropped := append([]string{}, current[:len(current)-1]...)
	wildcard := append(append([]string{}, current...), "Bash(gofmt -w *.go)")
	prefix := append(append([]string{}, current...), "Bash(gofmt -l:*)")
	for name, stored := range map[string][]string{"out of scope path": outOfScope, "dropped entry": dropped, "wildcard": wildcard, "prefix pattern": prefix} {
		p := copyPlan(t, plan)
		p.CodingAllowlist = stored
		if _, err = approvedCodingAllowlist(f, p); err == nil || !strings.Contains(err.Error(), "differs") {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, entry := range []string{"Bash(gofmt -w *.go)", "Bash(gofmt -l:*)"} {
		if _, err = harness.EffectiveCodingAllowlist([]string{entry}); err == nil {
			t.Fatalf("accepted %q", entry)
		}
	}
}

func TestAmendDerivesFormatterEntriesFromTheAmendedScope(t *testing.T) {
	o := gofmtFixture(t, defaultScopeAtom, gofmtGoFile)
	r, err := Run(context.Background(), o)
	if err != nil || r.Readiness == nil {
		t.Fatalf("intake stopped: %v %v", err, r.Notes)
	}
	next := copyPlan(t, r.Readiness.Plans[0].Plan)
	added := "lab/internal/x/amended.go"
	next.Analysis.Scope = append(next.Analysis.Scope, readiness.ScopedPath{Path: added, Reason: "Implementation", Evidence: next.Analysis.Scope[1].Evidence})
	next.CodingAllowlist = []string{"Read", "Bash(gofmt -w lab/evil.go)"}
	r, err = Amend(context.Background(), o.LabDir, r.ID, r.Readiness.Plans[0].Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	amended := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	want, err := harness.DeriveCodingAllowlist(r.FormulaSnapshot.YAML, amended.Analysis.Checks, amended.Analysis.Scope...)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(amended.CodingAllowlist, want) {
		t.Fatalf("amended allowlist %q, want derived %q", amended.CodingAllowlist, want)
	}
	for _, entry := range []string{"Bash(gofmt -w " + gofmtGoFile + ")", "Bash(gofmt -w " + added + ")"} {
		if !hasEntry(amended.CodingAllowlist, entry) {
			t.Fatalf("missing %q in %q", entry, amended.CodingAllowlist)
		}
	}
	if hasEntry(amended.CodingAllowlist, "Bash(gofmt -w lab/evil.go)") {
		t.Fatal("a submitted formatter entry survived the amendment")
	}
}

func TestGatePolicyAgreesWithApprovalOnFormatterAllowlists(t *testing.T) {
	o := gofmtFixture(t, policyScopeAtom, gofmtGoFile)
	run, err := Run(context.Background(), o)
	if err != nil || run.Readiness == nil {
		t.Fatalf("intake stopped: %v %v", err, run.Notes)
	}
	ready := loadRecord(t, o.LabDir, run.ID)
	if got := EvaluateGatePolicy(ready, formula.ScopeApproval); got.Outcome != PolicyApprove {
		t.Fatalf("a plan with the current formatter allowlist was not approved: %+v", got)
	}
	redigest := func(r *Record) {
		digest, err := r.Readiness.Plans[0].Plan.Digest()
		if err != nil {
			t.Fatal(err)
		}
		r.Readiness.Plans[0].Digest = digest
		r.Readiness.Events[0].Digest = digest
	}
	legacy, err := harness.DerivePreFormatterAllowlist(ready.FormulaSnapshot.YAML, ready.Readiness.Plans[0].Plan.Analysis.Checks)
	if err != nil {
		t.Fatal(err)
	}
	earlier := cloneRecord(t, ready)
	earlier.Readiness.Plans[0].Plan.CodingAllowlist = legacy
	redigest(&earlier)
	if got := EvaluateGatePolicy(earlier, formula.ScopeApproval); got.Outcome != PolicyApprove {
		t.Fatalf("a plan approved before formatter entries existed was declined: %+v", got)
	}
	current := ready.Readiness.Plans[0].Plan.CodingAllowlist
	outOfScope := append(append([]string{}, current...), "Bash(gofmt -w lab/internal/x/other.go)")
	dropped := append([]string{}, current[:len(current)-1]...)
	wildcard := append(append([]string{}, current...), "Bash(gofmt -w *.go)")
	for name, stored := range map[string][]string{"out of scope path": outOfScope, "dropped entry": dropped, "wildcard": wildcard} {
		r := cloneRecord(t, ready)
		r.Readiness.Plans[0].Plan.CodingAllowlist = stored
		redigest(&r)
		got := EvaluateGatePolicy(r, formula.ScopeApproval)
		if got.Outcome != PolicyDecline || got.Reason != "allowlist-mismatch" {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}
