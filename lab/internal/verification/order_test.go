package verification

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

func goCheck(id string, argv ...string) readiness.Check {
	return readiness.Check{ID: id, Category: "regression", Required: true, Dir: "lab", Argv: argv}
}

func TestExecutionOrderIsCheapestFirst(t *testing.T) {
	t.Parallel()
	commands := []readiness.Check{
		{ID: "setup", Category: "setup", Argv: []string{"go", "test", "./..."}},
		goCheck("full", "go", "test", "-timeout", "20m", "./..."),
		goCheck("targeted", "go", "test", "./internal/x/..."),
		goCheck("hygiene", "sh", "scripts/check.sh"),
		goCheck("build", "go", "build", "./..."),
		goCheck("vet", "go", "vet", "./..."),
		goCheck("gofmt", "sh", "-c", `test -z "$(gofmt -l internal cmd)"`),
		goCheck("all", "go", "test", "all"),
	}
	got := []string{}
	for _, i := range executionOrder(commands, 1, false) {
		got = append(got, commands[i].ID)
	}
	want := []string{"setup", "vet", "gofmt", "build", "targeted", "hygiene", "full", "all"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	// The protected view keeps plan order.
	for i, j := range executionOrder(commands, 1, true) {
		if i != j {
			t.Fatalf("protected order moved %d to %d", j, i)
		}
	}
	if checkCost(goCheck("args", "go", "test", "./x", "-args", "./...")) != costTargeted {
		t.Fatal("a pattern after -args reached go test")
	}
}

// TestCoveringBaselineNeverAppliesOutsideItsPackages: a targeted check is covered only
// by a Formula baseline in the same directory whose patterns name a superset of its
// packages, and only when the two differ in nothing else.
func TestCoveringBaselineNeverAppliesOutsideItsPackages(t *testing.T) {
	t.Parallel()
	baseline := goCheck("baseline", "go", "test", "-timeout", "20m", "./internal/...")
	whole := goCheck("whole", "go", "test", "./...")
	p := readiness.Plan{FormulaBaseline: []string{"baseline"}}
	cases := []struct {
		name    string
		check   readiness.Check
		covered bool
	}{
		{"inside", goCheck("t", "go", "test", "./internal/verification/...", "./internal/molecule"), true},
		{"the baseline's own root", goCheck("t", "go", "test", "-timeout=5m", "./internal/..."), true},
		{"outside", goCheck("t", "go", "test", "./internal/verification/...", "./cmd/..."), false},
		{"prefix is not a parent", goCheck("t", "go", "test", "./internalx/..."), false},
		{"the module root", goCheck("t", "go", "test", "."), false},
		{"another directory", func() readiness.Check { c := goCheck("t", "go", "test", "./internal/x"); c.Dir = "."; return c }(), false},
		{"an extra flag", goCheck("t", "go", "test", "-run", "TestX", "./internal/x"), false},
		{"race", goCheck("t", "go", "test", "-race", "./internal/x"), false},
		{"its own -json", goCheck("t", "go", "test", "-json", "./internal/x"), false},
		{"traversal", goCheck("t", "go", "test", "./internal/../cmd"), false},
		{"absolute", goCheck("t", "go", "test", "/internal/x"), false},
		{"vendor", goCheck("t", "go", "test", "./internal/vendor/x"), false},
		{"import path", goCheck("t", "go", "test", "example.test/internal/x"), false},
		{"shell", goCheck("t", "sh", "-c", "go test ./internal/x"), false},
		{"not required", func() readiness.Check { c := goCheck("t", "go", "test", "./internal/x"); c.Required = false; return c }(), false},
		{"select", func() readiness.Check {
			c := goCheck("t", "go", "test", "./internal/x")
			c.Select = readiness.SelectAffectedPackages
			return c
		}(), false},
	}
	for _, c := range cases {
		b, ok := coveringBaseline(p, c.check, []readiness.Check{baseline, c.check})
		if ok != c.covered || (ok && b.ID != "baseline") {
			t.Errorf("%s: covered %v by %q, want %v", c.name, ok, b.ID, c.covered)
		}
	}
	target := goCheck("t", "go", "test", "./cmd/x")
	if _, ok := coveringBaseline(p, target, []readiness.Check{baseline, whole, target}); ok {
		t.Fatal("a check that is not a Formula baseline covered another")
	}
	p.FormulaBaseline = []string{"baseline", "whole"}
	if b, ok := coveringBaseline(p, target, []readiness.Check{baseline, whole, target}); !ok || b.ID != "whole" {
		t.Fatalf("./... must cover every relative package: %v %q", ok, b.ID)
	}
	if _, ok := coveringBaseline(p, baseline, []readiness.Check{baseline, whole}); ok {
		t.Fatal("a baseline was itself skipped as covered")
	}
	selected := whole
	selected.Select = readiness.SelectAffectedPackages
	if _, ok := coveringBaseline(p, target, []readiness.Check{selected, target}); ok {
		t.Fatal("a selecting baseline, which may run fewer packages, covered a check")
	}
	p.ExpectedProtectedDeltas = []readiness.ProtectedDelta{{CheckID: "whole"}}
	if _, ok := coveringBaseline(p, target, []readiness.Check{baseline, whole, target}); ok {
		t.Fatal("a plan with declared deltas skipped a check")
	}
}

// TestNotRunAndForgedCoverageNeverMatch: a not-run record, and a covered record that no
// covering baseline of the approved plan backs, are never delivery evidence.
func TestNotRunAndForgedCoverageNeverMatch(t *testing.T) {
	t.Parallel()
	targeted := goCheck("targeted", "go", "test", "./pkg/...")
	p := readiness.Plan{Analysis: readiness.Analysis{Checks: []readiness.Check{targeted}}}
	notRun := notRunObservation(targeted, "regression", "tree", NotRunEarlierFailure)
	if notRun.Result.OK || !notRun.Result.Reconciled || notRun.Result.Incomplete() || MatchesCheckObservation(p, targeted, notRun, "tree", false) || MatchesCheckObservation(p, targeted, notRun, "tree", true) {
		t.Fatalf("not run: %+v", notRun)
	}
	notRun.Result.OK = true
	if MatchesCheckObservation(p, targeted, notRun, "tree", false) {
		t.Fatal("a not-run record with a forged pass matched")
	}
	forged := Observation{ID: "targeted", Category: "regression", SourceTree: "tree", Command: targeted, CoveredBy: "baseline", Result: process.Result{OK: true, Reconciled: true}}
	if MatchesCheckObservation(p, targeted, forged, "tree", true) || MatchesCheckObservation(p, targeted, forged, "tree", false) {
		t.Fatal("a covered record without a covering baseline in the plan matched")
	}
}

func TestFailureExcerptKeepsHeadAndTailWithoutLocalPaths(t *testing.T) {
	t.Parallel()
	long := "/work/dir/a.go:1: first line\n" + strings.Repeat("noise\n", 5000) + "--- FAIL: TestLast (0.00s)\n    a_test.go:9: want x\nFAIL\n"
	r := process.Result{Stdout: long, Stderr: "panic in /home/me/.cache/x token=ghp_" + strings.Repeat("a", 36) + "\n"}
	e := failureExcerpt(r, nil, localPaths(map[string]string{"/work/dir": "<workspace>", "/home/me": "~"}), true)
	if !e.StdoutCut || len(e.Stdout) > excerptHeadBytes+excerptTailBytes+200 || !strings.HasPrefix(e.Stdout, "<workspace>/a.go:1: first line") || !strings.Contains(e.Stdout, "want x") || !strings.Contains(e.Stdout, "between head and tail dropped") {
		t.Fatalf("stdout excerpt %q", e.Stdout)
	}
	if strings.Contains(e.Stderr, "/home/me") || strings.Contains(e.Stderr, "ghp_aaaa") || !strings.Contains(e.Stderr, "~/.cache/x") {
		t.Fatalf("stderr excerpt %q", e.Stderr)
	}
	if !reflect.DeepEqual(e.FailedTests, []string{"TestLast"}) {
		t.Fatalf("failed tests %v", e.FailedTests)
	}
	parsed := failureExcerpt(process.Result{Error: "exit status 2"}, []TestID{{"example.test/p", "TestA"}}, localPaths(nil), true)
	if parsed.Stderr != "exit status 2" || !reflect.DeepEqual(parsed.FailedTests, []string{"example.test/p TestA"}) {
		t.Fatalf("a failure without output kept nothing: %+v", parsed)
	}
	many := []TestID{}
	for i := 0; i < maxExcerptTests+5; i++ {
		many = append(many, TestID{"p", strings.Repeat("T", 400)})
	}
	if bounded := failureExcerpt(process.Result{}, many, localPaths(nil), true); len(bounded.FailedTests) != maxExcerptTests || !bounded.FailedTestsCut || len(bounded.FailedTests[0]) > maxExcerptTestName+4 {
		t.Fatalf("failed tests are unbounded: %d cut %v", len(bounded.FailedTests), bounded.FailedTestsCut)
	}
}

// orderFixture is baselineFixture with a plain targeted check, as a plan's combined
// targeted command is, and fake go behaviour chosen by mode: "pass" passes in both
// views, "fail" fails the targeted check in both, and "by-design" fails only the
// protected view. The fake go also logs the GOTMPDIR it ran with.
func orderFixture(t *testing.T, mode string) (string, string, readiness.Plan, CaptureResult) {
	t.Helper()
	bin, dir, p, c := baselineFixture(t, "by-design")
	fake := strings.Replace(baselineFakeGo, `printf '%s\n' "$*" >> "$bin/argv.log"`, `printf '%s\n' "$*" >> "$bin/argv.log"; printf '%s\n' "$GOTMPDIR" >> "$bin/gotmp.log"`, 1)
	switch mode {
	case "pass":
		fake = strings.Replace(fake, "if grep -q new pkg/pin_test.go; then", "if true; then", 1)
	case "fail":
		fake = strings.Replace(fake, "if grep -q new pkg/pin_test.go; then", "if false; then", 1)
	}
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	p.Analysis.Checks[0].Argv = []string{"go", "test", "./pkg/..."}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return bin, dir, p, c
}

func ids(obs []Observation) []string {
	out := []string{}
	for _, o := range obs {
		out = append(out, o.ID)
	}
	return out
}

// TestFailFastRecordsRemainingChecksAsNotRun: a failing cheap check (vet) stops the
// view at once; the targeted check and the full suite are recorded as not run, never
// as passed, and the protected view, its full suite included, is never started.
func TestFailFastRecordsRemainingChecksAsNotRun(t *testing.T) {
	bin, dir, p, c := orderFixture(t, "pass")
	vet := p.Analysis.Checks[1]
	vet.ID, vet.Argv = "vet", []string{"go", "vet", "./..."}
	p.Analysis.Checks = append(p.Analysis.Checks, vet)
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Error != "required command vet did not pass" {
		t.Fatalf("verdict %q", v.Error)
	}
	if got := ids(v.Checks); !reflect.DeepEqual(got, []string{"vet"}) {
		t.Fatalf("candidate checks that ran %v", got)
	}
	if v.Checks[0].NotRun != "" || v.Checks[0].Output == nil || !strings.Contains(v.Checks[0].Output.Stderr+v.Checks[0].Output.Stdout, "exit status 2") {
		t.Fatalf("the failing check lost its evidence: %+v", v.Checks[0])
	}
	if got := ids(v.NotRun); !reflect.DeepEqual(got, []string{"targeted", baselineCheck}) {
		t.Fatalf("not run %v", got)
	}
	for _, o := range v.NotRun {
		if o.NotRun != NotRunEarlierFailure || o.Result.OK || MatchesCheckObservation(p, o.Command, o, v.SourceTree, false) {
			t.Fatalf("a check after the failure: %+v", o)
		}
	}
	if v.CandidateError != v.Error {
		t.Fatalf("candidate error %q, error %q", v.CandidateError, v.Error)
	}
	if _, err := os.Stat(filepath.Join(bin, "argv.log")); !os.IsNotExist(err) {
		t.Fatalf("go test ran after the failing vet: %v %q", err, argvLog(t, bin))
	}
	if v.Protected == nil || v.Protected.RequiredChecksPassed || len(v.Protected.Checks) != 0 || len(v.Protected.NotRun) != 3 {
		t.Fatalf("protected %+v", v.Protected)
	}
	for _, o := range v.Protected.NotRun {
		if o.NotRun != NotRunCandidateFailure || o.Result.OK || MatchesCheckObservation(p, o.Command, o, v.Protected.SourceTree, true) {
			t.Fatalf("a protected check after the candidate failure: %+v", o)
		}
	}
	if v.Protected.Workspace != "" {
		t.Fatalf("a protected workspace was prepared: %q", v.Protected.Workspace)
	}
}

// TestFailingOutputIsKeptInBothViews: a failing targeted check keeps its head and tail
// and failing test names in the candidate view; a failing engine-streamed baseline keeps
// them in the protected view. Neither is left with empty output.
func TestFailingOutputIsKeptInBothViews(t *testing.T) {
	_, dir, p, c := orderFixture(t, "fail")
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || len(v.Checks) != 1 || len(v.NotRun) != 1 {
		t.Fatalf("candidate %q %v not run %v", v.Error, ids(v.Checks), ids(v.NotRun))
	}
	failed := v.Checks[0]
	if failed.Output == nil || !strings.Contains(failed.Output.Stdout, "--- FAIL: TestA") || !reflect.DeepEqual(failed.Output.FailedTests, []string{"TestA"}) || strings.Contains(failed.Output.Stdout, dir) {
		t.Fatalf("candidate failing output %+v", failed.Output)
	}
	if v.NotRun[0].ID != baselineCheck || v.NotRun[0].NotRun != NotRunEarlierFailure {
		t.Fatalf("the full suite after the targeted failure: %+v", v.NotRun[0])
	}

	_, dir, p, c = orderFixture(t, "by-design")
	v = runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Protected == nil || !strings.Contains(v.Error, "required command "+baselineCheck+" did not pass") {
		t.Fatalf("protected verdict %q", v.Error)
	}
	if got := ids(v.Protected.Checks); !reflect.DeepEqual(got, []string{baselineCheck}) {
		t.Fatalf("protected records %v", got)
	}
	baseline := v.Protected.Checks[0]
	if baseline.ID != baselineCheck || !baseline.JSONAdded || baseline.Result.Stdout == "" || baseline.Output == nil || !reflect.DeepEqual(baseline.Output.FailedTests, []string{baselinePkg + " TestA"}) || !strings.Contains(baseline.Output.Stdout, "--- FAIL: TestA") {
		t.Fatalf("protected failing output %+v", baseline)
	}
	if a := baseline.Result.Activity; a == nil || a.StdoutBytes == 0 {
		t.Fatalf("activity %+v", a)
	}
}

// TestProtectedTargetedCheckCoveredByBaseline: in the protected view a plain targeted
// check inside the baseline's packages is not run again; it is recorded as covered by
// the baseline with the baseline's outcome, and delivery accepts it only through that
// baseline.
func TestProtectedTargetedCheckCoveredByBaseline(t *testing.T) {
	bin, dir, p, c := orderFixture(t, "pass")
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Protected == nil || !v.Protected.RequiredChecksPassed {
		t.Fatalf("verdict %q %+v", v.Error, v.Protected)
	}
	if got, want := argvLog(t, bin), []string{"test ./pkg/...", "test -timeout 20m ./...", "test -json -timeout 20m ./..."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv %q, want %q", got, want)
	}
	// Records stay in plan order: the covered check first, then its baseline.
	if got := ids(v.Protected.Checks); !reflect.DeepEqual(got, []string{"targeted", baselineCheck}) {
		t.Fatalf("protected records %v", got)
	}
	covered := v.Protected.Checks[0]
	if covered.CoveredBy != baselineCheck || covered.NotRun != "" || covered.Result.Stdout != "" || covered.Result.ElapsedMS != 0 || !reflect.DeepEqual(covered.Command, p.Analysis.Checks[0]) {
		t.Fatalf("covered record %+v", covered)
	}
	if !MatchesCheckObservation(p, p.Analysis.Checks[0], covered, v.Protected.SourceTree, true) {
		t.Fatal("a covered check backed by its passing baseline did not match")
	}
	if MatchesCheckObservation(p, p.Analysis.Checks[0], covered, v.Protected.SourceTree, false) {
		t.Fatal("a covered record matched as candidate evidence")
	}
	// The same record under a plan whose targeted check leaves the baseline's packages
	// is refused.
	outside := p
	outside.Analysis.Checks = slices.Clone(p.Analysis.Checks)
	outside.Analysis.Checks[0].Argv = []string{"go", "test", "./pkg/...", "../elsewhere"}
	forged := covered
	forged.Command = outside.Analysis.Checks[0]
	if MatchesCheckObservation(outside, outside.Analysis.Checks[0], forged, v.Protected.SourceTree, true) {
		t.Fatal("coverage was accepted for a check outside the baseline's packages")
	}
	failing := covered
	failing.Result.OK = false
	if MatchesCheckObservation(p, p.Analysis.Checks[0], failing, v.Protected.SourceTree, true) {
		t.Fatal("a covered check whose baseline failed matched")
	}
}

// TestCheckCoveredByAFailingBaselineIsNotRun: when the covering baseline fails, the
// targeted check it covers never ran, so it is listed in NotRun as covered by that
// failed baseline, never as a copy of the failure among the checks, whether it comes
// before or after the baseline in plan order.
func TestCheckCoveredByAFailingBaselineIsNotRun(t *testing.T) {
	for _, after := range []bool{false, true} {
		bin, dir, p, c := orderFixture(t, "by-design")
		if after {
			p.Analysis.Checks = []readiness.Check{p.Analysis.Checks[1], p.Analysis.Checks[0]}
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
		}
		v := runDelta(t, dir, p, c)
		if v.RequiredChecksPassed || v.Protected == nil || !strings.HasPrefix(v.Protected.Error, "required command "+baselineCheck+" did not pass") {
			t.Fatalf("after %v: verdict %q", after, v.Error)
		}
		if got := ids(v.Protected.Checks); !reflect.DeepEqual(got, []string{baselineCheck}) {
			t.Fatalf("after %v: protected checks %v", after, got)
		}
		if len(v.Protected.NotRun) != 1 {
			t.Fatalf("after %v: not run %v", after, ids(v.Protected.NotRun))
		}
		o := v.Protected.NotRun[0]
		if o.ID != "targeted" || o.NotRun != "covered by "+baselineCheck+", which failed" || o.CoveredBy != baselineCheck || o.Result.OK || o.Result.Stdout != "" {
			t.Fatalf("after %v: covered record %+v", after, o)
		}
		targeted := p.Analysis.Checks[0]
		if after {
			targeted = p.Analysis.Checks[1]
		}
		if MatchesCheckObservation(p, targeted, o, v.Protected.SourceTree, true) {
			t.Fatalf("after %v: a check covered by a failing baseline matched", after)
		}
		// The protected view ran the baseline only.
		if got := argvLog(t, bin); len(got) != 3 {
			t.Fatalf("after %v: argv %q", after, got)
		}
	}
}

// TestCoverageNeverCrossesANestedModule: go test ./... skips a nested module, so a
// targeted package in one, or below a directory holding one, is never covered by the
// baseline; a package in the module itself still is.
func TestCoverageNeverCrossesANestedModule(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, d := range []string{"lab/internal/plain/sub", "lab/internal/nested/sub", "lab/tools/inner"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"lab/go.mod", "lab/internal/nested/go.mod", "lab/tools/inner/go.mod"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(f)), []byte("module x\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "lab", "internal", "plain"), filepath.Join(root, "lab", "linked")); err != nil {
		t.Fatal(err)
	}
	whole := goCheck("whole", "go", "test", "./...")
	internal := goCheck("internal", "go", "test", "./internal/...")
	for _, c := range []struct {
		name     string
		baseline readiness.Check
		pattern  string
		crosses  bool
	}{
		{"in the module", whole, "./internal/plain/...", false},
		{"a package of the module", whole, "./internal/plain/sub", false},
		{"the nested module itself", whole, "./internal/nested/...", true},
		{"below a nested module", whole, "./internal/nested/sub", true},
		{"a nested module's package", whole, "./tools/inner", true},
		{"below the baseline's root", internal, "./internal/nested/sub", true},
		{"inside the baseline's root", internal, "./internal/plain/sub", false},
		{"a missing directory", whole, "./internal/missing", true},
		{"through a symbolic link", whole, "./linked/sub", true},
		{"one of several", whole, "./internal/plain ./tools/inner", true},
	} {
		argv := append([]string{"go", "test"}, strings.Fields(c.pattern)...)
		if got := crossesNestedModule(root, goCheck("t", argv...), c.baseline); got != c.crosses {
			t.Errorf("%s: crosses %v, want %v", c.name, got, c.crosses)
		}
	}
	// protectedCoverage applies the refusal: the nested check runs on its own.
	p := readiness.Plan{FormulaBaseline: []string{"whole"}}
	commands := []readiness.Check{goCheck("t", "go", "test", "./internal/nested/sub"), goCheck("u", "go", "test", "./internal/plain/..."), whole}
	_, covered := protectedCoverage(p, commands, 0, root)
	if covered[0] || !covered[1] {
		t.Fatalf("covered %v", covered)
	}
}

type phaseRecorder struct {
	mu     sync.Mutex
	phases []string
}

func (r *phaseRecorder) Begin(view, phase string) func() {
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.phases = append(r.phases, strings.TrimSpace(view+" "+phase))
	}
}

