// Package forgesync reads GitHub Issue and pull request state back, read-only,
// and maps it with a Molecule's ledger lane to the one state every UI surface
// shows (#242). It never writes to GitHub or to the ledger: the sync is a
// projection, and SyncState is a pure function of what was read.
package forgesync

import (
	"sort"
	"strconv"
	"time"
)

// Issue states, state reasons and pull request states as GitHub's GraphQL API
// reports them. Any other value read is dropped by the fetcher.
const (
	IssueOpen   = "OPEN"
	IssueClosed = "CLOSED"

	ReasonCompleted  = "COMPLETED"
	ReasonNotPlanned = "NOT_PLANNED"
	ReasonReopened   = "REOPENED"

	PROpen   = "OPEN"
	PRClosed = "CLOSED"
	PRMerged = "MERGED"
)

// Molecule lanes, the same values as the UI's ledger lanes.
const (
	LaneRunning  = "running"
	LaneDecision = "decision"
	LaneBlocked  = "blocked"
	LaneFinished = "finished"
	LaneOther    = "other"
	LaneRetired  = "retired"
)

// Buckets are the lanes and counts a DisplayState is shown under. A ledger lane
// (running, decision, blocked, finished, other) is kept as is when GitHub state
// is unavailable or not checked.
const (
	BucketIssues   = "issues"
	BucketRunning  = LaneRunning
	BucketDecision = LaneDecision
	BucketBlocked  = LaneBlocked
	BucketDraftPR  = "draft-pr"
	BucketMerged   = "merged"
	BucketHistory  = "history"
	BucketRetired  = LaneRetired
)

// Kinds are the stable machine names of the matrix rows.
const (
	KindNotStarted         = "not-started"
	KindInProgress         = "in-progress"
	KindNeedsYou           = "needs-you"
	KindBlocked            = "blocked"
	KindNotStartedRetired  = "not-started-retired"
	KindDraftPR            = "draft-pr"
	KindInReview           = "pr-in-review"
	KindMergedIssueOpen    = "merged-issue-open"
	KindPRClosed           = "pr-closed"
	KindMerged             = "merged"
	KindClosedElsewhere    = "issue-closed-elsewhere"
	KindClosedNotPlanned   = "issue-closed-not-planned"
	KindClosedInProgress   = "issue-closed-in-progress"
	KindReopenedAfterMerge = "reopened-after-merge"
	KindUnavailable        = "issue-unavailable"
	KindRecorded           = "recorded"
	KindNotChecked         = "not-checked"
	KindRetired            = "retired"
)

// Notes are fixed texts; none carries GitHub or ledger content.
const (
	NoteHeadMoved       = "changed on GitHub after Forgecell published"
	NoteLocked          = "locked"
	NoteNotChecked      = "not checked"
	NoteRetireOrKeep    = "Issue was closed. Retire or keep?"
	NoteCloseOnGitHub   = "the pull request merged; close the Issue on GitHub if the work is done"
	NoteHeadDeleted     = "head branch deleted"
	NotePRNotFound      = "the published pull request was not found on GitHub"
	NotePRNotChecked    = "the published pull request was not checked"
	NoteOutsideMerge    = "closed by a pull request Forgecell did not publish"
	NoteMergedInHistory = "a retired attempt's pull request merged"
	NoteIssueClosed     = "Issue is closed"
	NoteStale           = "couldn't check GitHub just now; nothing is startable until GitHub state is current"
)

// Link is a closing pull request or commit. URL is built only from a validated
// number or commit id joined to the repository's fixed GitHub base.
type Link struct {
	Kind   string `json:"kind"` // "pull" or "commit"
	Number int64  `json:"number,omitempty"`
	Commit string `json:"commit,omitempty"`
	URL    string `json:"url"`
}

// Issue is one Issue as read from GitHub. Found is false when GitHub reports it
// not found, transferred or not readable; Unavailable then names why, in a
// fixed plain text.
type Issue struct {
	Number      int64    `json:"number"`
	Found       bool     `json:"found"`
	Unavailable string   `json:"unavailable,omitempty"`
	State       string   `json:"state,omitempty"`
	StateReason string   `json:"stateReason,omitempty"`
	ClosedAt    string   `json:"closedAt,omitempty"`
	ClosedBy    *Link    `json:"closedBy,omitempty"`
	Title       string   `json:"title,omitempty"`
	Labels      []string `json:"labels"`
	Milestone   string   `json:"milestone,omitempty"`
	CreatedAt   string   `json:"createdAt,omitempty"`
	UpdatedAt   string   `json:"updatedAt,omitempty"`
	Locked      bool     `json:"locked,omitempty"`
	// ReopenedAt is the time of the Issue's latest reopen, or "" when it was
	// never reopened or the time was not read.
	ReopenedAt string `json:"reopenedAt,omitempty"`
	URL        string `json:"url,omitempty"`
}

