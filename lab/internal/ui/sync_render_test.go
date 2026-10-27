package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// matrixCase is one row of the sync matrix in the cross-surface fixture: the
// Issue, the one Molecule that decides it (if any) and the row both surfaces
// must show.
type matrixCase struct {
	issue        int64
	molecule     string
	row          int
	kind, bucket string
	open         bool
}

// matrixCases cover every matrix row the sync names (#242): rows 1-16, with
// row 7's head-moved note, plus row 5's retired attempt.
var matrixCases = []matrixCase{
	{1, "", 1, forgesync.KindNotStarted, forgesync.BucketIssues, true},
	{2, "mol-2-run", 2, forgesync.KindInProgress, forgesync.BucketRunning, true},
	{3, "mol-3-scope", 3, forgesync.KindNeedsYou, forgesync.BucketDecision, true},
	{4, "mol-4-blocked", 4, forgesync.KindBlocked, forgesync.BucketBlocked, true},
	{5, "", 5, forgesync.KindNotStartedRetired, forgesync.BucketIssues, true},
	{6, "mol-6-draft", 6, forgesync.KindDraftPR, forgesync.BucketDraftPR, true},
	{17, "mol-17-review", 7, forgesync.KindInReview, forgesync.BucketDraftPR, true},
	{18, "mol-18-moved", 7, forgesync.KindInReview, forgesync.BucketDraftPR, true},
	{19, "mol-19-merged", 9, forgesync.KindMergedIssueOpen, forgesync.BucketMerged, true},
	{8, "mol-8-closed", 10, forgesync.KindPRClosed, forgesync.BucketIssues, true},
	{7, "mol-7-merged", 11, forgesync.KindMerged, forgesync.BucketMerged, false},
	{12, "mol-12-elsewhere", 12, forgesync.KindClosedElsewhere, forgesync.BucketHistory, false},
	{13, "mol-13-unplanned", 13, forgesync.KindClosedNotPlanned, forgesync.BucketHistory, false},
	{14, "mol-14-closed-run", 14, forgesync.KindClosedInProgress, forgesync.BucketDecision, false},
	{15, "mol-15-reopened", 15, forgesync.KindReopenedAfterMerge, forgesync.BucketIssues, true},
	{16, "mol-16-gone", 16, forgesync.KindUnavailable, forgesync.BucketBlocked, false},
}

// matrixLab is the Lab side of the fixture: one ledger per Molecule of
// matrixCases, published receipts for the pull requests, and a retired attempt
// for Issue 5.
func matrixLab(t *testing.T) string {
	t.Helper()
	lab, base := labFixture(t)
	os.Remove(filepath.Join(lab, "ledgers", base.ID+".json"))
	add := func(id string, n int64, status, phase string) {
		r := base
		r.ID, r.Status, r.Issue.Number, r.FinishedAt = id, status, n, ""
		r.Issue.URL = fmt.Sprintf("https://github.com/o/r/issues/%d", n)
		r.StartedAt = fmt.Sprintf("2026-10-24T10:%02d:00Z", n)
		r.Readiness = &readiness.State{SchemaVersion: "v1", Phase: phase}
		r.Atoms, r.HarnessAttempts = []molecule.Atom{{ID: "code", Type: "harness", Status: "active"}}, nil
		if status == molecule.StatusRetired {
			r.Retired = &molecule.Retirement{At: "2026-10-26T10:00:00Z", Reason: "stale", PriorStatus: "waiting"}
		}
		if status == "completed" {
			r.Atoms = []molecule.Atom{{ID: "code", Type: "harness", Status: "completed"}}
		}
		writeRecord(t, lab, r)
	}
	add("mol-2-run", 2, "running", "coding")
	add("mol-3-scope", 3, "waiting", "scope-waiting")
	add("mol-4-blocked", 4, "blocked", "blocked")
	add("mol-5-retired", 5, molecule.StatusRetired, "scope-waiting")
	add("mol-12-elsewhere", 12, "completed", "")
	add("mol-13-unplanned", 13, "completed", "")
	add("mol-14-closed-run", 14, "running", "coding")
	add("mol-16-gone", 16, "blocked", "blocked")
	for _, p := range []struct {
		id    string
		n, pr int64
	}{{"mol-6-draft", 6, 60}, {"mol-17-review", 17, 170}, {"mol-18-moved", 18, 180}, {"mol-19-merged", 19, 190}, {"mol-8-closed", 8, 80}, {"mol-7-merged", 7, 70}, {"mol-15-reopened", 15, 150}} {
		add(p.id, p.n, "completed", "review-waiting")
		writePublishedReceipt(t, lab, p.id, p.pr)
	}
	return lab
}

