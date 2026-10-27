package verification

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const (
	baselinePkg   = "example.test/fix/pkg"
	baselineOther = "example.test/fix/other"
	baselineScope = "pkg/pin_test.go"
	baselineCheck = "baseline-go-test-lab"
)

// baselineFakeGo stands in for the go tool, so no toolchain runs and nothing depends
// on time. It logs each argv next to itself, answers in -json only when its argv asks
// for it, and passes while the pinned test source is the candidate's ("new"). On the
// base version restored by the protected view it fails TestA; the mode file picks how
// the whole-module run (./...) fails beyond that.
const baselineFakeGo = `#!/bin/sh
bin=$(dirname "$0")
[ "$1" = test ] || exit 2
printf '%s\n' "$*" >> "$bin/argv.log"
shift
json=no
scope=targeted
for a in "$@"; do
	case "$a" in
	-args) break ;;
	-json) json=yes ;;
	-json=false) json=no ;;
	./...) scope=baseline ;;
	esac
done
mode=$(cat "$bin/mode")
pkg=example.test/fix/pkg
other=example.test/fix/other
ev() { printf '{"Action":"%s","Package":"%s","Test":"%s"}\n' "$1" "$2" "$3"; }
pk() { printf '{"Action":"%s","Package":"%s"}\n' "$1" "$2"; }
summ() { printf '{"Action":"output","Package":"%s","Output":"%s\\t%s\\t0.01s\\n"}\n' "$2" "$1" "$2"; }
passing() {
	if [ $json = yes ]; then
		ev run "$1" TestA; ev pass "$1" TestA; summ "ok  " "$1"; pk pass "$1"
	else
		printf 'ok  \t%s\t0.01s\n' "$1"
	fi
}
if grep -q new pkg/pin_test.go; then
	passing $pkg
	[ $scope = baseline ] && passing $other
	exit 0
fi
failing="TestA"
if [ $scope = baseline ]; then
	case "$mode" in
	extra) failing="TestA TestB" ;;
	panic) [ $json = yes ] && printf '{"Action":"output","Package":"%s","Test":"TestA","Output":"panic: boom\\n"}\n' $pkg ;;
	timeout) [ $json = yes ] && printf '{"Action":"output","Package":"%s","Output":"panic: test timed out after 20m0s\\n"}\n' $pkg ;;
	truncate) head -c 1200000 /dev/zero | tr '\0' x; echo; exit 1 ;;
	unparsed) echo not-json ;;
	buildfail)
		if [ $json = yes ]; then
			printf '{"ImportPath":"%s [%s.test]","Action":"build-output","Output":"# %s\\n"}\n' $pkg $pkg $pkg
			printf '{"ImportPath":"%s [%s.test]","Action":"build-output","Output":"pkg/pin_test.go:3:9: undefined: pinnedOld\\n"}\n' $pkg $pkg
			printf '{"ImportPath":"%s [%s.test]","Action":"build-fail"}\n' $pkg $pkg
			printf '{"Action":"output","Package":"%s","Output":"FAIL\\t%s [build failed]\\n"}\n' $pkg $pkg
			pk fail $pkg
		else
			printf '# %s\npkg/pin_test.go:3:9: undefined: pinnedOld\nFAIL\t%s [build failed]\n' $pkg $pkg
		fi
		exit 1 ;;
	testmain)
		if [ $json = yes ]; then
			printf '{"Action":"output","Package":"%s","Output":"TestMain: fixture setup refused\\n"}\n' $pkg
			summ FAIL $pkg; pk fail $pkg
		else
			printf 'TestMain: fixture setup refused\nFAIL\t%s\t0.01s\n' $pkg
		fi
		exit 1 ;;
	esac
fi
if [ $json = yes ]; then
	for t in $failing; do
		ev run $pkg $t
		if [ $t = TestB ]; then
			for n in $(seq 1 60); do printf '{"Action":"output","Package":"%s","Test":"TestB","Output":"    pin_test.go:%s: noise\\n"}\n' $pkg $n; done
			printf '{"Action":"output","Package":"%s","Test":"TestB","Output":"    pin_test.go:9: want old, got new\\n"}\n' $pkg
		fi
		ev fail $pkg $t
	done
	summ FAIL $pkg; pk fail $pkg
else
	for t in $failing; do printf '%s\n' "--- FAIL: $t"; done
	printf 'FAIL\t%s\t0.01s\n' $pkg
fi
[ $scope = baseline ] && passing $other
exit 1
`

