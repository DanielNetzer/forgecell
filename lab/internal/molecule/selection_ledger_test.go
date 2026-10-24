package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

func selectionVerification() verification.Result {
	affected := &verification.Selection{Mode: readiness.SelectAffectedPackages, Selected: 3, Total: 41, Packages: []string{"example.test/m/a", "example.test/m/b", "example.test/m/c"}}
	v := verification.Result{SourceTree: strings.Repeat("a", 40), RequiredChecksPassed: true, Checks: []verification.Observation{
		{ID: "go-test", Selection: affected},
		{ID: "gofmt"},
		{ID: "go-test-docs", Selection: &verification.Selection{Mode: verification.SelectionFullSuite, Reason: "non-Go file changed: \"docs/atoms.md\"\n\x1b[31mforged line"}},
	}}
	v.Protected = &verification.Result{Checks: []verification.Observation{{ID: "go-test", Selection: affected}, {ID: "gofmt"}}}
	return v
}

func TestSelectionLinesPerView(t *testing.T) {
	got := selectionLines(selectionVerification())
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines:\n%s", len(lines), got)
	}
	if lines[0] != "Candidate `go-test` selection: affected-packages (3 of 41 packages)" || lines[2] != "Protected regression `go-test` selection: affected-packages (3 of 41 packages)" {
		t.Fatalf("got\n%s", got)
	}
	if !strings.HasPrefix(lines[1], "Candidate `go-test-docs` selection: full suite (reason: non-Go file changed: \"docs/atoms.md\"") || strings.ContainsAny(lines[1], "\x1b\r") {
		t.Fatalf("the full-suite line is not one bounded, control-free line: %q", lines[1])
	}
	if got := selectionLines(verification.Result{Checks: []verification.Observation{{ID: "old"}}}); got != "" {
		t.Fatalf("a check without a selection rendered %q", got)
	}
	long := verification.Result{Checks: []verification.Observation{{ID: "go-test", Selection: &verification.Selection{Mode: verification.SelectionFullSuite, Reason: strings.Repeat("x", 10000)}}}}
	if got := selectionLines(long); len(got) > 1000 {
		t.Fatalf("an oversized reason rendered %d bytes", len(got))
	}
}

func TestSelectionFormulaBaselineCarriesSelectAndTriggers(t *testing.T) {
	check := formula.RequiredCheck{ID: "baseline-go-test-lab", Argv: []string{"go", "test", "./..."}, Dir: "lab", TimeoutMS: 1000, Workflow: ".github/workflows/lab.yml", Reason: "CI runs go test."}
	evidence := []readiness.Evidence{{ID: "workflow", Path: ".github/workflows/lab.yml", SHA256: strings.Repeat("e", 64)}}
	plain := readiness.Plan{Evidence: evidence}
	if err := applyFormulaBaseline(formula.Formula{RequiredChecks: []formula.RequiredCheck{check}}, &plain); err != nil {
		t.Fatal(err)
	}
	if plain.FullSuiteTriggers != nil || plain.Analysis.Checks[0].Select != "" {
		t.Fatalf("a Formula without select changed the plan: %+v", plain)
	}
	check.Select = readiness.SelectAffectedPackages
	selected := readiness.Plan{Evidence: evidence}
	if err := applyFormulaBaseline(formula.Formula{RequiredChecks: []formula.RequiredCheck{check}}, &selected); err != nil {
		t.Fatal(err)
	}
	if selected.Analysis.Checks[0].Select != readiness.SelectAffectedPackages || !reflect.DeepEqual(selected.FullSuiteTriggers, []string{"lab/cmd/**"}) {
		t.Fatalf("%+v %v", selected.Analysis.Checks[0], selected.FullSuiteTriggers)
	}
	declared := readiness.Plan{Evidence: evidence}
	if err := applyFormulaBaseline(formula.Formula{RequiredChecks: []formula.RequiredCheck{check}, FullSuiteTriggers: []string{"lab/internal/cli/**"}}, &declared); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(declared.FullSuiteTriggers, []string{"lab/internal/cli/**"}) {
		t.Fatalf("%v", declared.FullSuiteTriggers)
	}
}