// matrixFetcher is GitHub's side of the fixture.
func matrixFetcher() *syncFetcher {
	f := &syncFetcher{issues: map[int64]forgesync.Issue{}, prs: map[int64]forgesync.PR{}}
	for _, n := range []int64{1, 2, 3, 4, 5, 6, 8, 9, 17, 18, 19} {
		f.listing.Issues = append(f.listing.Issues, openSyncIssue(n))
	}
	reopened := openSyncIssue(15)
	reopened.StateReason, reopened.ReopenedAt = forgesync.ReasonReopened, "2026-10-19T10:00:00Z"
	f.listing.Issues = append(f.listing.Issues, reopened)
	f.listing.Total = len(f.listing.Issues)
	closed := func(n int64, reason string) {
		i := openSyncIssue(n)
		i.State, i.StateReason, i.ClosedAt = forgesync.IssueClosed, reason, "2026-10-18T10:00:00Z"
		f.issues[n] = i
	}
	closed(7, forgesync.ReasonCompleted)
	closed(12, forgesync.ReasonCompleted)
	closed(13, forgesync.ReasonNotPlanned)
	closed(14, forgesync.ReasonCompleted)
	f.issues[16] = forgesync.Issue{Number: 16, Found: false, Unavailable: "not found", Labels: []string{}}
	moved := syncPR(180, forgesync.PROpen, false)
	moved.HeadRefOid = strings.Repeat("f", 40)
	for _, p := range []forgesync.PR{syncPR(60, forgesync.PROpen, true), syncPR(170, forgesync.PROpen, false), moved, syncPR(190, forgesync.PRMerged, false), syncPR(80, forgesync.PRClosed, false), syncPR(70, forgesync.PRMerged, false), syncPR(150, forgesync.PRMerged, false)} {
		f.prs[p.Number] = p
	}
	return f
}

