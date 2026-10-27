package molecule

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const (
	acceptPackage = "example.test/lab/pin"
	acceptTest    = "lab/pin/pin_test.go"
	acceptCheck   = "go-test-lab"
)

// acceptFakeGo answers `go test` from the pinned test source the check runs against:
// the candidate's version says "new", the base version restored by the protected
// view says "old". It answers in -json only when its argv asks for it, and a -timeout
// argument marks the plain Formula baseline. The mode file next to it picks how the
// old version fails, so no Go toolchain runs and nothing depends on time.
const acceptFakeGo = `#!/bin/sh
[ "$1" = test ] || exit 2
mode=$(cat "$(dirname "$0")/mode")
pkg=example.test/lab/pin
json=no
baseline=no
for a in "$@"; do
	case "$a" in
	-json) json=yes ;;
	-timeout) baseline=yes ;;
	esac
done
fail() {
	for name in "$@"; do
		if [ $json = yes ]; then
			printf '{"Action":"run","Package":"%s","Test":"%s"}\n{"Action":"fail","Package":"%s","Test":"%s"}\n' "$failpkg" "$name" "$failpkg" "$name"
		else
			printf '%s\n' "--- FAIL: $name"
		fi
	done
	if [ $json = yes ]; then
		printf '{"Action":"fail","Package":"%s"}\n' "$failpkg"
	else
		printf 'FAIL\t%s\t0.01s\n' "$failpkg"
	fi
	exit 1
}
failpkg=$pkg
if [ "$mode" = candidate-fails ]; then
	fail TestX
fi
if grep -q new pin/pin_test.go || [ "$mode" = pass ]; then
	if [ "$mode" = candidate-audit ]; then
		printf stray > stray.txt
	fi
	if [ $json = yes ]; then
		printf '{"Action":"run","Package":"%s","Test":"TestX"}\n{"Action":"pass","Package":"%s","Test":"TestX"}\n{"Action":"pass","Package":"%s"}\n' "$pkg" "$pkg" "$pkg"
	else
		printf 'ok  \t%s\t0.01s\n' "$pkg"
	fi
	exit 0
fi
if [ $baseline = yes ] && [ "$mode" = baseline-extra ]; then
	fail TestX TestY
fi
case "$mode" in
by-design|candidate-audit|baseline-extra) fail TestX ;;
extra) fail TestX TestY ;;
other-package) failpkg=example.test/lab/other; fail TestY ;;
many) fail $(seq -f 'Test%g' 1 65) ;;
panic)
	printf '{"Action":"run","Package":"%s","Test":"TestX"}\n{"Action":"output","Package":"%s","Test":"TestX","Output":"panic: boom\\n"}\n{"Action":"fail","Package":"%s","Test":"TestX"}\n{"Action":"fail","Package":"%s"}\n' "$pkg" "$pkg" "$pkg" "$pkg"
	exit 1 ;;
timeout)
	printf '{"Action":"run","Package":"%s","Test":"TestX"}\n{"Action":"fail","Package":"%s","Test":"TestX"}\n{"Action":"output","Package":"%s","Output":"panic: test timed out after 10m0s\\n"}\n{"Action":"fail","Package":"%s"}\n' "$pkg" "$pkg" "$pkg" "$pkg"
	exit 1 ;;
esac
exit 3
`

// acceptFixture runs a Molecule whose coding changes an in-scope Go test source,
// so its required go test -json regression check passes on the candidate and fails
// in the protected view by mode. The fake go is first on PATH for this test, which
// is therefore not parallel.
func acceptFixture(t *testing.T, mode string) (Options, Record) {
	t.Helper()
	return acceptFixtureFiles(t, mode, map[string]string{acceptTest: "package pin\n\n// pinned: old\n"}, false, acceptJSONArgv)
}

var (
	acceptJSONArgv  = []string{"go", "test", "-json", "./..."}
	acceptPlainArgv = []string{"go", "test", "./..."}
)

