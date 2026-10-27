package verification

import (
	"strings"
	"testing"
)

// The candidate view lifts the capture cap for a delta-declaring go test -json
// check so the protected view can stream. It must not let a truncated check pass.
func TestHardening129CandidateViewFailsATruncatedStream(t *testing.T) {
	dir, p, c := deltaFixture(t, bigPrefix()+passingBody(deltaPkg, "TestPinned"), bigPrefix()+failingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if v.RequiredChecksPassed || v.IndependentAcceptanceVerified {
		t.Fatalf("a candidate check that exited 0 with truncated output passed: %+v", v)
	}
	if !strings.Contains(v.Error, "regression") || !strings.Contains(v.Error, "256000") || !strings.Contains(v.Error, "truncated") {
		t.Fatalf("the error does not name the command and the output limit: %q", v.Error)
	}
	if len(v.Checks) != 1 {
		t.Fatalf("the failed observation must be retained: %+v", v.Checks)
	}
	if obs := v.Checks[0]; obs.Result.OK || !obs.Result.Truncated || obs.Result.Code != 0 {
		t.Fatalf("the observation must record a failed, truncated run: %+v", obs.Result)
	}
	// Fail fast (#264): after a required candidate failure the protected view is the
	// not-run view, so no protected delta is accepted from it.
	if v.Protected == nil || v.Protected.RequiredChecksPassed || v.Protected.Workspace != "" || len(v.Protected.Checks) != 0 || len(v.Protected.NotRun) == 0 || len(v.AcceptedDeltas) != 0 {
		t.Fatalf("the protected view must be recorded as not run: %+v", v.Protected)
	}
	for _, o := range v.Protected.NotRun {
		if o.NotRun != NotRunCandidateFailure || o.Result.OK {
			t.Fatalf("a protected check after a candidate failure must be not run and not OK: %+v", o)
		}
	}
}

func TestHardening129CandidateViewKeepsSmallStreamsPassing(t *testing.T) {
	dir, p, c := deltaFixture(t, passingBody(deltaPkg, "TestPinned"), failingBody(deltaPkg, "TestPinned"), pinnedDelta(deltaPkg, "TestPinned"))
	v := runDelta(t, dir, p, c)
	if !v.RequiredChecksPassed || v.Error != "" || v.Checks[0].Result.Truncated || !v.Checks[0].Result.OK {
		t.Fatalf("a candidate stream under the limit must still pass: %+v", v)
	}
}
