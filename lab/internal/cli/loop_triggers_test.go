package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/learning"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// triggersWatch runs the loop command with an injected wait and a context the test
// controls, so no test sleeps. wait is called between passes with the interval.
func triggersWatch(t *testing.T, ctx context.Context, wait func(time.Duration) bool, args ...string) (int, string, string) {
	t.Helper()
	previous := watchWait
	watchWait = func(_ context.Context, d time.Duration) bool { return wait(d) }
	t.Cleanup(func() { watchWait = previous })
	var out, stderr bytes.Buffer
	code := runLoopCommand(ctx, args, "", &out, &stderr)
	return code, out.String(), stderr.String()
}

func TestTriggersWatchFlagsAreValidatedBeforeAnyEffect(t *testing.T) {
	marker := loopFakeGH(t)
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		t.Error("the loop ran despite invalid arguments")
		return molecule.LoopReport{}, nil
	})
	s := stubLearn(t)
	lab := filepath.Join(t.TempDir(), "absent")
	for _, extra := range [][]string{
		{"--watch", "--interval", "59s"},
		{"--watch", "--interval", "30s"},
		{"--watch", "--interval", "0"},
		{"--watch", "--interval", "0s"},
		{"--watch", "--interval", "-5m"},
		{"--watch", "--interval", "soon"},
		{"--watch", "--interval", "5"},
		{"--watch", "--interval"},
		{"--watch", "--once"},
		{"--once", "--watch", "--interval", "2m"},
		{"--interval", "5m"},
		{"--interval", "5m", "--once"},
		{"--watch=maybe"},
		{"--learn=maybe"},
		{"--watch", "stray"},
	} {
		code, out, stderr := runLoopCLI(append([]string{"--lab", lab}, extra...)...)
		if code != 1 || out != "" || stderr == "" {
			t.Fatalf("%v: exit %d, output %q, stderr %q", extra, code, out, stderr)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("GitHub was called for an invalid invocation")
	}
	if _, err := os.Stat(lab); !os.IsNotExist(err) {
		t.Fatal("a Lab was created for an invalid invocation")
	}
	if s.runs != 0 || s.evaluations != 0 {
		t.Fatal("learning ran for an invalid invocation")
	}
}

func TestTriggersWatchRepeatsOnceWithUnchangedOptionsAndTheDefaultInterval(t *testing.T) {
	stubLearn(t)
	lab := t.TempDir()
	var seen []molecule.LoopOptions
	var contexts []error
	stubLoop(t, func(ctx context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		seen = append(seen, o)
		contexts = append(contexts, ctx.Err())
		return molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{}}}, nil
	})
	stop, cancel := context.WithCancel(context.Background())
	defer cancel()
	var waits []time.Duration
	code, out, stderr := triggersWatch(t, stop, func(d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == 3 {
			cancel()
			return false
		}
		return true
	}, "--lab", lab, "--select", "label:ready", "--max-concurrent", "2", "--watch")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(seen) != 3 {
		t.Fatalf("%d passes, want 3 although no pass started or approved anything", len(seen))
	}
	for i, o := range seen {
		if !o.Once || o.Template.LabDir != lab || o.Select != (intake.Selector{Kind: "label", Value: "ready"}) || o.MaxConcurrent != 2 || o.Drain == nil || o.Work == nil {
			t.Fatalf("pass %d: %+v", i+1, o)
		}
		if contexts[i] != nil {
			t.Fatalf("pass %d ran on a cancelled context", i+1)
		}
	}
	for _, d := range waits {
		if d != 5*time.Minute {
			t.Fatalf("waited %s, want the 5m default", d)
		}
	}
	for _, want := range []string{"Watch pass 1", "Watch pass 2", "Watch pass 3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Watch pass 4") {
		t.Fatal("a pass started after the stop request")
	}

	waits = nil
	stop, cancel = context.WithCancel(context.Background())
	defer cancel()
	if code, _, _ = triggersWatch(t, stop, func(d time.Duration) bool { waits = append(waits, d); cancel(); return true }, "--lab", lab, "--watch", "--interval", "90s"); code != 0 || len(waits) != 1 || waits[0] != 90*time.Second {
		t.Fatalf("exit %d waits %v", code, waits)
	}
	if code, _, _ = triggersWatch(t, context.Background(), func(d time.Duration) bool { return false }, "--lab", lab, "--watch", "--interval", "1m"); code != 0 {
		t.Fatalf("the minimum interval was refused: %d", code)
	}
}