// PR is one pull request as read from GitHub.
type PR struct {
	Number         int64  `json:"number"`
	Found          bool   `json:"found"`
	State          string `json:"state,omitempty"`
	IsDraft        bool   `json:"isDraft,omitempty"`
	MergedAt       string `json:"mergedAt,omitempty"`
	MergeCommit    string `json:"mergeCommit,omitempty"`
	HeadRefOid     string `json:"headRefOid,omitempty"`
	HeadDeleted    bool   `json:"headDeleted,omitempty"`
	ReviewDecision string `json:"reviewDecision,omitempty"`
	// Checks is SUCCESS, FAILURE, PENDING or "" for none.
	Checks string `json:"checks,omitempty"`
	URL    string `json:"url,omitempty"`
}

// Molecule is one attempt for an Issue, from the ledger and its receipt.
type Molecule struct {
	ID        string
	Lane      string
	StartedAt string
	// Step is the running step or the waiting gate; Reason the blocked reason.
	// Both are display text the caller already bounded and redacted.
	Step   string
	Reason string
	// PRNumber and PublishedCommit come from a published delivery receipt.
	PRNumber        int64
	PublishedCommit string
}

// Work is every recorded attempt for one Issue and whether the loop's own
// summary (molecule.QueueSummary.Startable) allows a fresh Molecule.
type Work struct {
	Molecules        []Molecule
	SummaryStartable bool
}

// MergeView is an observed merge of a Forgecell pull request.
type MergeView struct {
	PR     int64  `json:"pr"`
	Commit string `json:"commit,omitempty"`
	At     string `json:"at,omitempty"`
	URL    string `json:"url,omitempty"`
}

