package molecule

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const (
	rebaseOldLauncher = "/opt/fc/releases/1.0.0/forgecell"
	rebaseNewLauncher = "/opt/fc/releases/1.0.1/forgecell"
	rebaseDecision    = "rebind-decision"
)

// rebaseFixture codes a Molecule under F1, whose learn Atom runs a native
// release launcher, verifies it and leaves a pending checks-only amendment.
func rebaseFixture(t *testing.T) (Options, Record, string) {
	t.Helper()
	o, _ := fixture(t)
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f1 := string(raw) + "  - {id: learn, type: learn, binding: codex, command: [" + rebaseOldLauncher + ", __adapter, codex, /bin/sh]}\n"
	writeFormula(t, o.LabDir, f1, "fixture-f1")
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if !CompletedCoding(r) || len(r.HarnessAttempts) != 1 {
		t.Fatalf("fixture did not complete coding: %s", statePhase(r))
	}
	next := copyPlan(t, r.Readiness.Plans[0].Plan)
	next.Continuation = "checks-only"
	if r, err = Amend(context.Background(), o.LabDir, r.ID, r.Readiness.Plans[0].Digest, next, ""); err != nil {
		t.Fatal(err)
	}
	return o, r, f1
}

func writeFormula(t *testing.T, lab, yaml, decision string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(lab, "formulas", "sample.yaml"), []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, lab, yaml, decision)
}

func ledgerBytes(t *testing.T, r Record) []byte {
	t.Helper()
	var all []byte
	for _, ext := range []string{".json", ".md"} {
		raw, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+ext))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, raw...)
	}
	return all
}

func latestDigest(r Record) string { return r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest }

func TestAmendRebaseFormulaCarriesPlanToLauncherRebind(t *testing.T) {
	t.Parallel()
	o, r, f1 := rebaseFixture(t)
	parent := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	f2 := strings.Replace(f1, rebaseOldLauncher, rebaseNewLauncher, 1)
	writeFormula(t, o.LabDir, f2, rebaseDecision)
	old, _ := formula.Parse([]byte(f1))
	rebound, _ := formula.Parse([]byte(f2))

	o.Approve = parent.Digest
	if _, err := Run(context.Background(), o); err == nil || err.Error() != "issue or Formula changed; refresh intake" {
		t.Fatalf("stale plan ran after rebind: %v", err)
	}
	r, err := RebaseFormula(context.Background(), o.LabDir, r.ID, parent.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Readiness.Plans) != 3 || statePhase(r) != "scope-waiting" || r.Status != "waiting" {
		t.Fatalf("rebase did not wait for approval: %s %s", statePhase(r), r.Status)
	}
	got := r.Readiness.Plans[2].Plan
	want := parent.Plan
	if got.Revision != want.Revision+1 || got.ParentDigest != parent.Digest || got.Continuation != "checks-only" ||
		!reflect.DeepEqual(got.Analysis, want.Analysis) || !reflect.DeepEqual(got.Artifacts, want.Artifacts) ||
		got.PausedTree != want.PausedTree || got.PausedArtifactsSHA256 != want.PausedArtifactsSHA256 {
		t.Fatal("rebase changed the retained plan")
	}
	// The fixture's coding binding is not native, so its recomputed identity is
	// unchanged here; it must still equal the one derived from F2.
	codingBinding, _ := rebound.ExecutionBinding(formula.Coding)
	binding, err := bindingHash(codingBinding, r.Workspace.SourceRoot, r.Workspace.BaseCommit)
	if err != nil {
		t.Fatal(err)
	}
	if got.Inputs.FormulaSHA256 != rebound.SHA256 || want.Inputs.FormulaSHA256 != old.SHA256 || got.Inputs.BindingSHA256 != binding || got.Inputs.EvidenceSHA256 != want.Inputs.EvidenceSHA256 {
		t.Fatal("rebase did not rebind exactly the Formula and binding identities")
	}
	if r.FormulaSnapshot.SHA256 != rebound.SHA256 || r.FormulaSnapshot.YAML != f2 {
		t.Fatal("snapshot not replaced")
	}
	rebases := 0
	for _, e := range r.Readiness.Events {
		if e.Kind != readiness.FormulaRebased {
			continue
		}
		rebases++
		if e.Digest != latestDigest(r) || e.FormulaFrom != old.SHA256 || e.FormulaTo != rebound.SHA256 || e.Decision != rebaseDecision || !strings.Contains(e.Detail, readiness.FormulaRebaseReason) {
			t.Fatalf("rebase event: %+v", e)
		}
		if strings.Contains(e.Detail, "/") {
			t.Fatalf("rebase event leaks a path: %s", e.Detail)
		}
	}
	if rebases != 1 {
		t.Fatalf("%d rebase events", rebases)
	}
	md, err := os.ReadFile(filepath.Join(r.LabDir, "ledgers", r.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	section := string(md)[strings.Index(string(md), "## Decisions and attempts"):]
	line := ""
	for _, l := range strings.Split(section, "\n") {
		if strings.Contains(l, readiness.FormulaRebased) {
			line = l
		}
	}
	if !strings.Contains(line, old.SHA256) || !strings.Contains(line, rebound.SHA256) || !strings.Contains(line, rebaseDecision) || strings.Contains(line, o.LabDir) {
		t.Fatalf("Markdown rebase event: %q", line)
	}

	// The stale parent can never be approved; the rebased digest must be.
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("parent approved after rebase")
	}
	o.Approve = latestDigest(r)
	r, err = Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || r.Verification == nil || !r.Verification.RequiredChecksPassed || r.Verification.SourceTree != want.PausedTree {
		t.Fatal("checks-only did not verify the retained tree without coding")
	}
	if _, err = resolveExecutionRoles(r); err != nil {
		t.Fatal(err)
	}
}

func TestAmendRebaseFormulaRefusesNonLauncherChanges(t *testing.T) {
	t.Parallel()
	cases := map[string]func(string) string{
		"provider path": func(s string) string { return strings.Replace(s, "codex, /bin/sh]", "codex, /bin/cat]", 1) },
		"adapter name":  func(s string) string { return strings.Replace(s, "__adapter, codex,", "__adapter, claude-code,", 1) },
		"comment":       func(s string) string { return strings.Replace(s, rebaseOldLauncher, rebaseNewLauncher, 1) + "# note\n" },
		"atom": func(s string) string {
			return strings.Replace(s, "{id: ship, type: ship}", "{id: shipped, type: ship}", 1)
		},
		"not a release": func(s string) string { return strings.Replace(s, rebaseOldLauncher, "/opt/fc/bin/forgecell", 1) },
		"traversal": func(s string) string {
			return strings.Replace(s, rebaseOldLauncher, "/opt/fc/releases/../releases/1.0.1/forgecell", 1)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o, r, f1 := rebaseFixture(t)
			parent := latestDigest(r)
			writeFormula(t, o.LabDir, change(f1), rebaseDecision)
			before := ledgerBytes(t, r)
			_, err := RebaseFormula(context.Background(), o.LabDir, r.ID, parent)
			if err == nil || err.Error() != "issue or Formula changed; refresh intake" {
				t.Fatalf("non-launcher change: %v", err)
			}
			if !bytes.Equal(before, ledgerBytes(t, r)) {
				t.Fatal("refused rebase changed the ledger")
			}
			o.Approve = parent
			if _, err = Run(context.Background(), o); err == nil || err.Error() != "issue or Formula changed; refresh intake" {
				t.Fatalf("checks-only ran after a refused rebase: %v", err)
			}
		})
	}
}