func TestTriggersStopDuringTheWaitReturnsWithoutAnotherPass(t *testing.T) {
	stubLearn(t)
	passes := 0
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		passes++
		return molecule.LoopReport{}, nil
	})
	stop, cancel := context.WithCancel(context.Background())
	defer cancel()
	code, _, _ := triggersWatch(t, stop, func(time.Duration) bool { cancel(); return false }, "--lab", t.TempDir(), "--watch")
	if code != 0 || passes != 1 {
		t.Fatalf("exit %d after %d passes", code, passes)
	}
}

func TestTriggersStopDuringAPassDoesNotCancelItAndStartsNoFurtherPass(t *testing.T) {
	s := stubLearn(t)
	stop, cancel := context.WithCancel(context.Background())
	defer cancel()
	var drained, workCancelled, listingCancelled bool
	stubLoop(t, func(ctx context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		cancel()
		select {
		case <-o.Drain:
			drained = true
		default:
		}
		listingCancelled = ctx.Err() != nil
		workCancelled = o.Work.Err() != nil
		return molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{
			{Repository: "acme/widgets", Issue: 11, MoleculeID: "mol-11-a", Started: true, State: molecule.LoopWaiting, Reason: "waiting at the scope gate"},
			{Repository: "acme/widgets", Issue: 12, State: molecule.LoopInterrupted, Reason: "the loop was interrupted before this Issue was started"},
		}}}}, nil
	})
	waited := false
	code, out, stderr := triggersWatch(t, stop, func(time.Duration) bool { waited = true; return true }, "--lab", t.TempDir(), "--watch", "--learn")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !drained || workCancelled || !listingCancelled {
		t.Fatalf("drained %t, in-flight work cancelled %t, listing context cancelled %t", drained, workCancelled, listingCancelled)
	}
	if waited || s.runs != 0 || strings.Contains(out, "Watch pass 2") {
		t.Fatalf("waited %t, learn runs %d\n%s", waited, s.runs, out)
	}
	if !strings.Contains(out, "acme/widgets#12 · interrupted") {
		t.Fatalf("the interrupted Issue was not reported:\n%s", out)
	}
}

func TestTriggersWatchExitCodeFollowsTheFinalPassAndAnErrorStopsIt(t *testing.T) {
	stubLearn(t)
	failed := molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{{Repository: "acme/widgets", Issue: 1, State: molecule.LoopStopped, Failed: true, Reason: "x"}}}}}
	stalled := molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{{Repository: "acme/widgets", Issue: 1, State: molecule.LoopStalled, Reason: "x"}}}}}
	clean := molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{}}}
	for name, c := range map[string]struct {
		reports []molecule.LoopReport
		want    int
	}{
		"final pass failed":     {[]molecule.LoopReport{clean, failed}, 2},
		"final pass stalled":    {[]molecule.LoopReport{failed, stalled}, 3},
		"final pass clean":      {[]molecule.LoopReport{failed, stalled, clean}, 0},
		"stalled beats nothing": {[]molecule.LoopReport{stalled}, 3},
	} {
		t.Run(name, func(t *testing.T) {
			n := 0
			stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
				n++
				return c.reports[n-1], nil
			})
			stop, cancel := context.WithCancel(context.Background())
			defer cancel()
			code, _, _ := triggersWatch(t, stop, func(time.Duration) bool {
				if n == len(c.reports) {
					cancel()
				}
				return true
			}, "--lab", t.TempDir(), "--watch")
			if code != c.want || n != len(c.reports) {
				t.Fatalf("exit %d after %d passes, want %d after %d", code, n, c.want, len(c.reports))
			}
		})
	}

	t.Run("a loop-level error", func(t *testing.T) {
		n := 0
		stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
			n++
			if n == 2 {
				return molecule.LoopReport{Repository: "acme/widgets"}, errors.New("GitHub data unavailable")
			}
			return clean, nil
		})
		waits := 0
		code, _, stderr := triggersWatch(t, context.Background(), func(time.Duration) bool { waits++; return true }, "--lab", t.TempDir(), "--watch")
		if code != 1 || n != 2 || waits != 1 || !strings.Contains(stderr, "GitHub data unavailable") {
			t.Fatalf("exit %d, %d passes, %d waits, stderr %q", code, n, waits, stderr)
		}
	})
}

