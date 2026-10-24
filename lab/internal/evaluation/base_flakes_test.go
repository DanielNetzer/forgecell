package evaluation

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// baseFlakeFixture commits a base with a test that always fails, one that always passes, one
// that fails only on its second run (counted in a state file outside the checkout) and one
// that passes only when the file ready.txt, which setup creates, is present, and returns the
// repo and base commit.
func baseFlakeFixture(t *testing.T) (*fixtureRepo, string) {
	t.Helper()
	f, _, _ := newFixture(t)
	state := filepath.Join(t.TempDir(), "runs")
	f.write("lab/flaky/flaky_test.go", fmt.Sprintf(`package flaky

import (
	"os"
	"testing"
)

func TestAlwaysFails(t *testing.T) { t.Fatal("broken at base") }

func TestAlwaysPasses(t *testing.T) {}

func TestSecondRunFails(t *testing.T) {
	f, err := os.OpenFile(%q, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("x")
	f.Close()
	b, _ := os.ReadFile(%q)
	if len(b) == 2 {
		t.Fatal("flaked")
	}
}

func TestNeedsSetup(t *testing.T) {
	if _, err := os.Stat("ready.txt"); err != nil {
		t.Fatal(err)
	}
}
`, state, state))
	f.git("add", ".")
	f.git("commit", "-qm", "base with flaky tests")
	return f, f.head()
}

// countBaseRuns wraps runBaseTest to count the real base runs per test.
func countBaseRuns(t *testing.T) map[string]int {
	t.Helper()
	var mu sync.Mutex
	runs := map[string]int{}
	original := runBaseTest
	runBaseTest = func(ctx context.Context, dir string, env []string, p, test string) baseRunOutcome {
		mu.Lock()
		runs[test]++
		mu.Unlock()
		return original(ctx, dir, env, p, test)
	}
	t.Cleanup(func() { runBaseTest = original })
	return runs
}

// TestClassifyBaseFlakesRunsGoTestAtBase re-runs real tests at the base commit, each exactly
// three times: one that fails every run is broken at base (not flaky), one that fails only
// on its second run is flaky at base (1/3 failed), one that passes three times is neither,
// and a test or package that does not exist at base is unclassified.
func TestClassifyBaseFlakesRunsGoTestAtBase(t *testing.T) {
	skipToolchainInShort(t)
	f, base := baseFlakeFixture(t)
	pkg := "forgecellreplayfixture/flaky"
	runs := countBaseRuns(t)
	items := []baseFlakeItem{
		{dir: "lab", failure: GoTestFailure{Package: pkg, Test: "TestAlwaysFails"}},
		{dir: "lab", failure: GoTestFailure{Package: pkg, Test: "TestAlwaysPasses"}},
		{dir: "lab", failure: GoTestFailure{Package: pkg, Test: "TestSecondRunFails"}},
		{dir: "lab", failure: GoTestFailure{Package: pkg, Test: "TestAddedByTheCandidate"}},
		{dir: "lab", failure: GoTestFailure{Package: "forgecellreplayfixture/missing", Test: "TestX"}},
	}
	got := classifyBaseFlakes(context.Background(), f.dir, base, nil, "", items)
	want := []RegressionFailure{
		{Package: pkg, Test: "TestAlwaysFails", BrokenAtBase: true, BaseFails: 3},
		{Package: pkg, Test: "TestAlwaysPasses", BasePasses: 3},
		{Package: pkg, Test: "TestSecondRunFails", FlakyAtBase: true, BaseFails: 1, BasePasses: 2},
		{Package: pkg, Test: "TestAddedByTheCandidate", Unclassified: true},
		{Package: "forgecellreplayfixture/missing", Test: "TestX", Unclassified: true},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("classified %+v\nwant %+v", got, want)
	}
	for _, it := range items {
		if runs[it.failure.Test] != baseFlakeRuns {
			t.Fatalf("base runs = %v, want %d for every test", runs, baseFlakeRuns)
		}
	}
	// The base checkout is removed and deregistered afterwards.
	if wl := f.git("worktree", "list", "--porcelain"); strings.Count(wl, "worktree ") != 1 {
		t.Fatalf("base re-run left a worktree registered:\n%s", wl)
	}
}

