package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// Base-flake classification. A cohort attempt whose hidden acceptance passed can still fail
// a regression check on a test that is flaky in the historical code itself, so the model's
// change did not cause it. Each failed regression test is re-run, alone, exactly
// baseFlakeRuns times in a fresh checkout of the attempt's base commit (from the per-attempt
// source, which holds only that commit), after the plan's setup, under the scrubbed Go
// environment. A test that both passes and fails there is FlakyAtBase; one that fails every
// run is BrokenAtBase (it may be the very bug the PR fixes, so it never excuses a failure).
const (
	// baseFlakeMaxTests caps the tests re-run per attempt; the rest are unclassified.
	baseFlakeMaxTests = 10
	// baseFlakeRuns is how many times each test is re-run at base, with no early stop.
	baseFlakeRuns = 3
	// baseFlakeTimeout bounds each base re-run.
	baseFlakeTimeout = 5 * time.Minute
)

// RegressionFailure is one Go test a failed regression check reported failing, with its base
// re-runs: BaseFails and BasePasses count the decided runs. FlakyAtBase marks a test that
// passed at least once and failed at least once at base; BrokenAtBase one that failed every
// run. Unclassified marks a test that was not re-run (beyond the per-attempt cap, its
// package unknown, or the base setup failed) or whose runs could not be decided as either
// (the package did not build at base, the test does not exist there, or a run did not
// complete). A test that passed every run is none of these.
type RegressionFailure struct {
	Package      string `json:"package"`
	Test         string `json:"test"`
	FlakyAtBase  bool   `json:"flakyAtBase"`
	BrokenAtBase bool   `json:"brokenAtBase,omitempty"`
	BaseFails    int    `json:"baseFails,omitempty"`
	BasePasses   int    `json:"basePasses,omitempty"`
	Unclassified bool   `json:"unclassified,omitempty"`
}

// baseRunOutcome is the result of one base re-run of one test.
type baseRunOutcome int

const (
	baseRunPassed baseRunOutcome = iota
	baseRunFailed
	baseRunUndecided
)

// regressionCheckFailure is one failed non-acceptance check observation and its failed tests.
type regressionCheckFailure struct {
	check    string
	failures []GoTestFailure
}

// baseFlakeItem is one failed test to classify, with the plan directory its check ran in.
type baseFlakeItem struct {
	dir     string
	failure GoTestFailure
}

