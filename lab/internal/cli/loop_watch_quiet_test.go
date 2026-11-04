package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// quietStep is one watch pass: the report and error the loop returns, or a
// resource reading below a soft floor that skips the pass.
type quietStep struct {
	report molecule.LoopReport
	err    error
	skip   string // "disk" or "memory" skips the pass with that reason
}

// quietWatch drives watchLoop through steps with the watchWait seam and a fake
// guard, so no test sleeps. It returns the exit code, stdout and stderr.
func quietWatch(t *testing.T, steps []quietStep) (int, string, string) {
	t.Helper()
	stubLearn(t)
	step := 0
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return steps[step].report, steps[step].err
	})
	previous := watchWait
	watchWait = func(context.Context, time.Duration) bool {
		step++
		return step < len(steps)
	}
	t.Cleanup(func() { watchWait = previous })
	guard := &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
		r := resources.Readings{Volumes: []resources.Volume{{Path: "/lab", Free: 50 * resources.GiB, Total: 100 * resources.GiB}}, Memory: 8 * resources.GiB, MemoryKnown: true}
		switch steps[step].skip {
		case "disk":
			r.Volumes[0].Free = resources.GiB
		case "memory":
			r.Memory = 2 * resources.GiB
		}
		return r
	})}
	var out, stderr bytes.Buffer
	lab := t.TempDir()
	options := molecule.LoopOptions{Template: molecule.Options{LabDir: lab, Resources: guard}, MaxConcurrent: 1}
	code := watchLoop(context.Background(), options, time.Minute, false, lab, &out, &stderr)
	return code, out.String(), stderr.String()
}

func quietItem(issue int64, state, reason string) molecule.LoopItem {
	return molecule.LoopItem{Repository: "acme/widgets", Issue: issue, MoleculeID: "mol-x", State: state, Reason: reason}
}

func quietReport(items ...molecule.LoopItem) molecule.LoopReport {
	return molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{Items: items}}}
}

// fullReport is what renderLoop prints for report, as a one-shot loop shows it.
func fullReport(report molecule.LoopReport) string {
	var b bytes.Buffer
	renderLoop(&b, report, molecule.LoopOptions{}.Select, 1)
	return b.String()
}

func TestWatchQuietFirstPassIsFullAndAnIdenticalPassIsOneLine(t *testing.T) {
	report := quietReport(quietItem(1, molecule.LoopWaiting, "waiting at the scope gate"), quietItem(2, molecule.LoopStalled, "approval never applied"))
	report.Passes[0].Notes = []string{"the selector matched no milestone"}
	code, out, stderr := quietWatch(t, []quietStep{{report: report}, {report: report}, {report: report}})
	want := "Watch pass 1\n" + fullReport(report) +
		"Watch pass 2: no change · 1 Issue(s) need attention\n" +
		"Watch pass 3: no change · 1 Issue(s) need attention\n"
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
	if !strings.Contains(fullReport(report), "Warning: the selector matched no milestone\n") || !strings.Contains(fullReport(report), "acme/widgets#2 · stalled") {
		t.Fatalf("pass 1 is not the full report:\n%s", out)
	}
	if code != 3 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}

	clean := quietReport(quietItem(1, molecule.LoopWaiting, "waiting at the scope gate"))
	_, out, _ = quietWatch(t, []quietStep{{report: clean}, {report: clean}})
	if lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n"); lines[len(lines)-1] != "Watch pass 2: no change" || strings.Count(out, "Watch pass 2") != 1 || strings.Count(out, "Lab loop for") != 1 {
		t.Fatalf("an unchanged pass is not exactly one line:\n%s", out)
	}
}

