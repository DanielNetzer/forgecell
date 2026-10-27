package ui

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
)

// Demo mode serves sample sync data, labelled sample=true, built through the
// same forgesync.SyncState as real mode. It never calls the fetcher or gh.

const demoRepo = "sample/lab"

const demoBase = "https://github.com/" + demoRepo

func demoIssue(n int64, title, state, reason string, labels ...string) forgesync.Issue {
	if labels == nil {
		labels = []string{}
	}
	return forgesync.Issue{Number: n, Found: true, State: state, StateReason: reason, Title: title, Labels: labels, UpdatedAt: "2026-10-26T12:00:00Z", URL: fmt.Sprintf("%s/issues/%d", demoBase, n)}
}

func demoPR(n int64, state string, draft bool) forgesync.PR {
	p := forgesync.PR{Number: n, Found: true, State: state, IsDraft: draft, Checks: "SUCCESS", URL: fmt.Sprintf("%s/pull/%d", demoBase, n)}
	if state == forgesync.PRMerged {
		p.MergedAt, p.MergeCommit = "2026-10-26T12:00:00Z", "0123456789abcdef0123456789abcdef01234567"
	}
	return p
}

// demoIssueCase is one sample open Issue with its sample work.
type demoIssueCase struct {
	issue     forgesync.Issue
	molecules []forgesync.Molecule
	startable bool
}

func demoMol(id, lane string, pr int64) forgesync.Molecule {
	return forgesync.Molecule{ID: id, Lane: lane, StartedAt: "2026-10-25T10:00:00Z", PRNumber: pr, PublishedCommit: "0123456789abcdef0123456789abcdef01234567"}
}

// demoIssueCases cover the open-Issue rows of the sync matrix.
func demoIssueCases() ([]demoIssueCase, map[int64]forgesync.PR) {
	locked := demoIssue(912, "Sample locked Issue", forgesync.IssueOpen, "", "docs")
	locked.Locked = true
	reopened := demoIssue(911, "Sample reopened after merge", forgesync.IssueOpen, forgesync.ReasonReopened)
	reopened.ReopenedAt = "2026-10-26T13:00:00Z"
	moved := demoPR(808, forgesync.PROpen, false)
	moved.HeadRefOid = "fedcba9876543210fedcba9876543210fedcba98"
	prs := map[int64]forgesync.PR{806: demoPR(806, forgesync.PROpen, true), 807: demoPR(807, forgesync.PROpen, false), 808: moved, 809: demoPR(809, forgesync.PRMerged, false), 810: demoPR(810, forgesync.PRClosed, false), 811: demoPR(811, forgesync.PRMerged, false)}
	return []demoIssueCase{
		{issue: demoIssue(901, "Sample Issue nobody started", forgesync.IssueOpen, "", "good first issue"), startable: true},
		{issue: demoIssue(902, "Sample Issue in progress", forgesync.IssueOpen, ""), molecules: []forgesync.Molecule{{ID: "demo-sync-902", Lane: forgesync.LaneRunning, Step: "harness", StartedAt: "2026-10-26T10:00:00Z"}}},
		{issue: demoIssue(903, "Sample Issue at a gate", forgesync.IssueOpen, ""), molecules: []forgesync.Molecule{{ID: "demo-sync-903", Lane: forgesync.LaneDecision, Step: "scope", StartedAt: "2026-10-26T10:00:00Z"}}},
		{issue: demoIssue(904, "Sample blocked Issue", forgesync.IssueOpen, ""), molecules: []forgesync.Molecule{{ID: "demo-sync-904", Lane: forgesync.LaneBlocked, Reason: "Required checks did not pass.", StartedAt: "2026-10-26T10:00:00Z"}}},
		{issue: demoIssue(905, "Sample Issue with retired attempts", forgesync.IssueOpen, ""), molecules: []forgesync.Molecule{demoMol("demo-sync-905", forgesync.LaneRetired, 0)}, startable: true},
		{issue: demoIssue(906, "Sample draft PR", forgesync.IssueOpen, ""), molecules: []forgesync.Molecule{demoMol("demo-sync-906", forgesync.LaneFinished, 806)}},
		{issue: demoIssue(907, "Sample PR in review", forgesync.IssueOpen, ""), molecules: []forgesync.Molecule{demoMol("demo-sync-907", forgesync.LaneFinished, 807)}},
		{issue: demoIssue(908, "Sample PR changed on GitHub", forgesync.IssueOpen, ""), molecules: []forgesync.Molecule{demoMol("demo-sync-908", forgesync.LaneFinished, 808)}},
		{issue: demoIssue(909, "Sample merged, Issue still open", forgesync.IssueOpen, ""), molecules: []forgesync.Molecule{demoMol("demo-sync-909", forgesync.LaneFinished, 809)}},
		{issue: demoIssue(910, "Sample PR closed without merge", forgesync.IssueOpen, ""), molecules: []forgesync.Molecule{demoMol("demo-sync-910", forgesync.LaneFinished, 810)}},
		{issue: reopened, molecules: []forgesync.Molecule{demoMol("demo-sync-911", forgesync.LaneFinished, 811)}},
		{issue: locked, startable: true},
	}, prs
}