func TestSelectionRequiresFormulaOrEvaluationPlanDeclaration(t *testing.T) {
	check := formula.RequiredCheck{ID: "baseline-go-test-lab", Argv: []string{"go", "test", "./..."}, Dir: "lab", TimeoutMS: 1000, Workflow: ".github/workflows/lab.yml", Reason: "CI runs go test.", Select: readiness.SelectAffectedPackages}
	f := formula.Formula{RequiredChecks: []formula.RequiredCheck{check}}
	evidence := []readiness.Evidence{{ID: "workflow", Path: ".github/workflows/lab.yml", SHA256: strings.Repeat("e", 64)}}
	plan := readiness.Plan{Evidence: evidence}
	if err := applyFormulaBaseline(f, &plan); err != nil {
		t.Fatal(err)
	}
	if err := requireFormulaBaseline(f, plan); err != nil {
		t.Fatalf("the Formula's own selection was refused: %v", err)
	}
	evaluationPlan := `{"checks":[{"name":"go-test","dir":"lab","argv":["go","test","./..."],"select":"affected-packages"}]}`
	evaluationCheck := readiness.Check{ID: "go-test", Category: "regression", Dir: "lab", Argv: []string{"go", "test", "./..."}, Definitions: []string{"evaluation-plan", "acceptance-input"}, Select: readiness.SelectAffectedPackages}
	withEvaluation := func(content string, c readiness.Check) readiness.Plan {
		p := readiness.Plan{Evidence: evidence, CheckInputs: []readiness.FileEvidence{{Evidence: readiness.Evidence{ID: "evaluation-plan", Path: "evaluation-plan.json"}, Content: content}}}
		p.Analysis.Checks = []readiness.Check{c}
		applyFullSuiteTriggers(formula.Formula{}, &p)
		return p
	}
	if err := requireFormulaSelection(formula.Formula{}, withEvaluation(evaluationPlan, evaluationCheck)); err != nil {
		t.Fatalf("the frozen evaluation plan's selection was refused: %v", err)
	}
	other := evaluationCheck
	other.Argv = []string{"go", "test", "-run", "TestX", "./..."}
	uncited := evaluationCheck
	uncited.Definitions = []string{"acceptance-input"}
	renamed := evaluationCheck
	renamed.ID = "acceptance"
	refused := []struct {
		name string
		plan readiness.Plan
	}{
		{"evaluation plan declares no select", withEvaluation(strings.Replace(evaluationPlan, `,"select":"affected-packages"`, "", 1), evaluationCheck)},
		{"argv differs from the evaluation plan", withEvaluation(evaluationPlan, other)},
		{"check does not cite the evaluation plan", withEvaluation(evaluationPlan, uncited)},
		{"acceptance command", withEvaluation(strings.Replace(evaluationPlan, `"go-test"`, `"acceptance"`, 1), renamed)},
		{"malformed evaluation plan", withEvaluation("{", evaluationCheck)},
		{"no evaluation plan", func() readiness.Plan {
			p := withEvaluation(evaluationPlan, evaluationCheck)
			p.CheckInputs = nil
			return p
		}()},
		{"supplied verification-input check", func() readiness.Plan {
			p := plan
			supplied := readiness.Check{ID: "human-go-test", Category: "regression", Dir: "lab", Argv: []string{"go", "test", "./..."}, Definitions: []string{"workflow"}, Select: readiness.SelectAffectedPackages}
			p.Analysis.Checks = append([]readiness.Check{supplied}, p.Analysis.Checks...)
			return p
		}()},
		{"setup select", func() readiness.Plan {
			p := plan
			p.Setup = []readiness.Check{{ID: "setup", Category: "setup", Argv: []string{"go", "test", "./..."}, Select: readiness.SelectAffectedPackages}}
			return p
		}()},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			if err := requireFormulaSelection(f, tc.plan); err == nil {
				t.Fatal("an undeclared selection was accepted")
			}
		})
	}
	for name, triggers := range map[string][]string{"dropped": nil, "replaced": {"docs/**"}, "added": {"lab/cmd/**", "docs/**"}} {
		p := plan
		p.FullSuiteTriggers = triggers
		if err := requireFormulaBaseline(f, p); err == nil || !strings.Contains(err.Error(), "fullSuiteTriggers must match") {
			t.Fatalf("%s triggers: %v", name, err)
		}
	}
	unselected := readiness.Plan{Evidence: evidence, FullSuiteTriggers: []string{"lab/cmd/**"}}
	if err := requireFormulaSelection(formula.Formula{}, unselected); err == nil {
		t.Fatal("triggers on a plan without a selection were accepted")
	}
}

