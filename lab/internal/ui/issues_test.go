package ui

import (
	"context"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const syncCommit = "0123456789abcdef0123456789abcdef01234567"

// syncFetcher is a fake forgesync.Fetcher: it serves fixed GitHub state and
// records every call, so no test runs gh.
type syncFetcher struct {
	mu        sync.Mutex
	lists     int
	states    int
	repos     []string
	listing   forgesync.Listing
	issues    map[int64]forgesync.Issue
	prs       map[int64]forgesync.PR
	err       error
	gotIssues []int64
}

func (f *syncFetcher) OpenIssues(_ context.Context, repo string) (forgesync.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	f.repos = append(f.repos, repo)
	if f.err != nil {
		return forgesync.Listing{}, f.err
	}
	return f.listing, nil
}

func (f *syncFetcher) States(_ context.Context, repo string, issues, prs []int64) (forgesync.States, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states++
	f.repos = append(f.repos, repo)
	f.gotIssues = issues
	out := forgesync.States{Issues: map[int64]forgesync.Issue{}, PRs: map[int64]forgesync.PR{}}
	for _, n := range issues {
		if i, ok := f.issues[n]; ok {
			out.Issues[n] = i
		}
	}
	for _, n := range prs {
		if p, ok := f.prs[n]; ok {
			out.PRs[n] = p
		}
	}
	return out, nil
}

func (f *syncFetcher) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists + f.states
}

type syncClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *syncClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *syncClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func openSyncIssue(n int64) forgesync.Issue {
	return forgesync.Issue{Number: n, Found: true, State: forgesync.IssueOpen, Title: fmt.Sprintf("Issue %d", n), Labels: []string{"bug"}, URL: fmt.Sprintf("https://github.com/o/r/issues/%d", n)}
}

func syncPR(n int64, state string, draft bool) forgesync.PR {
	return forgesync.PR{Number: n, Found: true, State: state, IsDraft: draft, HeadRefOid: syncCommit, MergedAt: "2026-10-18T10:00:00Z", MergeCommit: strings.Repeat("a", 40), URL: fmt.Sprintf("https://github.com/o/r/pull/%d", n)}
}

// newSyncFetcher is GitHub's side of syncLab: Issues 1-6, 8 and 9 open, Issue 7
// closed as completed; pull requests 60 (draft, open), 70 (merged) and 80
// (closed without merge).
func newSyncFetcher() *syncFetcher {
	f := &syncFetcher{issues: map[int64]forgesync.Issue{}, prs: map[int64]forgesync.PR{60: syncPR(60, forgesync.PROpen, true), 70: syncPR(70, forgesync.PRMerged, false), 80: syncPR(80, forgesync.PRClosed, false)}}
	for _, n := range []int64{1, 2, 3, 4, 5, 6, 8, 9} {
		f.listing.Issues = append(f.listing.Issues, openSyncIssue(n))
	}
	f.listing.Total = len(f.listing.Issues)
	closed := openSyncIssue(7)
	closed.State, closed.StateReason = forgesync.IssueClosed, forgesync.ReasonCompleted
	f.issues[7] = closed
	return f
}