// One fixture, every matrix row: the Issues view (/api/issues) and the Inbox,
// Work grid, counts and Molecule view (/api/sync) show the same state for each
// row, because every surface renders the server's one DisplayState.
func TestSyncRenderEverySurfaceAgreesForEveryMatrixRow(t *testing.T) {
	h, _ := syncHandler(t, Options{LabDir: matrixLab(t)}, matrixFetcher())
	v, rows := issueRows(t, h, "/api/issues")
	var s SyncView
	decode(t, do(t, h, authed("/api/sync")), &s)
	if !v.Checked || v.Stale || !v.Complete || !s.Checked || s.Stale {
		t.Fatalf("fixture not current: issues %+v sync %+v", v.Freshness, s.Freshness)
	}
	for _, c := range matrixCases {
		var states []forgesync.DisplayState
		if c.open {
			r, ok := rows[c.issue]
			if !ok {
				t.Fatalf("row %d: Issue %d is not listed", c.row, c.issue)
			}
			if r.LatestMolecule != c.molecule || r.Startable != r.State.Startable {
				t.Errorf("row %d: Issue %d molecule %q startable %v/%v", c.row, c.issue, r.LatestMolecule, r.Startable, r.State.Startable)
			}
			states = append(states, r.State)
		} else if _, listed := rows[c.issue]; listed {
			t.Errorf("row %d: closed or unavailable Issue %d is listed as open", c.row, c.issue)
		}
		if c.molecule != "" {
			d, ok := s.Molecules[c.molecule]
			if !ok {
				t.Fatalf("row %d: %s has no sync state", c.row, c.molecule)
			}
			states = append(states, d)
		}
		for _, d := range states {
			if d.Row != c.row || d.Kind != c.kind || d.Bucket != c.bucket || d.Label == "" || !d.Checked {
				t.Errorf("row %d (Issue %d): %+v", c.row, c.issue, d)
			}
		}
		// Both surfaces of one row show the same pill, bucket and merge.
		if len(states) == 2 {
			a, b := states[0], states[1]
			if a.Label != b.Label || a.Bucket != b.Bucket || a.Kind != b.Kind || !slices.Equal(a.Notes, b.Notes) || (a.ForgecellMerge == nil) != (b.ForgecellMerge == nil) {
				t.Errorf("row %d: the Issues view shows %+v, the Molecule surfaces %+v", c.row, a, b)
			}
		}
	}
	if d := s.Molecules["mol-18-moved"]; !slices.Contains(d.Notes, forgesync.NoteHeadMoved) {
		t.Errorf("head moved note missing: %v", d.Notes)
	}
	if d := s.Molecules["mol-14-closed-run"]; d.ClosedAt == "" || !slices.Contains(d.Notes, forgesync.NoteRetireOrKeep) {
		t.Errorf("row 14 lacks its closed time or note: %+v", d)
	}
	if m := s.Molecules["mol-7-merged"].ForgecellMerge; m == nil || m.PR != 70 || m.Commit == "" || m.At == "" {
		t.Errorf("row 11 merge %+v", m)
	}
	if !rows[1].Startable || rows[2].Startable || !rows[8].Startable || !rows[15].Startable {
		t.Errorf("startable: 1 %v 2 %v 8 %v 15 %v", rows[1].Startable, rows[2].Startable, rows[8].Startable, rows[15].Startable)
	}
	// The counts header every surface shows is /api/sync's bucket tally of the
	// non-retired Molecules: 1 running · 4 need you (decision and blocked) · 2
	// merged, with 2 closed history collapsed beside the merged.
	tally := map[string]int{}
	for _, d := range s.Molecules {
		if d.Bucket != forgesync.BucketRetired {
			tally[d.Bucket]++
		}
	}
	if !mapsEqual(tally, s.Counts) || s.Counts[forgesync.BucketRunning] != 1 || s.Counts[forgesync.BucketDecision]+s.Counts[forgesync.BucketBlocked] != 4 || s.Counts[forgesync.BucketMerged] != 2 || s.Counts[forgesync.BucketHistory] != 2 || s.Counts[forgesync.BucketRetired] != 0 {
		t.Fatalf("counts %v, tally %v", s.Counts, tally)
	}
	if d := s.Molecules["mol-5-retired"]; d.Bucket != forgesync.BucketRetired {
		t.Errorf("retired %+v", d)
	}
	// Needs you is served once, per Molecule: exactly the decision and blocked
	// states, row 14 among them, so the "need you" count matches the buckets.
	for id, d := range s.Molecules {
		if want := d.Bucket == forgesync.BucketDecision || d.Bucket == forgesync.BucketBlocked; s.NeedsYou[id] != want {
			t.Errorf("%s (%s): needsYou %v", id, d.Bucket, s.NeedsYou[id])
		}
	}
	if !s.NeedsYou["mol-14-closed-run"] || len(s.NeedsYou) != s.Counts[forgesync.BucketDecision]+s.Counts[forgesync.BucketBlocked] {
		t.Errorf("needsYou %v, counts %v", s.NeedsYou, s.Counts)
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// On stale GitHub state both surfaces fail closed together: nothing startable,
// each state noted, the reason one fixed text.
func TestSyncRenderStaleStateIsTheSameOnEverySurface(t *testing.T) {
	f := matrixFetcher()
	h, clk := syncHandler(t, Options{LabDir: matrixLab(t)}, f)
	issueRows(t, h, "/api/issues")
	clk.add(5 * time.Minute)
	f.mu.Lock()
	f.err = &forgesync.Failure{Reason: forgesync.ReasonOffline}
	f.mu.Unlock()
	v, rows := issueRows(t, h, "/api/issues?refresh=1")
	var s SyncView
	decode(t, do(t, h, authed("/api/sync")), &s)
	if !v.Stale || !s.Stale || v.Reason != forgesync.ReasonOffline || s.Reason != forgesync.ReasonOffline {
		t.Fatalf("stale: issues %+v sync %+v", v.Freshness, s.Freshness)
	}
	for n, r := range rows {
		if r.Startable || !slices.Contains(r.State.Notes, forgesync.NoteStale) {
			t.Errorf("Issue %d on stale state: %+v", n, r.State)
		}
		if d, ok := s.Molecules[r.LatestMolecule]; ok && (d.Label != r.State.Label || d.Bucket != r.State.Bucket) {
			t.Errorf("Issue %d: %q/%q vs %q/%q", n, r.State.Label, r.State.Bucket, d.Label, d.Bucket)
		}
	}
}

// Start intake re-reads GitHub: an Issue closed after the Issues view showed it
// startable is refused before the engine runs, as is any Issue while GitHub
// cannot be read; an open Issue starts.
func TestSyncRenderStartIntakeRefusedWhenTheIssueClosedMeanwhile(t *testing.T) {
	f := matrixFetcher()
	h, _ := syncHandler(t, Options{LabDir: matrixLab(t), SourceRoot: "/src/checkout"}, f)
	d := installDrive(h, &fakeDrive{run: func(context.Context, molecule.Options) error { return nil }})
	if _, rows := issueRows(t, h, "/api/issues"); !rows[1].Startable {
		t.Fatalf("Issue 1 not startable before it closed: %+v", rows[1])
	}
	// Issue 1 closes on GitHub after the list was shown.
	f.mu.Lock()
	f.listing.Issues = slices.DeleteFunc(f.listing.Issues, func(i forgesync.Issue) bool { return i.Number == 1 })
	gone := openSyncIssue(1)
	gone.State, gone.StateReason = forgesync.IssueClosed, forgesync.ReasonCompleted
	f.issues[1] = gone
	lists := f.lists
	f.mu.Unlock()
	refusedWith(t, do(t, h, gatePost("/api/runs", `{"issue":1}`)), RefusedIssueClosed)
	if f.lists != lists+1 {
		t.Errorf("Start intake did not read GitHub again: lists %d, before %d", f.lists, lists)
	}
	// Not found on GitHub.
	refusedWith(t, do(t, h, gatePost("/api/runs", `{"issue":16}`)), RefusedIssueUnavailable)
	if n := d.calls(); n != 0 {
		t.Fatalf("a refused start reached the engine %d times", n)
	}
	// An open Issue starts.
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":9}`)), http.StatusAccepted)
	if n := d.calls(); n != 1 {
		t.Fatalf("an open Issue did not start: %d engine calls", n)
	}
	// GitHub unreadable: refused with a fixed text, never gh's output.
	f.mu.Lock()
	f.err = &forgesync.Failure{Reason: forgesync.ReasonNotAuthenticated}
	f.mu.Unlock()
	w := do(t, h, gatePost("/api/runs", `{"issue":15}`))
	refusedWith(t, w, RefusedIssueNotChecked)
	if strings.Contains(w.Body.String(), "gh auth") || d.calls() != 1 {
		t.Fatalf("unreadable GitHub: %s, %d engine calls", w.Body, d.calls())
	}
}

// Start intake enforces the whole startable rule, as the Issues view shows it:
// an open Issue whose Molecule is at the scope gate (row 3), has a draft PR
// (row 6) or was merged (row 9) is refused, and so is any Issue while the
// ledgers cannot all be read. Only rows 1, 5, 10 and 15 start.
func TestSyncRenderStartIntakeEnforcesTheStartableRule(t *testing.T) {
	lab := matrixLab(t)
	f := matrixFetcher()
	h, _ := syncHandler(t, Options{LabDir: lab, SourceRoot: "/src/checkout"}, f)
	d := installDrive(h, &fakeDrive{run: func(context.Context, molecule.Options) error { return nil }})
	_, rows := issueRows(t, h, "/api/issues")
	for _, n := range []int64{3, 6, 19} {
		if rows[n].Startable {
			t.Fatalf("Issue %d listed startable: %+v", n, rows[n].State)
		}
		refusedWith(t, do(t, h, gatePost("/api/runs", fmt.Sprintf(`{"issue":%d}`, n))), RefusedIssueNotStartable)
	}
	for _, n := range []int64{1, 5, 8, 15} {
		if !rows[n].Startable || rows[n].State.Row != map[int64]int{1: 1, 5: 5, 8: 10, 15: 15}[n] {
			t.Fatalf("Issue %d: %+v", n, rows[n].State)
		}
	}
	if n := d.calls(); n != 0 {
		t.Fatalf("a refused start reached the engine %d times", n)
	}
	// A row the Issues view shows startable starts.
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":5}`)), http.StatusAccepted)
	if n := d.calls(); n != 1 {
		t.Fatalf("row 5 did not start: %d engine calls", n)
	}
	// An unreadable ledger makes the inventory incomplete: nothing starts, even
	// an Issue that was startable.
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "mol-99-bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, rows := issueRows(t, h, "/api/issues"); v.Complete || rows[1].Startable {
		t.Fatalf("incomplete inventory listed startable: complete %v, %+v", v.Complete, rows[1])
	}
	refusedWith(t, do(t, h, gatePost("/api/runs", `{"issue":1}`)), RefusedInventoryIncomplete)
	if n := d.calls(); n != 1 {
		t.Fatalf("an incomplete inventory reached the engine: %d engine calls", n)
	}
}

// The freshness check is never satisfied by an earlier read: a request that went
// away is not checked, even when the cache still holds a current snapshot, and a
// running Molecule is refused before GitHub is read at all.
func TestSyncRenderStartIntakeFreshnessAndBusyOrder(t *testing.T) {
	f := matrixFetcher()
	h, _ := syncHandler(t, Options{LabDir: matrixLab(t), SourceRoot: "/src/checkout"}, f)
	d := installDrive(h, &fakeDrive{run: func(context.Context, molecule.Options) error { return nil }})
	if _, rows := issueRows(t, h, "/api/issues"); !rows[1].Startable {
		t.Fatalf("Issue 1 not startable: %+v", rows[1])
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	if !h.(*handler).refuseIssueNotOpen(ctx, w, 1) {
		t.Fatal("a cancelled request was allowed to start on the previous snapshot")
	}
	refusedWith(t, w, RefusedIssueNotChecked)
	// Issue 2's Molecule is running: refused as busy, with no GitHub read.
	calls := f.calls()
	w = do(t, h, gatePost("/api/runs", `{"issue":2}`))
	var v gateRefusal
	if w.Code != http.StatusConflict || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Category != "busy" {
		t.Fatalf("running Molecule: %d %s", w.Code, w.Body)
	}
	if f.calls() != calls || d.calls() != 0 {
		t.Fatalf("a busy start read GitHub (%d calls, before %d) or ran (%d)", f.calls(), calls, d.calls())
	}
}

// A snapshot read before Start intake began (as from a flight it joined) never
// satisfies the check: one more forced read follows, and if GitHub's state is
// still older than the request it is not checked. A read at or after the start
// is used.
func TestSyncRenderStartIntakeNeedsASnapshotFromAfterItBegan(t *testing.T) {
	f := matrixFetcher()
	h, clk := syncHandler(t, Options{LabDir: matrixLab(t), SourceRoot: "/src/checkout"}, f)
	d := installDrive(h, &fakeDrive{run: func(context.Context, molecule.Options) error { return nil }})
	issueRows(t, h, "/api/issues")
	// The handler's clock runs a second ahead of the cache's, so every snapshot
	// the cache stamps is older than the request.
	h.(*handler).forge.now = func() time.Time { return clk.now().Add(time.Second) }
	f.mu.Lock()
	lists := f.lists
	f.mu.Unlock()
	refusedWith(t, do(t, h, gatePost("/api/runs", `{"issue":1}`)), RefusedIssueNotChecked)
	f.mu.Lock()
	reads := f.lists - lists
	f.mu.Unlock()
	if reads != 2 || d.calls() != 0 {
		t.Fatalf("an older snapshot: %d GitHub reads (want 2), %d engine calls", reads, d.calls())
	}
	// Same clock: the forced read is from after the start and Issue 1 starts.
	h.(*handler).forge.now = clk.now
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":1}`)), http.StatusAccepted)
	if n := d.calls(); n != 1 {
		t.Fatalf("a current read did not start: %d engine calls", n)
	}
}

// refusedWith checks a Start intake refusal: 409, the category, a message and a
// bounded next step, and no local path or secret.
func refusedWith(t *testing.T, w *httptest.ResponseRecorder, category string) {
	t.Helper()
	var v gateRefusal
	if w.Code != http.StatusConflict || json.Unmarshal(w.Body.Bytes(), &v) != nil || v.Category != category || v.Error == "" || !boundedReason(v.Reason) {
		t.Fatalf("status %d, want 409 %s: %s", w.Code, category, w.Body)
	}
	noLeak(t, w.Body.String())
}

// The served page and modules render the sync without state logic of their
// own, collapse history by default with its toggle in the URL, and carry no
// innerHTML or raw gh text.
func TestSyncRenderServedSurfaces(t *testing.T) {
	js := servedJS(t)
	page := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/")).Body.String()

	// Only core.js maps a sync kind; no view reads a matrix row or kind of its own.
	h := testHandler(t, Options{LabDir: t.TempDir()})
	for _, path := range []string{"/app.js", "/js/inbox.js", "/js/work.js", "/js/issues.js", "/js/molecule.js", "/js/molecule-decision.js"} {
		body := do(t, h, authed(path)).Body.String()
		if regexp.MustCompile(`\.row\b|\.kind === '(?:not-|in-|needs-|draft-|pr-|merged|issue-|reopened|recorded)|issue-closed|stateReason|reopenedAt|headRefOid`).MatchString(body) {
			t.Errorf("%s derives a sync state of its own", path)
		}
	}
	for _, want := range []string{
		"const CLOSED_IN_PROGRESS = 'issue-closed-in-progress';",
		"const HISTORY_BUCKETS = ['merged', 'history'];",
		"export function historyFromURL() { return new URLSearchParams(location.search).get('history') === '1'; }",
		"history.pushState(history.state, '', e.currentTarget.getAttribute('href'));",
		"wholeNumber.format(n('running')) + ' running', wholeNumber.format(needsYou().length) + ' need you', wholeNumber.format(n('merged')) + ' merged'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("core.js lacks %q", want)
		}
	}
	// Every surface renders the counts from the one helper.
	for _, want := range []string{"setText($('#inbox-sync-counts'), syncCountsText());", "$('#shelf-sync-counts').textContent = syncCountsText();", "setText(c.counts, syncCountsText());"} {
		if !strings.Contains(js, want) {
			t.Errorf("a surface does not show the shared counts: %q", want)
		}
	}
	// History is collapsed by default on the Inbox and the Work grid.
	if inbox := moduleFunction(t, js, "renderInbox"); !strings.Contains(inbox, "const showHistory = historyFromURL() && history.length > 0;") || !strings.Contains(inbox, "live(m) && inboxOf(m) && !inHistory(m)") || !strings.Contains(inbox, "$('#inbox-history-list').hidden = !showHistory;") {
		t.Errorf("the Inbox does not collapse history:\n%s", inbox)
	}
	if work := moduleFunction(t, js, "matches"); !strings.Contains(work, "const collapsed = state.filter === 'all' && !historyFromURL() && inHistory(m);") || !strings.Contains(work, "return inLane && !collapsed &&") {
		t.Error("the Work grid does not collapse history")
	}
	for _, want := range []string{
		`<a id="inbox-history" class="history-row" href="?view=inbox&amp;history=1" aria-controls="inbox-history-list" aria-expanded="false" hidden></a>`,
		`<div id="inbox-history-list" class="inbox-recent" hidden></div>`,
		`<a id="work-history" class="history-row" href="?view=work&amp;history=1" aria-expanded="false" hidden></a>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	// Row 14 is a Needs-you card with the server's label and notes.
	if closed := moduleFunction(t, js, "closedView"); !strings.Contains(closed, "return { kind: 'closed', title: s.label, summary: syncNotes(s).join(' · '), action: 'Open' };") {
		t.Errorf("row 14 is not a Needs-you card: %s", closed)
	}

	// The Issues view: one primary per row, Start intake only when the server
	// says startable, the pill the server's label, filters from its buckets.
	create := moduleFunction(t, js, "createIssueRow")
	if strings.Count(create, "btn-primary") != 1 || strings.Contains(create, "btn-danger") {
		t.Errorf("an Issue row does not have exactly one primary: %s", create)
	}
	update := moduleFunction(t, js, "updateIssueRow")
	for _, want := range []string{
		"const startable = row.startable === true && s.startable === true && drives();",
		"setText(slot(node, 'label'), s.label);",
		"setText(slot(node, 'notes'), syncNotes(s).join(' · '));",
		"const id = !startable && typeof row.latestMolecule === 'string' && MOLECULE_ID.test(row.latestMolecule) ? row.latestMolecule : '';",
		"setAttr(open, 'href', moleculeHref(id));",
	} {
		if !strings.Contains(update, want) {
			t.Errorf("updateIssueRow lacks %q", want)
		}
	}
	for _, want := range []string{
		"['not-started', 'Not started', ['issues']],", "['in-progress', 'In progress', ['running']],", "['needs-you', 'Needs you', ['decision', 'blocked']],", "['draft-pr', 'Draft PR', ['draft-pr']],", "['merged', 'Merged', ['merged']],",
		"if (push) history.pushState({ view: 'issues' }, '', href);",
		"for (const name of ['label', 'milestone', 'q']) if (q[name].trim()) p.set(name, q[name].trim());",
		"if (document.activeElement !== input && input.value !== q[name]) input.value = q[name];",
		"c.refresh.addEventListener('click', () => { if (state.syncPending) return; ctx.loadSync(true); });",
		"setAttr(c.refresh, 'aria-disabled', String(state.syncPending));",
		"setText(c.refresh, state.syncPending ? 'Refreshing…' : 'Refresh');",
		"if (view.announced !== live) { view.announced = live; setText(c.live, live); }",
		"value.value = mode === 'milestone' ? q.milestone : mode === 'label' ? q.label : '';",
		// Refresh reads GitHub once and one load runs at a time.
		"if (state.syncPending || state.sessionLost) return;",
		"const res = await api('/api/sync' + (refresh ? '?refresh=1' : ''));",
		"const more = await api('/api/issues');",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the served JS lacks %q", want)
		}
	}
	patch := moduleFunction(t, js, "patchRows")
	if strings.Contains(patch, "fill(") || !strings.Contains(patch, "existing.get(key)") {
		t.Errorf("Issue rows are rebuilt instead of patched by number: %s", patch)
	}
	// Stale rows go first, and the node holding focus is never moved.
	removed, placed := strings.Index(patch, "node.remove();"), strings.Index(patch, "updateIssueRow(node, row);")
	if removed < 0 || placed < 0 || removed > placed || !strings.Contains(patch, "if (node.parentNode === container && node.contains(document.activeElement)) {") || !strings.Contains(patch, "while (at && at !== node) { const next = at.nextElementSibling; container.appendChild(at); at = next; }") {
		t.Errorf("patchRows can move the focused row: %s", patch)
	}
	if strings.Contains(js, "c.refresh.disabled") {
		t.Error("Refresh is disabled while it may hold focus")
	}
	// A row with neither Start intake nor a Molecule shows one plain reason.
	if update := moduleFunction(t, js, "updateIssueRow"); !strings.Contains(update, "why = !startable && !id ? reasonText(s) : '';") || !strings.Contains(update, "reason.hidden = !why;") {
		t.Errorf("a row can be left without a status: %s", update)
	}
	if !strings.Contains(create, "dataset: { slot: 'reason' }, hidden: true") {
		t.Error("an Issue row has no reason slot")
	}
	// The Molecule view's pill is the server's DisplayState label once served.
	if pill := moduleFunction(t, js, "pillText"); !strings.Contains(pill, "if (sync) return sync.label;") {
		t.Errorf("the Molecule pill does not show the sync label: %s", pill)
	}
	// Needs you is the server's flag once /api/sync is loaded; no surface
	// re-derives it, and the Inbox headline counts the server's buckets.
	if needs := moduleFunction(t, js, "needsYou"); !strings.Contains(needs, "if (served) return Boolean(v) && Object.prototype.hasOwnProperty.call(served, m.id) && served[m.id] === true;") || strings.Contains(needs, "closedInProgress") {
		t.Errorf("needsYou does not use the served flags: %s", needs)
	}
	if inbox := moduleFunction(t, js, "renderInbox"); !strings.Contains(inbox, "const served = syncCounts(), lane = (k, b) => served ? count(served[b]) : counts[k] || 0;") {
		t.Errorf("the Inbox headline does not count the server's buckets: %s", inbox)
	}
	issues := between(t, page, `id="view-issues"`, "</section>")
	for _, want := range []string{
		`<form id="issues-form" class="issues-tools" role="search" aria-label="Filter Issues" novalidate>`,
		`name="label" maxlength="200" autocomplete="off" spellcheck="false" placeholder="bug…"`,
		`name="milestone" maxlength="200" autocomplete="off" spellcheck="false" placeholder="v1…"`,
		`name="q" maxlength="200" autocomplete="off" spellcheck="false" placeholder="Search by number or title…"`,
		`<button type="button" id="issues-refresh" class="btn btn-ghost btn-small">Refresh</button>`,
		`<p id="issues-live" class="sr-only" role="status" aria-live="polite"></p>`,
	} {
		if !strings.Contains(issues, want) {
			t.Errorf("the Issues view lacks %s", want)
		}
	}
	if n := strings.Count(issues, "aria-live"); n != 1 {
		t.Errorf("the Issues view has %d live regions", n)
	}

	// The Molecule view: the live Issue state or the state at intake, the merge
	// with production unverified, PR checks, and nothing to do once merged.
	for name, wants := range map[string][]string{
		"renderIssueSync": {"'closed ' + dayText(s.closedAt)", "recorded + ' at intake' + (at ? ' (' + at + ')' : '')", "'checks ' + (CHECKS.includes(pr.checks) ? pr.checks : 'none')", "syncNotes(s).join(' · ')"},
		"mergeText":       {"['Merged', commit, whenText(m.at), 'production unverified']", "m.commit.slice(0, 7)"},
		"mergedOf":        {"s && s.bucket === 'merged' ? s.forgecellMerge : null"},
		"renderDecision":  {"const merged = !scope && !delivery ? mergedOf(d) : null;", "merged ? 'Nothing to do · merged on GitHub'"},
		"renderSteps":     {"const s = merged ? 'done' :"},
	} {
		body := moduleFunction(t, js, name)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}

	// No markup sink and no raw gh text anywhere a surface renders.
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "stderr", "stdout", "gh api", "gh issue"} {
		if strings.Contains(js, bad) {
			t.Errorf("the served JS has %q", bad)
		}
	}
}

// Served CSS for the Issues view: 44px coarse targets, forced colors and a
// single column on phones.
func TestSyncRenderServedCSS(t *testing.T) {
	css := servedCSS(t)
	wantDecls(t, cssRules(css), ".sync-counts", "font-variant-numeric: tabular-nums;")
	wantDecls(t, cssRules(mediaBlocks(t, css, "@media (pointer: coarse)")), ".issues-filters .seg, .issue-action, .issues-submit, #issues-refresh, .issue-number a", "min-height: 44px;")
	wantDecls(t, cssRules(mediaBlocks(t, css, "@media (max-width: 720px)")), ".issue-row", "grid-template-columns: minmax(0, 1fr);")
	wantDecls(t, cssRules(mediaBlocks(t, css, "@media (forced-colors: active)")), ".issues-table, .issue-chip", "background: Canvas;", "border: 1px solid CanvasText;")
	if bad := uncoveredOutlineResets(css); len(bad) != 0 {
		t.Errorf("outline resets without a ring: %q", bad)
	}
}