// goImportPath bounds the package argument of a base re-run to an import path, so text a
// candidate printed in its output can never become a go test flag.
var goImportPath = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.~+/-]*$`)

// classifyRegressionFailures records the attempt's failed regression tests with their
// packages, re-runs them at the plan's base commit in source (the per-attempt source, after
// the plan's setup, with acceptance substituted as verification does) to classify them, and
// sets CorrectExcludingBaseFlakes. Like RegressionFailedTests it applies only to an attempt
// whose hidden acceptance passed. It must run before the per-attempt source is reclaimed.
func classifyRegressionFailures(ctx context.Context, a *Attempt, p Plan, source, acceptance string) {
	a.RegressionFailures, a.CorrectExcludingBaseFlakes = nil, a.Correct
	if a.HiddenAcceptance == nil || !*a.HiddenAcceptance {
		return
	}
	checks := collectRegressionFailures(*a)
	dirs := map[string]string{}
	for _, c := range p.Checks {
		dirs[c.Name] = c.Dir
	}
	seen := map[GoTestFailure]bool{}
	var items []baseFlakeItem
	for _, c := range checks {
		for _, f := range c.failures {
			if !seen[f] {
				seen[f] = true
				items = append(items, baseFlakeItem{dir: dirs[c.check], failure: f})
			}
		}
	}
	// Classify in package and test order, so the cap keeps the same tests on every run.
	sort.SliceStable(items, func(i, j int) bool {
		x, y := items[i].failure, items[j].failure
		if x.Package != y.Package {
			return x.Package < y.Package
		}
		return x.Test < y.Test
	})
	a.RegressionFailures = classifyBaseFlakes(ctx, source, p.BaseCommit, p.Setup, acceptance, items)
	a.CorrectExcludingBaseFlakes = a.Correct || correctExcludingBaseFlakes(*a, p)
}

// collectRegressionFailures returns the failed tests of every failed non-acceptance check of
// the attempt, to classify: the Molecule's candidate and protected views (from the full
// streams in its history) and the independent verification.
func collectRegressionFailures(a Attempt) []regressionCheckFailure {
	var out []regressionCheckFailure
	for _, f := range a.FailedCheckOutput {
		if f.Phase != failedPhaseCheck || f.ID == "acceptance" {
			continue
		}
		out = append(out, regressionCheckFailure{check: f.ID, failures: f.FailedTestPackages})
	}
	for _, obs := range evaluationVerification(a).Checks {
		if obs.Command.Name == "acceptance" || obs.Result.OK {
			continue
		}
		failures, _ := goTestFailures(harness.RedactSecrets(obs.Result.Stdout), harness.RedactSecrets(obs.Result.Stderr))
		out = append(out, regressionCheckFailure{check: obs.Command.Name, failures: failures})
	}
	return out
}

// correctExcludingBaseFlakes reports whether the attempt would be correct but for tests that
// are flaky at base, judged only from the evaluation's independent verification (which runs
// every plan check, never stopping at a failure), never from the Molecule's fail-fast
// verification (EvaluationVerification when the Molecule's verification stopped the tree):
// the attempt completed validly within scope and that verification ran to the end with
// every plan check observed and complete; acceptance passed; every other check
// passed except go test regression checks; and each failed go test check failed only on
// named tests (no panic, timeout, build or setup failure, or package failure outside a
// test, and a stream not cut short), every one of them FlakyAtBase. A check left unobserved
// is never assumed to have passed. The Molecule's own failures must agree as well
// (moleculeFailuresExcused).
func correctExcludingBaseFlakes(a Attempt, p Plan) bool {
	if a.Invalid != "" || !completed(a) || a.HiddenAcceptance == nil || !*a.HiddenAcceptance || !a.ScopeOK || !commitPattern.MatchString(a.Tree) {
		return false
	}
	// A stopped tree's attempt keeps the refusal as its Error; its own run must not error.
	v := evaluationVerification(a)
	if (a.Error != "" && a.EvaluationVerification == nil) || v.Error != "" || len(p.Checks) == 0 || len(v.Checks) != len(p.Checks) {
		return false
	}
	observed := map[string]Observation{}
	for _, obs := range v.Checks {
		observed[obs.Command.Name] = obs
	}
	flaky := map[GoTestFailure]bool{}
	for _, f := range a.RegressionFailures {
		if f.FlakyAtBase && !f.BrokenAtBase && !f.Unclassified {
			flaky[GoTestFailure{Package: f.Package, Test: f.Test}] = true
		}
	}
	excused := false
	for _, c := range p.Checks {
		obs, ok := observed[c.Name]
		if !ok || obs.Result.Incomplete() {
			return false
		}
		if obs.Result.OK {
			continue
		}
		if c.Name == "acceptance" || !goTestCommand(c) {
			return false
		}
		failures, unattributed := goTestFailures(harness.RedactSecrets(obs.Result.Stdout), harness.RedactSecrets(obs.Result.Stderr))
		if len(failures) == 0 || unattributed || streamCut(obs.Result) {
			return false
		}
		for _, f := range failures {
			if !flaky[f] {
				return false
			}
		}
		excused = true
	}
	return excused && moleculeFailuresExcused(a, p, flaky)
}

// moleculeFailuresExcused reports whether the Molecule's own failure evidence agrees with the
// independent verification's: every failure recorded in FailedCheckOutput (candidate and
// protected views) is a failed go test plan check whose failures are all named tests in
// flaky, with no unattributed failure and a complete failure list. A failed setup command,
// acceptance, any other check, or a go test failure on a test not flaky at base (one the
// candidate broke deterministically, even when the independent run did not see it) is not.
func moleculeFailuresExcused(a Attempt, p Plan, flaky map[GoTestFailure]bool) bool {
	checks := map[string]Command{}
	for _, c := range p.Checks {
		checks[c.Name] = c
	}
	for _, f := range a.FailedCheckOutput {
		c, ok := checks[f.ID]
		if f.Phase != failedPhaseCheck || !ok || c.Name == "acceptance" || !goTestCommand(c) {
			return false
		}
		if len(f.FailedTestPackages) == 0 || f.UnattributedFailure || f.FailedTestsTruncated {
			return false
		}
		for _, t := range f.FailedTestPackages {
			if !flaky[t] {
				return false
			}
		}
	}
	return true
}

// goTestCommand reports whether a plan check runs `go test`.
func goTestCommand(c Command) bool {
	return len(c.Argv) >= 2 && filepath.Base(c.Argv[0]) == "go" && c.Argv[1] == "test"
}

// classifyBaseFlakes re-runs up to baseFlakeMaxTests of items, in order, at base in a fresh
// worktree of source, after running setup there, each exactly baseFlakeRuns times, and
// returns one RegressionFailure per item with its counts. Items beyond the cap, with an
// unknown or unsafe package, or whose base checkout or setup could not be prepared are
// Unclassified, as is any test whose decided runs make it neither flaky nor broken nor
// passing every run.
func classifyBaseFlakes(ctx context.Context, source, base string, setup []Command, acceptance string, items []baseFlakeItem) []RegressionFailure {
	if len(items) == 0 {
		return nil
	}
	out := make([]RegressionFailure, len(items))
	var runnable []int
	for i, it := range items {
		out[i] = RegressionFailure{Package: it.failure.Package, Test: it.failure.Test}
		if !goImportPath.MatchString(it.failure.Package) || it.failure.Test == "" || !relative(it.dir, true) || len(runnable) >= baseFlakeMaxTests {
			out[i].Unclassified = true
			continue
		}
		runnable = append(runnable, i)
	}
	if len(runnable) == 0 {
		return out
	}
	unclassified := func() []RegressionFailure {
		for _, i := range runnable {
			out[i].Unclassified = true
		}
		return out
	}
	gomodcache, gocache, ok := resolveGoCache(ctx)
	if !ok {
		return unclassified()
	}
	parent, err := resources.MkdirTemp("", "forgecell-base-flakes-")
	if err != nil {
		return unclassified()
	}
	// The private HOME below can hold a read-only Go tree, so removal makes it writable
	// first; a failed removal stays registered for exit cleanup.
	defer resources.Remove(parent)
	worktree := filepath.Join(parent, "wt")
	release, err := resources.LockWorktrees(ctx, source)
	if err != nil {
		return unclassified()
	}
	added := gitHookless(ctx, source, "worktree", "add", "--detach", worktree, base)
	release()
	if !added.OK {
		return unclassified()
	}
	// Cleanup runs even after a cancellation, so the per-attempt source never keeps a
	// registered base worktree.
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		release, err := resources.LockWorktrees(cleanup, source)
		if err != nil {
			return
		}
		defer release()
		gitHookless(cleanup, source, "worktree", "remove", "--force", worktree)
		gitHookless(cleanup, source, "worktree", "prune")
	}()
	home, err := os.MkdirTemp(parent, "home-")
	if err != nil {
		return unclassified()
	}
	// A cohort attempt bounds each go command's build parallelism through GOFLAGS.
	env := resources.ApplyGoParallelism(ctx, scrubbedGoEnv(home, gomodcache, gocache))
	if !runBaseSetup(ctx, worktree, env, setup, acceptance) {
		return unclassified()
	}
	for _, i := range runnable {
		it := items[i]
		for run := 0; run < baseFlakeRuns; run++ {
			switch runBaseTest(ctx, filepath.Join(worktree, filepath.FromSlash(it.dir)), env, it.failure.Package, it.failure.Test) {
			case baseRunFailed:
				out[i].BaseFails++
			case baseRunPassed:
				out[i].BasePasses++
			}
		}
		switch f := &out[i]; {
		case f.BaseFails > 0 && f.BasePasses > 0:
			f.FlakyAtBase = true
		case f.BaseFails == baseFlakeRuns:
			f.BrokenAtBase = true
		case f.BasePasses != baseFlakeRuns:
			f.Unclassified = true
		}
	}
	return out
}

// runBaseSetup runs the plan's setup commands in the base worktree as verification does
// (each from its directory with its own timeout, {acceptance} and {workspace} substituted),
// under the base re-runs' scrubbed environment. It reports whether every command passed.
func runBaseSetup(ctx context.Context, worktree string, env []string, setup []Command, acceptance string) bool {
	env = append(append([]string(nil), env...), "FORGECELL_EVAL_WORKSPACE="+worktree)
	for _, c := range setup {
		if !relative(c.Dir, true) || len(c.Argv) == 0 {
			return false
		}
		argv := make([]string, len(c.Argv))
		for i, v := range c.Argv {
			argv[i] = strings.ReplaceAll(strings.ReplaceAll(v, "{acceptance}", acceptance), "{workspace}", worktree)
		}
		r := process.Run(ctx, process.Options{Argv: argv, Dir: filepath.Join(worktree, c.Dir), Env: env, Stdin: []byte{}, Timeout: time.Duration(c.TimeoutMS) * time.Millisecond, MaxOutputBytes: 2_000_000})
		if !r.OK {
			return false
		}
	}
	return true
}

// runBaseTest runs one test once at base: `go test -count=1 -run '^<name>$' <pkg>` (with
// -json to read the result, each subtest level anchored on its own) under env from dir. It
// is a variable only so tests can count and script the base runs.
var runBaseTest = func(ctx context.Context, dir string, env []string, pkg, test string) baseRunOutcome {
	r := process.Run(ctx, process.Options{
		Argv:           []string{"go", "test", "-json", "-count=1", "-buildvcs=false", "-run", baseTestRunPattern(test), pkg},
		Dir:            dir,
		Env:            env,
		Stdin:          []byte{},
		Timeout:        baseFlakeTimeout,
		MaxOutputBytes: 4_000_000,
	})
	return baseRunResult(r, test)
}

// baseTestRunPattern anchors each level of a test name (TestX/sub) for `go test -run`, so
// only that test runs; a top-level name yields `^<name>$`.
func baseTestRunPattern(test string) string {
	levels := strings.Split(test, "/")
	for i, l := range levels {
		levels[i] = "^" + regexp.QuoteMeta(l) + "$"
	}
	return strings.Join(levels, "/")
}

// baseRunResult decides one base run from its go test -json events. The test failed when its
// own event reports it failing, or the run hit its timeout after the test started and before
// it passed; it passed when the run succeeded with the test passing. Anything else (no such
// test or package at base, a build failure, a non-zero exit after the test passed, an
// interrupted or overflowing run) is undecided.
func baseRunResult(r process.Result, test string) baseRunOutcome {
	ran, failed, passed := false, false, false
	for _, line := range strings.Split(r.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev goTestEvent
		if json.Unmarshal([]byte(line), &ev) != nil || ev.Test != test {
			continue
		}
		switch ev.Action {
		case "run":
			ran = true
		case "fail":
			failed = true
		case "pass":
			passed = true
		}
	}
	switch {
	case r.Interrupted || r.ParentDeadline || r.Overflow:
		return baseRunUndecided
	case failed || (r.TimedOut && ran && !passed):
		return baseRunFailed
	case r.OK && passed:
		return baseRunPassed
	}
	return baseRunUndecided
}

// baseFlakeMark is the report's label for a classified regression failure, empty when it
// passed every base run or was not classified.
func baseFlakeMark(f RegressionFailure) string {
	switch {
	case f.Unclassified:
		return ""
	case f.FlakyAtBase:
		return fmt.Sprintf("flaky at base (%d/%d failed)", f.BaseFails, baseFlakeRuns)
	case f.BrokenAtBase:
		return "broken at base"
	}
	return ""
}