// TestConcurrentViewsUseTheirOwnGoTempDirs: when resources allow, the two views run
// concurrently, each in its own workspace with its own GOTMPDIR, and both are timed.
func TestConcurrentViewsUseTheirOwnGoTempDirs(t *testing.T) {
	bin, dir, p, c := orderFixture(t, "pass")
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	phases := &phaseRecorder{}
	ctx := resources.WithCheckTempDir(context.Background(), tmp)
	ctx = resources.WithViewConcurrency(ctx, func() (bool, string) { return true, "" })
	ctx = WithPhaseObserver(ctx, phases)
	dest := filepath.Join(t.TempDir(), "verify")
	v := RunObserved(ctx, dir, dest, p, c, nil)
	if !v.RequiredChecksPassed || v.Protected == nil || !v.Protected.RequiredChecksPassed || !slices.Contains(v.Notes, "The candidate and protected views ran concurrently.") {
		t.Fatalf("verdict %q notes %v", v.Error, v.Notes)
	}
	if v.Workspace == v.Protected.Workspace {
		t.Fatal("the views shared a workspace")
	}
	raw, err := os.ReadFile(filepath.Join(bin, "gotmp.log"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		seen[line] = true
	}
	candidate, protected := filepath.Join(tmp, resources.LabGoTempName), filepath.Join(tmp, resources.LabGoTempName, "protected")
	if len(seen) != 2 || !seen[candidate] || !seen[protected] {
		t.Fatalf("GOTMPDIRs %v, want %s and %s", seen, candidate, protected)
	}
	for _, want := range []string{PhaseModuleCheck, "protected " + PhaseProtectedTree, "candidate " + PhaseWorkspace, "protected " + PhaseWorkspace, "candidate " + PhaseCache, "protected " + PhaseEnvironment} {
		if !slices.Contains(phases.phases, want) {
			t.Fatalf("phases %v lack %q", phases.phases, want)
		}
	}
	// Without a decision, or refused, the views run one after the other and say why.
	_, dir, p, c = orderFixture(t, "pass")
	refused := resources.WithViewConcurrency(context.Background(), func() (bool, string) { return false, "resources: low memory" })
	v = RunObserved(refused, dir, filepath.Join(t.TempDir(), "verify"), p, c, nil)
	if !v.RequiredChecksPassed || !slices.Contains(v.Notes, "resources: low memory.") || slices.Contains(v.Notes, "The candidate and protected views ran concurrently.") {
		t.Fatalf("refused concurrency: %q %v", v.Error, v.Notes)
	}
}

// TestConcurrentProtectedViewStopsAfterCandidateFailure: a required candidate failure
// keeps the candidate's own error and never lets a protected record read as passed.
func TestConcurrentProtectedViewStopsAfterCandidateFailure(t *testing.T) {
	_, dir, p, c := orderFixture(t, "fail")
	ctx := resources.WithViewConcurrency(context.Background(), func() (bool, string) { return true, "" })
	v := RunObserved(ctx, dir, filepath.Join(t.TempDir(), "verify"), p, c, nil)
	if v.RequiredChecksPassed || v.Error != "required command targeted did not pass" || v.Protected == nil {
		t.Fatalf("verdict %q", v.Error)
	}
	// The protected view ran beside it; whatever it reached before it was stopped, a
	// record it never ran names why.
	if v.CandidateError != v.Error {
		t.Fatalf("candidate error %q, error %q", v.CandidateError, v.Error)
	}
	for _, o := range v.Protected.Checks {
		if o.NotRun != "" {
			t.Fatalf("a not-run record among the checks that ran: %+v", o)
		}
	}
	for _, o := range v.Protected.NotRun {
		// The protected baseline may fail on its own before the candidate failure stops it;
		// the targeted check it covers is then not run as covered by a failing check.
		coveredFailed := o.CoveredBy != "" && o.NotRun == coveredFailedReason(o.CoveredBy)
		if o.Result.OK || (o.NotRun != NotRunCandidateFailure && o.NotRun != NotRunEarlierFailure && !coveredFailed) {
			t.Fatalf("protected record %+v", o)
		}
	}
}

// TestStalledCheckEndsAsInterrupted: a check whose watchdog finds it stalled (here an
// injected clock past the wall budget) ends as interrupted: stalled, fails the view, and
// keeps its reason as output.
func TestStalledCheckEndsAsInterrupted(t *testing.T) {
	original := checkWatchdog
	t.Cleanup(func() { checkWatchdog = original })
	var mu sync.Mutex
	now := time.Unix(1_700_000_000, 0)
	checkWatchdog = func(time.Duration) *process.Watchdog {
		ticks := make(chan time.Time, 1)
		ticks <- time.Time{}
		return &process.Watchdog{Wall: time.Minute, Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			now = now.Add(time.Hour)
			return now
		}, Tick: func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} }}
	}
	dir, _ := sourceFixture(t)
	if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte("sleep 30\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest(t, dir, "add", "check.sh")
	gitTest(t, dir, "commit", "-m", "silent check")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, base, []string{"code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := runDelta(t, dir, p, c)
	o := v.Checks[0]
	if v.RequiredChecksPassed || !o.Result.Stalled || !o.Result.Interrupted || !strings.HasPrefix(o.Result.Error, "interrupted: stalled: ") || o.Output == nil || !strings.Contains(o.Output.Stderr, "stalled") {
		t.Fatalf("stalled check %+v output %+v", o.Result, o.Output)
	}
}
