package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/learning"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

type learnSeams struct {
	runs        int
	evaluations int
	labs        []string
	results     []learning.PendingResult
	runErr      error
	recurrence  learning.Recurrence
	evalErr     error
	items       []learning.PendingItem
	listErr     error
}

// stubLearn replaces every seam; the pending runner fails the test when a command
// other than `learn --pending` starts it.
func stubLearn(t *testing.T) *learnSeams {
	t.Helper()
	s := &learnSeams{}
	previousRun, previousEvaluate, previousList := runPendingLearn, evaluateRecurrence, listPendingLearn
	runPendingLearn = func(_ context.Context, o learning.PendingOptions) ([]learning.PendingResult, error) {
		s.runs++
		s.labs = append(s.labs, o.LabDir)
		return s.results, s.runErr
	}
	evaluateRecurrence = func(lab string) (learning.Recurrence, error) {
		s.evaluations++
		return s.recurrence, s.evalErr
	}
	listPendingLearn = func(string) ([]learning.PendingItem, error) { return s.items, s.listErr }
	t.Cleanup(func() {
		runPendingLearn, evaluateRecurrence, listPendingLearn = previousRun, previousEvaluate, previousList
	})
	return s
}

func learnCLI(args ...string) (int, string, string) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test")
	return code, out.String(), stderr.String()
}

var timeoutRequest = learning.PendingItem{Kind: "request", RequestID: "learn-request-0123456789abcdef", Class: molecule.FailureProcessTimedOut, MoleculeIDs: []string{"mol-1", "mol-2", "mol-3"}}

func TestPendingLearnRunsQueuedRequestsAndLeavesApprovalToThePerson(t *testing.T) {
	s := stubLearn(t)
	s.results = []learning.PendingResult{{RequestID: timeoutRequest.RequestID, Class: timeoutRequest.Class, MoleculeIDs: timeoutRequest.MoleculeIDs, SuggestionID: "suggestion-abc"}}
	lab := t.TempDir()
	code, out, stderr := learnCLI("learn", "--pending", "--lab", lab)
	if code != 0 || stderr != "" || s.runs != 1 || len(s.labs) != 1 {
		t.Fatalf("exit %d runs %d stderr %q", code, s.runs, stderr)
	}
	for _, want := range []string{timeoutRequest.RequestID, "mol-1, mol-2, mol-3", "saved suggestion suggestion-abc", "forgecell suggestion suggestion-abc --lab " + shellWord(lab)} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--approve") {
		t.Fatalf("approval must not be suggested as automatic:\n%s", out)
	}
}