func TestWatchQuietPrintsOnlyTheChangedItem(t *testing.T) {
	first := quietReport(quietItem(1, molecule.LoopWaiting, "waiting at the scope gate"), quietItem(2, molecule.LoopWaiting, "waiting at the delivery gate"))
	changed := molecule.LoopItem{Repository: "acme/widgets", Issue: 2, MoleculeID: "mol-x", State: molecule.LoopStalled, Reason: "interrupted", Proposal: &molecule.LoopProposal{Path: "p", Command: "forgecell recover mol-x", Followup: []string{"forgecell amend mol-x"}}}
	second := quietReport(quietItem(1, molecule.LoopWaiting, "waiting at the scope gate"), changed)
	_, out, _ := quietWatch(t, []quietStep{{report: first}, {report: second}})
	want := "Watch pass 1\n" + fullReport(first) +
		"Watch pass 2: 1 change(s) · 1 Issue(s) need attention\n" +
		"acme/widgets#2 · stalled · Molecule mol-x\n  interrupted\n" +
		"  Recovery proposal (saved; the loop never runs it): p\n    forgecell recover mol-x\n    then: forgecell amend mol-x\n"
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
}

func TestWatchQuietReportsNewAndDroppedItems(t *testing.T) {
	keyed := molecule.LoopItem{Repository: "acme/widgets", Key: "PROJ-7\x1b[2J", State: molecule.LoopWaiting, Reason: "waiting"}
	first := quietReport(quietItem(1, molecule.LoopWaiting, "a"), quietItem(2, molecule.LoopWaiting, "b"), keyed)
	second := quietReport(quietItem(2, molecule.LoopWaiting, "b"), quietItem(3, molecule.LoopWaiting, "c"))
	_, out, _ := quietWatch(t, []quietStep{{report: first}, {report: second}})
	tail := out[len("Watch pass 1\n"+fullReport(first)):]
	want := "Watch pass 2: 3 change(s)\n" +
		"acme/widgets#3 · waiting · Molecule mol-x\n  c\n" +
		"acme/widgets#1 · no longer eligible\n" +
		"acme/widgets " + flatten(keyed.Key) + " · no longer eligible\n"
	if tail != want {
		t.Fatalf("pass 2:\n%s\nwant:\n%s", tail, want)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("a hostile key reached the output raw: %q", out)
	}
}

func TestWatchQuietDoesNotRepeatWarningsOrStops(t *testing.T) {
	report := func(notes []string, stopped string) molecule.LoopReport {
		r := quietReport(quietItem(1, molecule.LoopWaiting, "a"))
		r.Passes[0].Notes, r.Passes[0].Stopped = notes, stopped
		return r
	}
	_, out, _ := quietWatch(t, []quietStep{
		{report: report([]string{"old warning"}, "harness login needed")},
		{report: report([]string{"old warning"}, "harness login needed")},
		{report: report([]string{"old warning", "new warning"}, "another stop")},
	})
	tail := out[strings.Index(out, "Watch pass 2"):]
	want := "Watch pass 2: no change\n" +
		"Watch pass 3: 2 change(s)\n" +
		"Warning: new warning\n" +
		"Pass 1 stopped: another stop\n"
	if tail != want {
		t.Fatalf("later passes:\n%s\nwant:\n%s", tail, want)
	}
}

func TestWatchQuietShortensARepeatedSkip(t *testing.T) {
	report := quietReport(quietItem(1, molecule.LoopWaiting, "a"))
	_, out, _ := quietWatch(t, []quietStep{{skip: "disk"}, {skip: "disk"}, {skip: "memory"}, {report: report}, {skip: "memory"}, {skip: "memory"}, {report: report}})
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	full := strings.Split(strings.TrimSuffix(fullReport(report), "\n"), "\n")
	want := []string{"Watch pass 1", "Pass skipped: resources: low disk", "Watch pass 2: skipped (same reason)", "Watch pass 3", "Pass skipped: resources: low memory", "Watch pass 4"}
	want = append(want, full...)
	want = append(want, "Watch pass 5", "Pass skipped: resources: low memory", "Watch pass 6: skipped (same reason)", "Watch pass 7: no change")
	if len(lines) != len(want) {
		t.Fatalf("output:\n%s", out)
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Fatalf("line %d %q, want %q:\n%s", i+1, lines[i], want[i], out)
		}
	}
}