func writePublishedReceipt(t *testing.T, lab, id string, pr int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(lab, "deliveries"), 0700); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"state":"published","title":"t","acceptanceReviewAcknowledged":false,"preview":{"moleculeId":%q,"diff":"secret diff"},"commit":%q,"url":"https://github.com/o/r/pull/%d","lifecycle":{"state":"OPEN"}}`, id, syncCommit, pr)
	if err := os.WriteFile(filepath.Join(lab, "deliveries", id+".json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

// syncLab is a fixture Lab with one Molecule per ledger lane the sync maps:
// Issue 2 running, 3 at the scope gate, 4 blocked, 5 retired only, and 6, 7 and
// 8 published as pull requests 60, 70 and 80. Issues 1 and 9 have no work.
func syncLab(t *testing.T) string {
	t.Helper()
	lab, base := labFixture(t)
	os.Remove(filepath.Join(lab, "ledgers", base.ID+".json"))
	add := func(id string, n int64, status, phase string) molecule.Record {
		r := base
		r.ID, r.Status, r.Issue.Number, r.FinishedAt = id, status, n, ""
		r.Issue.URL = fmt.Sprintf("https://github.com/o/r/issues/%d", n)
		r.StartedAt = fmt.Sprintf("2026-10-24T10:%02d:00Z", n)
		r.Readiness = &readiness.State{SchemaVersion: "v1", Phase: phase}
		r.Atoms, r.HarnessAttempts = []molecule.Atom{{ID: "code", Type: "harness", Status: "active"}}, nil
		return r
	}
	for _, r := range []molecule.Record{add("mol-2-run", 2, "running", "coding"), add("mol-3-scope", 3, "waiting", "scope-waiting"), add("mol-4-blocked", 4, "blocked", "blocked")} {
		writeRecord(t, lab, r)
	}
	retired := add("mol-5-retired", 5, molecule.StatusRetired, "scope-waiting")
	retired.Retired = &molecule.Retirement{At: "2026-10-26T10:00:00Z", Reason: "stale", PriorStatus: "waiting"}
	writeRecord(t, lab, retired)
	for _, p := range []struct {
		id string
		n  int64
	}{{"mol-6-draft", 6}, {"mol-7-merged", 7}, {"mol-8-closed", 8}} {
		r := add(p.id, p.n, "completed", "review-waiting")
		r.Atoms = []molecule.Atom{{ID: "code", Type: "harness", Status: "completed"}}
		writeRecord(t, lab, r)
		writePublishedReceipt(t, lab, p.id, p.n*10)
	}
	return lab
}

func syncHandler(t *testing.T, o Options, f forgesync.Fetcher) (http.Handler, *syncClock) {
	t.Helper()
	h := testHandler(t, o)
	clk := &syncClock{t: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	h.(*handler).forge = newIssueSync(f, clk.now)
	h.(*handler).forge.repository = func(string) string { return "o/r" }
	return h, clk
}

func issueRows(t *testing.T, h http.Handler, path string) (IssuesView, map[int64]IssueRow) {
	t.Helper()
	var v IssuesView
	decode(t, do(t, h, authed(path)), &v)
	rows := map[int64]IssueRow{}
	for _, r := range v.Issues {
		rows[r.Number] = r
	}
	return v, rows
}

func TestIssuesRowsAndStatesMatchTheFixture(t *testing.T) {
	lab := syncLab(t)
	h, _ := syncHandler(t, Options{LabDir: lab}, newSyncFetcher())
	v, rows := issueRows(t, h, "/api/issues")
	if v.Sample || v.Repository != "o/r" || !v.Checked || v.Reason != "" || !v.Complete || v.Truncated || len(v.Issues) != 8 {
		t.Fatalf("view %+v", v)
	}
	want := map[int64]struct {
		kind, bucket, molecule string
		startable              bool
	}{
		1: {forgesync.KindNotStarted, forgesync.BucketIssues, "", true},
		2: {forgesync.KindInProgress, forgesync.BucketRunning, "mol-2-run", false},
		3: {forgesync.KindNeedsYou, forgesync.BucketDecision, "mol-3-scope", false},
		4: {forgesync.KindBlocked, forgesync.BucketBlocked, "mol-4-blocked", false},
		5: {forgesync.KindNotStartedRetired, forgesync.BucketIssues, "", true},
		6: {forgesync.KindDraftPR, forgesync.BucketDraftPR, "mol-6-draft", false},
		8: {forgesync.KindPRClosed, forgesync.BucketIssues, "mol-8-closed", true},
		9: {forgesync.KindNotStarted, forgesync.BucketIssues, "", true},
	}
	queue := molecule.ReadQueueInventory(lab)
	for n, w := range want {
		r, ok := rows[n]
		if !ok {
			t.Fatalf("Issue %d missing", n)
		}
		if r.State.Kind != w.kind || r.State.Bucket != w.bucket || r.LatestMolecule != w.molecule || r.Startable != w.startable || r.Title != fmt.Sprintf("Issue %d", n) || r.URL != fmt.Sprintf("https://github.com/o/r/issues/%d", n) {
			t.Errorf("Issue %d: %+v", n, r)
		}
		if w.molecule != "" && r.MoleculeLink != "/?m="+w.molecule {
			t.Errorf("Issue %d link %q", n, r.MoleculeLink)
		}
		// startable agrees with the loop's QueueInventory.Summarize, widened only
		// by row 10 (a pull request closed without merge).
		s := queue.Summarize("o/r", n)
		if r.SummaryStartable != s.Startable() || r.SummaryState != s.State {
			t.Errorf("Issue %d summary %v %q, loop %v %q", n, r.SummaryStartable, r.SummaryState, s.Startable(), s.State)
		}
		if r.State.Row != 10 && r.Startable != s.Startable() {
			t.Errorf("Issue %d startable %v, loop %v", n, r.Startable, s.Startable())
		}
	}
	if rows[2].State.Label != "In progress · code" || rows[3].State.Label != "Needs you · scope" || rows[5].State.Label != "Not started (1 retired)" {
		t.Fatalf("labels %q %q %q", rows[2].State.Label, rows[3].State.Label, rows[5].State.Label)
	}
}

func TestSyncMoleculeStatesMatchTheFixture(t *testing.T) {
	h, _ := syncHandler(t, Options{LabDir: syncLab(t)}, newSyncFetcher())
	var v SyncView
	decode(t, do(t, h, authed("/api/sync")), &v)
	if v.Sample || !v.Checked || v.Reason != "" || len(v.Molecules) != 7 {
		t.Fatalf("view %+v", v)
	}
	want := map[string]struct{ kind, bucket string }{
		"mol-2-run":     {forgesync.KindInProgress, forgesync.BucketRunning},
		"mol-3-scope":   {forgesync.KindNeedsYou, forgesync.BucketDecision},
		"mol-4-blocked": {forgesync.KindBlocked, forgesync.BucketBlocked},
		"mol-5-retired": {forgesync.KindRetired, forgesync.BucketRetired},
		"mol-6-draft":   {forgesync.KindDraftPR, forgesync.BucketDraftPR},
		"mol-7-merged":  {forgesync.KindMerged, forgesync.BucketMerged},
		"mol-8-closed":  {forgesync.KindPRClosed, forgesync.BucketIssues},
	}
	for id, w := range want {
		d := v.Molecules[id]
		if d.Kind != w.kind || d.Bucket != w.bucket || d.MoleculeID != id || d.Startable {
			t.Errorf("%s: %+v", id, d)
		}
	}
	m := v.Molecules["mol-7-merged"].ForgecellMerge
	if m == nil || m.PR != 70 || m.Commit != strings.Repeat("a", 40) || m.URL != "https://github.com/o/r/pull/70" {
		t.Fatalf("merge %+v", m)
	}
	if v.Counts[forgesync.BucketMerged] != 1 || v.Counts[forgesync.BucketRetired] != 0 || v.Counts[forgesync.BucketBlocked] != 1 {
		t.Fatalf("counts %v", v.Counts)
	}
}

// The repository is always the active Formula's; no query parameter selects it.
func TestIssuesRepositoryCannotBeOverridden(t *testing.T) {
	f := newSyncFetcher()
	h, _ := syncHandler(t, Options{LabDir: syncLab(t)}, f)
	for _, path := range []string{"/api/issues?repo=evil/x&repository=evil/x&owner=evil&name=x", "/api/sync?repo=evil/x&repository=evil/x", "/api/issues?refresh=1&repo=evil/x"} {
		w := do(t, h, authed(path))
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "evil") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	for _, repo := range f.repos {
		if repo != "o/r" {
			t.Fatalf("fetched %q", repo)
		}
	}
	// No valid repository: a fixed reason, and GitHub is never read.
	f = newSyncFetcher()
	h, _ = syncHandler(t, Options{LabDir: syncLab(t)}, f)
	h.(*handler).forge.repository = func(string) string { return "../x" }
	v, _ := issueRows(t, h, "/api/issues")
	if v.Reason != forgesync.ReasonRepository || v.Checked || len(v.Issues) != 0 || f.calls() != 0 {
		t.Fatalf("invalid repository: %+v calls %d", v, f.calls())
	}
	var s SyncView
	decode(t, do(t, h, authed("/api/sync")), &s)
	if s.Reason != forgesync.ReasonRepository || s.Checked || f.calls() != 0 {
		t.Fatalf("invalid repository sync: %+v", s)
	}
}

// Both routes share one cache: GitHub is read at most once per TTL, and
// ?refresh=1 bypasses it.
func TestIssuesCacheIsHonouredAndRefreshBypassesIt(t *testing.T) {
	f := newSyncFetcher()
	h, clk := syncHandler(t, Options{LabDir: syncLab(t)}, f)
	issueRows(t, h, "/api/issues")
	if f.lists != 1 || f.states != 1 {
		t.Fatalf("first: lists %d states %d", f.lists, f.states)
	}
	// Open Issues come from the listing; only the closed one is read by number.
	if len(f.gotIssues) != 1 || f.gotIssues[0] != 7 {
		t.Fatalf("states asked for %v", f.gotIssues)
	}
	clk.add(30 * time.Second)
	v, _ := issueRows(t, h, "/api/issues")
	decode(t, do(t, h, authed("/api/sync")), &SyncView{})
	if f.lists != 1 || v.AgeSeconds != 30 || v.FetchedAt != "2026-10-27T09:00:00Z" {
		t.Fatalf("within the TTL: lists %d age %d at %s", f.lists, v.AgeSeconds, v.FetchedAt)
	}
	if v, _ = issueRows(t, h, "/api/issues?refresh=1"); f.lists != 2 || v.AgeSeconds != 0 {
		t.Fatalf("refresh: lists %d age %d", f.lists, v.AgeSeconds)
	}
	clk.add(forgesync.TTL)
	if issueRows(t, h, "/api/sync"); f.lists != 3 {
		t.Fatalf("after the TTL: lists %d", f.lists)
	}
}

// A gh failure gives one fixed reason and never gh's output, local paths or
// tokens; without a snapshot every Molecule keeps its ledger lane, not checked.
func TestIssuesGHFailureIsRedactedAndFallsBackToTheLedger(t *testing.T) {
	lab := syncLab(t)
	leak := "gh auth login required; config at " + lab + "/gh/hosts.yml token ghp_SECRETSECRET HOME=" + os.Getenv("HOME")
	gh := &forgesync.GH{Run: func(context.Context, process.Options) process.Result {
		return process.Result{Code: 4, Reconciled: true, Stderr: leak, Stdout: leak, Error: "exit status 4"}
	}}
	h, _ := syncHandler(t, Options{LabDir: lab}, gh)
	for _, path := range []string{"/api/issues", "/api/sync"} {
		w := do(t, h, authed(path))
		body := w.Body.String()
		if w.Code != http.StatusOK || strings.Contains(body, lab) || strings.Contains(body, "ghp_") || strings.Contains(body, "hosts.yml") || strings.Contains(body, "exit status") || !strings.Contains(body, forgesync.ReasonNotAuthenticated) {
			t.Fatalf("%s leaks or lacks the reason: %s", path, body)
		}
	}
	v, _ := issueRows(t, h, "/api/issues")
	if v.Checked || v.Stale || len(v.Issues) != 0 {
		t.Fatalf("issues without a snapshot: %+v", v)
	}
	var s SyncView
	decode(t, do(t, h, authed("/api/sync")), &s)
	d := s.Molecules["mol-6-draft"]
	if s.Checked || d.Checked || d.Kind != forgesync.KindNotChecked || d.Label != "Draft published" || d.Bucket != LaneFinished {
		t.Fatalf("ledger fallback %+v", d)
	}
	if b := s.Molecules["mol-4-blocked"]; b.Bucket != LaneBlocked || b.Checked {
		t.Fatalf("blocked fallback %+v", b)
	}
}

// After a good sync, a failure keeps the last snapshot with its age.
func TestIssuesFailureKeepsTheStaleSnapshot(t *testing.T) {
	f := newSyncFetcher()
	h, clk := syncHandler(t, Options{LabDir: syncLab(t)}, f)
	issueRows(t, h, "/api/issues")
	clk.add(4 * time.Minute)
	f.mu.Lock()
	f.err = &forgesync.Failure{Reason: forgesync.ReasonOffline}
	f.mu.Unlock()
	v, rows := issueRows(t, h, "/api/issues")
	if !v.Checked || !v.Stale || v.Reason != forgesync.ReasonOffline || v.AgeSeconds != 240 || rows[6].State.Kind != forgesync.KindDraftPR {
		t.Fatalf("stale %+v", v)
	}
	// Stale state fails closed: no row is startable, each says why.
	for n, r := range rows {
		if r.Startable || r.State.Startable || !slices.Contains(r.State.Notes, forgesync.NoteStale) {
			t.Fatalf("Issue %d on stale state: startable %v notes %v", n, r.Startable, r.State.Notes)
		}
	}
	var s SyncView
	decode(t, do(t, h, authed("/api/sync")), &s)
	if !s.Stale {
		t.Fatalf("sync not stale: %+v", s.Freshness)
	}
	for id, d := range s.Molecules {
		if d.Startable || !slices.Contains(d.Notes, forgesync.NoteStale) {
			t.Fatalf("%s on stale state: %+v", id, d)
		}
	}
}

// A snapshot older than twice the TTL is stale even when no sync failed.
func TestSyncFreshnessIsStaleAfterTwiceTheTTL(t *testing.T) {
	snap := &forgesync.Snapshot{FetchedAt: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)}
	if f := freshness(forgesync.Result{Snapshot: snap, Age: 2 * forgesync.TTL}); f.Stale {
		t.Fatalf("at 2x TTL: %+v", f)
	}
	if f := freshness(forgesync.Result{Snapshot: snap, Age: 2*forgesync.TTL + time.Second}); !f.Stale || !f.Checked {
		t.Fatalf("past 2x TTL: %+v", f)
	}
	if f := freshness(forgesync.Result{Reason: forgesync.ReasonOffline}); f.Stale || f.Checked {
		t.Fatalf("no snapshot: %+v", f)
	}
}

func TestIssuesTruncationIsShown(t *testing.T) {
	f := newSyncFetcher()
	f.listing.Total, f.listing.Truncated = 742, true
	h, _ := syncHandler(t, Options{LabDir: syncLab(t)}, f)
	v, _ := issueRows(t, h, "/api/issues")
	if !v.Truncated || v.Total != 742 || v.Shown != 8 || v.Note != "showing 8 of 742" {
		t.Fatalf("truncation %+v", v)
	}
}

// The sync routes keep every read guard: Host, Origin, Fetch Metadata, cookie,
// tab header and GET only. A refused request never reads GitHub.
func TestIssuesAndSyncRoutesKeepTheReadGuards(t *testing.T) {
	f := newSyncFetcher()
	h, _ := syncHandler(t, Options{LabDir: syncLab(t)}, f)
	for _, path := range []string{"/api/issues", "/api/sync", "/api/issues?refresh=1"} {
		forged := authed(path)
		forged.origin = "http://evil.example"
		crossSite := authed(path)
		crossSite.site = "cross-site"
		noTab := authed(path)
		noTab.header = map[string]string{sessionHeader: ""}
		wrongHost := authed(path)
		wrongHost.host = "localhost:4321"
		for name, r := range map[string]request{"forged Origin": forged, "cross-site": crossSite, "no cookie": {path: path}, "no tab header": noTab, "wrong Host": wrongHost} {
			if w := do(t, h, r); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d", name, path, w.Code)
			}
		}
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			r := authed(path)
			r.method, r.origin = method, "http://"+testHost
			if w := do(t, h, r); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
				t.Errorf("%s %s: %d Allow %q", method, path, w.Code, w.Header().Get("Allow"))
			}
		}
	}
	if f.calls() != 0 {
		t.Fatalf("a refused request read GitHub %d times", f.calls())
	}
}

// Demo mode serves labelled sample sync data and never calls the fetcher.
func TestIssuesDemoNeverCallsTheFetcher(t *testing.T) {
	f := newSyncFetcher()
	h, _ := syncHandler(t, Options{Demo: true}, f)
	_, rows := issueRows(t, h, "/api/issues?refresh=1")
	var v IssuesView
	decode(t, do(t, h, authed("/api/issues")), &v)
	if !v.Sample || !v.Checked || len(v.Issues) == 0 || !rows[901].Startable || rows[902].Startable || !rows[910].Startable || !rows[911].Startable || !rows[912].Locked {
		t.Fatalf("demo issues %+v", v)
	}
	var s SyncView
	decode(t, do(t, h, authed("/api/sync?refresh=1")), &s)
	if !s.Sample || len(s.Molecules) != demoCount || s.Molecules["demo-101"].Label == "" {
		t.Fatalf("demo sync: sample %v %d molecules", s.Sample, len(s.Molecules))
	}
	if f.calls() != 0 {
		t.Fatalf("demo called the fetcher %d times", f.calls())
	}
}
