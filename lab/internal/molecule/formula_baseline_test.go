package molecule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const baselineWorkflow = ".github/workflows/ci.yml"

// baselineFixture adds a committed pull-request workflow and re-approves the sample
// Formula with one reviewed baseline check that the fixture analysis never proposes.
func baselineFixture(t *testing.T) Options { return baselineFixtureCommand(t, "test -f code.txt") }

func baselineFixtureCommand(t *testing.T, command string) Options {
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
	entry, _ := json.Marshal(map[string]any{"id": "baseline-code", "argv": []string{"/bin/sh", "-c", command}, "dir": ".", "timeoutMs": 5000, "workflow": baselineWorkflow, "reason": "CI checks that code.txt exists."})
	approveFixture(t, o.LabDir, string(raw)+"requiredChecks:\n  - "+string(entry)+"\n", "baseline")
	return o
}

func baselinePlan(t *testing.T, o Options) (Record, readiness.Plan) {
	t.Helper()
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness == nil {
		t.Fatalf("intake stopped: %+v", r.Notes)
	}
	return r, r.Readiness.Plans[0].Plan
}

func TestFormulaBaselineJoinsEveryPlanAndAppearsInVerification(t *testing.T) {
	t.Parallel()
	o := baselineFixture(t)
	r, plan := baselinePlan(t, o)
	var baseline *readiness.Check
	for i, c := range plan.Analysis.Checks {
		if c.ID == "baseline-code" {
			baseline = &plan.Analysis.Checks[i]
		}
	}
	if baseline == nil || baseline.Category != "regression" || !baseline.Required || baseline.Dir != "." || baseline.TimeoutMS != 5000 {
		t.Fatalf("baseline check missing or mislabelled: %+v", plan.Analysis.Checks)
	}
	cited := ""
	for _, e := range plan.Evidence {
		if e.Path == baselineWorkflow {
			cited = e.ID
		}
	}
	if cited == "" || len(baseline.Definitions) != 1 || baseline.Definitions[0] != cited || len(baseline.Evidence) != 1 || baseline.Evidence[0] != cited {
		t.Fatalf("baseline does not cite the frozen workflow evidence: %+v", baseline)
	}
	if len(plan.FormulaBaseline) != 1 || plan.FormulaBaseline[0] != "baseline-code" {
		t.Fatalf("formulaBaseline = %v", plan.FormulaBaseline)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "Formula baseline: 1 required regression check(s)") {
		t.Fatalf("baseline not surfaced at the scope gate: %v", r.Notes)
	}
	o.Approve = r.Readiness.Plans[0].Digest
	done, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if done.Verification == nil || !done.Verification.RequiredChecksPassed {
		t.Fatalf("%+v", done.Verification)
	}
	observed := false
	for _, c := range done.Verification.Checks {
		if c.ID == "baseline-code" && c.Category == "regression" && c.Result.OK {
			observed = true
		}
	}
	if !observed {
		t.Fatalf("baseline result missing from verification output: %+v", done.Verification.Checks)
	}
}

func TestFormulaBaselineFailureBlocksVerification(t *testing.T) {
	t.Parallel()
	o := baselineFixtureCommand(t, `test "$(cat code.txt)" = base`)
	r, _ := baselinePlan(t, o)
	o.Approve = r.Readiness.Plans[0].Digest
	done, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "blocked" || done.Verification == nil || done.Verification.RequiredChecksPassed {
		t.Fatalf("a failing baseline check did not block verification: %+v", done.Verification)
	}
	failed := false
	for _, c := range done.Verification.Checks {
		if c.ID == "baseline-code" && !c.Result.OK {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("failing baseline result missing from verification output: %+v", done.Verification.Checks)
	}
}

func TestFormulaBaselineIsNotRedefinedByTheAnalysis(t *testing.T) {
	t.Parallel()
	o := baselineFixture(t)
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(ctx, b, i, s)
		a.Checks[0].ID = "baseline-code"
		return a, err
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" || r.Readiness != nil || !strings.Contains(strings.Join(r.Notes, "\n"), "reserved by the Formula baseline") {
		t.Fatalf("a proposed check redefined the baseline: %+v", r)
	}
}

func TestFormulaBaselineWithoutRequiredChecksKeepsPlansUnchanged(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	r, plan := baselinePlan(t, o)
	raw, err := json.Marshal(plan)
	if err != nil || len(plan.FormulaBaseline) != 0 || strings.Contains(string(raw), "formulaBaseline") || strings.Contains(strings.Join(r.Notes, "\n"), "Formula baseline") {
		t.Fatalf("a Formula without requiredChecks changed its plan: %s %v", raw, err)
	}
}

