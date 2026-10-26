package cli

import (
	"context"
	"flag"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/learning"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runLoop is a seam so tests can exercise rendering and exit codes without a
// full approved Lab.
var runLoop = molecule.RunLoop

const (
	defaultWatchInterval = 5 * time.Minute
	minWatchInterval     = time.Minute
)

// watchWait is a seam so tests never sleep. It reports false when the stop
// request arrived before the interval elapsed.
var watchWait = func(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// newResourceGuard builds the guard for a Lab's runs: the Lab's floors over the
// volumes holding TMPDIR, the Lab and GOCACHE, and available memory, with each
// soft-floor wait logged to stderr. A test binary gets no guard unless a test installs
// one through this seam, so a busy CI disk never holds an unrelated test for the
// guard's ten-minute wait.
var newResourceGuard = func(lab string, stderr io.Writer) (*resources.Guard, error) {
	if testing.Testing() {
		return nil, nil
	}
	guard, err := resources.NewGuard(lab)
	if err != nil {
		return nil, err
	}
	guard.Log = func(line string) { fmt.Fprintln(stderr, "forgecell:", line) }
	return guard, nil
}

// runLoopCommand validates every argument before it reads GitHub, writes a
// ledger or creates a Molecule. Exit codes mirror run: 1 for a loop-level error,
// 2 when a Molecule this run started or approved ended failed or blocked. Exit 3
// means the final pass left an Issue stalled (an approval that never applied or
// intake that never finished) or in a state the loop could not determine, which a
// wait at a human gate never does. With --watch the exit code is the one the
// final completed pass would produce under --once.
func runLoopCommand(ctx context.Context, args []string, defaultLab string, out, stderr io.Writer) int {
	flags := flag.NewFlagSet("loop", flag.ContinueOnError)
	flags.SetOutput(stderr)
	lab := flags.String("lab", defaultLab, "Lab directory")
	selection := flags.String("select", intake.SelectOldest, "Eligible Issues: oldest, label:NAME or milestone:TITLE")
	concurrent := flags.Int("max-concurrent", 1, "Molecules advanced at once (at least 1)")
	once := flags.Bool("once", false, "Make a single pass and exit")
	watch := flags.Bool("watch", false, "Repeat single passes every --interval until interrupted")
	interval := flags.Duration("interval", defaultWatchInterval, "Time between --watch passes (at least 1m)")
	learn := flags.Bool("learn", false, "Run the pending learn requests after each pass; suggestions stay pending")
	if err := flags.Parse(args); err != nil {
		return 1
	}
	fail := func(err error) int { fmt.Fprintln(stderr, "forgecell:", err); return 1 }
	if flags.NArg() != 0 {
		return fail(fmt.Errorf("loop accepts flags only"))
	}
	selector, err := intake.ParseSelector(*selection)
	if err != nil {
		return fail(err)
	}
	if *concurrent < 1 {
		return fail(fmt.Errorf("--max-concurrent must be at least 1"))
	}
	explicitInterval := false
	flags.Visit(func(f *flag.Flag) { explicitInterval = explicitInterval || f.Name == "interval" })
	if *watch && *once {
		return fail(fmt.Errorf("--watch repeats passes and cannot be combined with --once"))
	}
	if explicitInterval && !*watch {
		return fail(fmt.Errorf("--interval needs --watch"))
	}
	if *watch && *interval < minWatchInterval {
		return fail(fmt.Errorf("--interval must be at least %s", minWatchInterval))
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	guard, err := newResourceGuard(*lab, stderr)
	if err != nil {
		return fail(err)
	}
	options := molecule.LoopOptions{Template: molecule.Options{LabDir: *lab, SourceRoot: cwd, Resources: guard}, Select: selector, MaxConcurrent: *concurrent, Once: *once}
	if *watch {
		return watchLoop(ctx, options, *interval, *learn, *lab, out, stderr)
	}
	report, err := runLoop(ctx, options)
	renderLoop(out, report, selector, *concurrent)
	renderLearnNotices(out, stderr, *lab)
	if err != nil {
		return fail(err)
	}
	if *learn {
		runLearnAfterPass(ctx, *lab, *interval, out, stderr)
	}
	return loopExitCode(report)
}

// watchLoop makes one --once pass per interval. A stop request cancels ctx, which
// reaches only the Issue listing and the waits: Molecules already in flight run
// on a context the request does not cancel, and no further Issue or pass is
// started. A transient pass error (listing, ledger inventory) is reported and
// retried with exponential backoff capped at four intervals; any other error
// ends the watch. After a stop the exit code is the final completed pass's, or 1
// when the last pass ended in an error.
func watchLoop(ctx context.Context, options molecule.LoopOptions, interval time.Duration, learn bool, lab string, out, stderr io.Writer) int {
	options.Once = true
	options.Drain = ctx.Done()
	options.Work = context.WithoutCancel(ctx)
	code, failures := 0, 0
	errored := false
	for n := 1; ctx.Err() == nil; n++ {
		fmt.Fprintf(out, "Watch pass %d\n", n)
		// Below a soft floor the pass is skipped, with its reason in the pass report;
		// the next pass checks again after the interval.
		if guard := options.Template.Resources; guard != nil {
			if level, reason := guard.Check(); level != resources.OK {
				fmt.Fprintf(out, "Pass skipped: %s\n", reason)
				if !watchWait(ctx, interval) {
					break
				}
				continue
			}
		}
		report, err := runLoop(ctx, options)
		renderLoop(out, report, options.Select, options.MaxConcurrent)
		renderLearnNotices(out, stderr, lab)
		wait := interval
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if !molecule.IsTransientLoopError(err) {
				fmt.Fprintln(stderr, "forgecell:", err)
				return 1
			}
			failures++
			errored = true
			wait = min(interval<<min(failures, 2), 4*interval)
			fmt.Fprintf(stderr, "forgecell: %v; the next pass is in %s\n", err, wait)
		} else {
			failures, errored = 0, false
			code = loopExitCode(report)
			if learn && ctx.Err() == nil {
				runLearnAfterPass(ctx, lab, interval, out, stderr)
			}
		}
		if ctx.Err() != nil || !watchWait(ctx, wait) {
			break
		}
	}
	if errored {
		return 1
	}
	return code
}

// maxLearnAttempts is how many failed runs a pending learn request gets from the
// loop before it waits for a person.
const maxLearnAttempts = 3

// runLearnAfterPass runs the pending learn requests through the same
// learning.RunPending path as `forgecell learn --pending`, but skips a request
// that has failed maxLearnAttempts times or failed within three intervals, so a
// request that keeps failing does not start the bound model every pass. A person
// running the command by hand is never throttled. Failures are reported and never
// change the loop's exit code, and nothing is approved.
func runLearnAfterPass(ctx context.Context, lab string, interval time.Duration, out, stderr io.Writer) {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return
	}
	dir, err := filepath.Abs(lab)
	if err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return
	}
	results, err := runPendingLearn(ctx, learning.PendingOptions{LabDir: dir, Dir: cwd, MaxAttempts: maxLearnAttempts, RetryAfter: 3 * interval})
	if err != nil {
		fmt.Fprintln(stderr, "forgecell:", err)
		return
	}
	if len(results) == 0 {
		fmt.Fprintln(out, "No pending learn requests.")
	}
	failed := 0
	for _, r := range results {
		label := fmt.Sprintf("Learn request %s (%s, Molecules %s)", r.RequestID, r.Class, strings.Join(r.MoleculeIDs, ", "))
		switch {
		case r.Skipped != "":
			fmt.Fprintf(out, "%s: %s. It stays pending; run forgecell learn --pending --lab %s to retry it yourself.\n", label, flatten(r.Skipped), shellWord(dir))
		case r.Err != "":
			failed++
			fmt.Fprintf(out, "%s failed: %s\nThe request stays pending; run forgecell learn --pending --lab %s to retry.\n", label, flatten(r.Err), shellWord(dir))
		default:
			fmt.Fprintf(out, "%s saved suggestion %s (pending your decision).\nforgecell suggestion %s --lab %s\n", label, r.SuggestionID, r.SuggestionID, shellWord(dir))
		}
	}
	if failed > 0 {
		fmt.Fprintf(stderr, "forgecell: %d learn request(s) did not produce a suggestion\n", failed)
	}
}

