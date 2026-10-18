package evaluation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// verifyCandidateFixture commits a base with calc_test.go, other_test.go and golden.txt,
// then a candidate changing all of them, and returns a plan skeleton and a runner that
// verifies the candidate tree under a given plan and returns the regression check output.
func verifyCandidateFixture(t *testing.T) (Plan, func(Plan) string) {
	t.Helper()
	f, _, _ := newFixture(t)
	f.write("lab/calc/calc_test.go", valueTest(1))
	f.write("lab/calc/other_test.go", "package calc\n\n// other-base\n")
	f.write("lab/calc/golden.txt", "golden-base\n")
	f.git("add", ".")
	f.git("commit", "-qm", "existing test")
	base := f.head()
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", valueTest(2))
	f.write("lab/calc/other_test.go", "package calc\n\n// other-candidate\n")
	f.write("lab/calc/golden.txt", "golden-candidate\n")
	f.git("add", ".")
	f.git("commit", "-qm", "candidate")
	tree := f.git("rev-parse", "HEAD^{tree}")

	acceptance := filepath.Join(t.TempDir(), "acceptance.sh")
	script := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(acceptance, script, 0o755); err != nil {
		t.Fatal(err)
	}
	p := Plan{
		BaseCommit:       base,
		AllowedFiles:     []string{"lab/calc/calc.go", "lab/calc/calc_test.go", "lab/calc/golden.txt", "lab/calc/other_test.go"},
		AcceptanceSHA256: Hash(script),
		Checks:           []Command{{Name: "regression", Dir: "lab/calc", Argv: []string{"cat", "calc_test.go", "other_test.go", "golden.txt"}, TimeoutMS: 30000}},
	}
	a := Attempt{ScopeOK: true, Tree: tree, Molecule: molecule.Record{Status: "recorded", Atoms: []molecule.Atom{{Type: "harness", Status: "done"}}}}
	return p, func(p Plan) string {
		t.Helper()
		destination := filepath.Join(t.TempDir(), "verify")
		t.Cleanup(func() { f.git("worktree", "remove", "--force", destination) })
		v := verify(context.Background(), f.dir, destination, acceptance, p, a)
		if v.Error != "" || !v.Correct || len(v.Checks) != 1 {
			t.Fatalf("verification failed: error=%q correct=%v checks=%d", v.Error, v.Correct, len(v.Checks))
		}
		return v.Checks[0].Result.Stdout
	}
}

// TestVerifyAppliesCandidateTestFiles proves independent verification applies the
// candidate's version of a plan's candidate test file, matching the exempted protected
// run, while every other original check file keeps its base version: a non-test file and
// a conventional test file a recheck supplied as an original check.
func TestVerifyAppliesCandidateTestFiles(t *testing.T) {
	p, run := verifyCandidateFixture(t)
	p.OriginalCheckFiles = []string{"lab/calc/calc_test.go", "lab/calc/golden.txt"}
	p.CandidateTestFiles = []string{"lab/calc/calc_test.go"}
	// A recheck only extends OriginalCheckFiles.
	p.OriginalCheckFiles = append(p.OriginalCheckFiles, "lab/calc/other_test.go")
	if err := validateCandidateTestFiles(p); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(protectedExemptPaths(p), ","); got != "lab/calc/calc_test.go" {
		t.Fatalf("exempt set = %q", got)
	}
	got := run(p)
	if !strings.Contains(got, "Value() != 2") {
		t.Fatalf("verification did not apply the candidate version of the candidate test file:\n%s", got)
	}
	if !strings.Contains(got, "golden-base") || strings.Contains(got, "golden-candidate") {
		t.Fatalf("verification applied the candidate version of a non-test original check file:\n%s", got)
	}
	if !strings.Contains(got, "other-base") || strings.Contains(got, "other-candidate") {
		t.Fatalf("verification applied the candidate version of a recheck-supplied original check:\n%s", got)
	}
}

// TestVerifyHandAuthoredPlanExemptsNothing proves a plan without CandidateTestFiles keeps
// every original check file, conventional test paths included, at its base version.
func TestVerifyHandAuthoredPlanExemptsNothing(t *testing.T) {
	p, run := verifyCandidateFixture(t)
	p.OriginalCheckFiles = []string{"lab/calc/calc_test.go", "lab/calc/golden.txt", "lab/calc/other_test.go"}
	if got := protectedExemptPaths(p); len(got) != 0 {
		t.Fatalf("hand-authored plan exempts %q", got)
	}
	got := run(p)
	if strings.Contains(got, "Value() != 2") || strings.Contains(got, "other-candidate") || strings.Contains(got, "golden-candidate") {
		t.Fatalf("verification applied a candidate version without candidate test files:\n%s", got)
	}
}

func TestValidateCandidateTestFiles(t *testing.T) {
	base := Plan{
		AllowedFiles:       []string{"lab/calc/calc.go", "lab/calc/calc_test.go", "lab/calc/golden.txt", "lab/calc/other_test.go"},
		OriginalCheckFiles: []string{"lab/calc/calc_test.go", "lab/calc/golden.txt"},
	}
	for name, files := range map[string][]string{
		"parent":           {"../lab/calc/calc_test.go"},
		"duplicate":        {"lab/calc/calc_test.go", "lab/calc/calc_test.go"},
		"not original":     {"lab/calc/other_test.go"},
		"out of scope":     {"lab/x/x_test.go"},
		"not a test":       {"lab/calc/golden.txt"},
		"implementation":   {"lab/calc/calc.go"},
		"unclean relative": {"lab/calc/./calc_test.go"},
	} {
		p := base
		p.CandidateTestFiles = files
		if err := validateCandidateTestFiles(p); err == nil {
			t.Errorf("%s: candidate test files %q accepted", name, files)
		}
		for _, f := range protectedExemptPaths(p) {
			if f != "lab/calc/calc_test.go" {
				t.Errorf("%s: invalid candidate test file %q exempt", name, f)
			}
		}
	}
	p := base
	p.CandidateTestFiles = []string{"lab/calc/calc_test.go"}
	if err := validateCandidateTestFiles(p); err != nil {
		t.Fatalf("valid candidate test file refused: %v", err)
	}
}