// baselineFixture commits a fixture module with a pinned test source, a targeted
// go test -json check of its package and a plain go test Formula baseline over the
// whole module. The candidate rewrites the pinned source. The fake go is first on
// PATH, so the test is not parallel.
func baselineFixture(t *testing.T, mode string, deltas ...readiness.ProtectedDelta) (string, string, readiness.Plan, CaptureResult) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(baselineFakeGo), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir, _ := sourceFixture(t)
	for file, body := range map[string]string{"go.mod": "module example.test/fix\n\ngo 1.22\n", baselineScope: "package pkg\n\n// pinned: old\n"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "pinned test")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	p.Analysis.Scope = []readiness.ScopedPath{{Path: baselineScope, Reason: "The pinned behaviour changes by design", Evidence: []string{"issue"}}}
	targeted := p.Analysis.Checks[0]
	targeted.ID, targeted.Argv = "targeted", []string{"go", "test", "-json", "./pkg/..."}
	baseline := p.Analysis.Checks[0]
	baseline.ID, baseline.Argv, baseline.Reason = baselineCheck, []string{"go", "test", "-timeout", "20m", "./..."}, "Formula baseline"
	p.Analysis.Checks = []readiness.Check{targeted, baseline}
	p.FormulaBaseline = []string{baselineCheck}
	p.ExpectedProtectedDeltas = deltas
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, baselineScope), []byte("package pkg\n\n// pinned: new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Capture(context.Background(), dir, base, []string{baselineScope}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return bin, dir, p, c
}

// baselineDelta declares test as amend --accept-protected-failures does: observed on
// check, applying by identity (deltaScope identity).
func baselineDelta(check, test string) readiness.ProtectedDelta {
	d := legacyDelta(check, test)
	d.DeltaScope = readiness.DeltaScopeIdentity
	return d
}

// legacyDelta declares test as every earlier plan did: for the check it names only.
func legacyDelta(check, test string) readiness.ProtectedDelta {
	return readiness.ProtectedDelta{CheckID: check, Package: baselinePkg, Test: test, ScopePath: baselineScope, Reason: "The pinned behaviour changes by design", Evidence: []string{"issue"}}
}