func TestTriggersLearnIsOptInAndLeavesEveryDecisionToThePerson(t *testing.T) {
	s := stubLearn(t)
	s.results = []learning.PendingResult{
		{RequestID: timeoutRequest.RequestID, Class: timeoutRequest.Class, MoleculeIDs: timeoutRequest.MoleculeIDs, SuggestionID: "suggestion-abc"},
		{RequestID: "learn-request-aaaaaaaaaaaaaaaa", Class: molecule.FailureProtectedRegressionFailed, MoleculeIDs: []string{"mol-9"}, Err: "provider unavailable"},
	}
	s.items = []learning.PendingItem{timeoutRequest}
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return molecule.LoopReport{}, nil
	})
	lab := t.TempDir()

	code, out, _ := runLoopCLI("--lab", lab, "--once")
	if code != 0 || s.runs != 0 || !strings.Contains(out, "Pending learn request "+timeoutRequest.RequestID) {
		t.Fatalf("without --learn: exit %d, runs %d\n%s", code, s.runs, out)
	}

	code, out, stderr := runLoopCLI("--lab", lab, "--once", "--learn")
	if code != 0 {
		t.Fatalf("a failed learn request changed the loop exit code: %d", code)
	}
	if s.runs != 1 || s.evaluations != 2 || len(s.labs) != 1 || s.labs[0] != lab {
		t.Fatalf("runs %d evaluations %d labs %v", s.runs, s.evaluations, s.labs)
	}
	for _, want := range []string{"saved suggestion suggestion-abc", "forgecell suggestion suggestion-abc --lab " + shellWord(lab), "failed: provider unavailable", "stays pending"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--approve") || strings.Contains(out, "--dismiss") {
		t.Fatalf("the loop offered or took a decision:\n%s", out)
	}
	if !strings.Contains(stderr, "1 learn request(s) did not produce a suggestion") {
		t.Fatalf("the failed request was not reported: %q", stderr)
	}
}

func TestTriggersLearnRunsAfterEveryWatchPassAndNotAfterALoopError(t *testing.T) {
	s := stubLearn(t)
	passes := 0
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		passes++
		if passes == 3 {
			return molecule.LoopReport{}, errors.New("listing failed")
		}
		return molecule.LoopReport{}, nil
	})
	code, _, stderr := triggersWatch(t, context.Background(), func(time.Duration) bool { return true }, "--lab", t.TempDir(), "--watch", "--learn")
	if code != 1 || !strings.Contains(stderr, "listing failed") || passes != 3 {
		t.Fatalf("exit %d after %d passes: %s", code, passes, stderr)
	}
	if s.runs != 2 || s.evaluations != 3 {
		t.Fatalf("learn runs %d (want 2: one per completed pass), evaluations %d", s.runs, s.evaluations)
	}

	s.runs = 0
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return molecule.LoopReport{}, errors.New("listing failed")
	})
	if code, _, _ = runLoopCLI("--lab", t.TempDir(), "--once", "--learn"); code != 1 || s.runs != 0 {
		t.Fatalf("exit %d, learn runs %d after a loop-level error", code, s.runs)
	}
}