// acceptPlainFixture is acceptFixture with a plain `go test ./...` required
// regression check, as intake plans write it: the protected view adds -json itself.
func acceptPlainFixture(t *testing.T, mode string) (Options, Record) {
	t.Helper()
	return acceptFixtureFiles(t, mode, map[string]string{acceptTest: acceptPinnedBase}, false, acceptPlainArgv)
}

const (
	acceptBaseline   = "baseline-go-test-lab"
	acceptOtherTest  = "lab/pin/aaa_test.go"
	acceptPinnedBase = "package pin\n\nimport \"testing\"\n\n// pinned: old\nfunc TestX(t *testing.T) {}\n"
)

// acceptBaselineFixture adds a plain `go test -timeout 20m ./...` Formula-style
// baseline after the targeted go test -json check, and a second in-scope test file of
// the package that sorts first and declares TestY, not TestX.
func acceptBaselineFixture(t *testing.T, mode string) (Options, Record) {
	t.Helper()
	return acceptFixtureFiles(t, mode, map[string]string{acceptTest: acceptPinnedBase, acceptOtherTest: "package pin\n\nimport \"testing\"\n\nfunc TestY(t *testing.T) {}\n"}, true, acceptJSONArgv)
}

func acceptFixtureFiles(t *testing.T, mode string, tests map[string]string, baseline bool, argv []string) (Options, Record) {
	t.Helper()
	bin := resolvedTempDir(t)
	if err := writeStub(filepath.Join(bin, "go"), []byte(acceptFakeGo), 0700); err != nil {
		t.Fatal(err)
	}
	setAcceptMode(t, bin, mode)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	o, repo := fixture(t)
	files := map[string]string{"lab/go.mod": "module example.test/lab\n"}
	for file, body := range tests {
		files[file] = body
	}
	for file, body := range files {
		if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(file)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	testGit(t, repo, "add", ".")
	testGit(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-qm", "pinned test")
	stub := []byte("#!/bin/sh\ncat > request.json\nprintf changed > code.txt\nprintf 'package pin\\n\\n// pinned: new\\n' > " + acceptTest + "\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n")
	if err := writeStub(filepath.Join(filepath.Dir(repo), "harness"), stub, 0700); err != nil {
		t.Fatal(err)
	}
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		a, err := analyze(ctx, b, i, s)
		manifest := ""
		for _, e := range s.Evidence {
			if e.Path == "lab/go.mod" {
				manifest = e.ID
			}
		}
		a.Scope = append(a.Scope, readiness.ScopedPath{Path: acceptTest, Reason: "The pinned behaviour changes by design", Evidence: []string{"issue"}})
		a.Checks = append(a.Checks, readiness.Check{ID: acceptCheck, Category: "regression", Argv: append([]string{}, argv...), Dir: "lab", TimeoutMS: fixtureCheckTimeoutMS, Required: true, Definitions: []string{manifest}, Reason: "Module tests", Evidence: []string{"issue", manifest}})
		if baseline {
			a.Scope = append(a.Scope, readiness.ScopedPath{Path: acceptOtherTest, Reason: "Another test of the package", Evidence: []string{"issue"}})
			a.Checks = append(a.Checks, readiness.Check{ID: acceptBaseline, Category: "regression", Argv: []string{"go", "test", "-timeout", "20m", "./..."}, Dir: "lab", TimeoutMS: fixtureCheckTimeoutMS, Required: true, Definitions: []string{manifest}, Reason: "Formula baseline", Evidence: []string{"issue", manifest}})
		}
		return a, err
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || !CompletedCoding(r) || r.Verification == nil {
		t.Fatalf("fixture did not complete coding and verification: %s", statePhase(r))
	}
	return o, r
}

func setAcceptMode(t *testing.T, bin, mode string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, "mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
}

func acceptBin(t *testing.T) string {
	t.Helper()
	return strings.SplitN(os.Getenv("PATH"), string(os.PathListSeparator), 2)[0]
}

func TestAcceptProtectedFailuresDeclaresTheByDesignFailureAndReachesReview(t *testing.T) {
	o, r := acceptFixture(t, "by-design")
	if statePhase(r) != "failed" || r.Verification.Protected == nil || r.Verification.Protected.RequiredChecksPassed {
		t.Fatalf("fixture did not fail only in the protected view: %s %+v", statePhase(r), r.Verification)
	}
	parent := latestPlan(r)
	tree := r.Capture.Tree
	r, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true})
	if err != nil {
		t.Fatal(err)
	}
	got := latestPlan(r)
	p := got.Plan
	if p.Revision != parent.Plan.Revision+1 || p.ParentDigest != parent.Digest || got.Digest == parent.Digest || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("revision %d parent %s digest %s phase %s", p.Revision, p.ParentDigest, got.Digest, r.Readiness.Phase)
	}
	if p.Continuation != "checks-only" || !reflect.DeepEqual(p.Analysis.Scope, parent.Plan.Analysis.Scope) || !reflect.DeepEqual(p.Analysis.Checks, parent.Plan.Analysis.Checks) {
		t.Fatalf("continuation %q or scope/checks changed", p.Continuation)
	}
	if len(p.ExpectedProtectedDeltas) != 1 {
		t.Fatalf("deltas %+v", p.ExpectedProtectedDeltas)
	}
	d := p.ExpectedProtectedDeltas[0]
	if d.CheckID != acceptCheck || d.Package != acceptPackage || d.Test != "TestX" || d.ScopePath != acceptTest || !strings.Contains(d.Reason, acceptCheck) || !strings.Contains(d.Reason, tree) || len(d.Evidence) != 1 || d.Evidence[0] != "issue" {
		t.Fatalf("delta %+v", d)
	}
	if summary := AmendDiffSummary(parent.Plan, p); summary != "expected protected failures: 1, continuation: checks-only" {
		t.Fatalf("summary %q", summary)
	}
	if strings.Contains(string(ledgerBytes(t, r)), o.LabDir+"/verification-history") {
		t.Fatal("the amendment recorded a local evidence path")
	}
	// Nothing is approved implicitly: the old digest still cannot run the new plan.
	for _, e := range r.Readiness.Events {
		if e.Kind == "approved" && e.Digest == got.Digest {
			t.Fatal("the amendment approved its own digest")
		}
	}
	o.Approve = got.Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if r.Readiness.Phase != "review-waiting" || r.Verification == nil || !r.Verification.RequiredChecksPassed || len(r.HarnessAttempts) != 1 || r.Capture.Tree != tree {
		t.Fatalf("checks-only did not accept the declared failure: phase %s attempts %d %+v", r.Readiness.Phase, len(r.HarnessAttempts), r.Verification)
	}
	if len(r.Verification.AcceptedDeltas) != 1 || r.Verification.AcceptedDeltas[0].Test != "TestX" {
		t.Fatalf("accepted deltas %+v", r.Verification.AcceptedDeltas)
	}
	// The passing run leaves nothing to accept.
	before := ledgerBytes(t, r)
	if _, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true}); err == nil || !strings.Contains(err.Error(), "did not fail") {
		t.Fatalf("a passing verification was turned into deltas: %v", err)
	}
	if !bytes.Equal(before, ledgerBytes(t, r)) {
		t.Fatal("a refused amend changed the ledger")
	}
}