func TestFormulaBaselineRequireRefusesOmittedAlteredOrUnlabelled(t *testing.T) {
	t.Parallel()
	o := baselineFixture(t)
	_, plan := baselinePlan(t, o)
	loaded, err := formula.Load(o.LabDir, "")
	if err != nil || !loaded.Approved {
		t.Fatalf("%+v %v", loaded, err)
	}
	f := loaded.Formula
	if err = requireFormulaBaseline(f, plan); err != nil {
		t.Fatalf("intake plan refused: %v", err)
	}
	withoutBaseline := func(p *readiness.Plan) {
		kept := []readiness.Check{}
		for _, c := range p.Analysis.Checks {
			if c.ID != "baseline-code" {
				kept = append(kept, c)
			}
		}
		p.Analysis.Checks = kept
	}
	alter := func(change func(*readiness.Check)) func(*readiness.Plan) {
		return func(p *readiness.Plan) {
			checks := append([]readiness.Check(nil), p.Analysis.Checks...)
			for i := range checks {
				if checks[i].ID == "baseline-code" {
					change(&checks[i])
				}
			}
			p.Analysis.Checks = checks
		}
	}
	tests := []struct {
		name   string
		mutate func(*readiness.Plan)
		want   string
	}{
		{"omitted", func(p *readiness.Plan) { withoutBaseline(p); p.FormulaBaseline = nil }, "omits Formula baseline check baseline-code"},
		{"argv altered", alter(func(c *readiness.Check) { c.Argv = []string{"true"} }), "alters Formula baseline check"},
		{"dir altered", alter(func(c *readiness.Check) { c.Dir = "lab" }), "alters Formula baseline check"},
		{"timeout altered", alter(func(c *readiness.Check) { c.TimeoutMS = 1 }), "alters Formula baseline check"},
		{"category altered", alter(func(c *readiness.Check) { c.Category = "candidate" }), "alters Formula baseline check"},
		{"not required", alter(func(c *readiness.Check) { c.Required = false }), "alters Formula baseline check"},
		{"unlabelled", func(p *readiness.Plan) { p.FormulaBaseline = nil }, "does not label Formula baseline check"},
		{"extra label", func(p *readiness.Plan) {
			p.FormulaBaseline = append(append([]string(nil), p.FormulaBaseline...), "check")
		}, "declares no such check"},
		{"workflow not frozen", func(p *readiness.Plan) {
			kept := []readiness.Evidence{}
			for _, e := range p.Evidence {
				if e.Path != baselineWorkflow {
					kept = append(kept, e)
				}
			}
			p.Evidence = kept
		}, "not in the frozen repository evidence"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := plan
			tt.mutate(&p)
			err := requireFormulaBaseline(f, p)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
	noBaseline := f
	noBaseline.RequiredChecks = nil
	if err = requireFormulaBaseline(noBaseline, plan); err == nil {
		t.Fatal("a plan labelling a check the Formula does not declare was accepted")
	}
}

// forgedEvaluationPlan is a correctly hashed evaluation-plan check input that
// declares select for the check go-test, as a hand-written amendment could supply.
func forgedEvaluationPlan() readiness.FileEvidence {
	content := `{"checks":[{"name":"go-test","dir":".","argv":["go","test","./..."],"select":"affected-packages"}]}`
	return readiness.FileEvidence{Evidence: readiness.Evidence{ID: evaluationPlanCheckInput, Path: "evaluation-plan.json", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content}
}

func TestHardening209AmendRefusesForgedEvaluationPlanInput(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	r, err := Run(context.Background(), o)
	if err != nil || r.Readiness == nil {
		t.Fatalf("intake: %v %+v", err, r.Notes)
	}
	parent := r.Readiness.Plans[0]
	forged := copyPlan(t, parent.Plan)
	input := forgedEvaluationPlan()
	forged.CheckInputs = append(forged.CheckInputs, input)
	forged.Evidence = append(forged.Evidence, input.Evidence)
	forged.Analysis.Checks = append(forged.Analysis.Checks, readiness.Check{ID: "go-test", Category: "regression", Argv: []string{"go", "test", "./..."}, Dir: ".", TimeoutMS: 5000, Required: true, Definitions: []string{evaluationPlanCheckInput}, Reason: "Selected tests", Evidence: []string{"issue"}, Select: readiness.SelectAffectedPackages})
	if !evaluationPlanDeclaresSelect(forged, forged.Analysis.Checks[len(forged.Analysis.Checks)-1]) {
		t.Fatal("the forgery must be one evaluationPlanDeclaresSelect would trust")
	}
	before := ledgerBytes(t, r)
	if _, err = Amend(context.Background(), o.LabDir, r.ID, parent.Digest, forged, ""); err == nil || !strings.Contains(err.Error(), "cannot add, change or remove the frozen evaluation-plan check input") {
		t.Fatalf("a forged evaluation-plan input was amended: %v", err)
	}
	if !bytes.Equal(before, ledgerBytes(t, r)) {
		t.Fatal("a refused amendment changed the ledger")
	}
	// An amendment that leaves the check inputs alone is still accepted.
	amended, err := Amend(context.Background(), o.LabDir, r.ID, parent.Digest, copyPlan(t, parent.Plan), "")
	if err != nil || len(amended.Readiness.Plans) != 2 {
		t.Fatalf("an unchanged amendment was refused: %v", err)
	}
}

func TestHardening209SelectingPlanFreezesCheckInputsAndEvidence(t *testing.T) {
	t.Parallel()
	input := forgedEvaluationPlan()
	selecting := readiness.Check{ID: "go-test", Select: readiness.SelectAffectedPackages, Definitions: []string{evaluationPlanCheckInput}}
	previous := readiness.Plan{CheckInputs: []readiness.FileEvidence{input}, Evidence: []readiness.Evidence{{ID: "file-1", Path: "a.go", SHA256: strings.Repeat("a", 64)}, input.Evidence}}
	previous.Analysis.Checks = []readiness.Check{selecting}
	if err := refuseSelectionInputChange(previous, previous); err != nil {
		t.Fatalf("unchanged inputs refused: %v", err)
	}
	moreEvidence := previous
	moreEvidence.Evidence = append(append([]readiness.Evidence(nil), previous.Evidence...), readiness.Evidence{ID: "file-2", Path: "b.go", SHA256: strings.Repeat("b", 64)})
	otherInput := previous
	otherInput.CheckInputs = append([]readiness.FileEvidence{input}, readiness.FileEvidence{Evidence: readiness.Evidence{ID: "extra", Path: "extra.json"}, Content: "{}"})
	for name, next := range map[string]readiness.Plan{"evidence": moreEvidence, "check input": otherInput} {
		if err := refuseSelectionInputChange(previous, next); err == nil || !strings.Contains(err.Error(), "must keep the check inputs and evidence unchanged") {
			t.Errorf("%s change after a selecting plan: %v", name, err)
		}
	}
	// A selection on a Formula baseline check is bound by the Formula, not the input.
	baselineOnly := previous
	baselineOnly.CheckInputs = nil
	baselineOnly.FormulaBaseline = []string{"go-test"}
	next := baselineOnly
	next.Evidence = moreEvidence.Evidence
	if err := refuseSelectionInputChange(baselineOnly, next); err != nil {
		t.Fatalf("a baseline selection must not freeze evidence: %v", err)
	}
	removed := previous
	removed.CheckInputs = nil
	if err := refuseSelectionInputChange(previous, removed); err == nil {
		t.Fatal("removing the evaluation-plan input was accepted")
	}
}

func TestFormulaBaselineApprovalRefusesAmendedPlanWithoutBaseline(t *testing.T) {
	t.Parallel()
	o := baselineFixture(t)
	r, plan := baselinePlan(t, o)
	old := r.Readiness.Plans[0]

	labelOnly := plan
	labelOnly.Analysis.Checks = nil
	for _, c := range plan.Analysis.Checks {
		if c.ID != "baseline-code" {
			labelOnly.Analysis.Checks = append(labelOnly.Analysis.Checks, c)
		}
	}
	if _, err := Amend(context.Background(), r.LabDir, r.ID, old.Digest, labelOnly, ""); err == nil {
		t.Fatal("a formulaBaseline label without its check was amended")
	}

	next := labelOnly
	next.FormulaBaseline = nil
	amended, err := Amend(context.Background(), r.LabDir, r.ID, old.Digest, next, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = amended.Readiness.Plans[1].Digest
	if _, err = Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "omits Formula baseline check baseline-code") {
		t.Fatalf("approval of a plan without the baseline: %v", err)
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("coding ran under a plan without the Formula baseline")
	}
}