func TestTriggersLoopReportShowsTheRecoveryProposal(t *testing.T) {
	stubLearn(t)
	lab := t.TempDir()
	command := "forgecell recover mol-12-a --plan " + strings.Repeat("a", 64) + " --confirm-stopped --lab " + shellWord(lab)
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{
			{Repository: "acme/widgets", Issue: 12, MoleculeID: "mol-12-a", State: molecule.LoopStalled, Reason: "interrupted", Proposal: &molecule.LoopProposal{Path: filepath.Join(lab, "recovery-proposals", "mol-12-a.json"), Command: command, Followup: []string{"forgecell amend mol-12-a --parent x --plan FILE"}}},
			{Repository: "acme/widgets", Issue: 13, MoleculeID: "mol-13-a", State: molecule.LoopStalled, Reason: "interrupted", Proposal: &molecule.LoopProposal{Path: "p", Command: command, Existing: true}},
		}}}}, nil
	})
	code, out, _ := runLoopCLI("--lab", lab, "--once")
	if code != 3 {
		t.Fatalf("exit %d, want 3 for a stalled Issue", code)
	}
	for _, want := range []string{"Recovery proposal (saved; the loop never runs it): " + filepath.Join(lab, "recovery-proposals", "mol-12-a.json"), "    " + command, "then: forgecell amend mol-12-a", "Recovery proposal (already proposed; the loop never runs it): p"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestTriggersHelpListsTheFlagsAndKeepsTheSafetySentence(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"--help"}, nil, &out, &stderr, "test"); code != 0 {
		t.Fatal(stderr.String())
	}
	for _, want := range []string{"loop [--lab DIR] [--select oldest|label:L|milestone:M] [--max-concurrent N] [--once]", "--watch [--interval DURATION] [--learn]", "default 5m, at least 1m", "never delivers, merges or approves a human gate", "recovery proposal"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help does not mention %q", want)
		}
	}
}

