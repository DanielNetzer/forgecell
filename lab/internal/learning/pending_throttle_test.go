package learning

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

func fixedClock(t *testing.T, at time.Time) *time.Time {
	t.Helper()
	now := at
	old := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = old })
	return &now
}

func requestState(t *testing.T, lab, id string) Request {
	t.Helper()
	r, _, err := loadRequest(lab, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPendingThrottleCooldownThenAttemptCap(t *testing.T) {
	lab, _, request := queuedTimeouts(t, boundYAML)
	now := fixedClock(t, time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	interval := time.Minute
	options := PendingOptions{LabDir: lab, MaxAttempts: 3, RetryAfter: 3 * interval}
	f := &fakeLearn{yaml: boundYAML, fail: func() process.Result { return process.Result{Error: "exit status 1"} }}

	results, err := runPending(context.Background(), options, f.invoke)
	if err != nil || len(results) != 1 || results[0].Err == "" || results[0].Skipped != "" || f.calls != 1 {
		t.Fatalf("first run: %+v %v calls=%d", results, err, f.calls)
	}

	*now = now.Add(2 * interval)
	results, err = runPending(context.Background(), options, f.invoke)
	if err != nil || len(results) != 1 || results[0].Skipped == "" || results[0].Err != "" || f.calls != 1 {
		t.Fatalf("inside the cooldown the request must be skipped without a model call: %+v %v calls=%d", results, err, f.calls)
	}
	if r := requestState(t, lab, request.ID); r.Status != requestPending || r.Attempts != 1 {
		t.Fatalf("a skipped request is unchanged: %+v", r)
	}

	*now = now.Add(2 * interval)
	if results, _ = runPending(context.Background(), options, f.invoke); results[0].Skipped != "" || f.calls != 2 {
		t.Fatalf("after the cooldown it runs again: %+v calls=%d", results, f.calls)
	}
	*now = now.Add(4 * interval)
	if results, _ = runPending(context.Background(), options, f.invoke); results[0].Skipped != "" || f.calls != 3 {
		t.Fatalf("third attempt: %+v calls=%d", results, f.calls)
	}

	*now = now.Add(100 * interval)
	results, err = runPending(context.Background(), options, f.invoke)
	if err != nil || len(results) != 1 || f.calls != 3 || !strings.Contains(results[0].Skipped, "waiting for operator: 3 failed attempts") {
		t.Fatalf("the cap holds however long it waits: %+v %v calls=%d", results, err, f.calls)
	}
	if r := requestState(t, lab, request.ID); r.Status != requestPending || r.Attempts != 3 {
		t.Fatalf("%+v", r)
	}

	f.fail = nil
	results, err = runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
	if err != nil || len(results) != 1 || results[0].Skipped != "" || results[0].Err != "" || results[0].SuggestionID == "" || f.calls != 4 {
		t.Fatalf("learn --pending by hand is never throttled: %+v %v calls=%d", results, err, f.calls)
	}
}

func TestPendingThrottleZeroOptionsRunEveryRequest(t *testing.T) {
	lab, _, _ := queuedTimeouts(t, boundYAML)
	fixedClock(t, time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	f := &fakeLearn{yaml: boundYAML, fail: func() process.Result { return process.Result{Error: "exit status 1"} }}
	for i := 1; i <= 5; i++ {
		results, err := runPending(context.Background(), PendingOptions{LabDir: lab}, f.invoke)
		if err != nil || len(results) != 1 || results[0].Skipped != "" || f.calls != i {
			t.Fatalf("run %d: %+v %v calls=%d", i, results, err, f.calls)
		}
	}
}

func TestPendingThrottleCancelledRunIsNotAFailedAttempt(t *testing.T) {
	lab, _, request := queuedTimeouts(t, boundYAML)
	fixedClock(t, time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC))
	options := PendingOptions{LabDir: lab, MaxAttempts: 3, RetryAfter: 3 * time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	invoke := func(context.Context, []string, string, time.Duration, map[string]any) process.Result {
		calls++
		cancel()
		return process.Result{Error: "signal: interrupt"}
	}
	results, err := runPending(ctx, options, invoke)
	if err != nil || len(results) != 1 || results[0].Err == "" || results[0].SuggestionID != "" || calls != 1 {
		t.Fatalf("%+v %v calls=%d", results, err, calls)
	}
	r := requestState(t, lab, request.ID)
	if r.Status != requestPending || r.Attempts != 0 || r.LastFailureAt != "" || r.LastFailure != "" || r.RunNonce != "" {
		t.Fatalf("a cancelled run counts neither as an attempt nor as a failure: %+v", r)
	}

	f := &fakeLearn{yaml: boundYAML}
	results, err = runPending(context.Background(), options, f.invoke)
	if err != nil || len(results) != 1 || results[0].Skipped != "" || results[0].SuggestionID == "" || f.calls != 1 {
		t.Fatalf("the next pass runs it at once: %+v %v calls=%d", results, err, f.calls)
	}
}
