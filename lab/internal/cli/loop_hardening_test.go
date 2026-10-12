package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

func TestHardeningLoopExitsNonZeroOnlyWhenTheFinalPassStalled(t *testing.T) {
	lab := t.TempDir()
	waiting := molecule.LoopItem{Repository: "acme/widgets", Issue: 11, MoleculeID: "mol-11-a", State: molecule.LoopWaiting, Reason: "the scope gate needs a human"}
	locked := molecule.LoopItem{Repository: "acme/widgets", Issue: 12, MoleculeID: "mol-12-b", State: molecule.LoopLocked, Reason: "another loop holds this Molecule's lock, so it was skipped"}
	stalled := molecule.LoopItem{Repository: "acme/widgets", Issue: 13, MoleculeID: "mol-13-c", State: molecule.LoopStalled, Reason: "the policy approved this plan but the approval was not applied: source base changed"}
	var report molecule.LoopReport
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) { return report, nil })
	run := func() (int, string) {
		t.Helper()
		code, out, stderr := runLoopCLI("--lab", lab, "--once")
		if stderr != "" {
			t.Fatalf("stderr %q", stderr)
		}
		return code, out
	}

	report = molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{waiting, locked}}}}
	code, out := run()
	if code != 0 || strings.Contains(out, "made no progress") {
		t.Fatalf("waiting at a human gate or skipping a locked Molecule changed the output or exit code: %d\n%s", code, out)
	}

	report.Passes[0].Items = append(report.Passes[0].Items, stalled)
	code, out = run()
	for _, want := range []string{"acme/widgets#13 · stalled · Molecule mol-13-c", "the approval was not applied: source base changed", "1 Issue(s) made no progress in the final pass"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the report lacks %q:\n%s", want, out)
		}
	}
	if code != 3 {
		t.Fatalf("a stalled Issue exited %d, want 3", code)
	}

	report.Passes[0].Items[2].Failed = true
	if code, _ = run(); code != 2 {
		t.Fatalf("a failed Molecule must still exit 2, got %d", code)
	}

	report.Passes[0].Items[2].Failed = false
	report.Passes = append(report.Passes, molecule.LoopPass{Items: []molecule.LoopItem{waiting}})
	if code, out = run(); code != 0 || strings.Contains(out, "made no progress") {
		t.Fatalf("an earlier pass's stall was counted: %d\n%s", code, out)
	}
}

func TestHardeningLoopExitsNonZeroForAnIssueInAnUndeterminedState(t *testing.T) {
	waiting := molecule.LoopItem{Repository: "acme/widgets", Issue: 11, MoleculeID: "mol-11-a", State: molecule.LoopWaiting, Reason: "the scope gate needs a human"}
	unknown := molecule.LoopItem{Repository: "acme/widgets", Issue: 14, MoleculeID: "mol-14-d", State: molecule.LoopUnknown, Reason: "the recorded Molecules for this Issue cannot be ordered, so none was advanced"}
	report := molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{waiting, unknown}}}}
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) { return report, nil })

	code, out, stderr := runLoopCLI("--lab", t.TempDir(), "--once")
	if code != 3 || stderr != "" || !strings.Contains(out, "acme/widgets#14 · unknown") || !strings.Contains(out, "1 Issue(s) made no progress in the final pass") {
		t.Fatalf("an Issue the loop could not determine exited %d:\n%s\n%s", code, out, stderr)
	}

	report.Passes[0].Items[1].State = molecule.LoopStalled
	report.Passes[0].Items = append(report.Passes[0].Items, molecule.LoopItem{Repository: "acme/widgets", Issue: 15, State: molecule.LoopUnknown, Reason: "x"})
	if code, out, _ = runLoopCLI("--lab", t.TempDir(), "--once"); code != 3 || !strings.Contains(out, "2 Issue(s) made no progress") {
		t.Fatalf("stalled and undetermined Issues were not counted together: %d\n%s", code, out)
	}
}