// DisplayState is what every surface shows for one Issue or one Molecule.
type DisplayState struct {
	Row    int    `json:"row"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Bucket string `json:"bucket"`
	// Checked is false when no GitHub state was read: the state is ledger only.
	Checked   bool     `json:"checked"`
	Startable bool     `json:"startable"`
	Locked    bool     `json:"locked,omitempty"`
	Notes     []string `json:"notes"`
	// MoleculeID is the attempt that decides the state; EarlierAttempts are the
	// others, newest first.
	MoleculeID      string   `json:"moleculeId,omitempty"`
	EarlierAttempts []string `json:"earlierAttempts"`
	Retired         int      `json:"retired,omitempty"`
	PR              *PRView  `json:"pr,omitempty"`
	// ForgecellMerge is set only when a Forgecell-published pull request is
	// observed merged; analytics counts merged PRs only from it.
	ForgecellMerge *MergeView `json:"forgecellMerge,omitempty"`
	ClosedBy       *Link      `json:"closedBy,omitempty"`
	ClosedAt       string     `json:"closedAt,omitempty"`
}

// PRView is the published pull request's live state.
type PRView struct {
	Number int64  `json:"number"`
	State  string `json:"state"`
	Draft  bool   `json:"draft,omitempty"`
	Checks string `json:"checks"`
	Review string `json:"review,omitempty"`
	URL    string `json:"url,omitempty"`
}

func live(lane string) bool {
	return lane == LaneRunning || lane == LaneDecision || lane == LaneBlocked
}

func startedAt(m Molecule) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, m.StartedAt)
	return t, err == nil
}

// newestFirst orders attempts newest first; an unparseable start time sorts
// last, and ties break by id so the order never depends on input order.
func newestFirst(ms []Molecule) []Molecule {
	out := append([]Molecule{}, ms...)
	sort.SliceStable(out, func(i, j int) bool {
		a, aok := startedAt(out[i])
		b, bok := startedAt(out[j])
		switch {
		case aok != bok:
			return aok
		case aok && !a.Equal(b):
			return a.After(b)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// SyncState is the one state of an Issue and its recorded work. issue is nil
// when no GitHub state was read; prs holds the pull requests read, by number.
//
// Precedence: a live Molecule (running, gate or blocked) beats everything except
// a closed Issue (row 14); then pull request states; then Issue states. The
// newest non-retired Molecule decides, and retired ones never do, but a retired
// Molecule whose pull request merged still counts as a Forgecell merge.
func SyncState(issue *Issue, prs map[int64]PR, work Work) DisplayState {
	d := DisplayState{Notes: []string{}, EarlierAttempts: []string{}}
	var decider *Molecule
	for _, m := range newestFirst(work.Molecules) {
		if m.Lane == LaneRetired {
			d.Retired++
			if mv := forgecellMerge(m, prs); mv != nil && d.ForgecellMerge == nil {
				d.ForgecellMerge = mv
			}
		}
		if decider == nil && m.Lane != LaneRetired {
			m := m
			decider = &m
			continue
		}
		d.EarlierAttempts = append(d.EarlierAttempts, m.ID)
	}
	if decider != nil {
		d.MoleculeID = decider.ID
	}
	if issue == nil {
		d.Kind, d.Row = KindNotChecked, 0
		d.Notes = append(d.Notes, NoteNotChecked)
		if decider == nil {
			d.Label, d.Bucket = "Not checked", BucketIssues
			return d
		}
		d.Label, d.Bucket = ledgerLabel(*decider), decider.Lane
		return d
	}
	d.Checked = true
	if !issue.Found {
		d.Row, d.Kind = 16, KindUnavailable
		d.Label = "Issue unavailable · " + issue.Unavailable
		d.Bucket = BucketIssues
		if decider != nil {
			d.Bucket = decider.Lane
			d.Notes = append(d.Notes, ledgerLabel(*decider))
		}
		if issue.Locked {
			d.Locked = true
			d.Notes = append(d.Notes, NoteLocked)
		}
		return d
	}
	open := issue.State == IssueOpen
	if !open {
		d.ClosedAt, d.ClosedBy = issue.ClosedAt, issue.ClosedBy
	}
	var pr *PR
	if decider != nil && decider.PRNumber > 0 {
		if p, ok := prs[decider.PRNumber]; ok {
			pr = &p
		}
	}
	if pr != nil && pr.Found {
		d.PR = &PRView{Number: pr.Number, State: pr.State, Draft: pr.IsDraft, Checks: checksText(pr.Checks), Review: reviewText(pr.ReviewDecision), URL: pr.URL}
		if mv := forgecellMerge(*decider, prs); mv != nil {
			d.ForgecellMerge = mv
		}
	}
	switch {
	case decider != nil && live(decider.Lane) && !open:
		d.Row, d.Kind, d.Label, d.Bucket = 14, KindClosedInProgress, "Issue closed while in progress", BucketDecision
		d.Notes = append(d.Notes, NoteRetireOrKeep)
	case decider != nil && live(decider.Lane):
		d.Label, d.Bucket = ledgerLabel(*decider), decider.Lane
		switch decider.Lane {
		case LaneRunning:
			d.Row, d.Kind = 2, KindInProgress
		case LaneDecision:
			d.Row, d.Kind = 3, KindNeedsYou
		default:
			d.Row, d.Kind = 4, KindBlocked
		}
	case pr != nil && pr.Found && pr.State == PROpen:
		d.Bucket = BucketDraftPR
		if pr.IsDraft {
			d.Row, d.Kind, d.Label = 6, KindDraftPR, "Draft PR open"
		} else {
			d.Row, d.Kind, d.Label = 7, KindInReview, "PR in review"
		}
		if pr.HeadRefOid != "" && decider.PublishedCommit != "" && pr.HeadRefOid != decider.PublishedCommit {
			d.Notes = append(d.Notes, NoteHeadMoved)
		}
		if !open {
			d.Notes = append(d.Notes, NoteIssueClosed)
		}
	case !open && issue.StateReason == ReasonNotPlanned:
		d.Row, d.Kind, d.Label, d.Bucket = 13, KindClosedNotPlanned, "Issue closed · not planned", BucketHistory
	case !open && d.ForgecellMerge != nil:
		d.Row, d.Kind, d.Label, d.Bucket = 11, KindMerged, "Merged", BucketMerged
		if pr == nil || pr.State != PRMerged {
			d.Notes = append(d.Notes, NoteMergedInHistory)
		}
	case !open:
		d.Row, d.Kind, d.Label, d.Bucket = 12, KindClosedElsewhere, "Issue closed elsewhere", BucketHistory
		if c := issue.ClosedBy; c != nil && c.Kind == "pull" && (pr == nil || c.Number != pr.Number) {
			d.Notes = append(d.Notes, NoteOutsideMerge)
		}
	case pr != nil && pr.Found && pr.State == PRMerged && reopenedAfter(issue.ReopenedAt, pr.MergedAt):
		d.Row, d.Kind, d.Label, d.Bucket = 15, KindReopenedAfterMerge, "Reopened after merge", BucketIssues
	case pr != nil && pr.Found && pr.State == PRMerged:
		d.Row, d.Kind, d.Label, d.Bucket = 9, KindMergedIssueOpen, "Merged · Issue still open", BucketMerged
		d.Notes = append(d.Notes, NoteCloseOnGitHub)
	case pr != nil && pr.Found && pr.State == PRClosed:
		d.Row, d.Kind, d.Label, d.Bucket = 10, KindPRClosed, "PR closed without merge", BucketIssues
	case decider != nil:
		// Recorded work no row names, such as a Molecule that finished without a
		// pull request, or whose pull request was not found: its ledger lane stays.
		d.Kind, d.Label, d.Bucket = KindRecorded, ledgerLabel(*decider), decider.Lane
		switch {
		case decider.PRNumber > 0 && pr == nil:
			d.Notes = append(d.Notes, NotePRNotChecked)
		case decider.PRNumber > 0:
			d.Notes = append(d.Notes, NotePRNotFound)
		}
	case d.Retired > 0:
		d.Row, d.Kind, d.Label, d.Bucket = 5, KindNotStartedRetired, "Not started ("+strconv.Itoa(d.Retired)+" retired)", BucketIssues
	default:
		d.Row, d.Kind, d.Label, d.Bucket = 1, KindNotStarted, "Not started", BucketIssues
	}
	if pr != nil && pr.Found && pr.HeadDeleted && pr.State == PROpen {
		d.Notes = append(d.Notes, NoteHeadDeleted)
	}
	// Startable: the Issue is open and readable, and either the loop's summary
	// allows a fresh Molecule or the latest one ended in a closed or reopened
	// pull request (rows 10 and 15). The loop itself keeps the summary only.
	d.Startable = open && (work.SummaryStartable && (d.Row == 1 || d.Row == 5) || d.Row == 10 || d.Row == 15)
	if issue.Locked {
		d.Locked = true
		d.Notes = append(d.Notes, NoteLocked)
	}
	return d
}

// ForMolecule is the state of one Molecule: the Issue state with that Molecule
// as the only attempt, or Retired for a retired one, noting a merged pull
// request. Startable is an Issue's property and is never set here.
func ForMolecule(issue *Issue, prs map[int64]PR, m Molecule) DisplayState {
	if m.Lane == LaneRetired {
		d := DisplayState{Kind: KindRetired, Label: "Retired", Bucket: BucketRetired, Checked: issue != nil, Notes: []string{}, EarlierAttempts: []string{}, MoleculeID: m.ID}
		if issue == nil {
			d.Notes = append(d.Notes, NoteNotChecked)
		}
		if mv := forgecellMerge(m, prs); mv != nil {
			d.ForgecellMerge, d.Label = mv, "Retired · PR merged"
		}
		return d
	}
	d := SyncState(issue, prs, Work{Molecules: []Molecule{m}})
	d.Startable = false
	return d
}

// reopenedAfter reports a reopen strictly after the merge. A reopen before the
// merge, or either time missing or unreadable, is not one: the row stays
// merged (row 9) and not startable.
func reopenedAfter(reopenedAt, mergedAt string) bool {
	r, err := time.Parse(time.RFC3339, reopenedAt)
	if err != nil {
		return false
	}
	m, err := time.Parse(time.RFC3339, mergedAt)
	return err == nil && r.After(m)
}

// forgecellMerge is the merge of m's own published pull request, if observed.
func forgecellMerge(m Molecule, prs map[int64]PR) *MergeView {
	if m.PRNumber <= 0 {
		return nil
	}
	p, ok := prs[m.PRNumber]
	if !ok || !p.Found || p.State != PRMerged {
		return nil
	}
	return &MergeView{PR: p.Number, Commit: p.MergeCommit, At: p.MergedAt, URL: p.URL}
}

// ledgerLabel is the ledger-only state of an attempt.
func ledgerLabel(m Molecule) string {
	with := func(head, detail string) string {
		if detail == "" {
			return head
		}
		return head + " · " + detail
	}
	switch m.Lane {
	case LaneRunning:
		return with("In progress", m.Step)
	case LaneDecision:
		return with("Needs you", m.Step)
	case LaneBlocked:
		return with("Blocked", m.Reason)
	case LaneFinished:
		if m.PRNumber > 0 {
			return "Draft published"
		}
		return "Finished"
	case LaneRetired:
		return "Retired"
	}
	return "Recorded"
}

func checksText(s string) string {
	switch s {
	case "SUCCESS":
		return "passing"
	case "FAILURE", "ERROR":
		return "failing"
	case "PENDING", "EXPECTED":
		return "pending"
	}
	return "none"
}

func reviewText(s string) string {
	switch s {
	case "APPROVED":
		return "approved"
	case "CHANGES_REQUESTED":
		return "changes requested"
	case "REVIEW_REQUIRED":
		return "review required"
	}
	return ""
}