// The #257 flow: a plain go test check's first protected run has no deltas, yet the
// protected view streams it with -json, so the by-design failure is recorded without
// changing the verdict, --accept-protected-failures declares it from that run, and the
// approved checks-only run accepts it.
func TestAcceptProtectedFailuresWorksOnTheFirstPlainGoTestFailure(t *testing.T) {
	o, r := acceptPlainFixture(t, "by-design")
	v := r.Verification
	if statePhase(r) != "failed" || v.RequiredChecksPassed || v.Protected == nil || v.Protected.RequiredChecksPassed || len(v.AcceptedDeltas) != 0 || len(v.Protected.Checks) != 1 {
		t.Fatalf("the first protected run must still fail: %s %+v", statePhase(r), v)
	}
	found := false
	for _, c := range v.Checks {
		if c.ID != acceptCheck {
			continue
		}
		found = true
		if c.JSONAdded || !c.Result.OK || !reflect.DeepEqual(c.Command.Argv, acceptPlainArgv) || strings.Contains(c.Result.Stdout, `"Action"`) {
			t.Fatalf("the candidate view was changed: %+v", c)
		}
	}
	if !found {
		t.Fatalf("the candidate view did not run %s", acceptCheck)
	}
	obs := v.Protected.Checks[0]
	if !obs.JSONAdded || obs.Result.OK || !reflect.DeepEqual(obs.Command.Argv, acceptPlainArgv) || len(obs.AcceptedDeltas) != 0 {
		t.Fatalf("protected observation: %+v", obs)
	}
	if want := []verification.TestID{{Package: acceptPackage, Test: "TestX"}}; !reflect.DeepEqual(obs.FailedTests, want) {
		t.Fatalf("failed tests %v, want %v", obs.FailedTests, want)
	}
	parent := latestPlan(r)
	tree := r.Capture.Tree
	r, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true})
	if err != nil {
		t.Fatalf("the first by-design failure could not be accepted: %v", err)
	}
	got := latestPlan(r)
	if !reflect.DeepEqual(got.Plan.Analysis.Checks, parent.Plan.Analysis.Checks) || got.Plan.Continuation != "checks-only" {
		t.Fatalf("the amendment changed the checks: %+v", got.Plan.Analysis.Checks)
	}
	deltas := got.Plan.ExpectedProtectedDeltas
	if len(deltas) != 1 || deltas[0].Test != "TestX" || deltas[0].Package != acceptPackage || deltas[0].CheckID != acceptCheck || deltas[0].ScopePath != acceptTest || deltas[0].DeltaScope != readiness.DeltaScopeIdentity {
		t.Fatalf("deltas %+v", deltas)
	}
	for _, e := range r.Readiness.Events {
		if e.Kind == "approved" && e.Digest == got.Digest {
			t.Fatal("the amendment approved its own digest")
		}
	}
	o.Approve = got.Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	v = r.Verification
	if r.Readiness.Phase != "review-waiting" || v == nil || !v.RequiredChecksPassed || len(r.HarnessAttempts) != 1 || r.Capture.Tree != tree {
		t.Fatalf("checks-only did not accept the declared failure: phase %s attempts %d %+v", r.Readiness.Phase, len(r.HarnessAttempts), v)
	}
	if len(v.AcceptedDeltas) != 1 || v.AcceptedDeltas[0].Test != "TestX" || !v.Protected.Checks[0].JSONAdded {
		t.Fatalf("accepted deltas %+v", v.AcceptedDeltas)
	}
	if strings.Contains(string(ledgerBytes(t, r)), o.LabDir+"/verification-history") {
		t.Fatal("the ledger recorded a local evidence path")
	}
}