// TestClassifyBaseFlakesRunsSetupFirst: the plan's setup runs in the base checkout before the
// re-runs, so a test that needs a file setup creates passes there; without setup it fails
// every run; and a failing setup leaves every test unclassified with no re-run.
func TestClassifyBaseFlakesRunsSetupFirst(t *testing.T) {
	skipToolchainInShort(t)
	f, base := baseFlakeFixture(t)
	item := []baseFlakeItem{{dir: "lab", failure: GoTestFailure{Package: "forgecellreplayfixture/flaky", Test: "TestNeedsSetup"}}}
	runs := countBaseRuns(t)
	setup := []Command{{Name: "ready", Dir: "lab/flaky", Argv: []string{"sh", "-c", "printf ok > ready.txt"}, TimeoutMS: 30_000}}
	if got := classifyBaseFlakes(context.Background(), f.dir, base, setup, "", item); fmt.Sprint(got) != fmt.Sprint([]RegressionFailure{{Package: "forgecellreplayfixture/flaky", Test: "TestNeedsSetup", BasePasses: 3}}) {
		t.Fatalf("with setup: %+v", got)
	}
	if got := classifyBaseFlakes(context.Background(), f.dir, base, nil, "", item); len(got) != 1 || !got[0].BrokenAtBase {
		t.Fatalf("without setup: %+v", got)
	}
	runs["TestNeedsSetup"] = 0
	failing := []Command{{Name: "broken", Dir: "lab", Argv: []string{"sh", "-c", "exit 3"}, TimeoutMS: 30_000}}
	if got := classifyBaseFlakes(context.Background(), f.dir, base, failing, "", item); len(got) != 1 || !got[0].Unclassified || got[0].FlakyAtBase || got[0].BrokenAtBase || runs["TestNeedsSetup"] != 0 {
		t.Fatalf("failing setup: %+v runs=%v", got, runs)
	}
}

// TestClassifyBaseFlakesCapsReruns: at most baseFlakeMaxTests tests are re-run per attempt,
// in order, each exactly baseFlakeRuns times; the rest, and any test with no or an unsafe
// package, are unclassified.
func TestClassifyBaseFlakesCapsReruns(t *testing.T) {
	f, base, _ := newFixture(t)
	var mu sync.Mutex
	ran := map[string]int{}
	original := runBaseTest
	runBaseTest = func(_ context.Context, _ string, _ []string, pkg, test string) baseRunOutcome {
		mu.Lock()
		defer mu.Unlock()
		ran[test]++
		if ran[test] == 2 {
			return baseRunPassed
		}
		return baseRunFailed
	}
	t.Cleanup(func() { runBaseTest = original })
	items := []baseFlakeItem{
		{dir: "lab", failure: GoTestFailure{Test: "TestNoPackage"}},
		{dir: "lab", failure: GoTestFailure{Package: "-exec=/bin/false", Test: "TestUnsafe"}},
	}
	for i := 0; i < 12; i++ {
		items = append(items, baseFlakeItem{dir: "lab", failure: GoTestFailure{Package: "example.test/pkg", Test: fmt.Sprintf("Test%02d", i)}})
	}
	got := classifyBaseFlakes(context.Background(), f.dir, base, nil, "", items)
	if len(ran) != baseFlakeMaxTests {
		t.Fatalf("re-ran %d tests %v, want %d", len(ran), ran, baseFlakeMaxTests)
	}
	for test, n := range ran {
		if n != baseFlakeRuns {
			t.Fatalf("%s re-ran %d times, want %d", test, n, baseFlakeRuns)
		}
	}
	for i, r := range got {
		switch {
		case i < 2 || i >= 2+baseFlakeMaxTests:
			if !r.Unclassified || r.FlakyAtBase {
				t.Fatalf("item %d = %+v, want unclassified", i, r)
			}
		case !r.FlakyAtBase || r.Unclassified || r.BaseFails != 2 || r.BasePasses != 1:
			t.Fatalf("item %d = %+v, want flaky at base (2/3 failed)", i, r)
		}
	}
}

