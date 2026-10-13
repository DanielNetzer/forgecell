package learning

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

var readinessAnalysis = readiness.Analysis{}

func protectedFailure() *verification.Result {
	return &verification.Result{Protected: &verification.Result{RequiredChecksPassed: false}}
}

type fakeLearn struct {
	calls   int
	ledgers [][]string
	fail    func() process.Result
	yaml    string
}

func (f *fakeLearn) invoke(_ context.Context, command []string, _ string, _ time.Duration, request map[string]any) process.Result {
	f.calls++
	var ids []string
	for _, raw := range request["ledgers"].([]json.RawMessage) {
		var r struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &r)
		ids = append(ids, r.ID)
	}
	f.ledgers = append(f.ledgers, ids)
	if f.fail != nil {
		return f.fail()
	}
	candidate := strings.Replace(f.yaml, "command: [echo, checks]", "command: [echo, targeted]", 1)
	b, _ := json.Marshal(map[string]string{"summary": "Raise the coding timeout.", "rationale": "Several Molecules timed out; limited evidence.", "expectedImpact": "May avoid repeated timeouts.", "evaluation": "Compare timeouts on comparable work.", "yaml": candidate})
	return process.Result{OK: true, Stdout: string(b)}
}

// runFakeLearn runs the pending requests with an invoker that never touches a provider.
func runFakeLearn(t *testing.T, lab, yaml string, fail func(string) process.Result) []PendingResult {
	t.Helper()
	f := &fakeLearn{yaml: yaml}
	if fail != nil {
		f.fail = func() process.Result { return fail("") }
	}
	results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil {
		t.Fatal(err)
	}
	return results
}

func queuedTimeouts(t *testing.T, yaml string) (string, []string, Request) {
	t.Helper()
	lab := recurrenceLab(t, yaml)
	ids := writeTimeouts(t, lab, yaml, "mol", oldFinish, 3)
	got, err := EvaluateRecurrence(lab)
	if err != nil || len(got.Created) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	return lab, ids, got.Created[0]
}

func TestPendingLearnRunsExactlyTheRequestedMoleculesAndOnlySavesASuggestion(t *testing.T) {
	lab, ids, request := queuedTimeouts(t, boundYAML)
	f := &fakeLearn{yaml: boundYAML}
	results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil || len(results) != 1 || results[0].Err != "" || results[0].SuggestionID == "" || results[0].RequestID != request.ID {
		t.Fatalf("%+v %v", results, err)
	}
	if f.calls != 1 || strings.Join(f.ledgers[0], ",") != strings.Join(ids, ",") {
		t.Fatalf("the learn request must carry exactly the triggering Molecules: %v", f.ledgers)
	}
	suggestion, err := Read(lab, results[0].SuggestionID)
	if err != nil || suggestion.Status != "pending" || strings.Join(suggestion.MoleculeIDs, ",") != strings.Join(ids, ",") {
		t.Fatalf("%+v %v", suggestion, err)
	}
	current, _ := os.ReadFile(filepath.Join(lab, "formulas", "default.yaml"))
	if string(current) != boundYAML {
		t.Fatal("learning must never change the Formula")
	}
	requests, _, err := readRequests(lab)
	if err != nil || len(requests) != 1 || requests[0].Status != requestSuggested || requests[0].SuggestionID != suggestion.ID || requests[0].Attempts != 1 || requests[0].RunNonce != "" {
		t.Fatalf("%+v %v", requests, err)
	}
	items, err := Pending(lab)
	if err != nil || len(items) != 1 || items[0].Kind != "suggestion" || items[0].SuggestionID != suggestion.ID {
		t.Fatalf("the saved suggestion stays visible until a person decides: %+v %v", items, err)
	}
	again, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil || len(again) != 0 || f.calls != 1 {
		t.Fatalf("a second run must not create a second suggestion: %+v %v calls=%d", again, err, f.calls)
	}
	entries, _ := os.ReadDir(filepath.Join(lab, "suggestions"))
	if len(entries) != 1 {
		t.Fatalf("%d suggestions", len(entries))
	}
}