func TestTriggersDoctorListsRecoveryProposalsReadOnly(t *testing.T) {
	stubLearn(t)
	_, base := doctorFixture(t)
	lab := filepath.Join(base, "lab $(touch pwned) 'x'")
	provider, _ := markerProvider(t)
	launcher := writeTool(t, t.TempDir(), "forgecell", "#!/bin/sh\nexit 0\n")
	saveBinding(t, lab, "codex", []string{launcher, "__adapter", "codex", provider})
	_, quiet := runDoctor(t, lab)
	if _, present := decodeDoctor(t, quiet)["recoveryProposals"]; present {
		t.Fatalf("doctor output must be unchanged when no proposal exists:\n%s", quiet)
	}

	digest := strings.Repeat("c", 64)
	id := "mol-12-abc"
	ledger := molecule.Record{
		ID: id, LabDir: lab, Status: "running", Issue: molecule.Issue{Number: 12, Repo: "acme/widgets"},
		Readiness:       &readiness.State{Phase: "scope-waiting", Plans: []readiness.Proposal{{Digest: digest}}},
		HarnessAttempts: []molecule.HarnessAttempt{{ID: "attempt-1", PlanDigest: digest, Result: process.Result{Reconciled: true}, Evidence: &molecule.VerificationFile{Path: "evidence/x.json", SHA256: strings.Repeat("a", 64)}}},
	}
	write := func(file string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ledgerFile := filepath.Join(lab, "ledgers", id+".json")
	proposalFile := filepath.Join(lab, "recovery-proposals", id+".json")
	write(ledgerFile, ledger)
	write(proposalFile, map[string]any{"schemaVersion": "v1", "moleculeId": id, "planDigest": digest, "command": "rm -rf /"})
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(ledgerFile, old, old); err != nil {
		t.Fatal(err)
	}
	ledgerBefore, _ := os.ReadFile(ledgerFile)
	proposalBefore, _ := os.ReadFile(proposalFile)

	_, out := runDoctor(t, lab)
	items, _ := decodeDoctor(t, out)["recoveryProposals"].([]any)
	want := "forgecell recover " + shellWord(id) + " --plan " + shellWord(digest) + " --confirm-stopped --lab " + shellWord(lab)
	if len(items) != 1 || items[0].(map[string]any)["command"] != want || items[0].(map[string]any)["moleculeId"] != id {
		t.Fatalf("%v", items)
	}
	_, human := runDoctor(t, lab, "--verbose")
	if !strings.Contains(human, "Recovery proposals") || !strings.Contains(human, want) || strings.Contains(human, "rm -rf") {
		t.Fatalf("%s", human)
	}
	if after, _ := os.ReadFile(ledgerFile); !bytes.Equal(after, ledgerBefore) {
		t.Fatal("doctor changed the ledger")
	}
	if after, _ := os.ReadFile(proposalFile); !bytes.Equal(after, proposalBefore) {
		t.Fatal("doctor changed the proposal")
	}
	if _, err := os.Stat(filepath.Join(lab, "pwned")); !os.IsNotExist(err) {
		t.Fatal("a command in the Lab path was executed")
	}

	now := time.Now()
	if err := os.Chtimes(ledgerFile, now, now); err != nil {
		t.Fatal(err)
	}
	_, out = runDoctor(t, lab)
	if _, present := decodeDoctor(t, out)["recoveryProposals"]; present {
		t.Fatalf("a proposal for a ledger that was written again was listed:\n%s", out)
	}
	if err := os.Chtimes(ledgerFile, old, old); err != nil {
		t.Fatal(err)
	}

	ledger.Status = "failed"
	write(ledgerFile, ledger)
	_, out = runDoctor(t, lab)
	if _, present := decodeDoctor(t, out)["recoveryProposals"]; present {
		t.Fatalf("a proposal for a ledger that is no longer interrupted was listed:\n%s", out)
	}
}

func TestTriggersTransientPassErrorsBackOffAndRecover(t *testing.T) {
	s := stubLearn(t)
	clean := molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{}}}
	transient := molecule.TransientLoopError{Err: errors.New("gh: network unreachable")}
	script := []error{nil, transient, transient, transient, transient, nil}
	n := 0
	stop, cancel := context.WithCancel(context.Background())
	defer cancel()
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		err := script[n]
		n++
		if err != nil {
			return molecule.LoopReport{Repository: "acme/widgets"}, err
		}
		return clean, nil
	})
	var waits []time.Duration
	code, _, stderr := triggersWatch(t, stop, func(d time.Duration) bool {
		waits = append(waits, d)
		if n == len(script) {
			cancel()
		}
		return true
	}, "--lab", t.TempDir(), "--watch", "--interval", "2m", "--learn")
	if code != 0 || n != len(script) {
		t.Fatalf("exit %d after %d passes: %s", code, n, stderr)
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 8 * time.Minute, 8 * time.Minute, 2 * time.Minute}
	if len(waits) != len(want) {
		t.Fatalf("waits %v, want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits %v, want %v", waits, want)
		}
	}
	if strings.Count(stderr, "network unreachable") != 4 || !strings.Contains(stderr, "the next pass is in 4m0s") {
		t.Fatalf("the transient errors were not reported:\n%s", stderr)
	}
	if s.runs != 2 {
		t.Fatalf("learn ran %d times, want only after the two successful passes", s.runs)
	}

	t.Run("stopped while failing", func(t *testing.T) {
		stop, cancel := context.WithCancel(context.Background())
		defer cancel()
		stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
			return molecule.LoopReport{Repository: "acme/widgets"}, transient
		})
		if code, _, _ := triggersWatch(t, stop, func(time.Duration) bool { cancel(); return false }, "--lab", t.TempDir(), "--watch"); code != 1 {
			t.Fatalf("exit %d, want 1 for a watch stopped while its passes fail", code)
		}
	})
	t.Run("a configuration error", func(t *testing.T) {
		stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
			return molecule.LoopReport{}, errors.New("the approved Formula could not be reloaded")
		})
		waited := false
		code, _, stderr := triggersWatch(t, context.Background(), func(time.Duration) bool { waited = true; return true }, "--lab", t.TempDir(), "--watch")
		if code != 1 || waited || !strings.Contains(stderr, "could not be reloaded") {
			t.Fatalf("exit %d waited %t: %s", code, waited, stderr)
		}
	})
}