// demoIssues is GET /api/issues in demo mode.
func demoIssues() IssuesView {
	cases, prs := demoIssueCases()
	out := IssuesView{Sample: true, Repository: demoRepo, Freshness: Freshness{Checked: true, FetchedAt: "2026-10-26T12:00:00Z"}, Complete: true, Total: len(cases), Shown: len(cases), Issues: []IssueRow{}}
	for _, c := range cases {
		issue := c.issue
		state := forgesync.SyncState(&issue, prs, forgesync.Work{Molecules: c.molecules, SummaryStartable: c.startable})
		row := IssueRow{Number: issue.Number, Title: issue.Title, URL: issue.URL, Labels: issue.Labels, UpdatedAt: issue.UpdatedAt, Locked: issue.Locked, State: state, Startable: state.Startable, SummaryState: "sample", SummaryStartable: c.startable}
		if state.MoleculeID != "" {
			row.LatestMolecule = state.MoleculeID
		}
		out.Issues = append(out.Issues, row)
	}
	return out
}

// demoSync is GET /api/sync in demo mode: every sample Molecule of demoList,
// with sample GitHub state. Finished sample Molecules cycle through a merged
// pull request with a closed Issue, a draft pull request and a closed one.
func demoSync() SyncView {
	out := SyncView{Sample: true, Repository: demoRepo, Freshness: Freshness{Checked: true, FetchedAt: "2026-10-26T12:00:00Z"}, Molecules: map[string]forgesync.DisplayState{}, Counts: map[string]int{}, NeedsYou: map[string]bool{}}
	for _, s := range demoList().Molecules {
		m := forgesync.Molecule{ID: s.ID, Lane: s.Lane, Step: s.CurrentAtom}
		issue := demoIssue(s.Number, s.Title, forgesync.IssueOpen, "")
		prs := map[int64]forgesync.PR{}
		if s.Lane == LaneFinished {
			m.PRNumber = 5000 + s.Number
			switch s.Number % 3 {
			case 0:
				prs[m.PRNumber] = demoPR(m.PRNumber, forgesync.PRMerged, false)
				issue.State, issue.StateReason = forgesync.IssueClosed, forgesync.ReasonCompleted
			case 1:
				prs[m.PRNumber] = demoPR(m.PRNumber, forgesync.PROpen, true)
			default:
				prs[m.PRNumber] = demoPR(m.PRNumber, forgesync.PRClosed, false)
			}
		}
		d := forgesync.ForMolecule(&issue, prs, m)
		out.Molecules[s.ID] = d
		if d.Bucket != forgesync.BucketRetired {
			out.Counts[d.Bucket]++
		}
		if needsYou(d) {
			out.NeedsYou[s.ID] = true
		}
	}
	return out
}