func TestPendingLearnFailureOrTimeoutKeepsTheRequestPending(t *testing.T) {
	long := strings.Repeat("x", 2000)
	cases := map[string]process.Result{
		"failure": {Error: "exit status 1", Stderr: "line one\nline two\x1b[31m password=hunter2 " + long},
		"timeout": {TimedOut: true, Error: "process timed out"},
	}
	for name, failing := range cases {
		t.Run(name, func(t *testing.T) {
			lab, _, request := queuedTimeouts(t, boundYAML)
			f := &fakeLearn{yaml: boundYAML, fail: func() process.Result { return failing }}
			results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
			if err != nil || len(results) != 1 || results[0].Err == "" || results[0].SuggestionID != "" {
				t.Fatalf("%+v %v", results, err)
			}
			if strings.ContainsAny(results[0].Err, "\n\x1b") || utf8.RuneCountInString(results[0].Err) > 301 {
				t.Fatalf("failure text must be one bounded line: %q", results[0].Err)
			}
			if name == "failure" && strings.Contains(results[0].Err, "hunter2") {
				t.Fatalf("secrets must be redacted: %q", results[0].Err)
			}
			if _, err = os.Stat(filepath.Join(lab, "suggestions")); err == nil {
				t.Fatal("a failed run must save nothing")
			}
			requests, _, _ := readRequests(lab)
			if len(requests) != 1 || requests[0].ID != request.ID || requests[0].Status != requestPending || requests[0].LastFailure == "" || requests[0].LastFailureAt == "" || requests[0].Attempts != 1 || requests[0].RunNonce != "" {
				t.Fatalf("%+v", requests)
			}
			f.fail = nil
			retried, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
			if err != nil || len(retried) != 1 || retried[0].SuggestionID == "" || f.calls != 2 {
				t.Fatalf("a failed request must stay retryable: %+v %v", retried, err)
			}
			requests, _, _ = readRequests(lab)
			if requests[0].Status != requestSuggested || requests[0].LastFailure != "" || requests[0].Attempts != 2 {
				t.Fatalf("%+v", requests)
			}
		})
	}
}

func TestPendingLearnFailureTextIsDisplaySafe(t *testing.T) {
	hostile := "fail " + string(rune(0x202e)) + "gnp.exe" + string(rune(0x2066)) + " zw" + string(rune(0x200b)) + string(rune(0x00ad)) + string(rune(0x2028)) + "sep \xff\xfe bad " + strings.Repeat("é", 400)
	lab, _, _ := queuedTimeouts(t, boundYAML)
	f := &fakeLearn{yaml: boundYAML, fail: func() process.Result { return process.Result{Error: "exit status 1", Stderr: hostile} }}
	results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil || len(results) != 1 || results[0].Err == "" {
		t.Fatalf("%+v %v", results, err)
	}
	items, err := Pending(lab)
	if err != nil || len(items) != 1 {
		t.Fatalf("%+v %v", items, err)
	}
	for _, text := range []string{results[0].Err, items[0].LastFailure} {
		if !utf8.ValidString(text) || utf8.RuneCountInString(text) > 300 || !strings.Contains(text, "gnp.exe") {
			t.Fatalf("one valid bounded line, cut by runes: %d %q", utf8.RuneCountInString(text), text)
		}
		for _, r := range text {
			if r == 0x202e || r == 0x2066 || r == 0x200b || r == 0x00ad || r == 0x2028 || r == '\n' || r == utf8.RuneError {
				t.Fatalf("unsafe rune %U in %q", r, text)
			}
		}
	}
}

func TestPendingLearnDisplayTextCutsByRunesNotBytes(t *testing.T) {
	got := DisplayText(strings.Repeat("日", 50), 10)
	if !utf8.ValidString(got) || utf8.RuneCountInString(got) != 10 || !strings.HasSuffix(got, "…") {
		t.Fatalf("%q", got)
	}
	joined := "a b" + string(rune(0x2060)) + "c\x00d\xffe"
	if got := DisplayText(joined, 50); got != "a b c de" {
		t.Fatalf("%q", got)
	}
}