func TestPendingLearnReportsFailuresKeepsRequestsPendingAndExitsNonzero(t *testing.T) {
	s := stubLearn(t)
	s.results = []learning.PendingResult{
		{RequestID: timeoutRequest.RequestID, Class: timeoutRequest.Class, MoleculeIDs: timeoutRequest.MoleculeIDs, Err: "process timed out"},
		{RequestID: "learn-request-aaaaaaaaaaaaaaaa", Class: molecule.FailureProtectedRegressionFailed, MoleculeIDs: []string{"mol-9"}, SuggestionID: "suggestion-ok"},
	}
	code, out, stderr := learnCLI("learn", "--pending", "--lab", t.TempDir())
	if code != 1 || !strings.Contains(out, "failed: process timed out") || !strings.Contains(out, "stays pending") || !strings.Contains(out, "saved suggestion suggestion-ok") || !strings.Contains(stderr, "1 learn request(s)") {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	s.runErr = context.DeadlineExceeded
	s.results = nil
	if code, _, stderr = learnCLI("learn", "--pending", "--lab", t.TempDir()); code != 1 || !strings.Contains(stderr, "deadline") {
		t.Fatalf("exit %d %s", code, stderr)
	}
}

func TestPendingLearnWithNothingQueuedSaysSo(t *testing.T) {
	stubLearn(t)
	code, out, _ := learnCLI("learn", "--pending", "--lab", t.TempDir())
	if code != 0 || !strings.Contains(out, "No pending learn requests.") {
		t.Fatalf("exit %d %q", code, out)
	}
}

func TestPendingLearnJSONListsResults(t *testing.T) {
	s := stubLearn(t)
	s.results = []learning.PendingResult{{RequestID: timeoutRequest.RequestID, Class: timeoutRequest.Class, MoleculeIDs: timeoutRequest.MoleculeIDs, SuggestionID: "suggestion-abc"}}
	code, out, _ := learnCLI("learn", "--pending", "--json", "--lab", t.TempDir())
	var decoded struct {
		Results []learning.PendingResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil || code != 0 || len(decoded.Results) != 1 || decoded.Results[0].SuggestionID != "suggestion-abc" {
		t.Fatalf("%d %v %s", code, err, out)
	}
}

func TestPendingLearnCannotBeCombinedWithMoleculeIdsOrDecisions(t *testing.T) {
	s := stubLearn(t)
	for _, args := range [][]string{
		{"learn", "mol-1", "--pending"},
		{"learn", "--pending", "mol-1"},
		{"learn", "--pending", "--approve"},
		{"learn", "--pending", "--dismiss"},
		{"suggestion", "--pending"},
		{"suggestion", "suggestion-x", "--pending"},
	} {
		if code, out, stderr := learnCLI(append(args, "--lab", t.TempDir())...); code != 1 || out != "" || stderr == "" {
			t.Fatalf("%v: exit %d %q %q", args, code, out, stderr)
		}
	}
	if s.runs != 0 {
		t.Fatal("a rejected combination started a learn run")
	}
}

func TestPendingLearnHelpListsTheFlag(t *testing.T) {
	code, out, _ := learnCLI("--help")
	if code != 0 || !strings.Contains(out, "learn --pending [--lab DIR] [--json]") {
		t.Fatalf("%d %s", code, out)
	}
}

func TestPendingLearnLoopReportShowsRequestsAndNeverStartsAModel(t *testing.T) {
	s := stubLearn(t)
	s.recurrence = learning.Recurrence{Recommendations: []string{"Recurring process-timed-out; no learn Atom is bound"}}
	failed := timeoutRequest
	failed.LastFailure, failed.Attempts = "provider unavailable", 1
	s.items = []learning.PendingItem{failed, {Kind: "suggestion", RequestID: "learn-request-bbbbbbbbbbbbbbbb", Class: molecule.FailureAnalysisPolicyViolation, MoleculeIDs: []string{"mol-7"}, SuggestionID: "suggestion-xyz"}}
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return molecule.LoopReport{}, nil
	})
	lab := t.TempDir()
	code, out, stderr := runLoopCLI("--lab", lab, "--once")
	if code != 0 || stderr != "" {
		t.Fatalf("a pending request must not change the loop exit code: %d %s", code, stderr)
	}
	for _, want := range []string{"Recurring process-timed-out; no learn Atom is bound", "Pending learn request " + timeoutRequest.RequestID, "forgecell learn --pending --lab", "The last learn run failed (attempt 1): provider unavailable", "Pending Formula suggestion suggestion-xyz from recurring analysis-policy-violation"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if s.runs != 0 || s.evaluations != 1 {
		t.Fatalf("the loop must evaluate rules and never start a model: runs=%d evaluations=%d", s.runs, s.evaluations)
	}
}

func TestPendingLearnUntrustedTextIsDisplaySafeEverywhere(t *testing.T) {
	hostile := "boom " + string(rune(0x202e)) + "gnp.exe" + string(rune(0x2028)) + "\x1b[31m" + strings.Repeat("é", 400) + "\xff"
	check := func(where, text string) {
		t.Helper()
		if !utf8.ValidString(text) {
			t.Fatalf("%s: invalid UTF-8", where)
		}
		for _, r := range text {
			if r == 0x202e || r == 0x2028 || r == 0x1b {
				t.Fatalf("%s: unsafe rune %U in %q", where, r, text)
			}
		}
	}
	s := stubLearn(t)
	failed := timeoutRequest
	failed.LastFailure, failed.Attempts = hostile, 1
	s.items = []learning.PendingItem{failed}
	s.recurrence = learning.Recurrence{Skipped: []string{"mol-bad.json: " + hostile}}
	s.results = []learning.PendingResult{{RequestID: timeoutRequest.RequestID, Class: timeoutRequest.Class, MoleculeIDs: timeoutRequest.MoleculeIDs, Err: hostile}}
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return molecule.LoopReport{}, nil
	})
	code, out, stderr := runLoopCLI("--lab", t.TempDir(), "--once")
	if code != 0 || !strings.Contains(out, "gnp.exe") || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "skipped unreadable items: mol-bad.json: ") {
		t.Fatalf("%d\n%s\n%s", code, out, stderr)
	}
	check("loop report", out)
	check("loop diagnostics", stderr)
	s.listErr = errors.New(hostile)
	code, out, stderr = learnCLI("learn", "--pending", "--lab", t.TempDir())
	if code != 1 {
		t.Fatalf("%d", code)
	}
	check("learn --pending report", out)
	check("learn --pending diagnostics", stderr)
	if !strings.Contains(stderr, "pending learn requests unavailable") {
		t.Fatalf("an unreadable request must be reported once: %q", stderr)
	}
}