func argvLog(t *testing.T, bin string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bin, "argv.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestProtectedDeltaCoversTheTargetedCheckAndThePlainBaseline(t *testing.T) {
	bin, dir, p, c := baselineFixture(t, "by-design", baselineDelta("targeted", "TestA"))
	approved := append([]string{}, p.Analysis.Checks[1].Argv...)
	digest, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Error != "" || v.Protected == nil || !v.Protected.RequiredChecksPassed || len(v.Protected.Checks) != 2 {
		t.Fatalf("a by-design failure in the targeted check and the baseline was refused: %q %+v", v.Error, v.Protected)
	}
	// The candidate view ran exactly the approved argv; only the protected baseline run
	// had -json inserted after go test.
	want := []string{"test -json ./pkg/...", "test -timeout 20m ./...", "test -json ./pkg/...", "test -json -timeout 20m ./..."}
	if got := argvLog(t, bin); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if after, err := p.Digest(); err != nil || after != digest || !reflect.DeepEqual(p.Analysis.Checks[1].Argv, approved) {
		t.Fatalf("the plan changed: digest %s -> %s, argv %v", digest, after, p.Analysis.Checks[1].Argv)
	}
	for _, o := range v.Checks {
		if o.JSONAdded || len(o.AcceptedDeltas) != 0 || !o.Result.OK {
			t.Fatalf("the candidate view was changed: %+v", o)
		}
	}
	if strings.Contains(v.Checks[1].Result.Stdout, `"Action"`) || !reflect.DeepEqual(v.Checks[1].Command, p.Analysis.Checks[1]) {
		t.Fatalf("candidate baseline: %+v", v.Checks[1])
	}
	targeted, baseline := v.Protected.Checks[0], v.Protected.Checks[1]
	if targeted.JSONAdded || targeted.Result.OK || len(targeted.AcceptedDeltas) != 1 || targeted.AcceptedDeltas[0].Test != "TestA" {
		t.Fatalf("targeted: %+v", targeted)
	}
	if !baseline.JSONAdded || baseline.Result.OK || len(baseline.AcceptedDeltas) != 1 || baseline.AcceptedDeltas[0].CheckID != "targeted" {
		t.Fatalf("baseline: jsonAdded %t accepted %+v", baseline.JSONAdded, baseline.AcceptedDeltas)
	}
	if !reflect.DeepEqual(baseline.FailedTests, []TestID{{baselinePkg, "TestA"}}) || !reflect.DeepEqual(baseline.Command.Argv, approved) {
		t.Fatalf("baseline failed %v, command %v", baseline.FailedTests, baseline.Command.Argv)
	}
	// The retained stdout is a bounded plain summary, never the -json stream.
	out := baseline.Result.Stdout
	if strings.Contains(out, `"Action"`) || !strings.Contains(out, "--- FAIL: TestA ("+baselinePkg+")") || !strings.Contains(out, "FAIL\t"+baselinePkg+"\t0.01s") || !strings.Contains(out, "ok  \t"+baselineOther+"\t0.01s") || baseline.Result.Truncated || baseline.Result.RawStdout != out {
		t.Fatalf("baseline stdout %q", out)
	}
	if baseline.Packages == nil || baseline.Packages.Run != 2 {
		t.Fatalf("package counts from the summary: %+v", baseline.Packages)
	}
	if len(v.AcceptedDeltas) != 1 || len(v.Warnings) != 0 {
		t.Fatalf("a delta accepted by two checks must be listed once: %+v %v", v.AcceptedDeltas, v.Warnings)
	}
	// Delivery re-checks the baseline by identity and by its recorded set.
	tree := v.Protected.SourceTree
	if !MatchesCheckObservation(p, p.Analysis.Checks[1], baseline, tree, true) {
		t.Fatal("delivery rejected the baseline covered by a delta observed on the targeted check")
	}
	compact := baseline
	compact.Result.Stdout, compact.Result.RawStdout = "", ""
	if !MatchesCheckObservation(p, p.Analysis.Checks[1], compact, tree, true) {
		t.Fatal("delivery rejected the compact projection of the baseline")
	}
	widened := compact
	widened.FailedTests = append(append([]TestID{}, compact.FailedTests...), TestID{baselinePkg, "TestB"})
	if MatchesCheckObservation(p, p.Analysis.Checks[1], widened, tree, true) {
		t.Fatal("an uncovered recorded baseline failure matched")
	}
	forged := targeted
	forged.JSONAdded = true
	if MatchesCheckObservation(p, p.Analysis.Checks[0], forged, tree, true) {
		t.Fatal("a jsonAdded claim on a check that already requests -json matched")
	}
	got, err := ProtectedFailuresForDeltas(baseline)
	if err != nil || !reflect.DeepEqual(got, baseline.FailedTests) {
		t.Fatalf("accept helper on an engine-streamed check: %v %v", got, err)
	}
}

func TestProtectedBaselineUndeclaredFailureStillFails(t *testing.T) {
	_, dir, p, c := baselineFixture(t, "extra", baselineDelta("targeted", "TestA"))
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Protected == nil || len(v.Protected.Checks) != 2 || !strings.Contains(v.Error, "required command "+baselineCheck+" did not pass") || !strings.Contains(v.Error, baselinePkg+" TestB") {
		t.Fatalf("an undeclared baseline failure passed: %q", v.Error)
	}
	baseline := v.Protected.Checks[1]
	if !baseline.JSONAdded || len(baseline.AcceptedDeltas) != 0 {
		t.Fatalf("baseline: %+v", baseline)
	}
	// The cleanly parsed failing set is recorded for --accept-protected-failures; it
	// never changes the verdict.
	if !reflect.DeepEqual(baseline.FailedTests, []TestID{{baselinePkg, "TestA"}, {baselinePkg, "TestB"}}) || MatchesCheckObservation(p, p.Analysis.Checks[1], baseline, v.Protected.SourceTree, true) {
		t.Fatalf("recorded failing set %v", baseline.FailedTests)
	}
	// The plain summary keeps a bounded tail of the undeclared failure's own output, so
	// a reviewer can see why it failed: its last 40 lines, saying what was dropped.
	out := baseline.Result.Stdout
	header := "=== output tail of failing test TestB (" + baselinePkg + "): last 40 line(s), 21 earlier line(s) dropped\n"
	if !strings.Contains(out, header) || !strings.Contains(out, "    pin_test.go:9: want old, got new\n") || !strings.Contains(out, "pin_test.go:22: noise") || strings.Contains(out, "pin_test.go:21: noise") || strings.Contains(out, `"Action"`) {
		t.Fatalf("baseline summary lacks the bounded tail: %q", out)
	}
	if strings.Contains(out, "output tail of failing test TestA") {
		t.Fatalf("a test without output gained a tail: %q", out)
	}
}