func TestAcceptProtectedFailuresUndeclaredExtraFailureStillFails(t *testing.T) {
	o, r := acceptFixture(t, "by-design")
	r, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true})
	if err != nil {
		t.Fatal(err)
	}
	setAcceptMode(t, acceptBin(t), "extra")
	o.Approve = latestPlan(r).Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if r.Verification == nil || r.Verification.RequiredChecksPassed || !strings.Contains(r.Verification.Error, "undeclared protected regression failures") || !strings.Contains(r.Verification.Error, acceptPackage+" TestY") {
		t.Fatalf("an undeclared extra failure passed: %+v", r.Verification)
	}
	// Accepting again names exactly the latest failing set of the check, never more.
	if r, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range latestPlan(r).Plan.ExpectedProtectedDeltas {
		names = append(names, d.Test)
	}
	if strings.Join(names, ",") != "TestX,TestY" {
		t.Fatalf("deltas %v", names)
	}
}

// The #235 flow: a by-design failure declared from the targeted check also covers the
// plain Formula baseline, which the protected view runs with -json added. When the
// baseline then fails one more test, accepting again declares the union of both
// checks' failing tests once, each with the in-scope file that declares it.
func TestAcceptProtectedFailuresCoversThePlainBaselineAndDeclaresTheUnion(t *testing.T) {
	o, r := acceptBaselineFixture(t, "by-design")
	if r.Verification.Protected == nil || len(r.Verification.Protected.Checks) != 1 || r.Verification.Protected.Checks[0].ID != acceptCheck {
		t.Fatalf("the protected view must stop on the targeted check first: %+v", r.Verification.Protected)
	}
	r, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true})
	if err != nil {
		t.Fatal(err)
	}
	deltas := latestPlan(r).Plan.ExpectedProtectedDeltas
	if len(deltas) != 1 || deltas[0].Test != "TestX" || deltas[0].CheckID != acceptCheck || deltas[0].ScopePath != acceptTest {
		t.Fatalf("ScopePath must be the file declaring TestX, not the first test file of the package: %+v", deltas)
	}
	// Identity matching is the digest-bound opt-in the amend sets and review shows.
	if deltas[0].DeltaScope != readiness.DeltaScopeIdentity || deltas[0].ScopeText() != "apply to every protected go test check running the package" {
		t.Fatalf("the accepted delta did not opt into identity matching: %+v", deltas[0])
	}

	// The baseline also fails an undeclared test: it still fails, by name.
	setAcceptMode(t, acceptBin(t), "baseline-extra")
	o.Approve = latestPlan(r).Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	v := r.Verification
	if v == nil || v.RequiredChecksPassed || v.Protected == nil || len(v.Protected.Checks) != 2 || !strings.Contains(v.Error, "required command "+acceptBaseline+" did not pass") || !strings.Contains(v.Error, acceptPackage+" TestY") {
		t.Fatalf("an undeclared baseline failure passed: %+v", v)
	}
	if b := v.Protected.Checks[1]; !b.JSONAdded || len(b.FailedTests) != 2 || b.Command.Argv[2] != "-timeout" {
		t.Fatalf("the ledger's compact projection lost the engine-streamed evidence: %+v", b)
	}
	if r, err = AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range latestPlan(r).Plan.ExpectedProtectedDeltas {
		got = append(got, d.Test+"@"+d.CheckID+"@"+d.ScopePath)
	}
	if want := []string{"TestX@" + acceptCheck + "@" + acceptTest, "TestY@" + acceptBaseline + "@" + acceptOtherTest}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deltas %v, want the union once %v", got, want)
	}

	o.Approve = latestPlan(r).Digest
	if r, err = Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	v = r.Verification
	if r.Readiness.Phase != "review-waiting" || v == nil || !v.RequiredChecksPassed || len(v.AcceptedDeltas) != 2 {
		t.Fatalf("the declared union was not accepted in both checks: phase %s %+v", r.Readiness.Phase, v)
	}
	targeted, baseline := v.Protected.Checks[0], v.Protected.Checks[1]
	if targeted.JSONAdded || len(targeted.AcceptedDeltas) != 1 || !baseline.JSONAdded || len(baseline.AcceptedDeltas) != 2 {
		t.Fatalf("targeted %+v baseline %+v", targeted, baseline)
	}
	lines := protectedJSONAddedLines(*v)
	if !strings.Contains(lines, "`"+acceptBaseline+"` jsonAdded") || !strings.Contains(lines, "observed on check `"+acceptCheck+"`") || strings.Contains(lines, o.LabDir) {
		t.Fatalf("ledger lines %q", lines)
	}
	if strings.Contains(string(ledgerBytes(t, r)), o.LabDir+"/verification-history") {
		t.Fatal("the ledger recorded a local evidence path")
	}
}

