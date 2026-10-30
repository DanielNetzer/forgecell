package ui

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationDriveIssueNumberBound(t *testing.T) {
	if maxIssueNumber != 2147483647 {
		t.Fatalf("maxIssueNumber = %d", maxIssueNumber)
	}
	if runKey(42) != "#42" || runKey(maxIssueNumber) != "#2147483647" {
		t.Fatalf("runKey = %q %q", runKey(42), runKey(maxIssueNumber))
	}
	lab, _ := labFixture(t)
	h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{run: func(context.Context, molecule.Options) error { return nil }})
	for _, body := range []string{`{"issue":0}`, `{"issue":-1}`, `{"issue":2147483648}`, `{"issue":9007199254740991}`, `{"issue":"12"}`, `{"issue":12.5}`} {
		refusalOf(t, do(t, h, gatePost("/api/runs", body)), http.StatusBadRequest, "invalid")
	}
	if n := d.calls(); n != 0 {
		t.Fatalf("out-of-bound issues reached the engine %d times", n)
	}
	v := viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", fmt.Sprintf(`{"issue":%d}`, maxIssueNumber))), http.StatusAccepted)
	if v.Issue != maxIssueNumber {
		t.Fatalf("accepted run %+v", v)
	}
	waitJob(t, h, runKey(maxIssueNumber))
	if n := d.calls(); n != 1 || d.runs[0].Issue != "2147483647" || d.runs[0].Approve != "" {
		t.Fatalf("engine calls %d %+v", n, d.runs)
	}
	if v = viewOf[RunJobView](t, do(t, h, authed("/api/runs/2147483647")), http.StatusOK); v.Issue != maxIssueNumber {
		t.Fatalf("read-back %+v", v)
	}
	for _, path := range []string{"/api/runs/2147483648", "/api/runs/0", "/api/runs/-1", "/api/runs/+5", "/api/runs/007", "/api/runs/1e3"} {
		refusalOf(t, do(t, h, authed(path)), http.StatusBadRequest, "invalid")
	}
	refusalOf(t, do(t, h, authed("/api/runs/2147483646")), http.StatusNotFound, "none")
}