func TestAmendRebaseFormulaFailsSafe(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		prepare func(t *testing.T, o Options, r Record, f2 string) Record
		want    string
	}{
		"unapproved rebind": {func(t *testing.T, o Options, r Record, f2 string) Record {
			if err := os.WriteFile(filepath.Join(o.LabDir, "formulas", "sample.yaml"), []byte(f2), 0600); err != nil {
				t.Fatal(err)
			}
			return r
		}, "Formula execution approval rejected"},
		"Formula write lock": {func(t *testing.T, o Options, r Record, f2 string) Record {
			writeFormula(t, o.LabDir, f2, rebaseDecision)
			if err := os.Mkdir(filepath.Join(o.LabDir, ".formula-write.lock"), 0700); err != nil {
				t.Fatal(err)
			}
			return r
		}, "Formula activation is locked"},
		"no rebind": {func(t *testing.T, o Options, r Record, f2 string) Record { return r }, "nothing to rebase"},
		"uncertain attempt": {func(t *testing.T, o Options, r Record, f2 string) Record {
			writeFormula(t, o.LabDir, f2, rebaseDecision)
			r.HarnessAttempts[len(r.HarnessAttempts)-1].Result.Reconciled = false
			if err := save(r); err != nil {
				t.Fatal(err)
			}
			return r
		}, "harness outcome"},
		"recaptured tree differs": {func(t *testing.T, o Options, r Record, f2 string) Record {
			writeFormula(t, o.LabDir, f2, rebaseDecision)
			if err := os.WriteFile(filepath.Join(r.Workspace.Path, "code.txt"), []byte("drifted"), 0600); err != nil {
				t.Fatal(err)
			}
			return r
		}, "pending workspace changed"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o, r, f1 := rebaseFixture(t)
			r = c.prepare(t, o, r, strings.Replace(f1, rebaseOldLauncher, rebaseNewLauncher, 1))
			before := ledgerBytes(t, r)
			_, err := RebaseFormula(context.Background(), o.LabDir, r.ID, latestDigest(r))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
			if !bytes.Equal(before, ledgerBytes(t, r)) {
				t.Fatal("refused rebase changed the ledger")
			}
		})
	}
}

func TestAmendRebaseFormulaCarriesCompletedCodingAsChecksOnly(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f1 := string(raw) + "  - {id: learn, type: learn, binding: codex, command: [" + rebaseOldLauncher + ", __adapter, codex, /bin/sh]}\n"
	writeFormula(t, o.LabDir, f1, "fixture-f1")
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	parent := r.Readiness.Plans[0]
	writeFormula(t, o.LabDir, strings.Replace(f1, rebaseOldLauncher, rebaseNewLauncher, 1), rebaseDecision)
	if r, err = RebaseFormula(context.Background(), o.LabDir, r.ID, parent.Digest); err != nil {
		t.Fatal(err)
	}
	got := r.Readiness.Plans[1].Plan
	if got.Continuation != "checks-only" || got.PausedTree != r.HarnessAttempts[0].Capture.Tree || !reflect.DeepEqual(got.Analysis, parent.Plan.Analysis) {
		t.Fatal("completed coding not carried as checks-only on the retained tree")
	}
	o.Approve = latestDigest(r)
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || r.Verification == nil || !r.Verification.RequiredChecksPassed {
		t.Fatal("rebased checks-only reran coding or failed")
	}
}
