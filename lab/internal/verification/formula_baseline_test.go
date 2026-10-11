package verification

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const (
	formattedSource   = "package x\n\nfunc F() {}\n"
	unformattedSource = "package x\nfunc  F( ) {}\n"
	baseTestSource    = "package x\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {}\n"
	changedTestSource = "package x\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tt.Log(\"changed\")\n}\n"
)

// baselineGofmtFixture commits a workflow and a formatted Go file, then returns a plan
// carrying a Formula baseline gofmt check that cites the frozen workflow.
func baselineGofmtFixture(t *testing.T) (string, readiness.Plan) {
	t.Helper()
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Fatalf("gofmt is required for the baseline fixture: %v", err)
	}
	dir, _ := sourceFixture(t)
	workflow := []byte("name: CI\non: pull_request\njobs: {}\n")
	if err := os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), workflow, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte(formattedSource), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "code_test.go"), []byte(baseTestSource), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "workflow and source")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	p.Evidence = []readiness.Evidence{{ID: "workflow", Path: ".github/workflows/ci.yml", SHA256: fmt.Sprintf("%x", sha256.Sum256(workflow))}}
	p.Analysis.Scope = []readiness.ScopedPath{{Path: "code.go", Reason: "Implementation", Evidence: []string{"issue"}}}
	p.Analysis.Checks = []readiness.Check{{ID: "baseline-gofmt", Category: "regression", Argv: []string{"sh", "-c", `test -z "$(gofmt -l .)"`}, Dir: ".", TimeoutMS: 60000, Required: true, Definitions: []string{"workflow"}, Reason: "Formula baseline: CI runs the gofmt check", Evidence: []string{"workflow"}}}
	p.FormulaBaseline = []string{"baseline-gofmt"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return dir, p
}

func TestFormulaBaselineGofmtResultAppearsInVerificationOutput(t *testing.T) {
	dir, p := baselineGofmtFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte("package x\n\nfunc G() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, p.Inputs.BaseCommit, []string{"code.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	observed := []Observation{}
	v := RunObserved(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c, func(o Observation) error {
		observed = append(observed, o)
		return nil
	})
	if !v.RequiredChecksPassed || len(v.Checks) != 1 {
		t.Fatalf("formatted source failed the baseline: %+v", v)
	}
	got := v.Checks[0]
	if got.ID != "baseline-gofmt" || got.Category != "regression" || !got.Result.OK {
		t.Fatalf("baseline observation = %+v", got)
	}
	if len(observed) != 1 || observed[0].ID != "baseline-gofmt" {
		t.Fatalf("baseline observation was not recorded: %+v", observed)
	}
}

func TestFormulaBaselineGofmtRunsInProtectedRegressionView(t *testing.T) {
	dir, p := baselineGofmtFixture(t)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "code_test.go", Reason: "Update test", Evidence: []string{"issue"}})
	if err := os.WriteFile(filepath.Join(dir, "code_test.go"), []byte(changedTestSource), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, p.Inputs.BaseCommit, []string{"code.go", "code_test.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if !v.RequiredChecksPassed || v.Protected == nil || !v.Protected.RequiredChecksPassed || len(v.Protected.Checks) != 1 || v.Protected.Checks[0].ID != "baseline-gofmt" {
		t.Fatalf("baseline did not run in the protected view: %+v", v)
	}
}

func TestFormulaBaselineGofmtFailureBlocksVerification(t *testing.T) {
	dir, p := baselineGofmtFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "code.go"), []byte(unformattedSource), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, p.Inputs.BaseCommit, []string{"code.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if v.RequiredChecksPassed || v.Error == "" || len(v.Checks) != 1 {
		t.Fatalf("unformatted source passed verification: %+v", v)
	}
	if got := v.Checks[0]; got.ID != "baseline-gofmt" || got.Category != "regression" || got.Result.OK {
		t.Fatalf("failing baseline observation = %+v", got)
	}
}
