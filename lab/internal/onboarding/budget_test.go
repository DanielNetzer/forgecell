package onboarding

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
)

// Bootstrap Formulas carry a reviewed ceiling, so intake can propose longer
// per-Molecule timeouts for large scopes instead of always capping at timeoutMs.
func TestBootstrapFormulaCarriesReviewedTimeoutCeiling(t *testing.T) {
	for name, tc := range map[string]struct {
		timeout, ceiling         int
		wantTimeout, wantCeiling int
	}{
		"defaults": {0, 0, DefaultHarnessTimeoutMS, DefaultMaxTimeoutMS},
		"explicit": {1200000, 2400000, 1200000, 2400000},
		"timeout only above default ceiling is impossible; equal ceiling allowed": {3600000, 0, 3600000, 3600000},
	} {
		t.Run(name, func(t *testing.T) {
			root, runner := repoFixture(t)
			out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: filepath.Join(t.TempDir(), "lab"), Launcher: "/installed/forgecell", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner, HarnessTimeoutMS: tc.timeout, MaxTimeoutMS: tc.ceiling})
			if err != nil || out.Proposal == nil {
				t.Fatalf("%+v %v", out, err)
			}
			var proposed struct {
				Harness struct {
					TimeoutMS    int `yaml:"timeoutMs"`
					MaxTimeoutMS int `yaml:"maxTimeoutMs"`
				} `yaml:"harness"`
			}
			if err := yaml.Unmarshal([]byte(out.Proposal.YAML), &proposed); err != nil {
				t.Fatal(err)
			}
			if proposed.Harness.TimeoutMS != tc.wantTimeout || proposed.Harness.MaxTimeoutMS != tc.wantCeiling {
				t.Fatalf("harness budget %+v, want %d/%d", proposed.Harness, tc.wantTimeout, tc.wantCeiling)
			}
			f, err := formula.Parse([]byte(out.Proposal.YAML))
			if err != nil {
				t.Fatalf("bootstrap Formula must parse: %v", err)
			}
			if got := f.Harness.TimeoutCeilingMS(); got != tc.wantCeiling {
				t.Fatalf("ceiling %d, want %d", got, tc.wantCeiling)
			}
		})
	}
}

func TestHarnessBudgetRejectsInvalidValues(t *testing.T) {
	for _, tc := range [][2]int{{-1, 0}, {3600001, 0}, {0, 3600001}, {1200000, 600000}, {0, 600000}} {
		if _, _, err := harnessBudget(InitOptions{HarnessTimeoutMS: tc[0], MaxTimeoutMS: tc[1]}); err == nil {
			t.Fatalf("accepted timeout %d ceiling %d", tc[0], tc[1])
		}
	}
}

func approvedBootstrapLab(t *testing.T) (string, string, Runner) {
	t.Helper()
	root, runner := repoFixture(t)
	dir := filepath.Join(t.TempDir(), "lab")
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner, HarnessTimeoutMS: 900000, MaxTimeoutMS: 900000})
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	return root, dir, runner
}

// An explicit rebind may carry a timeout budget and must preserve everything else.
func TestRebindAppliesExplicitBudgetAndPreservesFormula(t *testing.T) {
	root, dir, runner := approvedBootstrapLab(t)
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/new/forgecell", Preferred: "codex", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner, MaxTimeoutMS: 3600000})
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	f, err := formula.Parse([]byte(out.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if f.Harness.TimeoutMS != 900000 || f.Harness.MaxTimeoutMS != 3600000 || f.Harness.Command[0] != "/new/forgecell" {
		t.Fatalf("rebind budget not applied: %+v", f.Harness)
	}
	before, err := formula.Parse([]byte(out.BeforeYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Atoms) != len(f.Atoms) || f.ID != before.ID {
		t.Fatalf("rebind changed more than the binding and budget")
	}
}

func TestBudgetFlagsAreRefusedWhereTheyWouldBeIgnored(t *testing.T) {
	root, dir, runner := approvedBootstrapLab(t)
	cands := []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}
	if _, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Candidates: cands, Runner: runner, MaxTimeoutMS: 3600000}); err == nil {
		t.Fatal("budget flags without --harness were silently ignored on an approved Formula")
	}
	if _, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/new/forgecell", Preferred: "codex", Candidates: cands, Runner: runner}); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/new/forgecell", Preferred: "codex", Candidates: cands, Runner: runner, MaxTimeoutMS: 3600000}); err == nil {
		t.Fatal("budget flags were silently ignored while a proposal is pending")
	}
	if _, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/new/forgecell", Preferred: "codex", Candidates: cands, Runner: runner, MaxTimeoutMS: -5}); err == nil {
		t.Fatal("invalid budget accepted")
	}
}
