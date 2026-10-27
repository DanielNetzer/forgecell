package forgesync

import (
	"slices"
	"testing"
)

const published = "0123456789abcdef0123456789abcdef01234567"

func openIssue() *Issue {
	return &Issue{Number: 7, Found: true, State: IssueOpen, Labels: []string{}}
}

func closedIssue(reason string) *Issue {
	return &Issue{Number: 7, Found: true, State: IssueClosed, StateReason: reason, ClosedAt: "2026-10-18T10:00:00Z", Labels: []string{}}
}

func mol(id, lane, started string) Molecule {
	return Molecule{ID: id, Lane: lane, StartedAt: started}
}

func pubMol(id, lane, started string, pr int64) Molecule {
	m := mol(id, lane, started)
	m.PRNumber, m.PublishedCommit = pr, published
	return m
}

func pr(n int64, state string, draft bool) PR {
	return PR{Number: n, Found: true, State: state, IsDraft: draft, HeadRefOid: published, Checks: "SUCCESS", ReviewDecision: "REVIEW_REQUIRED", MergedAt: "2026-10-18T09:00:00Z", MergeCommit: "1111111111111111111111111111111111111111"}
}

func prs(ps ...PR) map[int64]PR {
	out := map[int64]PR{}
	for _, p := range ps {
		out[p.Number] = p
	}
	return out
}

