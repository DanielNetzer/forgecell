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
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const (
	acceptPackage = "example.test/lab/pin"
	acceptTest    = "lab/pin/pin_test.go"
	acceptCheck   = "go-test-lab"
)

// acceptFakeGo answers `go test -json` from the pinned test source the check runs
// against: the candidate's version says "new", the base version restored by the
// protected view says "old". The mode file next to it picks how the old version
// fails, so no Go toolchain runs and nothing depends on time.
const acceptFakeGo = `#!/bin/sh
[ "$1" = test ] || exit 2
mode=$(cat "$(dirname "$0")/mode")
pkg=example.test/lab/pin
fail() {
	for name in "$@"; do
		printf '{"Action":"run","Package":"%s","Test":"%s"}\n{"Action":"fail","Package":"%s","Test":"%s"}\n' "$failpkg" "$name" "$failpkg" "$name"
	done
	printf '{"Action":"fail","Package":"%s"}\n' "$failpkg"
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
	printf '{"Action":"run","Package":"%s","Test":"TestX"}\n{"Action":"pass","Package":"%s","Test":"TestX"}\n{"Action":"pass","Package":"%s"}\n' "$pkg" "$pkg" "$pkg"
	exit 0
fi
case "$mode" in
by-design|candidate-audit) fail TestX ;;
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
	bin := resolvedTempDir(t)
	if err := writeStub(filepath.Join(bin, "go"), []byte(acceptFakeGo), 0700); err != nil {
		t.Fatal(err)
	}
	setAcceptMode(t, bin, mode)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	o, repo := fixture(t)
	for file, body := range map[string]string{"lab/go.mod": "module example.test/lab\n", acceptTest: "package pin\n\n// pinned: old\n"} {
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
		a.Checks = append(a.Checks, readiness.Check{ID: acceptCheck, Category: "regression", Argv: []string{"go", "test", "-json", "./..."}, Dir: "lab", TimeoutMS: fixtureCheckTimeoutMS, Required: true, Definitions: []string{manifest}, Reason: "Module tests", Evidence: []string{"issue", manifest}})
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