// The per-test tail is bounded in lines, bytes and tracked tests, a passing test drops
// its tail, an unfinished test keeps one, and the summary says when tails were cut.
func TestStreamedFailureTailsAreBounded(t *testing.T) {
	p := newFailedTestParser()
	event := func(action, test, output string) {
		raw, err := json.Marshal(goTestEvent{Action: action, Package: baselinePkg, Test: test, Output: output})
		if err != nil {
			t.Fatal(err)
		}
		p.Line(raw)
	}
	event("run", "TestPass", "")
	event("output", "TestPass", "secret of a passing test\n")
	event("pass", "TestPass", "")
	event("run", "TestBig", "")
	for i := 0; i < 3; i++ {
		event("output", "TestBig", strings.Repeat("y", maxTailBytes)+"\n")
	}
	event("output", "TestBig", "last line\n")
	event("fail", "TestBig", "")
	event("run", "TestHung", "")
	event("output", "TestHung", "panic: boom\n")
	for i := 0; i < maxTailedTests+10; i++ {
		id := fmt.Sprintf("TestMany%d", i)
		event("run", id, "")
		event("output", id, "x\n")
		event("fail", id, "")
	}
	if len(p.tails) > maxTailedTests {
		t.Fatalf("%d tails tracked", len(p.tails))
	}
	summary := p.tailSummary()
	if strings.Contains(summary, "passing test") || !strings.Contains(summary, "output tail of failing test TestBig") || !strings.Contains(summary, "    last line\n") || !strings.Contains(summary, "tails cut at their bounds") {
		t.Fatalf("tail summary: %.400q", summary)
	}
	if big := p.tails[TestID{baselinePkg, "TestBig"}]; big == nil || big.bytes > maxTailBytes || len(big.lines) > maxTailLines || big.dropped == 0 {
		t.Fatalf("TestBig tail: %+v", big)
	}
	if !strings.Contains(summary, "output tail of unfinished test TestHung ("+baselinePkg+"): last 1 line(s)\n    panic: boom\n") {
		t.Fatalf("the unfinished test lost its tail: %.400q", summary)
	}
	if len(summary) > maxTailSummaryBytes+300 {
		t.Fatalf("tail summary of %d bytes", len(summary))
	}
}

func TestProtectedBaselineStreamFailsClosed(t *testing.T) {
	for mode, wanted := range map[string]string{
		"panic":    "stopped early",
		"timeout":  "stopped early",
		"truncate": "output is incomplete",
		"unparsed": "not go test -json",
	} {
		t.Run(mode, func(t *testing.T) {
			_, dir, p, c := baselineFixture(t, mode, baselineDelta("targeted", "TestA"))
			v := runDelta(t, dir, p, c)
			if v.RequiredChecksPassed || v.Protected == nil || len(v.Protected.Checks) != 2 || !strings.Contains(v.Error, "required command "+baselineCheck+" did not pass") || !strings.Contains(v.Error, wanted) {
				t.Fatalf("a baseline that must fail closed passed or named another reason: %q", v.Error)
			}
			baseline := v.Protected.Checks[1]
			if !baseline.JSONAdded || len(baseline.AcceptedDeltas) != 0 || len(baseline.FailedTests) != 0 || len(v.AcceptedDeltas) != 0 {
				t.Fatalf("a refused stream recorded evidence it cannot support: %+v", baseline)
			}
			if _, err := ProtectedFailuresForDeltas(baseline); err == nil {
				t.Fatal("a refused engine-streamed baseline was turned into a failing set")
			}
			if len(baseline.Result.Stdout) > maxStreamSummaryBytes+200 {
				t.Fatalf("the retained summary is unbounded: %d bytes", len(baseline.Result.Stdout))
			}
		})
	}
}