// TestSyncStateMatrix has one case per row of the #242 matrix, asserting the
// label, bucket and startable; startable is yes exactly for rows 1, 5, 10 and 15.
func TestSyncStateMatrix(t *testing.T) {
	const t1, t2 = "2026-10-20T10:00:00Z", "2026-10-21T10:00:00Z"
	moved := pr(5, PROpen, true)
	moved.HeadRefOid = "2222222222222222222222222222222222222222"
	movedReady := pr(5, PROpen, false)
	movedReady.HeadRefOid = moved.HeadRefOid
	// pr() merges at 2026-10-18T09:00:00Z.
	reopened := openIssue()
	reopened.StateReason, reopened.ReopenedAt = ReasonReopened, "2026-10-19T09:00:00Z"
	reopenedBefore := openIssue()
	reopenedBefore.StateReason, reopenedBefore.ReopenedAt = ReasonReopened, "2026-10-17T09:00:00Z"
	reopenedUnknown := openIssue()
	reopenedUnknown.StateReason = ReasonReopened
	locked := openIssue()
	locked.Locked = true
	lockedUnavailable := &Issue{Number: 7, Unavailable: UnavailableNoAccess, Locked: true}
	cases := []struct {
		name      string
		row       int
		issue     *Issue
		prs       map[int64]PR
		work      Work
		label     string
		bucket    string
		startable bool
		note      string
	}{
		{"1 open, no work", 1, openIssue(), nil, Work{SummaryStartable: true}, "Not started", BucketIssues, true, ""},
		{"2 open, running", 2, openIssue(), nil, Work{Molecules: []Molecule{{ID: "m", Lane: LaneRunning, Step: "queued for checks", StartedAt: t1}}}, "In progress · queued for checks", BucketRunning, false, ""},
		{"3 open, at a gate", 3, openIssue(), nil, Work{Molecules: []Molecule{{ID: "m", Lane: LaneDecision, Step: "scope", StartedAt: t1}}}, "Needs you · scope", BucketDecision, false, ""},
		{"4 open, blocked", 4, openIssue(), nil, Work{Molecules: []Molecule{{ID: "m", Lane: LaneBlocked, Reason: "checks failed", StartedAt: t1}}}, "Blocked · checks failed", BucketBlocked, false, ""},
		{"5 open, only retired", 5, openIssue(), nil, Work{Molecules: []Molecule{mol("a", LaneRetired, t1), mol("b", LaneRetired, t2)}, SummaryStartable: true}, "Not started (2 retired)", BucketIssues, true, ""},
		{"6 open, draft PR", 6, openIssue(), prs(pr(5, PROpen, true)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Draft PR open", BucketDraftPR, false, ""},
		{"7 open, PR ready", 7, openIssue(), prs(pr(5, PROpen, false)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "PR in review", BucketDraftPR, false, ""},
		{"8 open, head moved", 6, openIssue(), prs(moved), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Draft PR open", BucketDraftPR, false, NoteHeadMoved},
		{"9 open, PR merged", 9, openIssue(), prs(pr(5, PRMerged, false)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Merged · Issue still open", BucketMerged, false, NoteCloseOnGitHub},
		{"10 open, PR closed", 10, openIssue(), prs(pr(5, PRClosed, false)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "PR closed without merge", BucketIssues, true, ""},
		{"11 closed completed, merged", 11, closedIssue(ReasonCompleted), prs(pr(5, PRMerged, false)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Merged", BucketMerged, false, ""},
		{"12 closed completed, no PR", 12, closedIssue(ReasonCompleted), nil, Work{SummaryStartable: true}, "Issue closed elsewhere", BucketHistory, false, ""},
		{"12 closed completed, PR closed", 12, closedIssue(ReasonCompleted), prs(pr(5, PRClosed, false)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Issue closed elsewhere", BucketHistory, false, ""},
		{"13 closed not planned", 13, closedIssue(ReasonNotPlanned), prs(pr(5, PRMerged, false)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Issue closed · not planned", BucketHistory, false, ""},
		{"14 closed while running", 14, closedIssue(ReasonCompleted), nil, Work{Molecules: []Molecule{mol("m", LaneRunning, t1)}}, "Issue closed while in progress", BucketDecision, false, NoteRetireOrKeep},
		{"14 closed at a gate", 14, closedIssue(ReasonNotPlanned), nil, Work{Molecules: []Molecule{mol("m", LaneDecision, t1)}}, "Issue closed while in progress", BucketDecision, false, NoteRetireOrKeep},
		{"15 reopened after merge", 15, reopened, prs(pr(5, PRMerged, false)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Reopened after merge", BucketIssues, true, ""},
		{"16 not found, running", 16, &Issue{Number: 7, Unavailable: UnavailableNotFound}, nil, Work{Molecules: []Molecule{mol("m", LaneRunning, t1)}}, "Issue unavailable · " + UnavailableNotFound, BucketRunning, false, "In progress"},
		{"16 no access, no work", 16, &Issue{Number: 7, Unavailable: UnavailableNoAccess}, nil, Work{SummaryStartable: true}, "Issue unavailable · " + UnavailableNoAccess, BucketIssues, false, ""},
		{"17 locked, not started", 1, locked, nil, Work{SummaryStartable: true}, "Not started", BucketIssues, true, NoteLocked},
		{"8 PR ready, head moved", 7, openIssue(), prs(movedReady), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "PR in review", BucketDraftPR, false, NoteHeadMoved},
		{"9 reopened before the merge", 9, reopenedBefore, prs(pr(5, PRMerged, false)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Merged · Issue still open", BucketMerged, false, NoteCloseOnGitHub},
		{"9 reopened, time unknown", 9, reopenedUnknown, prs(pr(5, PRMerged, false)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Merged · Issue still open", BucketMerged, false, NoteCloseOnGitHub},
		{"6 closed Issue, open draft PR", 6, closedIssue(ReasonCompleted), prs(pr(5, PROpen, true)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}}, "Draft PR open", BucketDraftPR, false, NoteIssueClosed},
		{"17 locked + 2 running", 2, locked, nil, Work{Molecules: []Molecule{{ID: "m", Lane: LaneRunning, Step: "code", StartedAt: t1}}}, "In progress · code", BucketRunning, false, NoteLocked},
		{"17 locked + 3 at a gate", 3, locked, nil, Work{Molecules: []Molecule{{ID: "m", Lane: LaneDecision, Step: "review", StartedAt: t1}}}, "Needs you · review", BucketDecision, false, NoteLocked},
		{"17 locked + 4 blocked", 4, locked, nil, Work{Molecules: []Molecule{{ID: "m", Lane: LaneBlocked, Reason: "checks failed", StartedAt: t1}}}, "Blocked · checks failed", BucketBlocked, false, NoteLocked},
		{"17 locked + 16 unavailable", 16, lockedUnavailable, nil, Work{Molecules: []Molecule{mol("m", LaneBlocked, t1)}}, "Issue unavailable · " + UnavailableNoAccess, BucketBlocked, false, NoteLocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := SyncState(c.issue, c.prs, c.work)
			if d.Row != c.row || d.Label != c.label || d.Bucket != c.bucket || d.Startable != c.startable || !d.Checked {
				t.Fatalf("got row %d %q %q startable %v checked %v; want row %d %q %q startable %v", d.Row, d.Label, d.Bucket, d.Startable, d.Checked, c.row, c.label, c.bucket, c.startable)
			}
			if c.note != "" && !slices.Contains(d.Notes, c.note) {
				t.Fatalf("notes %v lack %q", d.Notes, c.note)
			}
			if c.issue.Locked != d.Locked {
				t.Fatalf("locked %v", d.Locked)
			}
		})
	}
}

// Only rows 1, 5, 10 and 15 are ever startable, and never for a closed or
// unavailable Issue, whatever the loop's summary says.
func TestSyncStateStartableNeedsAnOpenReadableIssue(t *testing.T) {
	for _, issue := range []*Issue{nil, closedIssue(ReasonCompleted), closedIssue(ReasonNotPlanned), {Number: 7, Unavailable: UnavailableNotFound}} {
		if d := SyncState(issue, nil, Work{SummaryStartable: true}); d.Startable {
			t.Fatalf("%+v startable: %+v", issue, d)
		}
	}
	// Row 1 also needs the loop's summary: an incomplete inventory is not startable.
	if d := SyncState(openIssue(), nil, Work{}); d.Row != 1 || d.Startable {
		t.Fatalf("row 1 without the summary: %+v", d)
	}
}

func TestSyncStatePrecedence(t *testing.T) {
	const t1, t2 = "2026-10-20T10:00:00Z", "2026-10-21T10:00:00Z"
	// A live Molecule beats a merged pull request of an earlier attempt.
	d := SyncState(openIssue(), prs(pr(5, PRMerged, false)), Work{Molecules: []Molecule{pubMol("old", LaneFinished, t1, 5), mol("new", LaneRunning, t2)}})
	if d.Row != 2 || d.MoleculeID != "new" || !slices.Equal(d.EarlierAttempts, []string{"old"}) {
		t.Fatalf("live first: %+v", d)
	}
	// PR state beats Issue state: an open PR on a closed Issue stays a draft PR.
	d = SyncState(closedIssue(ReasonCompleted), prs(pr(5, PROpen, true)), Work{Molecules: []Molecule{pubMol("m", LaneFinished, t1, 5)}})
	if d.Row != 6 || d.Bucket != BucketDraftPR {
		t.Fatalf("PR before Issue: %+v", d)
	}
	// A blocked Molecule on a closed Issue is row 14, out of Blocked.
	d = SyncState(closedIssue(ReasonCompleted), nil, Work{Molecules: []Molecule{mol("m", LaneBlocked, t1)}})
	if d.Row != 14 || d.Bucket == BucketBlocked {
		t.Fatalf("blocked on a closed Issue: %+v", d)
	}
}

func TestSyncStateSeveralMoleculesNewestNonRetiredDecides(t *testing.T) {
	ms := []Molecule{
		pubMol("first", LaneFinished, "2026-10-20T10:00:00Z", 5),
		mol("retired-newest", LaneRetired, "2026-10-23T10:00:00Z"),
		pubMol("second", LaneFinished, "2026-10-22T10:00:00Z", 6),
		mol("undated", LaneOther, ""),
	}
	d := SyncState(openIssue(), prs(pr(5, PRClosed, false), pr(6, PROpen, false)), Work{Molecules: ms})
	if d.MoleculeID != "second" || d.Row != 7 || d.Retired != 1 {
		t.Fatalf("decider: %+v", d)
	}
	if want := []string{"retired-newest", "first", "undated"}; !slices.Equal(d.EarlierAttempts, want) {
		t.Fatalf("earlier %v, want %v", d.EarlierAttempts, want)
	}
	// Input order never changes the decider.
	slices.Reverse(ms)
	if again := SyncState(openIssue(), prs(pr(5, PRClosed, false), pr(6, PROpen, false)), Work{Molecules: ms}); again.MoleculeID != "second" {
		t.Fatalf("order-dependent: %+v", again)
	}
}

// A retired Molecule never decides, but its merged pull request counts as a
// Forgecell merge in history and analytics.
func TestSyncStateRetiredAndMerged(t *testing.T) {
	retired := pubMol("r", LaneRetired, "2026-10-20T10:00:00Z", 5)
	d := SyncState(closedIssue(ReasonCompleted), prs(pr(5, PRMerged, false)), Work{Molecules: []Molecule{retired}, SummaryStartable: true})
	if d.Row != 11 || d.Bucket != BucketMerged || d.ForgecellMerge == nil || d.ForgecellMerge.PR != 5 || d.MoleculeID != "" {
		t.Fatalf("retired merged, Issue closed: %+v", d)
	}
	d = SyncState(openIssue(), prs(pr(5, PRMerged, false)), Work{Molecules: []Molecule{retired}, SummaryStartable: true})
	if d.Row != 5 || !d.Startable || d.ForgecellMerge == nil {
		t.Fatalf("retired merged, Issue open: %+v", d)
	}
	one := ForMolecule(openIssue(), prs(pr(5, PRMerged, false)), retired)
	if one.Bucket != BucketRetired || one.ForgecellMerge == nil || one.Label != "Retired · PR merged" {
		t.Fatalf("ForMolecule retired: %+v", one)
	}
}

// A merge or close by a pull request Forgecell did not publish is row 12 with
// that link, never a Forgecell merge.
func TestSyncStateMergeOutsideForgecell(t *testing.T) {
	issue := closedIssue(ReasonCompleted)
	issue.ClosedBy = &Link{Kind: "pull", Number: 99, URL: "https://github.com/o/r/pull/99"}
	for _, work := range []Work{{}, {Molecules: []Molecule{pubMol("m", LaneFinished, "2026-10-20T10:00:00Z", 5)}}} {
		d := SyncState(issue, prs(pr(5, PRClosed, false), pr(99, PRMerged, false)), work)
		if d.Row != 12 || d.ForgecellMerge != nil || d.ClosedBy == nil || d.ClosedBy.Number != 99 || !slices.Contains(d.Notes, NoteOutsideMerge) {
			t.Fatalf("outside merge: %+v", d)
		}
	}
}

// Without GitHub state every row keeps its ledger lane, labelled not checked,
// and nothing is startable.
func TestSyncStateNotChecked(t *testing.T) {
	d := SyncState(nil, nil, Work{Molecules: []Molecule{pubMol("m", LaneFinished, "2026-10-20T10:00:00Z", 5)}, SummaryStartable: true})
	if d.Checked || d.Kind != KindNotChecked || d.Label != "Draft published" || d.Bucket != LaneFinished || d.Startable || !slices.Contains(d.Notes, NoteNotChecked) {
		t.Fatalf("not checked: %+v", d)
	}
	// A published Molecule whose pull request was not read is not guessed.
	d = SyncState(openIssue(), nil, Work{Molecules: []Molecule{pubMol("m", LaneFinished, "2026-10-20T10:00:00Z", 5)}})
	if d.Kind != KindRecorded || d.Label != "Draft published" || !slices.Contains(d.Notes, NotePRNotChecked) {
		t.Fatalf("PR not read: %+v", d)
	}
	d = SyncState(openIssue(), prs(PR{Number: 5}), Work{Molecules: []Molecule{pubMol("m", LaneFinished, "2026-10-20T10:00:00Z", 5)}})
	if d.Kind != KindRecorded || !slices.Contains(d.Notes, NotePRNotFound) {
		t.Fatalf("PR not found: %+v", d)
	}
}

func TestSyncStatePRView(t *testing.T) {
	p := pr(5, PROpen, true)
	p.Checks, p.ReviewDecision = "FAILURE", "CHANGES_REQUESTED"
	d := SyncState(openIssue(), prs(p), Work{Molecules: []Molecule{pubMol("m", LaneFinished, "2026-10-20T10:00:00Z", 5)}})
	if d.PR == nil || d.PR.Checks != "failing" || d.PR.Review != "changes requested" || !d.PR.Draft {
		t.Fatalf("PR view: %+v", d.PR)
	}
	p.Checks, p.ReviewDecision = "", ""
	d = SyncState(openIssue(), prs(p), Work{Molecules: []Molecule{pubMol("m", LaneFinished, "2026-10-20T10:00:00Z", 5)}})
	if d.PR.Checks != "none" || d.PR.Review != "" {
		t.Fatalf("PR view without checks: %+v", d.PR)
	}
}