func TestPendingLearnReconcilesARunThatDiedAfterSavingItsSuggestion(t *testing.T) {
	lab, ids, request := queuedTimeouts(t, boundYAML)
	nonce, err := newNonce()
	if err != nil {
		t.Fatal(err)
	}
	request.RunNonce, request.RunStartedAt = nonce, stamp()
	if err = saveRequest(lab, request); err != nil {
		t.Fatal(err)
	}
	f := &fakeLearn{yaml: boundYAML}
	saved, err := create(context.Background(), Options{LabDir: lab, MoleculeIDs: ids}, f.invoke)
	if err != nil || f.calls != 1 {
		t.Fatalf("%v %d", err, f.calls)
	}
	results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil || len(results) != 1 || results[0].SuggestionID != saved.ID || results[0].Err != "" || f.calls != 1 {
		t.Fatalf("the saved suggestion must be adopted without a second model call: %+v %v calls=%d", results, err, f.calls)
	}
	requests, _, _ := readRequests(lab)
	if len(requests) != 1 || requests[0].Status != requestSuggested || requests[0].SuggestionID != saved.ID || requests[0].RunNonce != "" || requests[0].RunStartedAt != "" {
		t.Fatalf("%+v", requests)
	}
	if entries, _ := os.ReadDir(filepath.Join(lab, "suggestions")); len(entries) != 1 {
		t.Fatalf("%d suggestions", len(entries))
	}
	if again, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke); err != nil || len(again) != 0 || f.calls != 1 {
		t.Fatalf("%+v %v", again, err)
	}
}

func TestPendingLearnRerunsWhenTheDeadRunSavedNothing(t *testing.T) {
	lab, _, request := queuedTimeouts(t, boundYAML)
	nonce, _ := newNonce()
	request.RunNonce, request.RunStartedAt = nonce, stamp()
	if err := saveRequest(lab, request); err != nil {
		t.Fatal(err)
	}
	if items, err := Pending(lab); err != nil || len(items) != 1 || items[0].Kind != "request" {
		t.Fatalf("an interrupted request stays visible and pending: %+v %v", items, err)
	}
	f := &fakeLearn{yaml: boundYAML}
	results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil || len(results) != 1 || results[0].SuggestionID == "" || f.calls != 1 {
		t.Fatalf("%+v %v calls=%d", results, err, f.calls)
	}
	requests, _, _ := readRequests(lab)
	if requests[0].Status != requestSuggested || requests[0].RunNonce != "" {
		t.Fatalf("%+v", requests)
	}
}

func TestPendingLearnDoesNotAdoptASuggestionFromBeforeTheIntent(t *testing.T) {
	lab, ids, request := queuedTimeouts(t, boundYAML)
	f := &fakeLearn{yaml: boundYAML}
	if _, err := create(context.Background(), Options{LabDir: lab, MoleculeIDs: ids}, f.invoke); err != nil {
		t.Fatal(err)
	}
	nonce, _ := newNonce()
	request.RunNonce, request.RunStartedAt = nonce, stamp()
	if err := saveRequest(lab, request); err != nil {
		t.Fatal(err)
	}
	results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil || len(results) != 1 || f.calls != 2 {
		t.Fatalf("an older suggestion is not this run's output: %+v %v calls=%d", results, err, f.calls)
	}
}

func TestPendingLearnStaleRequestIsNeverRun(t *testing.T) {
	lab, _, _ := queuedTimeouts(t, boundYAML)
	approveLearning(t, lab, boundYAML+"# generation 2\n", "generation-2")
	f := &fakeLearn{yaml: boundYAML}
	results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil || len(results) != 0 || f.calls != 0 {
		t.Fatalf("%+v %v calls=%d", results, err, f.calls)
	}
	requests, _, _ := readRequests(lab)
	if len(requests) != 1 || requests[0].Status != requestStale {
		t.Fatalf("%+v", requests)
	}
}

func TestPendingLearnWithoutRequestsDoesNothing(t *testing.T) {
	lab := recurrenceLab(t, boundYAML)
	f := &fakeLearn{yaml: boundYAML}
	results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil || len(results) != 0 || f.calls != 0 {
		t.Fatalf("%+v %v", results, err)
	}
	if _, err = runPending(context.Background(), PendingOptions{LabDir: t.TempDir()}, f.invoke); !errors.Is(err, ErrNoActiveFormula) {
		t.Fatalf("%v", err)
	}
}

func TestPendingLearnConcurrentRunsCreateOneSuggestion(t *testing.T) {
	lab, _, _ := queuedTimeouts(t, boundYAML)
	release := lockRunForTest(t, lab)
	f := &fakeLearn{yaml: boundYAML}
	results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	release()
	if err != nil || len(results) != 1 || results[0].Err == "" || f.calls != 0 {
		t.Fatalf("a request held by another run must be skipped: %+v %v", results, err)
	}
}

func lockRunForTest(t *testing.T, lab string) func() {
	t.Helper()
	requests, _, err := readRequests(lab)
	if err != nil || len(requests) != 1 {
		t.Fatalf("%v %v", requests, err)
	}
	release, err := lockRun(lab, requests[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return release
}