func TestTriggersStopInterruptsAHungListing(t *testing.T) {
	stubLearn(t)
	stop, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	stubLoop(t, func(ctx context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		close(entered)
		<-ctx.Done()
		if o.Work.Err() != nil {
			t.Error("the stop request cancelled the work context")
		}
		return molecule.LoopReport{Repository: "acme/widgets"}, molecule.TransientLoopError{Err: ctx.Err()}
	})
	done := make(chan int, 1)
	go func() {
		code, _, _ := triggersWatch(t, stop, func(time.Duration) bool { t.Error("waited after a stop"); return false }, "--lab", t.TempDir(), "--watch")
		done <- code
	}()
	<-entered
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d: a pass interrupted by the stop request is not an error", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the stop request did not interrupt the hung listing")
	}
}

func TestTriggersLearnSkipsRequestsThatKeepFailingOnlyWhenTheLoopRunsThem(t *testing.T) {
	s := stubLearn(t)
	var seen []learning.PendingOptions
	runPendingLearn = func(_ context.Context, o learning.PendingOptions) ([]learning.PendingResult, error) {
		seen = append(seen, o)
		return []learning.PendingResult{{RequestID: "learn-request-aaaaaaaaaaaaaaaa", Class: molecule.FailureProcessTimedOut, MoleculeIDs: []string{"mol-1"}, Skipped: "waiting for operator: 3 failed attempts"}}, nil
	}
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return molecule.LoopReport{}, nil
	})
	lab := t.TempDir()

	code, out, stderr := runLoopCLI("--lab", lab, "--once", "--learn")
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if len(seen) != 1 || seen[0].MaxAttempts != 3 || seen[0].RetryAfter != 15*time.Minute {
		t.Fatalf("options %+v, want 3 attempts and a cooldown of three default intervals", seen)
	}
	if !strings.Contains(out, "waiting for operator: 3 failed attempts") || strings.Contains(out, "failed:") {
		t.Fatalf("the skipped request was not reported as waiting:\n%s", out)
	}

	seen = nil
	stop, cancel := context.WithCancel(context.Background())
	defer cancel()
	if code, _, _ = triggersWatch(t, stop, func(time.Duration) bool { cancel(); return true }, "--lab", lab, "--watch", "--interval", "10m", "--learn"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if len(seen) != 1 || seen[0].RetryAfter != 30*time.Minute {
		t.Fatalf("options %+v, want a cooldown of three intervals", seen)
	}

	seen = nil
	runPendingLearn = func(_ context.Context, o learning.PendingOptions) ([]learning.PendingResult, error) {
		seen = append(seen, o)
		return nil, nil
	}
	if code, _, _ = learnCLI("learn", "--pending", "--lab", lab); code != 0 || len(seen) != 1 || seen[0].MaxAttempts != 0 || seen[0].RetryAfter != 0 {
		t.Fatalf("learn --pending by hand was throttled: exit %d, %+v", code, seen)
	}
	if s.runs != 0 {
		t.Fatal("the shared stub ran")
	}
}
