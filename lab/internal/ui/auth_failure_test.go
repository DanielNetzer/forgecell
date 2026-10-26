package ui

import (
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

const (
	authUIToken = "sk-ant-oat01-FAKEAUTHTOKEN0123456789abcdef"
	authUIHome  = "/Users/forgecell-fake-home/.claude/.credentials.json"
)

func authFailedRecord(lab, id string) molecule.Record {
	hint := harness.NotAuthenticatedHint("claude-code")
	return molecule.Record{
		SchemaVersion: "v0", Kind: "molecule", ID: id, Status: "failed", FormulaID: "f", Mode: "ticket",
		StartedAt: "2026-10-25T10:00:00Z", FinishedAt: "2026-10-25T10:01:00Z", LabDir: lab,
		Issue: molecule.Issue{Repo: "o/r", Number: 9, Title: "Fix it", URL: "https://github.com/o/r/issues/9", State: "OPEN"},
		Atoms: []molecule.Atom{{ID: "intake", Type: "intake", Status: "completed", Detail: "Read issue"}, {ID: "code", Type: "harness", Status: "failed", Detail: hint}},
		Notes: []string{hint},
		HarnessAttempts: []molecule.HarnessAttempt{{ID: "h1", PlanDigest: "d1", FailureClass: harness.HarnessNotAuthenticated,
			Coding: &harness.CodingOutcome{SchemaVersion: "v1", Outcome: "unknown", Reason: hint, Paths: []string{}}}},
	}
}

func TestHarnessNotAuthenticatedUIBlockedReasonAndAttemptClass(t *testing.T) {
	lab := t.TempDir()
	want := harness.NotAuthenticatedHint("claude-code")
	ok := authFailedRecord(lab, "mol-9-auth")
	ok.HarnessAttempts[0].Result.Error = want
	writeRecord(t, lab, ok)
	// A hostile ledger whose recorded error is provider text still shows only a
	// fixed hint.
	hostile := authFailedRecord(lab, "mol-9-hostile")
	hostile.Issue.Number = 10
	hostile.HarnessAttempts[0].Result.Error = "Failed to authenticate: OAuth session expired (" + authUIToken + " at " + authUIHome + ")"
	writeRecord(t, lab, hostile)
	h := testHandler(t, Options{LabDir: lab})

	var d Detail
	w := do(t, h, authed("/api/molecules/"+ok.ID))
	decode(t, w, &d)
	if d.Lane != LaneBlocked || !strings.HasPrefix(d.Next.Copy, want) || !strings.Contains(d.Next.Title, "harness not authenticated") {
		t.Fatalf("next %+v lane %s", d.Next, d.Lane)
	}
	if len(d.Attempts) != 1 || d.Attempts[0].FailureClass != harness.HarnessNotAuthenticated {
		t.Fatalf("attempts %+v", d.Attempts)
	}

	var hd Detail
	hw := do(t, h, authed("/api/molecules/"+hostile.ID))
	decode(t, hw, &hd)
	if !strings.HasPrefix(hd.Next.Copy, harness.NotAuthenticatedHint("")) {
		t.Fatalf("hostile next %+v", hd.Next)
	}
	list := do(t, h, authed("/api/molecules"))
	for name, body := range map[string]string{"detail": w.Body.String(), "hostile detail": hw.Body.String(), "list": list.Body.String()} {
		noLeak(t, body, authUIToken, authUIHome, "forgecell-fake-home", "OAuth session expired")
		if strings.Contains(body, lab) {
			t.Fatalf("%s names the Lab path", name)
		}
	}
}

// A review-stage authentication failure after a successful coding attempt shows the
// same fixed hint as the blocked reason and the class on the review attempt.
func TestHarnessNotAuthenticatedUIReviewAttempt(t *testing.T) {
	lab := t.TempDir()
	want := harness.NotAuthenticatedHint("claude-code")
	r := authFailedRecord(lab, "mol-9-review")
	r.Status = "blocked"
	r.HarnessAttempts[0].FailureClass = ""
	r.HarnessAttempts[0].Coding = &harness.CodingOutcome{SchemaVersion: "v1", Outcome: "completed", Reason: "implemented", Paths: []string{}}
	r.ReviewAttempts = []molecule.ReviewAttempt{{Ordinal: 0, HarnessAttemptID: "h1", Binding: "claude-code", Status: molecule.ReviewFailed, Reason: want, FailureClass: harness.HarnessNotAuthenticated}}
	writeRecord(t, lab, r)
	var d Detail
	w := do(t, testHandler(t, Options{LabDir: lab}), authed("/api/molecules/"+r.ID))
	decode(t, w, &d)
	if d.Lane != LaneBlocked || !strings.HasPrefix(d.Next.Copy, want) {
		t.Fatalf("next %+v lane %s", d.Next, d.Lane)
	}
	var review *AttemptView
	for i := range d.Attempts {
		if d.Attempts[i].Kind == "review" {
			review = &d.Attempts[i]
		}
	}
	if review == nil || review.FailureClass != harness.HarnessNotAuthenticated {
		t.Fatalf("attempts %+v", d.Attempts)
	}
	noLeak(t, w.Body.String(), authUIToken, authUIHome, "forgecell-fake-home", "OAuth session expired")
}

func TestHarnessNotAuthenticatedUIOtherFailuresKeepTheirReason(t *testing.T) {
	lab := t.TempDir()
	r := authFailedRecord(lab, "mol-9-other")
	r.HarnessAttempts[0].FailureClass = ""
	r.HarnessAttempts[0].Result.Error = "exit status 1"
	writeRecord(t, lab, r)
	var d Detail
	decode(t, do(t, testHandler(t, Options{LabDir: lab}), authed("/api/molecules/"+r.ID)), &d)
	if strings.Contains(d.Next.Title+d.Next.Copy, "not authenticated") || d.Attempts[0].FailureClass != "" {
		t.Fatalf("unclassified failure changed: %+v %+v", d.Next, d.Attempts)
	}
}
