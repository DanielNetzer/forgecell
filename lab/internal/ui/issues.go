package ui

import (
	"context"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// issueSync is one server's read-only GitHub sync (#242): the forgesync cache
// and its fetcher, and the active Formula's repository. Tests replace the
// fetcher, the clock and the repository the same way as the other seams.
type issueSync struct {
	cache *forgesync.Cache
	// repository is the active Formula's repository; it never comes from a
	// request.
	repository func(lab string) string
	// now is the cache's clock, so Start intake can tell a snapshot read after
	// it began from an older one and the loop view can age the last answer.
	now        func() time.Time
	receiptsMu sync.Mutex
	receipts   map[string]publishedEntry
	// last is the latest answer get returned, which the loop view reads
	// without starting a sync.
	lastMu sync.Mutex
	last   forgesync.Result
}

func newIssueSync(fetcher forgesync.Fetcher, now func() time.Time) *issueSync {
	if fetcher == nil {
		fetcher = forgesync.NewGH()
	}
	if now == nil {
		now = time.Now
	}
	return &issueSync{cache: forgesync.NewCache(fetcher, now), repository: func(lab string) string { return formula.ActiveIdentity(lab).Repository }, receipts: map[string]publishedEntry{}, now: now}
}

// get is the cache's answer for want, remembered for currentSnapshot.
func (s *issueSync) get(ctx context.Context, want forgesync.Want, refresh bool) forgesync.Result {
	res := s.cache.Get(ctx, want, refresh)
	s.lastMu.Lock()
	s.last = res
	s.lastMu.Unlock()
	return res
}

// currentSnapshot is the latest remembered sync of repo while it is still
// current (a successful sync at most twice the TTL old), or nil. It never
// starts a sync.
func (s *issueSync) currentSnapshot(repo string) *forgesync.Snapshot {
	s.lastMu.Lock()
	res := s.last
	s.lastMu.Unlock()
	if res.Snapshot == nil || res.Reason != "" || repo == "" || !strings.EqualFold(res.Snapshot.Repository, repo) {
		return nil
	}
	if s.now().Sub(res.Snapshot.FetchedAt) > 2*forgesync.TTL {
		return nil
	}
	return res.Snapshot
}

// published is the pull request number and commit of one published receipt.
type published struct {
	number int64
	commit string
}

type publishedEntry struct {
	stamp fileStamp
	value published
}

var pullNumber = regexp.MustCompile(`/pull/([1-9][0-9]{0,15})$`)

var publishedCommit = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// published reads the pull request of a Molecule's published receipt, reusing
// the last read while the receipt file is unchanged. Anything but a published
// receipt naming the Issue repository's pull request yields the zero value.
func (h *handler) published(id, repo string) published {
	s := h.forge
	path := filepath.Join(h.opts.LabDir, "deliveries", id+".json")
	st, cacheable := stamp(path)
	if cacheable {
		s.receiptsMu.Lock()
		e, ok := s.receipts[id]
		s.receiptsMu.Unlock()
		if ok && e.stamp == st {
			return e.value
		}
	}
	var out published
	receipt, found, err := h.receipts(h.opts.LabDir, id)
	if found && err == nil && receipt.State == "published" && pullURL(receipt.URL, repo) {
		if m := pullNumber.FindStringSubmatch(receipt.URL); m != nil {
			out.number, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if publishedCommit.MatchString(receipt.Commit) {
			out.commit = receipt.Commit
		}
	}
	if cacheable {
		s.receiptsMu.Lock()
		if len(s.receipts) >= maxSyncMolecules {
			s.receipts = map[string]publishedEntry{}
		}
		s.receipts[id] = publishedEntry{st, out}
		s.receiptsMu.Unlock()
	}
	return out
}

// maxSyncMolecules bounds the receipt cache; the inventory itself is bounded.
const maxSyncMolecules = 8192

// maxStepRunes bounds the ledger step and reason shown with a state.
const maxStepRunes = 200

func bounded(s string) string {
	if utf8.RuneCountInString(s) <= maxStepRunes {
		return s
	}
	return string([]rune(s)[:maxStepRunes]) + "…"
}

// syncMolecule maps one listed Summary to the sync's input.
func (h *handler) syncMolecule(s Summary, repo string) forgesync.Molecule {
	m := forgesync.Molecule{ID: s.ID, Lane: s.Lane, StartedAt: s.StartedAt}
	switch s.Lane {
	case LaneRunning:
		m.Step = s.Activity
		if m.Step == "" {
			m.Step = s.CurrentAtom
		}
	case LaneDecision:
		m.Step = "review"
		if s.Phase == "scope-waiting" {
			m.Step = "scope"
		}
	case LaneBlocked:
		m.Reason = s.Status
		if s.Inbox.Kind == InboxBlocked && s.Inbox.Summary != "" {
			m.Reason = s.Inbox.Summary
		}
	}
	m.Step, m.Reason = bounded(m.Step), bounded(m.Reason)
	if s.Delivery != "" {
		p := h.published(s.ID, repo)
		m.PRNumber, m.PublishedCommit = p.number, p.commit
	}
	return m
}

// syncWork is one bounded pass over the Lab for the sync: every listed
// Molecule, those of the repository grouped by Issue, and the numbers to read.
type syncWork struct {
	list    MoleculeList
	inputs  map[string]forgesync.Molecule
	byIssue map[int64][]forgesync.Molecule
	want    forgesync.Want
}

func (h *handler) syncWork(repo string) syncWork {
	w := syncWork{list: h.list(ledgerETag(h.opts.LabDir)), inputs: map[string]forgesync.Molecule{}, byIssue: map[int64][]forgesync.Molecule{}, want: forgesync.Want{Repository: repo}}
	seenIssue, seenPR := map[int64]bool{}, map[int64]bool{}
	for _, s := range w.list.Molecules {
		if repo == "" || !strings.EqualFold(s.Repository, repo) {
			continue
		}
		m := h.syncMolecule(s, repo)
		w.inputs[s.ID] = m
		w.byIssue[s.Number] = append(w.byIssue[s.Number], m)
		if !seenIssue[s.Number] {
			seenIssue[s.Number] = true
			w.want.Issues = append(w.want.Issues, s.Number)
		}
		if m.PRNumber > 0 && !seenPR[m.PRNumber] {
			seenPR[m.PRNumber] = true
			w.want.PRs = append(w.want.PRs, m.PRNumber)
		}
	}
	return w
}

// syncRepository is the active Formula's repository when it is a valid
// owner/name, and "" otherwise.
func (h *handler) syncRepository() string {
	return repository(h.forge.repository(h.opts.LabDir))
}

// Freshness is the GitHub state's age and the latest failure, shared by both
// sync routes.
type Freshness struct {
	// Checked is false when no GitHub state was ever read: states are ledger only.
	Checked bool `json:"checked"`
	// Reason is the fixed reason the latest sync failed; with Checked it means the
	// state shown is the last good one, AgeSeconds old.
	Reason string `json:"reason,omitempty"`
	// Stale means the state shown is not current (the latest sync failed, or it is
	// older than twice the TTL): then nothing is startable.
	Stale      bool   `json:"stale"`
	FetchedAt  string `json:"fetchedAt,omitempty"`
	AgeSeconds int64  `json:"ageSeconds"`
}

func freshness(r forgesync.Result) Freshness {
	f := Freshness{Checked: r.Snapshot != nil, Reason: r.Reason, Stale: r.Snapshot != nil && !r.Current()}
	if r.Snapshot != nil {
		f.FetchedAt = r.Snapshot.FetchedAt.UTC().Format(time.RFC3339)
		f.AgeSeconds = int64(r.Age / time.Second)
	}
	return f
}

// refresh reports ?refresh=1, the only query parameter the sync routes read.
func refresh(r *http.Request) bool { return r.URL.Query().Get("refresh") == "1" }

// IssuesView is GET /api/issues: the open Issues of the active Formula's
// repository, oldest first, each with its one sync state.
type IssuesView struct {
	Sample     bool   `json:"sample"`
	Repository string `json:"repository"`
	Freshness
	// Total is GitHub's count of open Issues; Truncated means Issues lists fewer.
	Total     int    `json:"total"`
	Shown     int    `json:"shown"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note,omitempty"`
	// Complete is false when the Lab's ledgers could not all be read; then no
	// Issue is startable.
	Complete bool       `json:"complete"`
	Issues   []IssueRow `json:"issues"`
}

// IssueRow is one open Issue. Startable is the sync's rule; SummaryStartable is
// the loop's own (molecule.QueueSummary.Startable), which the loop keeps.
type IssueRow struct {
	Number           int64                  `json:"number"`
	Title            string                 `json:"title"`
	URL              string                 `json:"url"`
	Labels           []string               `json:"labels"`
	Milestone        string                 `json:"milestone,omitempty"`
	UpdatedAt        string                 `json:"updatedAt,omitempty"`
	Locked           bool                   `json:"locked,omitempty"`
	State            forgesync.DisplayState `json:"state"`
	Startable        bool                   `json:"startable"`
	SummaryState     string                 `json:"summaryState"`
	SummaryStartable bool                   `json:"summaryStartable"`
	LatestMolecule   string                 `json:"latestMolecule,omitempty"`
	MoleculeLink     string                 `json:"moleculeLink,omitempty"`
}

func (h *handler) issues(w http.ResponseWriter, r *http.Request) {
	if h.opts.Demo {
		h.json(w, http.StatusOK, demoIssues())
		return
	}
	repo := h.syncRepository()
	out := IssuesView{Repository: repo, Issues: []IssueRow{}}
	if repo == "" {
		out.Reason = forgesync.ReasonRepository
		h.json(w, http.StatusOK, out)
		return
	}
	work := h.syncWork(repo)
	res := h.forge.get(r.Context(), work.want, refresh(r))
	out.Freshness = freshness(res)
	queue := molecule.ReadQueueInventory(h.opts.LabDir)
	out.Complete = queue.Complete && work.list.Complete
	snap := res.Snapshot
	if snap == nil {
		h.json(w, http.StatusOK, out)
		return
	}
	out.Total, out.Shown, out.Truncated = snap.Listing.Total, len(snap.Listing.Issues), snap.Listing.Truncated
	if out.Truncated {
		out.Note = "showing " + strconv.Itoa(out.Shown) + " of " + strconv.Itoa(out.Total)
	}
	for _, issue := range snap.Listing.Issues {
		summary := queue.Summarize(repo, issue.Number)
		issue := issue
		state := forgesync.SyncState(&issue, snap.PRs, forgesync.Work{Molecules: work.byIssue[issue.Number], SummaryStartable: summary.Startable()})
		if !out.Complete {
			state.Startable = false
		}
		// Stale GitHub state never allows a start.
		if out.Stale {
			state.Startable = false
			state.Notes = append(state.Notes, forgesync.NoteStale)
		}
		row := IssueRow{Number: issue.Number, Title: issue.Title, URL: issue.URL, Labels: issue.Labels, Milestone: issue.Milestone, UpdatedAt: issue.UpdatedAt, Locked: issue.Locked, State: state, Startable: state.Startable, SummaryState: summary.State, SummaryStartable: summary.Startable()}
		if row.Labels == nil {
			row.Labels = []string{}
		}
		if id := state.MoleculeID; moleculeID.MatchString(id) {
			row.LatestMolecule, row.MoleculeLink = id, "/?m="+id
		}
		out.Issues = append(out.Issues, row)
	}
	h.json(w, http.StatusOK, out)
}