func TestAcceptProtectedFailuresRefusals(t *testing.T) {
	for _, c := range []struct{ mode, want string }{
		{"candidate-fails", "the candidate view failed"},
		// Every candidate check recorded OK, but the view failed its post-check audit.
		{"candidate-audit", "the candidate view failed"},
		{"other-package", "no in-scope _test.go file"},
		{"panic", "stopped early"},
		{"timeout", "stopped early"},
		{"many", "at most 64 expected protected deltas"},
		{"pass", "did not fail"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			o, r := acceptFixture(t, c.mode)
			before := ledgerBytes(t, r)
			_, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, AmendRequest{AcceptProtectedFailures: true})
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "--accept-protected-failures refused: ") {
				t.Fatalf("got %v, want a refusal naming %q", err, c.want)
			}
			if strings.Contains(err.Error(), o.LabDir) || strings.Contains(err.Error(), o.SourceRoot) {
				t.Fatalf("refusal names a local path: %v", err)
			}
			if !bytes.Equal(before, ledgerBytes(t, r)) {
				t.Fatal("a refused amend changed the ledger")
			}
		})
	}
}

func TestAcceptProtectedFailuresRefusesAProtectedViewOfTheCandidateTree(t *testing.T) {
	t.Parallel()
	tree, derived := strings.Repeat("a", 40), strings.Repeat("b", 40)
	check := readiness.Check{ID: acceptCheck, Category: "regression", Argv: []string{"go", "test", "-json", "./..."}, Dir: "lab", Required: true}
	p := readiness.Plan{Analysis: readiness.Analysis{Checks: []readiness.Check{check}}}
	protected := func(source string) verification.Result {
		obs := verification.Observation{ID: check.ID, Category: "regression", SourceTree: source, Command: check}
		return verification.Result{SourceTree: tree, Protected: &verification.Result{SourceTree: source, Setup: []verification.Observation{}, Checks: []verification.Observation{obs}, Error: "required command " + check.ID + " did not pass"}}
	}
	if _, _, err := failingProtectedCheck(p, protected(derived)); err != nil {
		t.Fatalf("a derived protected tree was refused: %v", err)
	}
	if _, _, err := failingProtectedCheck(p, protected(tree)); err == nil || !strings.Contains(err.Error(), "did not run a derived protected tree") {
		t.Fatalf("a protected view of the candidate tree was accepted: %v", err)
	}
}