func TestProtectedBaselineWithoutDeclaredDeltasRunsAsBefore(t *testing.T) {
	bin, dir, p, c := baselineFixture(t, "by-design")
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Error != "protected regression did not pass: required command targeted did not pass" {
		t.Fatalf("verdict: %q", v.Error)
	}
	if got := argvLog(t, bin); !reflect.DeepEqual(got, []string{"test -json ./pkg/...", "test -timeout 20m ./...", "test -json ./pkg/..."}) {
		t.Fatalf("argv = %q", got)
	}
	if o := v.Protected.Checks[0]; o.JSONAdded || !reflect.DeepEqual(o.FailedTests, []TestID{{baselinePkg, "TestA"}}) {
		t.Fatalf("targeted: %+v", o)
	}
}

// Identity matching is an explicit, digest-bound opt-in: an already approved plan
// whose delta names only the targeted check never excuses the same test failing in
// the baseline, which then runs exactly as approved; the same plan with deltaScope
// identity passes both, under a different digest.
func TestProtectedDeltaIdentityScopeIsAnExplicitOptIn(t *testing.T) {
	bin, dir, p, c := baselineFixture(t, "by-design", legacyDelta("targeted", "TestA"))
	legacy, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Protected == nil || len(v.Protected.Checks) != 2 || !strings.Contains(v.Error, "required command "+baselineCheck+" did not pass") {
		t.Fatalf("a CheckID-bound delta excused the baseline: %q", v.Error)
	}
	// The protected view streams the plain baseline with -json even without applicable
	// deltas (#257), which records its failing set but never excuses it.
	if targeted, baseline := v.Protected.Checks[0], v.Protected.Checks[1]; len(targeted.AcceptedDeltas) != 1 || !baseline.JSONAdded || len(baseline.AcceptedDeltas) != 0 {
		t.Fatalf("targeted %+v baseline %+v", targeted, baseline)
	}
	if got := argvLog(t, bin); got[3] != "test -json -timeout 20m ./..." {
		t.Fatalf("the baseline without applicable deltas was not streamed: %q", got)
	}

	_, dir, p, c = baselineFixture(t, "by-design", baselineDelta("targeted", "TestA"))
	if identity, err := p.Digest(); err != nil || identity == legacy {
		t.Fatalf("deltaScope identity must change the digest: %v", err)
	}
	if v = runDelta(t, dir, p, c); !v.RequiredChecksPassed || !v.Protected.Checks[1].JSONAdded || len(v.Protected.Checks[1].AcceptedDeltas) != 1 {
		t.Fatalf("the identity-scoped plan: %q %+v", v.Error, v.Protected)
	}
}

// Plans that name a check by CheckID keep working: deltas citing each check, the
// plain baseline included, validate and are honoured there, and a legacy
// shell-wrapped -json check keeps the deltas that name it while the baseline applies
// an identity-scoped one.
func TestProtectedDeltasNamingACheckIDAreStillHonoured(t *testing.T) {
	_, dir, p, c := baselineFixture(t, "by-design", legacyDelta("targeted", "TestA"), legacyDelta(baselineCheck, "TestA"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || len(v.AcceptedDeltas) != 2 || v.AcceptedDeltas[1].CheckID != baselineCheck {
		t.Fatalf("deltas naming each check: %q %+v", v.Error, v.AcceptedDeltas)
	}
	if !v.Protected.Checks[1].JSONAdded || len(v.Protected.Checks[0].AcceptedDeltas) != 1 || v.Protected.Checks[1].AcceptedDeltas[0].CheckID != baselineCheck {
		t.Fatalf("protected checks: %+v", v.Protected.Checks)
	}

	bin, dir, p, c := baselineFixture(t, "by-design", baselineDelta("targeted", "TestA"))
	p.Analysis.Checks[0].Argv = []string{"/bin/sh", "-c", "go test -json ./pkg/..."}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	v = runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Protected == nil || len(v.Protected.Checks[0].AcceptedDeltas) != 1 || len(v.Protected.Checks[1].AcceptedDeltas) != 1 || !v.Protected.Checks[1].JSONAdded {
		t.Fatalf("a legacy wrapper plan: %q %+v", v.Error, v.Protected)
	}
	if got := argvLog(t, bin); got[3] != "test -json -timeout 20m ./..." {
		t.Fatalf("argv = %q", got)
	}
	for i, check := range p.Analysis.Checks {
		if !MatchesCheckObservation(p, check, v.Protected.Checks[i], v.Protected.SourceTree, true) {
			t.Fatalf("delivery rejected protected check %s", check.ID)
		}
	}
}
