package verification

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func streamParse(stream string) ([]TestID, error) {
	p := newFailedTestParser()
	for _, line := range strings.Split(stream, "\n") {
		if err := p.Line([]byte(line)); err != nil {
			return nil, err
		}
	}
	return p.Finish()
}

// The incremental parser and the whole-string parser must agree on every stream,
// including each fail-closed refusal.
func TestStreamedParsingMatchesStringParsing(t *testing.T) {
	const pkg = `"Package":"example.test/p"`
	for name, stream := range map[string]string{
		"clean declared failure": events(`{"Action":"run",`+pkg+`,"Test":"TestA"}`, `{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `{"Action":"fail",`+pkg+`}`),
		"subtests and parents":   events(`{"Action":"fail",`+pkg+`,"Test":"TestA/sub"}`, `{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `{"Action":"fail",`+pkg+`}`),
		"blank lines and CRLF":   "\n" + `{"Action":"fail",` + pkg + `,"Test":"TestA"}` + "\r\n\r\n" + `{"Action":"fail",` + pkg + `}` + "\r\n",
		"passing stream":         events(`{"Action":"run",`+pkg+`,"Test":"TestA"}`, `{"Action":"pass",`+pkg+`,"Test":"TestA"}`, `{"Action":"pass",`+pkg+`}`),
		"recovered panic text":   events(`{"Action":"output",`+pkg+`,"Test":"TestA","Output":"panic: recovered\n"}`, `{"Action":"pass",`+pkg+`,"Test":"TestA"}`, `{"Action":"pass",`+pkg+`}`),
		"non-JSON line":          events(`{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `not json`, `{"Action":"fail",`+pkg+`}`),
		"event without action":   events(`{"Package":"example.test/p"}`),
		"build failure":          events(`{"Action":"build-fail","ImportPath":"example.test/p"}`, `{"Action":"fail",`+pkg+`}`),
		"panic abort":            events(`{"Action":"run",`+pkg+`,"Test":"TestA"}`, `{"Action":"output",`+pkg+`,"Output":"panic: boom\n"}`, `{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `{"Action":"fail",`+pkg+`}`),
		"timeout abort":          events(`{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `{"Action":"output",`+pkg+`,"Output":"panic: test timed out after 1s\n"}`, `{"Action":"fail",`+pkg+`}`),
		"started never finished": events(`{"Action":"run",`+pkg+`,"Test":"TestB"}`, `{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `{"Action":"fail",`+pkg+`}`),
		"failed package only":    events(`{"Action":"fail",` + pkg + `}`),
		"empty stream":           "",
		"only blank lines":       "\n\n  \n",
		"truncated final line":   events(`{"Action":"fail",`+pkg+`,"Test":"TestA"}`, `{"Action":"fail","Pack`),
	} {
		t.Run(name, func(t *testing.T) {
			want, wantErr := parseFailedTests(stream)
			got, gotErr := streamParse(stream)
			if (wantErr == nil) != (gotErr == nil) || (wantErr != nil && wantErr.Error() != gotErr.Error()) {
				t.Fatalf("errors differ: string %v, streamed %v", wantErr, gotErr)
			}
			if !reflect.DeepEqual(want, got) {
				t.Fatalf("failing sets differ: string %v, streamed %v", want, got)
			}
		})
	}
}

func TestStreamedParserKeepsOnlyFailureStateNotTheStream(t *testing.T) {
	p := newFailedTestParser()
	filler := `{"Action":"output","Package":"example.test/p","Test":"TestA","Output":"` + strings.Repeat("x", 1000) + `\n"}`
	for i := 0; i < 5000; i++ {
		if err := p.Line([]byte(filler)); err != nil {
			t.Fatal(err)
		}
	}
	if len(p.failed) != 0 || len(p.started) != 0 || len(p.failedPackages) != 0 || len(p.aborted) != 0 {
		t.Fatalf("output events must not accumulate state: %+v", p)
	}
}

// The plain summary an engine-streamed check keeps is bounded and says when it was
// cut; the parser's package memory is bounded too and fails closed past it.
func TestStreamSummaryAndPackageMemoryAreBounded(t *testing.T) {
	p := newFailedTestParser()
	line := `{"Action":"output","Package":"example.test/p","Output":"ok  \texample.test/p\t0.01s\n"}`
	for i := 0; i < 5000; i++ {
		p.Line([]byte(line))
	}
	p.Line([]byte(`{"Action":"output","Package":"example.test/p","Test":"TestA","Output":"FAIL\texample.test/q\t0.01s\n"}`))
	var failed []TestID
	for i := 0; i < 5000; i++ {
		failed = append(failed, TestID{Package: "example.test/p", Test: "TestMany" + strings.Repeat("x", 20)})
	}
	summary := p.Summary(failed)
	if len(summary) > maxStreamSummaryBytes+200 || !strings.Contains(summary, "summary cut") || strings.Contains(summary, `"Action"`) || strings.Contains(summary, "example.test/q") {
		t.Fatalf("summary of %d bytes: %.200q", len(summary), summary)
	}
	if !strings.HasPrefix(summary, "--- FAIL: TestMany") || !strings.Contains(summary, "ok  \texample.test/p\t0.01s\n") {
		t.Fatalf("summary keeps failing ids then package lines: %.200q", summary)
	}
	if short := newFailedTestParser().Summary(nil); short != "" {
		t.Fatalf("an empty stream has an empty summary: %q", short)
	}
	many := newFailedTestParser()
	for i := 0; i <= maxStreamedPackages; i++ {
		many.Line([]byte(`{"Action":"pass","Package":"example.test/p` + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + `/` + strconv.Itoa(i) + `"}`))
	}
	if _, err := many.Finish(); err == nil || !strings.Contains(err.Error(), "packages") || len(many.packages) > maxStreamedPackages {
		t.Fatalf("package memory: %d, %v", len(many.packages), err)
	}
}

func TestUncoveredFailuresRequireDeclaredParents(t *testing.T) {
	declare := func(tests ...string) []readiness.ProtectedDelta {
		var out []readiness.ProtectedDelta
		for _, test := range tests {
			out = append(out, pinnedDelta(deltaPkg, test))
		}
		return out
	}
	failed := []TestID{{deltaPkg, "TestParent"}, {deltaPkg, "TestParent/sub"}, {deltaPkg, "TestParent/sub/case"}}
	if got := uncoveredFailures(declare("TestParent/sub/case"), failed); !reflect.DeepEqual(got, failed[:2]) {
		t.Fatalf("undeclared parents must be uncovered: %v", got)
	}
	if got := uncoveredFailures(declare("TestParent", "TestParent/sub", "TestParent/sub/case"), failed); len(got) != 0 {
		t.Fatalf("declared parents are covered: %v", got)
	}
	if got := uncoveredFailures(declare("TestParent/sub/case", "TestParent/sub"), failed); !reflect.DeepEqual(got, failed[:1]) {
		t.Fatalf("only the top-level parent is undeclared: %v", got)
	}
}

func TestUndeclaredParentFailureNamesTheParentAndTheRule(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestParent/sub/case", "TestParent/sub", "TestParent"), pinnedDelta(deltaPkg, "TestParent/sub/case"))
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || len(v.AcceptedDeltas) != 0 {
		t.Fatalf("an undeclared parent was masked: %+v", v)
	}
	for _, want := range []string{deltaPkg + " TestParent (", deltaPkg + " TestParent/sub (", "parent of declared failing subtest TestParent/sub/case", "must be declared explicitly"} {
		if !strings.Contains(v.Error, want) {
			t.Fatalf("error does not say %q: %q", want, v.Error)
		}
	}
}

func TestDeclaredParentsAreAccepted(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestParent/sub/case", "TestParent/sub", "TestParent"),
		pinnedDelta(deltaPkg, "TestParent/sub/case"), pinnedDelta(deltaPkg, "TestParent/sub"), pinnedDelta(deltaPkg, "TestParent"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || len(v.AcceptedDeltas) != 3 || len(v.Warnings) != 0 {
		t.Fatalf("declared parents were refused: %+v", v)
	}
}

func TestDeliveryRecheckRequiresDeclaredParents(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestParent/sub", "TestParent"), pinnedDelta(deltaPkg, "TestParent/sub"), pinnedDelta(deltaPkg, "TestParent"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Protected == nil {
		t.Fatalf("setup: %+v", v)
	}
	check, tree, obs := p.Analysis.Checks[0], v.Protected.SourceTree, v.Protected.Checks[0]
	obs.Result.Stdout, obs.Result.RawStdout = "", ""
	if !MatchesCheckObservation(p, check, obs, tree, true) {
		t.Fatal("a recorded failing set with declared parents was rejected")
	}
	subtestOnly := p
	subtestOnly.ExpectedProtectedDeltas = p.ExpectedProtectedDeltas[:1]
	if MatchesCheckObservation(subtestOnly, check, obs, tree, true) {
		t.Fatal("delivery accepted a recorded failing parent that the plan does not declare")
	}
}

func TestPassingDeclaredDeltaWarningsSayToAmendThePlan(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), passingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], deltaPkg+" TestPinned") || !strings.Contains(v.Warnings[0], "amend the plan to remove it") {
		t.Fatalf("the did-not-fail warning must say to amend the plan: %+v", v.Warnings)
	}
	if err := os.WriteFile(filepath.Join(dir, "testdata", "pin.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	unchanged, err := Capture(context.Background(), dir, p.Inputs.BaseCommit, []string{"testdata/pin.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v = runDelta(t, dir, p, unchanged)
	if !v.RequiredChecksPassed || v.Protected != nil || len(v.Warnings) != 1 || !strings.Contains(v.Warnings[0], "not exercised") || !strings.Contains(v.Warnings[0], "amend the plan to remove it") {
		t.Fatalf("the not-exercised warning must say to amend the plan: %+v", v)
	}
}

func TestEvaluatorRefusesATruncatedRetainedStream(t *testing.T) {
	declared := []readiness.ProtectedDelta{pinnedDelta(deltaPkg, "TestPinned")}
	stream := `{"Action":"fail","Package":"example.test/pkg","Test":"TestPinned"}` + "\n" + `{"Action":"fail","Package":"example.test/pkg"}` + "\n"
	if _, err := EvaluateProtectedDeltas(declared, process.Result{Reconciled: true, Code: 1, Truncated: true, Stdout: stream}); err == nil {
		t.Fatal("a retained prefix was parsed as if it were the complete stream")
	}
	parser := newFailedTestParser()
	for _, line := range strings.Split(stream, "\n") {
		parser.Line([]byte(line))
	}
	r := process.Result{Reconciled: true, Code: 1, Truncated: true, Stdout: "prefix only"}
	e, err := evaluateStreamedDeltas(declared, r, parser)
	if err != nil || len(e.Accepted) != 1 || len(e.Failed) != 1 {
		t.Fatalf("the streamed failing set was not used: %v %+v", err, e)
	}
	for i, bad := range []process.Result{
		{Reconciled: true, Code: 1, TimedOut: true},
		{Reconciled: true, Code: 1, ParentDeadline: true},
		{Reconciled: true, Code: 1, Interrupted: true},
		{Reconciled: true, Code: 1, Overflow: true},
		{Code: 1},
		{Reconciled: true, Code: 0},
	} {
		if _, err := evaluateStreamedDeltas(declared, bad, parser); err == nil {
			t.Fatalf("incomplete streamed result %d was accepted", i)
		}
	}
}

func TestDeliveryRecheckUsesTheRecordedSetForATruncatedObservation(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Protected == nil {
		t.Fatalf("setup: %+v", v)
	}
	check, tree, obs := p.Analysis.Checks[0], v.Protected.SourceTree, v.Protected.Checks[0]
	obs.Result.Truncated, obs.Result.Stdout = true, "{not a complete stream"
	if !MatchesCheckObservation(p, check, obs, tree, true) {
		t.Fatal("a truncated retained stream must be judged by the recorded failing set")
	}
	obs.FailedTests = append(obs.FailedTests, TestID{Package: deltaPkg, Test: "TestOther"})
	if MatchesCheckObservation(p, check, obs, tree, true) {
		t.Fatal("an uncovered recorded failure matched")
	}
}

// bigPrefix prints well over 256,000 bytes of valid go test -json output events.
func bigPrefix() string {
	return "i=0\nwhile [ $i -lt 1500 ]; do\nprintf '%s\\n' '{\"Action\":\"output\",\"Package\":\"example.test/pkg\",\"Test\":\"TestPinned\",\"Output\":\"" + strings.Repeat("x", 120) + "\\n\"}'\ni=$((i+1))\ndone\n"
}

func TestProtectedDeltasAreEvaluatedFromAStreamOverTheCaptureCap(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), bigPrefix()+failingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Error != "" || v.Protected == nil || !v.Protected.RequiredChecksPassed {
		t.Fatalf("a large declared failure was refused: %+v", v.Error)
	}
	if len(v.AcceptedDeltas) != 1 || v.AcceptedDeltas[0].Test != "TestPinned" || len(v.Warnings) != 0 {
		t.Fatalf("accepted delta not listed: %+v", v)
	}
	if r := v.Checks[0].Result; r.Truncated || r.Overflow {
		t.Fatalf("the candidate view keeps its overflow guard and passes only a complete stream: %+v", r)
	}
	if r := v.Protected.Checks[0].Result; r.Overflow || !r.Truncated || len(r.Stdout) > 256000 || r.Activity == nil || r.Activity.StdoutBytes <= 256000 {
		t.Fatalf("protected: the retained capture must stay capped while the stream is read in full: truncated=%t overflow=%t retained=%d activity=%+v", r.Truncated, r.Overflow, len(r.Stdout), r.Activity)
	}
	obs := v.Protected.Checks[0]
	if obs.Result.OK || len(obs.FailedTests) != 1 || len(obs.AcceptedDeltas) != 1 {
		t.Fatalf("protected observation lost the delta evidence: %+v", obs)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var back Result
	if err = json.Unmarshal(raw, &back); err != nil || !back.Protected.Checks[0].Result.Truncated || len(back.Protected.Checks[0].FailedTests) != 1 {
		t.Fatalf("streamed evidence did not survive the JSON projection: %v", err)
	}
	check, tree := p.Analysis.Checks[0], v.Protected.SourceTree
	if !MatchesCheckObservation(p, check, obs, tree, true) {
		t.Fatal("delivery rejected a streamed protected observation covered by the plan")
	}
}

func TestStreamedProtectedDeltasKeepEveryFailClosedRule(t *testing.T) {
	long := "head -c 1200000 /dev/zero | tr '\\0' x\necho\nexit 1"
	cases := []struct {
		name, body, wanted string
	}{
		{"undeclared test", failingBody(deltaPkg, "TestPinned", "TestOther"), deltaPkg + " TestOther"},
		{"build failure", `printf '%s\n' '{"Action":"build-fail","ImportPath":"example.test/pkg"}'` + "\nexit 2", "build failure"},
		{"non-JSON line", "echo not-json\n" + deltaLine("fail", deltaPkg, "TestPinned") + "\n" + deltaLine("fail", deltaPkg, "") + "\nexit 1", "not go test -json"},
		{"started but never finished", deltaLine("run", deltaPkg, "TestB") + "\n" + deltaLine("fail", deltaPkg, "TestPinned") + "\n" + deltaLine("fail", deltaPkg, "") + "\nexit 1", "started but never finished"},
		{"abort marker", `printf '%s\n' '{"Action":"output","Package":"example.test/pkg","Test":"TestPinned","Output":"panic: boom\n"}'` + "\n" + failingBody(deltaPkg, "TestPinned"), "stopped early"},
		{"package failure without a failing test", deltaLine("fail", deltaPkg, "") + "\nexit 1", "failed without a failing test"},
		{"non-zero exit without a failing test", deltaLine("pass", deltaPkg, "TestPinned") + "\nexit 1", "without any failing test"},
		{"line over the stream bound", long, "output is incomplete"},
		{"timeout", strings.TrimSuffix(failingBody(deltaPkg, "TestPinned"), "exit 1") + "sleep 30\nexit 1", "output is incomplete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), bigPrefix()+tc.body, pinnedDelta(deltaPkg, "TestPinned"))
			v := runDelta(t, dir, p, c)
			if v.RequiredChecksPassed || v.IndependentAcceptanceVerified || v.Protected == nil || v.Protected.RequiredChecksPassed || len(v.AcceptedDeltas) != 0 {
				t.Fatalf("a streamed run that must refuse passed: %+v", v)
			}
			if !strings.Contains(v.Error, "protected regression did not pass") || !strings.Contains(v.Error, tc.wanted) {
				t.Fatalf("error does not name the failure %q: %q", tc.wanted, v.Error)
			}
		})
	}
}

func TestCapAndOverflowAreUnchangedWithoutDeclaredDeltasOrJSON(t *testing.T) {
	dir, p, c := deltaFixture(t, bigPrefix()+passingBody(deltaPkg, "TestPinned"), passingBody(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || !v.Checks[0].Result.Overflow || v.Checks[0].Result.Truncated {
		t.Fatalf("a -json check without declared deltas keeps the cap: %+v", v.Checks[0].Result)
	}
	dir, p, c = deltaFixture(t, bigPrefix()+passingBody(deltaPkg, "TestPinned"), passingBody(deltaPkg, "TestPinned"))
	p.Analysis.Checks[0].Argv = []string{"/bin/sh", "check.sh"}
	v = runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || !v.Checks[0].Result.Overflow {
		t.Fatalf("a check that does not request go test -json keeps the cap: %+v", v.Checks[0].Result)
	}
	if raw, _ := json.Marshal(process.Result{}); strings.Contains(string(raw), "truncated") {
		t.Fatalf("an untruncated result must keep its earlier encoding: %s", raw)
	}
}

// plainFirstRunFixture is baselineFixture with no declared deltas and a single
// required regression check, the plain go test -timeout 20m ./... unless argv is
// given: a Molecule's first protected run. pass makes the fake go pass in both views.
func plainFirstRunFixture(t *testing.T, mode string, pass bool, argv ...string) (string, string, readiness.Plan, CaptureResult) {
	t.Helper()
	bin, dir, p, c := baselineFixture(t, mode)
	if pass {
		fake := strings.Replace(baselineFakeGo, "if grep -q new pkg/pin_test.go; then", "if true; then", 1)
		if err := os.WriteFile(filepath.Join(bin, "go"), []byte(fake), 0700); err != nil {
			t.Fatal(err)
		}
	}
	check := p.Analysis.Checks[1]
	check.ID = "plain"
	if len(argv) > 0 {
		check.Argv = argv
	}
	p.Analysis.Checks, p.FormulaBaseline = []readiness.Check{check}, nil
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return bin, dir, p, c
}

// A first protected run declares no deltas, yet the protected view streams a plain
// direct go test check with -json, so --accept-protected-failures has a trustworthy
// failing set to declare; the verdict and the candidate view are unchanged.
func TestFirstProtectedRunWithoutDeltasStreamsAPlainGoTestCheck(t *testing.T) {
	bin, dir, p, c := plainFirstRunFixture(t, "by-design", false)
	approved := append([]string{}, p.Analysis.Checks[0].Argv...)
	digest, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Protected == nil || v.Protected.RequiredChecksPassed || !strings.Contains(v.Error, "required command plain did not pass") || len(v.AcceptedDeltas) != 0 || len(v.Warnings) != 0 {
		t.Fatalf("recording the failing set must not change the verdict: %q %+v", v.Error, v.Protected)
	}
	if got, want := argvLog(t, bin), []string{"test -timeout 20m ./...", "test -json -timeout 20m ./..."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if after, err := p.Digest(); err != nil || after != digest || !reflect.DeepEqual(p.Analysis.Checks[0].Argv, approved) {
		t.Fatalf("the plan changed: digest %s -> %s", digest, after)
	}
	candidate := v.Checks[0]
	if candidate.JSONAdded || !candidate.Result.OK || !reflect.DeepEqual(candidate.Command, p.Analysis.Checks[0]) || strings.Contains(candidate.Result.Stdout, `"Action"`) {
		t.Fatalf("the candidate view was changed: %+v", candidate)
	}
	obs := v.Protected.Checks[0]
	if !obs.JSONAdded || obs.Result.OK || !reflect.DeepEqual(obs.Command.Argv, approved) || len(obs.AcceptedDeltas) != 0 {
		t.Fatalf("protected observation: %+v", obs)
	}
	want := []TestID{{baselinePkg, "TestA"}}
	if !reflect.DeepEqual(obs.FailedTests, want) {
		t.Fatalf("failing tests = %v, want %v", obs.FailedTests, want)
	}
	if out := obs.Result.Stdout; strings.Contains(out, `"Action"`) || !strings.Contains(out, "--- FAIL: TestA ("+baselinePkg+")") || obs.Result.Truncated || len(out) > maxStreamSummaryBytes+200 {
		t.Fatalf("protected stdout %q", out)
	}
	if got, err := ProtectedFailuresForDeltas(obs); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("accept helper on a first-run engine-streamed check: %v %v", got, err)
	}
	if MatchesCheckObservation(p, p.Analysis.Checks[0], obs, v.Protected.SourceTree, true) {
		t.Fatal("a recorded failing set without declared deltas must never match")
	}
}

func TestFirstProtectedRunWithoutDeltasKeepsAPassingVerdict(t *testing.T) {
	bin, dir, p, c := plainFirstRunFixture(t, "by-design", true)
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Error != "" || v.Protected == nil || !v.Protected.RequiredChecksPassed || len(v.AcceptedDeltas) != 0 || len(v.Warnings) != 0 {
		t.Fatalf("a passing no-delta run changed verdict: %q %+v", v.Error, v.Protected)
	}
	if got := argvLog(t, bin); !reflect.DeepEqual(got, []string{"test -timeout 20m ./...", "test -json -timeout 20m ./..."}) {
		t.Fatalf("argv = %q", got)
	}
	obs := v.Protected.Checks[0]
	if !obs.JSONAdded || !obs.Result.OK || len(obs.FailedTests) != 0 || strings.Contains(obs.Result.Stdout, `"Action"`) {
		t.Fatalf("protected observation: %+v", obs)
	}
	if obs.Packages == nil || obs.Packages.Run != 2 {
		t.Fatalf("package counts from the summary: %+v", obs.Packages)
	}
	if v.Checks[0].JSONAdded || !v.Checks[0].Result.OK {
		t.Fatalf("the candidate view was changed: %+v", v.Checks[0])
	}
}

// A refused stream, including a build failure under engine-added -json and a package
// that fails without a failing test (a TestMain exit), fails closed with nothing
// declarable, yet keeps the build and package diagnostics a plain run would show.
func TestFirstProtectedRunWithoutDeltasFailsClosedOnAnUntrustedStream(t *testing.T) {
	for mode, wants := range map[string][]string{
		"panic":     {"panic: boom"},
		"timeout":   {"test timed out after"},
		"truncate":  nil,
		"unparsed":  {"=== raw stdout head", "    not-json"},
		"buildfail": {"build failure in", "=== build output\n    # " + baselinePkg + "\n    pkg/pin_test.go:3:9: undefined: pinnedOld\n", "FAIL\t" + baselinePkg + " [build failed]"},
		"testmain":  {"failed without a failing test", "=== package output\n    TestMain: fixture setup refused\n", "FAIL\t" + baselinePkg + "\t0.01s"},
	} {
		t.Run(mode, func(t *testing.T) {
			_, dir, p, c := plainFirstRunFixture(t, mode, false)
			v := runDelta(t, dir, p, c)
			if v.RequiredChecksPassed || v.Protected == nil || v.Protected.RequiredChecksPassed || len(v.AcceptedDeltas) != 0 {
				t.Fatalf("an untrusted stream passed: %q", v.Error)
			}
			obs := v.Protected.Checks[0]
			if !obs.JSONAdded || obs.Result.OK || len(obs.FailedTests) != 0 || len(obs.Result.Stdout) > maxStreamSummaryBytes+maxStreamDiagnosticsBytes+200 {
				t.Fatalf("a refused stream recorded evidence it cannot support: %+v", obs)
			}
			out := obs.Result.Stdout
			if !strings.Contains(out, "go test -json stream was refused") || strings.Contains(out, dir) {
				t.Fatalf("refused stream diagnostics: %q", out)
			}
			for _, want := range wants {
				if !strings.Contains(out, want) {
					t.Fatalf("diagnostics lack %q: %q", want, out)
				}
			}
			if _, err := ProtectedFailuresForDeltas(obs); err == nil {
				t.Fatal("a refused first-run stream was turned into a failing set")
			}
		})
	}
}

// Argv the engine must not trust as a stream: -json=false after the inserted -json
// turns it back into plain output, which the parser refuses; -json after -args reaches
// the test binary, not go test, so the check is not streamed and its plain output never
// parses. Either fails closed with nothing declarable.
func TestFirstProtectedRunRefusesArgvThatDefeatsTheStream(t *testing.T) {
	for name, c := range map[string]struct {
		argv      []string
		jsonAdded bool
		protected string
	}{
		"json=false":       {[]string{"go", "test", "-json=false", "-timeout", "20m", "./..."}, true, "test -json -json=false -timeout 20m ./..."},
		"json after -args": {[]string{"go", "test", "-timeout", "20m", "./...", "-args", "-json"}, false, "test -timeout 20m ./... -args -json"},
	} {
		t.Run(name, func(t *testing.T) {
			bin, dir, p, capture := plainFirstRunFixture(t, "by-design", false, c.argv...)
			v := runDelta(t, dir, p, capture)
			if v.RequiredChecksPassed || v.Protected == nil || v.Protected.RequiredChecksPassed || len(v.AcceptedDeltas) != 0 {
				t.Fatalf("verdict: %q", v.Error)
			}
			if got := argvLog(t, bin); len(got) != 2 || got[1] != c.protected {
				t.Fatalf("argv = %q, want protected %q", got, c.protected)
			}
			obs := v.Protected.Checks[0]
			if obs.JSONAdded != c.jsonAdded || obs.Result.OK || len(obs.FailedTests) != 0 || !reflect.DeepEqual(obs.Command.Argv, c.argv) {
				t.Fatalf("protected observation: %+v", obs)
			}
			if c.jsonAdded && (!strings.Contains(obs.Result.Stdout, "stream was refused") || !strings.Contains(obs.Result.Stdout, "--- FAIL: TestA")) {
				t.Fatalf("the refused plain output was not kept as diagnostics: %q", obs.Result.Stdout)
			}
			if _, err := ProtectedFailuresForDeltas(obs); err == nil {
				t.Fatalf("a %s check was turned into a failing set", name)
			}
		})
	}
}

// GOFLAGS=-failfast in the env the check runs with could let one failure hide others,
// so the protected view does not add -json to it and nothing becomes declarable.
func TestFirstProtectedRunDoesNotStreamUnderGoflagsFailfast(t *testing.T) {
	bin, dir, p, c := plainFirstRunFixture(t, "by-design", false)
	p.Policy.Environment = append(append([]string{}, p.Policy.Environment...), "GOFLAGS")
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFLAGS", "-failfast")
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Protected == nil || len(v.AcceptedDeltas) != 0 {
		t.Fatalf("verdict: %q", v.Error)
	}
	if got := argvLog(t, bin); !reflect.DeepEqual(got, []string{"test -timeout 20m ./...", "test -timeout 20m ./..."}) {
		t.Fatalf("a GOFLAGS=-failfast check was streamed: %q", got)
	}
	obs := v.Protected.Checks[0]
	if obs.JSONAdded || len(obs.FailedTests) != 0 {
		t.Fatalf("protected observation: %+v", obs)
	}
	if _, err := ProtectedFailuresForDeltas(obs); err == nil {
		t.Fatal("a GOFLAGS=-failfast check was turned into a failing set")
	}
}

func TestFirstProtectedRunDoesNotStreamWrappersOrFailfast(t *testing.T) {
	for name, argv := range map[string][]string{
		"failfast": {"go", "test", "-failfast", "-timeout", "20m", "./..."},
		"wrapper":  {"/bin/sh", "-c", "go test -timeout 20m ./..."},
	} {
		t.Run(name, func(t *testing.T) {
			bin, dir, p, c := plainFirstRunFixture(t, "by-design", false, argv...)
			v := runDelta(t, dir, p, c)
			if v.RequiredChecksPassed || v.Protected == nil || len(v.AcceptedDeltas) != 0 {
				t.Fatalf("verdict: %q", v.Error)
			}
			for _, line := range argvLog(t, bin) {
				if strings.Contains(line, "-json") {
					t.Fatalf("a %s check was streamed: %q", name, line)
				}
			}
			obs := v.Protected.Checks[0]
			if obs.JSONAdded || len(obs.FailedTests) != 0 || !reflect.DeepEqual(obs.Command.Argv, argv) {
				t.Fatalf("protected observation: %+v", obs)
			}
			if _, err := ProtectedFailuresForDeltas(obs); err == nil {
				t.Fatalf("a %s check's plain output was turned into a failing set", name)
			}
		})
	}
}

func TestProtectedFailureWithoutDeltasRecordsTheFailingTests(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestPinned", "TestOther"))
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.Error != "protected regression did not pass: required command regression did not pass" || len(v.AcceptedDeltas) != 0 {
		t.Fatalf("recording the failing set must not change the verdict: %+v", v)
	}
	want := []TestID{{deltaPkg, "TestOther"}, {deltaPkg, "TestPinned"}}
	if got := v.Protected.Checks[0].FailedTests; !reflect.DeepEqual(got, want) {
		t.Fatalf("failing tests = %v, want %v", got, want)
	}
	if MatchesCheckObservation(p, p.Analysis.Checks[0], v.Protected.Checks[0], v.Protected.SourceTree, true) {
		t.Fatal("a recorded failing set without declared deltas must never match")
	}
}