func loopExitCode(report molecule.LoopReport) int {
	if report.Failed() {
		return 2
	}
	if report.NonProgress() > 0 {
		return 3
	}
	return 0
}

func renderLoop(out io.Writer, report molecule.LoopReport, selector intake.Selector, concurrent int) {
	if report.Repository == "" {
		return
	}
	fmt.Fprintf(out, "Lab loop for %s · select %s · max concurrent %d\n", report.Repository, selector, concurrent)
	for n, pass := range report.Passes {
		fmt.Fprintf(out, "Pass %d: %d eligible Issue(s)\n", n+1, len(pass.Items))
		for _, note := range pass.Notes {
			fmt.Fprintf(out, "Warning: %s\n", note)
		}
		for _, item := range pass.Items {
			id := item.MoleculeID
			if id == "" {
				id = "none"
			}
			actions := ""
			if item.Started {
				actions += " · started intake"
			}
			if item.Approved {
				actions += " · approved by policy"
			}
			fmt.Fprintf(out, "%s#%d · %s · Molecule %s%s\n  %s\n", item.Repository, item.Issue, item.State, id, actions, item.Reason)
			if p := item.Proposal; p != nil {
				saved := "saved"
				if p.Existing {
					saved = "already proposed"
				}
				fmt.Fprintf(out, "  Recovery proposal (%s; the loop never runs it): %s\n    %s\n", saved, flatten(p.Path), p.Command)
				for _, step := range p.Followup {
					fmt.Fprintf(out, "    then: %s\n", step)
				}
			}
		}
		if pass.Stopped != "" {
			fmt.Fprintf(out, "Pass %d stopped: %s\n", n+1, flatten(pass.Stopped))
		}
	}
	if n := report.NonProgress(); n > 0 {
		fmt.Fprintf(out, "%d Issue(s) made no progress in the final pass and need attention\n", n)
	}
}