func TestPendingLearnLoopOutputIsUnchangedWhenNothingIsPending(t *testing.T) {
	s := stubLearn(t)
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return molecule.LoopReport{}, nil
	})
	code, out, stderr := runLoopCLI("--lab", t.TempDir(), "--once")
	if code != 0 || out != "" || stderr != "" || s.runs != 0 {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
}

func TestPendingLearnEvaluationFailureIsOneDiagnosticLine(t *testing.T) {
	s := stubLearn(t)
	s.evalErr = context.Canceled
	stubLoop(t, func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return molecule.LoopReport{}, nil
	})
	code, out, stderr := runLoopCLI("--lab", t.TempDir(), "--once")
	if code != 0 || out != "" || strings.Count(stderr, "\n") != 1 || !strings.Contains(stderr, "learning recurrence check skipped") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	s.evalErr = learning.ErrNoActiveFormula
	if _, _, stderr = runLoopCLI("--lab", t.TempDir(), "--once"); stderr != "" {
		t.Fatalf("a Lab without a Formula has nothing to say: %q", stderr)
	}
}

func TestPendingLearnDoctorShowsPendingRequests(t *testing.T) {
	s := stubLearn(t)
	_, lab := doctorFixture(t)
	provider, _ := markerProvider(t)
	launcher := writeTool(t, t.TempDir(), "forgecell", "#!/bin/sh\nexit 0\n")
	saveBinding(t, lab, "codex", []string{launcher, "__adapter", "codex", provider})
	_, quiet := runDoctor(t, lab)
	if _, present := decodeDoctor(t, quiet)["learnRequests"]; present {
		t.Fatalf("doctor output must be unchanged when nothing is pending:\n%s", quiet)
	}
	s.items = []learning.PendingItem{timeoutRequest}
	_, out := runDoctor(t, lab)
	report := decodeDoctor(t, out)
	items, _ := report["learnRequests"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["requestId"] != timeoutRequest.RequestID || items[0].(map[string]any)["class"] != molecule.FailureProcessTimedOut {
		t.Fatalf("%v", report["learnRequests"])
	}
	_, human := runDoctor(t, lab, "--verbose")
	for _, want := range []string{"Pending learning", "Pending learn request " + timeoutRequest.RequestID, "forgecell learn --pending --lab"} {
		if !strings.Contains(human, want) {
			t.Fatalf("missing %q:\n%s", want, human)
		}
	}
	if s.runs != 0 || s.evaluations != 0 {
		t.Fatalf("doctor only reads: runs=%d evaluations=%d", s.runs, s.evaluations)
	}
}