func TestWatchQuietAPassErrorKeepsTheComparison(t *testing.T) {
	first := quietReport(quietItem(1, molecule.LoopWaiting, "a"))
	partial := quietReport(quietItem(1, molecule.LoopStalled, "listing cut short"))
	transient := molecule.TransientLoopError{Err: errors.New("gh: network unreachable")}
	code, out, stderr := quietWatch(t, []quietStep{{report: first}, {report: partial, err: transient}, {report: first}})
	want := "Watch pass 1\n" + fullReport(first) +
		"Watch pass 2\n" + fullReport(partial) +
		"Watch pass 3: no change\n"
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
	if code != 0 || strings.Count(stderr, "network unreachable") != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}

	// A watch whose first pass failed has nothing to compare with yet.
	_, out, _ = quietWatch(t, []quietStep{{report: partial, err: transient}, {report: first}})
	if want := "Watch pass 1\n" + fullReport(partial) + "Watch pass 2\n" + fullReport(first); out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
}

func TestWatchQuietExitCodesAreUnchanged(t *testing.T) {
	failed := quietReport(molecule.LoopItem{Repository: "acme/widgets", Issue: 1, State: molecule.LoopStopped, Failed: true, Reason: "x"})
	stalled := quietReport(quietItem(1, molecule.LoopStalled, "x"))
	clean := quietReport()
	transient := molecule.TransientLoopError{Err: errors.New("gh: network unreachable")}
	for name, c := range map[string]struct {
		steps []quietStep
		want  int
	}{
		"failed then unchanged":       {[]quietStep{{report: failed}, {report: failed}}, 2},
		"stalled then unchanged":      {[]quietStep{{report: stalled}, {report: stalled}}, 3},
		"stalled then clean":          {[]quietStep{{report: stalled}, {report: clean}}, 0},
		"ends in a transient error":   {[]quietStep{{report: clean}, {report: clean, err: transient}}, 1},
		"recovers after an error":     {[]quietStep{{report: stalled}, {err: transient}, {report: stalled}}, 3},
		"a skip keeps the final code": {[]quietStep{{report: failed}, {skip: "disk"}}, 2},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, _ := quietWatch(t, c.steps); code != c.want {
				t.Fatalf("exit %d, want %d", code, c.want)
			}
		})
	}
	t.Run("a loop-level error", func(t *testing.T) {
		code, _, stderr := quietWatch(t, []quietStep{{report: clean}, {report: clean, err: errors.New("GitHub data unavailable")}, {report: clean}})
		if code != 1 || !strings.Contains(stderr, "GitHub data unavailable") {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
	})
}

func TestWatchQuietLeavesTheOneShotReportByteIdentical(t *testing.T) {
	stubLearn(t)
	report := molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{
		Notes: []string{"a note"},
		Items: []molecule.LoopItem{
			{Repository: "acme/widgets", Issue: 11, MoleculeID: "mol-11-a", Started: true, Approved: true, State: molecule.LoopWaiting, Reason: "waiting at the scope gate"},
			{Repository: "acme/widgets", Issue: 12, State: molecule.LoopStalled, Reason: "interrupted", Proposal: &molecule.LoopProposal{Path: "p", Command: "forgecell recover mol-12", Followup: []string{"step one"}, Existing: true}},
			{Repository: "acme/widgets", Key: "PROJ-1", MoleculeID: "mol-k", State: molecule.LoopWaiting, Reason: "keyed"},
		},
		Stopped: "the harness needs a login",
	}}}
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) { return report, nil })
	code, out, _ := runLoopCLI("--lab", t.TempDir(), "--once")
	want := "Lab loop for acme/widgets · select oldest · max concurrent 1\n" +
		"Pass 1: 3 eligible Issue(s)\n" +
		"Warning: a note\n" +
		"acme/widgets#11 · waiting · Molecule mol-11-a · started intake · approved by policy\n  waiting at the scope gate\n" +
		"acme/widgets#12 · stalled · Molecule none\n  interrupted\n" +
		"  Recovery proposal (already proposed; the loop never runs it): p\n    forgecell recover mol-12\n    then: step one\n" +
		"acme/widgets PROJ-1 · waiting · Molecule mol-k\n  keyed\n" +
		"Pass 1 stopped: the harness needs a login\n" +
		"1 Issue(s) made no progress in the final pass and need attention\n"
	if code != 3 || out != want {
		t.Fatalf("exit %d, output:\n%s\nwant:\n%s", code, out, want)
	}
}