// TestBaseRunResult decides one base run from its go test -json events: the test's own fail
// event or a timeout after it started fails it; a non-zero exit after its pass event, like
// any other unclear result, is undecided.
func TestBaseRunResult(t *testing.T) {
	ev := func(action, test string) string {
		return fmt.Sprintf(`{"Action":%q,"Package":"p","Test":%q}`, action, test)
	}
	for name, c := range map[string]struct {
		r    process.Result
		want baseRunOutcome
	}{
		"pass":                 {process.Result{OK: true, Stdout: ev("run", "TestX") + "\n" + ev("pass", "TestX")}, baseRunPassed},
		"fail":                 {process.Result{Code: 1, Stdout: ev("run", "TestX") + "\n" + ev("fail", "TestX")}, baseRunFailed},
		"timed out":            {process.Result{TimedOut: true, Code: -1, Stdout: ev("run", "TestX")}, baseRunFailed},
		"exit without event":   {process.Result{Code: 1, Stdout: ev("run", "TestX")}, baseRunUndecided},
		"exit after pass":      {process.Result{Code: 1, Stdout: ev("run", "TestX") + "\n" + ev("pass", "TestX") + "\n" + `{"Action":"fail","Package":"p"}`}, baseRunUndecided},
		"timed out after pass": {process.Result{TimedOut: true, Code: -1, Stdout: ev("run", "TestX") + "\n" + ev("pass", "TestX")}, baseRunUndecided},
		"timed out before run": {process.Result{TimedOut: true, Code: -1}, baseRunUndecided},
		"no such test":         {process.Result{OK: true, Stdout: `{"Action":"pass","Package":"p"}`}, baseRunUndecided},
		"build failure":        {process.Result{Code: 1, Stdout: `{"Action":"fail","Package":"p"}`}, baseRunUndecided},
		"interrupted":          {process.Result{Interrupted: true, Stdout: ev("run", "TestX") + "\n" + ev("fail", "TestX")}, baseRunUndecided},
		"other test":           {process.Result{Code: 1, Stdout: ev("fail", "TestY")}, baseRunUndecided},
	} {
		if got := baseRunResult(c.r, "TestX"); got != c.want {
			t.Fatalf("%s: outcome %d, want %d", name, got, c.want)
		}
	}
	if got := baseTestRunPattern("TestX/sub.case"); got != `^TestX$/^sub\.case$` {
		t.Fatalf("subtest pattern = %q", got)
	}
	if got := baseTestRunPattern("TestX"); got != "^TestX$" {
		t.Fatalf("top-level pattern = %q", got)
	}
}

