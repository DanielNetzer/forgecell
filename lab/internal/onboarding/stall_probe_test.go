package onboarding

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// A later healthy probe is a separate observation: it never edits or reclassifies
// the earlier stalled one, and it authorizes nothing by itself.
func TestHealthyProbeAfterStalledProbeIsASeparateObservation(t *testing.T) {
	var stalledCalls int
	stalled := func(_ context.Context, o process.Options) process.Result {
		stalledCalls++
		if o.Timeout != 5*time.Second {
			t.Errorf("a probe call must stay bounded, got timeout %s", o.Timeout)
		}
		return process.Result{TimedOut: true, Code: -1, Error: context.DeadlineExceeded.Error(), ElapsedMS: o.Timeout.Milliseconds(), Activity: &process.Activity{}}
	}
	first := Probe(context.Background(), "cursor", "/fake/cursor-agent", t.TempDir(), stalled)
	if first.Readiness == "ready" || len(first.Failures) != 2 {
		t.Fatalf("a stalled probe must not be ready: %+v", first)
	}
	for _, step := range first.Failures {
		if step.Result != "timeout" {
			t.Fatalf("step %s recorded result class %q, want timeout", step.Step, step.Result)
		}
	}
	if stalledCalls != 2 {
		t.Fatalf("the stalled probe made %d calls; a stall must not be retried automatically", stalledCalls)
	}
	earlier := first.Detail

	healthy := func(_ context.Context, o process.Options) process.Result {
		ok := process.Result{OK: true, Reconciled: true, Activity: &process.Activity{}}
		switch strings.Join(o.Argv[1:], " ") {
		case "--version":
			ok.Stdout = "Cursor Agent 1.2.3"
		case "--help":
			ok.Stdout = "Cursor Agent\n--print --output-format --sandbox --mode --trust"
		case "status --format json":
			ok.Stdout = `{"isAuthenticated":true}`
		}
		return ok
	}
	second := Probe(context.Background(), "cursor", "/fake/cursor-agent", t.TempDir(), healthy)
	if second.Readiness != "ready" {
		t.Fatalf("the subsequent healthy probe must be ready: %+v", second)
	}
	for _, step := range second.Failures {
		if step.Step != "isolation" {
			t.Fatalf("unexpected failing probe step %+v", step)
		}
	}
	if !strings.Contains(second.Detail, "Model access is checked only during a run.") {
		t.Fatalf("a ready probe must not claim more than CLI readiness: %s", second.Detail)
	}
	if first.Readiness == "ready" || first.Detail != earlier || len(first.Failures) != 2 || first.Failures[0].Result != "timeout" {
		t.Fatalf("the later probe rewrote the earlier observation: %+v", first)
	}
}