// selectionBaselineFixture approves the sample Formula with a selected go test
// baseline check, so intake plans carry select and the Formula's fullSuiteTriggers.
func selectionBaselineFixture(t *testing.T) Options {
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
	entry, _ := json.Marshal(map[string]any{"id": "baseline-go-test", "argv": []string{"go", "test", "./..."}, "dir": ".", "timeoutMs": 5000, "workflow": baselineWorkflow, "reason": "CI runs go test.", "select": readiness.SelectAffectedPackages})
	approveFixture(t, o.LabDir, string(raw)+"requiredChecks:\n  - "+string(entry)+"\n", "selection")
	return o
}

func TestSelectionApprovalRefusesAmendedTriggersOrSelect(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(*readiness.Plan)
		want   string
	}{
		{"dropped triggers", func(p *readiness.Plan) { p.FullSuiteTriggers = nil }, "fullSuiteTriggers must match"},
		{"select added to a plan regression check", func(p *readiness.Plan) {
			added := p.Analysis.Checks[0]
			added.ID, added.Category, added.Argv, added.Select = "amended-go-test", "regression", []string{"go", "test", "./..."}, readiness.SelectAffectedPackages
			p.Analysis.Checks = append([]readiness.Check{added}, p.Analysis.Checks...)
		}, "check amended-go-test declares select"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := selectionBaselineFixture(t)
			r, plan := baselinePlan(t, o)
			if !reflect.DeepEqual(plan.FullSuiteTriggers, []string{"lab/cmd/**"}) {
				t.Fatalf("intake plan triggers = %v", plan.FullSuiteTriggers)
			}
			next := plan
			next.Analysis.Checks = append([]readiness.Check(nil), plan.Analysis.Checks...)
			tc.mutate(&next)
			amended, err := Amend(context.Background(), r.LabDir, r.ID, r.Readiness.Plans[0].Digest, next, "")
			if err != nil {
				t.Fatal(err)
			}
			o.Approve = amended.Readiness.Plans[1].Digest
			if _, err = Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approval: %v, want %q", err, tc.want)
			}
			if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
				t.Fatal("coding ran under an amended selection")
			}
		})
	}
}

func TestSelectionSuppliedVerificationInputsAreRefusedAtIntake(t *testing.T) {
	t.Parallel()
	o := selectionBaselineFixture(t)
	o.Checks = []readiness.Check{{ID: "human-go-test", Category: "regression", Argv: []string{"go", "test", "./..."}, Dir: ".", TimeoutMS: 1000, Required: true, Definitions: []string{"human-input"}, Reason: "Human check", Evidence: []string{"human-input"}, Select: readiness.SelectAffectedPackages}}
	content := "reviewed"
	o.CheckInputs = []readiness.FileEvidence{{Evidence: readiness.Evidence{ID: "human-input", Path: "human-input.txt", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}, Content: content}}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Readiness != nil || !strings.Contains(strings.Join(r.Notes, "\n"), "check human-go-test declares select") {
		t.Fatalf("a supplied selection reached a plan: %+v", r.Notes)
	}
}

func TestSelectionSurvivesTheLedgerProjection(t *testing.T) {
	lab := t.TempDir()
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	projected := compactVerification(selectionVerification())
	if projected.Checks[0].Selection == nil || projected.Protected.Checks[0].Selection == nil || projected.Checks[0].Selection.Selected != 3 {
		t.Fatalf("the compact projection dropped the selection: %+v", projected.Checks[0])
	}
	r := Record{ID: "mol-selection", LabDir: lab, Verification: &projected}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(lab, "ledgers", "mol-selection.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"Candidate `go-test` selection: affected-packages (3 of 41 packages)", "Protected regression `go-test` selection: affected-packages (3 of 41 packages)", "Candidate `go-test-docs` selection: full suite (reason: "} {
		if !strings.Contains(string(raw), line) {
			t.Fatalf("ledger lacks %q:\n%s", line, raw)
		}
	}
}