// TestAcceptProtectedFailuresSkipsCoveredTargetedChecks: a targeted check the protected
// view skipped as covered by the full suite, whether it comes before or after that
// baseline in plan order, is not the check the view stopped on: a not-run record
// (the baseline failed) or a covered record (it passed) is ignored, and the failing
// baseline is the one whose failures are declared. A covered record the plan does not
// back is refused.
func TestAcceptProtectedFailuresSkipsCoveredTargetedChecks(t *testing.T) {
	t.Parallel()
	tree, derived := strings.Repeat("a", 40), strings.Repeat("b", 40)
	targeted := readiness.Check{ID: acceptCheck, Category: "regression", Argv: []string{"go", "test", "./internal/..."}, Dir: "lab", Required: true}
	baseline := readiness.Check{ID: acceptBaseline, Category: "regression", Argv: []string{"go", "test", "-timeout", "20m", "./..."}, Dir: "lab", Required: true}
	failing := verification.Observation{ID: baseline.ID, Category: "regression", SourceTree: derived, Command: baseline, JSONAdded: true, Result: process.Result{Reconciled: true, Code: 1}}
	stopped := "required command " + baseline.ID + " did not pass"
	for name, checks := range map[string][]readiness.Check{
		"targeted first": {targeted, baseline},
		"baseline first": {baseline, targeted},
	} {
		p := readiness.Plan{FormulaBaseline: []string{baseline.ID}, Analysis: readiness.Analysis{Checks: checks}}
		if id, ok := verification.CoveringCheckID(p, targeted); !ok || id != baseline.ID {
			t.Fatalf("%s: the fixture's targeted check is not covered: %q %v", name, id, ok)
		}
		notRun := verification.Observation{ID: targeted.ID, Category: "regression", SourceTree: derived, Command: targeted, CoveredBy: baseline.ID, NotRun: "covered by " + baseline.ID + ", which failed", Result: process.Result{Reconciled: true, Code: -1}}
		result := verification.Result{SourceTree: tree, Protected: &verification.Result{SourceTree: derived, Setup: []verification.Observation{}, Checks: []verification.Observation{failing}, NotRun: []verification.Observation{notRun}, Error: stopped}}
		got, err := failingProtectedChecks(p, result)
		if err != nil || len(got) != 1 || got[0].check.ID != baseline.ID {
			t.Fatalf("%s: %v %+v", name, err, got)
		}
		// A covered record of a passing baseline among the checks is ignored as well.
		passed := failing
		passed.Result = process.Result{OK: true, Reconciled: true}
		other := readiness.Check{ID: "later", Category: "regression", Argv: []string{"go", "test", "-json", "./cmd/..."}, Dir: "lab", Required: true}
		p.Analysis.Checks = append(slices.Clone(checks), other)
		covered := verification.Observation{ID: targeted.ID, Category: "regression", SourceTree: derived, Command: targeted, CoveredBy: baseline.ID, Result: passed.Result}
		later := verification.Observation{ID: other.ID, Category: "regression", SourceTree: derived, Command: other, Result: process.Result{Reconciled: true, Code: 1}}
		ran := []verification.Observation{covered, passed, later}
		if name == "baseline first" {
			ran = []verification.Observation{passed, covered, later}
		}
		result.Protected = &verification.Result{SourceTree: derived, Setup: []verification.Observation{}, Checks: ran, Error: "required command later did not pass"}
		if c, _, err := failingProtectedCheck(p, result); err != nil || c.ID != other.ID {
			t.Fatalf("%s: a passing covered record: %v %q", name, err, c.ID)
		}
		// Coverage the plan does not give is refused.
		p.FormulaBaseline = nil
		result.Protected = &verification.Result{SourceTree: derived, Setup: []verification.Observation{}, Checks: []verification.Observation{failing}, NotRun: []verification.Observation{notRun}, Error: stopped}
		if _, err := failingProtectedChecks(p, result); err == nil || !strings.Contains(err.Error(), "does not cover it with") {
			t.Fatalf("%s: forged coverage: %v", name, err)
		}
	}
}

func TestAcceptProtectedFailuresRequestValidation(t *testing.T) {
	t.Parallel()
	o, r := amendFixture(t)
	before := ledgerBytes(t, r)
	for name, c := range map[string]struct {
		req  AmendRequest
		want string
	}{
		"with add-scope":         {AmendRequest{AcceptProtectedFailures: true, AddScope: []ScopeAddition{{Path: "lab/x/new.go", Reason: "New"}}}, "takes no --add-scope"},
		"without a verification": {AmendRequest{AcceptProtectedFailures: true}, "no verification of the retained tree"},
		"with --run":             {AmendRequest{AcceptProtectedFailures: true, Run: true}, "takes no --run: review the printed EXPECTED PROTECTED-TEST FAILURES, then approve the new digest with forgecell run <issue> --approve <digest>"},
	} {
		_, err := AmendFromLatest(context.Background(), o.LabDir, r.ID, c.req)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: %v, want %q", name, err, c.want)
		}
		if !bytes.Equal(before, ledgerBytes(t, r)) {
			t.Fatalf("%s: a refused amend changed the ledger", name)
		}
	}
}