// TestCorrectExcludingBaseFlakesFromIndependentVerification: the flag is judged from the
// independent verification alone. With every check observed, acceptance, go vet and gofmt
// passing and go test failing only on a test flaky at base, the attempt is correct
// excluding base flakes; a gofmt or vet failure, a test broken at or stable at base, a
// non-test failure (a timeout panic) beside the flaky test, an unobserved check, or an
// independent verification that never ran (the Molecule's fail-fast run stopped at go
// test) keep it false. Correct is never changed.
func TestCorrectExcludingBaseFlakesFromIndependentVerification(t *testing.T) {
	f, base, _ := newFixture(t)
	scripted := map[string][]baseRunOutcome{
		"TestFlaky":  {baseRunFailed, baseRunPassed, baseRunFailed},
		"TestBroken": {baseRunFailed, baseRunFailed, baseRunFailed},
		"TestStable": {baseRunPassed, baseRunPassed, baseRunPassed},
	}
	runs := map[string]int{}
	original := runBaseTest
	runBaseTest = func(_ context.Context, _ string, _ []string, _, test string) baseRunOutcome {
		n := runs[test]
		runs[test]++
		return scripted[test][n%baseFlakeRuns]
	}
	t.Cleanup(func() { runBaseTest = original })

	yes := true
	goTestCmd := Command{Name: "go-test", Dir: "lab", Argv: []string{"go", "test", "./..."}}
	vetCmd := Command{Name: "go-vet", Dir: "lab", Argv: []string{"go", "vet", "./..."}}
	gofmtCmd := Command{Name: "gofmt", Dir: "lab", Argv: []string{"sh", "-c", `test -z "$(gofmt -l internal cmd)"`}}
	acceptanceCmd := Command{Name: "acceptance", Dir: ".", Argv: []string{"/bin/sh", "{acceptance}"}}
	plan := Plan{BaseCommit: base, Checks: []Command{acceptanceCmd, goTestCmd, vetCmd, gofmtCmd}}
	plainFail := func(tests ...string) string {
		var b strings.Builder
		for _, test := range tests {
			b.WriteString("--- FAIL: " + test + " (0.00s)\n")
		}
		return b.String() + "FAIL\nFAIL\tforgecellreplayfixture/flaky\t0.01s\nFAIL\n"
	}
	pass := func(c Command) Observation { return Observation{Command: c, Result: process.Result{OK: true}} }
	fail := func(c Command, stdout string) Observation {
		return Observation{Command: c, Result: process.Result{Code: 1, Stdout: stdout}}
	}
	attempt := func(checks ...Observation) Attempt {
		a := Attempt{HiddenAcceptance: &yes, ScopeOK: true, Tree: strings.Repeat("a", 40), Molecule: molecule.Record{Atoms: []molecule.Atom{{Type: "harness", Status: "done"}}}}
		a.Verification.Checks = append([]Observation{pass(acceptanceCmd)}, checks...)
		return a
	}
	flakyFailure := RegressionFailure{Package: "forgecellreplayfixture/flaky", Test: "TestFlaky", FlakyAtBase: true, BaseFails: 2, BasePasses: 1}

	flaky := attempt(fail(goTestCmd, plainFail("TestFlaky")), pass(vetCmd), pass(gofmtCmd))
	classifyRegressionFailures(context.Background(), &flaky, plan, f.dir, "")
	if fmt.Sprint(flaky.RegressionFailures) != fmt.Sprint([]RegressionFailure{flakyFailure}) || !flaky.CorrectExcludingBaseFlakes || flaky.Correct {
		t.Fatalf("flaky attempt failures=%+v correctExcludingBaseFlakes=%v correct=%v", flaky.RegressionFailures, flaky.CorrectExcludingBaseFlakes, flaky.Correct)
	}

	timeoutPanic := "--- FAIL: TestFlaky (0.00s)\npanic: test timed out after 1s\nFAIL\tforgecellreplayfixture/flaky\t1.01s\nFAIL\n"
	for name, c := range map[string]struct {
		a    Attempt
		want []RegressionFailure
	}{
		"gofmt violation":  {attempt(fail(goTestCmd, plainFail("TestFlaky")), pass(vetCmd), fail(gofmtCmd, "")), []RegressionFailure{flakyFailure}},
		"vet failure":      {attempt(fail(goTestCmd, plainFail("TestFlaky")), fail(vetCmd, "# forgecellreplayfixture/calc\nself-assignment of v to v\n"), pass(gofmtCmd)), []RegressionFailure{flakyFailure}},
		"broken at base":   {attempt(fail(goTestCmd, plainFail("TestBroken")), pass(vetCmd), pass(gofmtCmd)), []RegressionFailure{{Package: "forgecellreplayfixture/flaky", Test: "TestBroken", BrokenAtBase: true, BaseFails: 3}}},
		"stable at base":   {attempt(fail(goTestCmd, plainFail("TestStable")), pass(vetCmd), pass(gofmtCmd)), []RegressionFailure{{Package: "forgecellreplayfixture/flaky", Test: "TestStable", BasePasses: 3}}},
		"flaky and stable": {attempt(fail(goTestCmd, plainFail("TestFlaky", "TestStable")), pass(vetCmd), pass(gofmtCmd)), []RegressionFailure{flakyFailure, {Package: "forgecellreplayfixture/flaky", Test: "TestStable", BasePasses: 3}}},
		"timeout panic":    {attempt(fail(goTestCmd, timeoutPanic), pass(vetCmd), pass(gofmtCmd)), []RegressionFailure{flakyFailure}},
		"gofmt unobserved": {attempt(fail(goTestCmd, plainFail("TestFlaky")), pass(vetCmd)), []RegressionFailure{flakyFailure}},
	} {
		classifyRegressionFailures(context.Background(), &c.a, plan, f.dir, "")
		if c.a.CorrectExcludingBaseFlakes || c.a.Correct || fmt.Sprint(c.a.RegressionFailures) != fmt.Sprint(c.want) {
			t.Fatalf("%s: failures=%+v correctExcludingBaseFlakes=%v, want %+v and false", name, c.a.RegressionFailures, c.a.CorrectExcludingBaseFlakes, c.want)
		}
	}

	// The Molecule's fail-fast verification stopped at go test on the flaky test, so the
	// independent verification refused to run: vet and gofmt were never observed, which
	// never counts as passing, although the test is still classified.
	stopped := attempt()
	stopped.Verification = Verification{Error: "run contains failed or blocked Atoms"}
	stopped.Error = stopped.Verification.Error
	stopped.FailedCheckOutput = []FailedCheckOutput{{ID: "go-test", Phase: failedPhaseCheck, Code: 1, FailedTests: []string{"TestFlaky"}, FailedTestPackages: []GoTestFailure{{"forgecellreplayfixture/flaky", "TestFlaky"}}}}
	classifyRegressionFailures(context.Background(), &stopped, plan, f.dir, "")
	if stopped.CorrectExcludingBaseFlakes || fmt.Sprint(stopped.RegressionFailures) != fmt.Sprint([]RegressionFailure{flakyFailure}) {
		t.Fatalf("stopped attempt failures=%+v correctExcludingBaseFlakes=%v", stopped.RegressionFailures, stopped.CorrectExcludingBaseFlakes)
	}

	// The Molecule's own failures must be excused too. A Molecule that failed go test only
	// on the base-flaky test (in both views) agrees with the independent run.
	moleculeFlaky := attempt(fail(goTestCmd, plainFail("TestFlaky")), pass(vetCmd), pass(gofmtCmd))
	moleculeFlaky.FailedCheckOutput = []FailedCheckOutput{
		{ID: "go-test", Phase: failedPhaseCheck, Code: 1, FailedTests: []string{"TestFlaky"}, FailedTestPackages: []GoTestFailure{{"forgecellreplayfixture/flaky", "TestFlaky"}}},
		{ID: "go-test", Phase: failedPhaseCheck, Protected: true, Code: 1, FailedTests: []string{"TestFlaky"}, FailedTestPackages: []GoTestFailure{{"forgecellreplayfixture/flaky", "TestFlaky"}}},
	}
	classifyRegressionFailures(context.Background(), &moleculeFlaky, plan, f.dir, "")
	if !moleculeFlaky.CorrectExcludingBaseFlakes {
		t.Fatalf("molecule flaky attempt failures=%+v correctExcludingBaseFlakes=false", moleculeFlaky.RegressionFailures)
	}
	// The Molecule failed on a test the candidate broke deterministically (it passes at
	// base every run) while the independent run failed only on the unrelated base-flaky
	// test: the flag is false. So are a Molecule failure on another check, an unattributed
	// or truncated Molecule failure list, and a failed setup command.
	stableMolecule := []GoTestFailure{{"forgecellreplayfixture/flaky", "TestStable"}}
	flakyMolecule := []GoTestFailure{{"forgecellreplayfixture/flaky", "TestFlaky"}}
	for name, mf := range map[string]FailedCheckOutput{
		"deterministic": {ID: "go-test", Phase: failedPhaseCheck, Code: 1, FailedTests: []string{"TestStable"}, FailedTestPackages: stableMolecule},
		"protected vet": {ID: "go-vet", Phase: failedPhaseCheck, Protected: true, Code: 1},
		"acceptance":    {ID: "acceptance", Phase: failedPhaseCheck, Code: 1, FailedTests: []string{"TestFlaky"}, FailedTestPackages: flakyMolecule},
		"unattributed":  {ID: "go-test", Phase: failedPhaseCheck, Code: 1, FailedTests: []string{"TestFlaky"}, FailedTestPackages: flakyMolecule, UnattributedFailure: true},
		"truncated":     {ID: "go-test", Phase: failedPhaseCheck, Code: 1, FailedTests: []string{"TestFlaky"}, FailedTestPackages: flakyMolecule, FailedTestsTruncated: true},
		"no named test": {ID: "go-test", Phase: failedPhaseCheck, Code: 1},
		"setup":         {ID: "setup-1", Phase: failedPhaseSetup, Code: 1},
		"unknown check": {ID: "other", Phase: failedPhaseCheck, Code: 1, FailedTests: []string{"TestFlaky"}, FailedTestPackages: flakyMolecule},
	} {
		a := attempt(fail(goTestCmd, plainFail("TestFlaky")), pass(vetCmd), pass(gofmtCmd))
		a.FailedCheckOutput = []FailedCheckOutput{mf}
		classifyRegressionFailures(context.Background(), &a, plan, f.dir, "")
		if a.CorrectExcludingBaseFlakes || a.Correct {
			t.Fatalf("molecule %s: failures=%+v correctExcludingBaseFlakes=%v correct=%v, want false", name, a.RegressionFailures, a.CorrectExcludingBaseFlakes, a.Correct)
		}
		if name == "deterministic" {
			want := []RegressionFailure{flakyFailure, {Package: "forgecellreplayfixture/flaky", Test: "TestStable", BasePasses: 3}}
			if fmt.Sprint(a.RegressionFailures) != fmt.Sprint(want) {
				t.Fatalf("molecule deterministic: failures=%+v, want %+v", a.RegressionFailures, want)
			}
		}
	}

	// A failed hidden acceptance records nothing and re-runs nothing.
	no := false
	rejected := attempt(fail(goTestCmd, plainFail("TestFlaky")), pass(vetCmd), pass(gofmtCmd))
	rejected.HiddenAcceptance = &no
	classifyRegressionFailures(context.Background(), &rejected, plan, f.dir, "")
	if rejected.RegressionFailures != nil || rejected.CorrectExcludingBaseFlakes {
		t.Fatalf("rejected attempt failures=%+v correctExcludingBaseFlakes=%v", rejected.RegressionFailures, rejected.CorrectExcludingBaseFlakes)
	}
	// A correct attempt is always correct excluding base flakes.
	correct := attempt(pass(goTestCmd), pass(vetCmd), pass(gofmtCmd))
	correct.Correct = true
	classifyRegressionFailures(context.Background(), &correct, plan, f.dir, "")
	if !correct.CorrectExcludingBaseFlakes || correct.RegressionFailures != nil {
		t.Fatalf("correct attempt failures=%+v correctExcludingBaseFlakes=%v", correct.RegressionFailures, correct.CorrectExcludingBaseFlakes)
	}
}
