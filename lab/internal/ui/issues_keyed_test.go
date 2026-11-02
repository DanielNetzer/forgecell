package ui

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// issuesListFake is a fake tracker plugin that also lists: pages by cursor,
// every List counted, and an optional hold so a test can keep a listing
// running. No plugin process starts and nothing reaches the network.
type issuesListFake struct {
	*keyedFake
	lmu       sync.Mutex
	pages     map[string]contract.TrackerPage
	err       error
	lists     int
	selects   []string
	limits    []int
	selectors []string
	descErr   error
	described int
	prepared  int
	observed  int
	// hold, when set, holds every List until it is closed; entered receives
	// once a List is held.
	hold    chan struct{}
	entered chan struct{}
}

func (f *issuesListFake) List(ctx context.Context, selector, cursor string, limit int) (contract.TrackerPage, error) {
	f.lmu.Lock()
	f.lists++
	f.selects, f.limits = append(f.selects, selector), append(f.limits, limit)
	page, ok := f.pages[cursor]
	err, hold, entered := f.err, f.hold, f.entered
	f.lmu.Unlock()
	if hold != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-hold:
		case <-ctx.Done():
			return contract.TrackerPage{}, ctx.Err()
		}
	}
	if err != nil {
		return contract.TrackerPage{}, err
	}
	if !ok {
		return contract.TrackerPage{}, errors.New("no such page")
	}
	return page, nil
}

func (f *issuesListFake) Describe(context.Context) (contract.TrackerDescription, error) {
	f.lmu.Lock()
	defer f.lmu.Unlock()
	f.described++
	if f.descErr != nil {
		return contract.TrackerDescription{}, f.descErr
	}
	return contract.TrackerDescription{DisplayName: "Fake tracker", KeyPattern: `^ENG-[0-9]+$`, States: []string{"open", "closed"}, Selectors: f.selectors}, nil
}

func (f *issuesListFake) count() (lists, described int) {
	f.lmu.Lock()
	defer f.lmu.Unlock()
	return f.lists, f.described
}

func (f *issuesListFake) setPages(pages map[string]contract.TrackerPage) {
	f.lmu.Lock()
	f.pages = pages
	f.lmu.Unlock()
}

// installKeyedListing makes h a tracker-plugin Lab served by fake.
func installKeyedListing(h http.Handler, fake *issuesListFake, runs *keyedRuns) {
	installKeyed(h, fake.keyedFake, runs)
	h.(*handler).trackerFor = func(lab string, f formula.Formula, o mcpadapter.Options) (tracker.Keyed, bool, error) {
		fake.lmu.Lock()
		fake.prepared++
		if o.Observer != nil {
			fake.observed++
		}
		fake.lmu.Unlock()
		return fake, true, nil
	}
}

func newIssuesListFake(pages map[string]contract.TrackerPage) *issuesListFake {
	return &issuesListFake{keyedFake: &keyedFake{pattern: `^ENG-[0-9]+$`}, pages: pages, selectors: []string{"oldest", "label:"}}
}

// issuesKeyedDone waits for h's latest background listing to finish.
func issuesKeyedDone(t *testing.T, h http.Handler) {
	t.Helper()
	c := &h.(*handler).issuesKeyed
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done == nil {
		t.Fatal("no keyed listing was started")
	}
	<-done
}

func keyedItem(key, state string) contract.TrackerItem {
	return contract.TrackerItem{Key: key, Title: "Fix " + key, Labels: []string{"bug"}, UpdatedAt: "2026-11-01T10:00:00Z", State: state}
}

func onePage(items ...contract.TrackerItem) map[string]contract.TrackerPage {
	return map[string]contract.TrackerPage{"": {Items: items}}
}

func keyedRows(t *testing.T, h http.Handler) (IssuesView, map[string]IssueRow) {
	t.Helper()
	v := viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	rows := map[string]IssueRow{}
	for _, r := range v.Issues {
		rows[r.Key] = r
	}
	return v, rows
}

// keyedIssuesHandler is a tracker-plugin Lab that can drive, its sync clock
// and a GitHub fetcher that counts every call, so a test can prove the keyed
// view never reads GitHub.
func keyedIssuesHandler(t *testing.T, fake *issuesListFake, runs *keyedRuns) (http.Handler, *syncClock, *syncFetcher, string, molecule.Record) {
	t.Helper()
	lab, base := labFixture(t)
	gh := &syncFetcher{}
	h, clk := syncHandler(t, Options{LabDir: lab, SourceRoot: "/src/checkout", Resources: guardWith(64 << 30)}, gh)
	installDrive(h, &fakeDrive{})
	installKeyedListing(h, fake, runs)
	return h, clk, gh, lab, base
}

func TestIssuesKeyedListsRowsWithStartabilityAndNeverReadsGitHub(t *testing.T) {
	milestone := "v1"
	hostile := contract.TrackerItem{Key: "ENG-1", Title: "Fix‮ one\x00 <b>", Labels: []string{"bug", "", "ui "}, Milestone: &milestone, UpdatedAt: "2026-11-01T10:00:00Z", State: "open"}
	fake := newIssuesListFake(onePage(hostile, keyedItem("ENG-2", "open"), keyedItem("ENG-3", "closed"), keyedItem("bad\nkey", "open"), keyedItem("ENG-1", "open")))
	runs := &keyedRuns{}
	h, _, gh, lab, base := keyedIssuesHandler(t, fake, runs)
	writeRecord(t, lab, keyedRecord(base, "mol-eng-2-run", "ENG-2", now(), "running", ""))

	// The first poll never waits: an empty view marked loading starts the listing.
	v := viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	if !v.Keyed || !v.Loading || v.Checked || len(v.Issues) != 0 || v.Repository != "o/r" {
		t.Fatalf("first keyed poll %+v", v)
	}
	issuesKeyedDone(t, h)
	v, rows := keyedRows(t, h)
	if v.Loading || !v.Checked || v.Stale || !v.Complete || v.Truncated || v.Total != 2 || v.Shown != 2 || len(rows) != 2 {
		t.Fatalf("keyed view %+v", v)
	}
	one, two := rows["ENG-1"], rows["ENG-2"]
	if one.Number != 0 || !one.Startable || !one.SummaryStartable || one.SummaryState != "untouched" || one.URL != "" || one.Milestone != "v1" || one.UpdatedAt != "2026-11-01T10:00:00Z" {
		t.Fatalf("untouched row %+v", one)
	}
	if one.Title != "Fix one <b>" || strings.Join(one.Labels, ",") != "bug,ui" {
		t.Fatalf("tracker text is not bounded display text: %q %q", one.Title, one.Labels)
	}
	if two.Startable || two.SummaryStartable || two.LatestMolecule != "mol-eng-2-run" || two.MoleculeLink != "/?m=mol-eng-2-run" || two.State.Bucket != forgesync.BucketRunning {
		t.Fatalf("running row %+v", two)
	}
	if lists, _ := fake.count(); lists != 1 || fake.selects[0] != "oldest" || fake.limits[0] != 100 || fake.observed != 0 {
		t.Fatalf("lists %d, selects %q, limits %v, observed %d", lists, fake.selects, fake.limits, fake.observed)
	}
	if gh.calls() != 0 {
		t.Fatalf("the keyed view read GitHub %d times", gh.calls())
	}
	// Starting a key with an active Molecule is refused, and nothing starts.
	refusalOf(t, do(t, h, gatePost("/api/runs", `{"key":"ENG-2"}`)), http.StatusConflict, "busy")
	if runs.calls() != 0 {
		t.Fatalf("a refused keyed start reached the engine %d times", runs.calls())
	}
}

func TestIssuesKeyedCacheBoundsPluginCalls(t *testing.T) {
	fake := newIssuesListFake(onePage(keyedItem("ENG-1", "open")))
	h, clk, _, _, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
	viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	issuesKeyedDone(t, h)
	// Polls within the TTL start no plugin call.
	for range 3 {
		clk.add(19 * time.Second)
		if v, _ := keyedRows(t, h); !v.Checked || v.Stale || len(v.Issues) != 1 {
			t.Fatalf("cached view %+v", v)
		}
	}
	if lists, _ := fake.count(); lists != 1 {
		t.Fatalf("polls within the TTL listed %d times", lists)
	}

	// Past the TTL a slow listing starts; every poll meanwhile gets the cached
	// view at once, marked stale, and starts nothing more.
	pages := map[string]contract.TrackerPage{}
	for i, cursor := range []string{"", "c1", "c2", "c3", "c4"} {
		next := "c" + string(rune('1'+i))
		pages[cursor] = contract.TrackerPage{Items: []contract.TrackerItem{keyedItem("ENG-"+string(rune('1'+i))+"0", "open")}, NextCursor: &next}
	}
	fake.setPages(pages)
	hold, entered := make(chan struct{}), make(chan struct{}, 1)
	fake.lmu.Lock()
	fake.hold, fake.entered = hold, entered
	fake.lmu.Unlock()
	clk.add(4 * time.Second)
	for i := range 2 {
		v, rows := keyedRows(t, h)
		if !v.Checked || !v.Stale || v.Loading || len(rows) != 1 || rows["ENG-1"].Startable || !hasNote(rows["ENG-1"].State.Notes, issuesKeyedNoteStale) {
			t.Fatalf("poll %d during a listing %+v", i, v)
		}
		if i == 0 {
			<-entered
		}
	}
	if lists, _ := fake.count(); lists != 2 {
		t.Fatalf("polls during a listing started %d listings", lists-1)
	}
	close(hold)
	issuesKeyedDone(t, h)
	// Pages left past the bound make the view truncated, never complete.
	v, rows := keyedRows(t, h)
	if !v.Truncated || v.Stale || v.Shown != 5 || len(rows) != 5 || v.Note != "showing the first 5" {
		t.Fatalf("bounded listing %+v", v)
	}
	if lists, _ := fake.count(); lists != 1+issuesKeyedMaxPages {
		t.Fatalf("a listing read %d pages, want %d", lists-1, issuesKeyedMaxPages)
	}

	// A longer needs.minPollSeconds lengthens the cache time.
	h.(*handler).issuesKeyedPoll = func(string, formula.Formula) time.Duration { return 2 * time.Minute }
	fake.lmu.Lock()
	fake.hold = nil
	fake.lmu.Unlock()
	clk.add(time.Minute)
	keyedRows(t, h)
	issuesKeyedDone(t, h)
	listed, _ := fake.count()
	clk.add(time.Minute + 30*time.Second)
	keyedRows(t, h)
	if lists, _ := fake.count(); lists != listed {
		t.Fatalf("a poll within needs.minPollSeconds listed again: %d, %d", lists, listed)
	}
	clk.add(time.Minute)
	keyedRows(t, h)
	issuesKeyedDone(t, h)
	if lists, _ := fake.count(); lists == listed {
		t.Fatal("no listing past needs.minPollSeconds")
	}
}

func hasNote(notes []string, want string) bool {
	for _, n := range notes {
		if n == want {
			return true
		}
	}
	return false
}

func TestIssuesKeyedFailedListingFailsSafeAndIsNotRetriedPerPoll(t *testing.T) {
	stuck := ""
	for name, pages := range map[string]map[string]contract.TrackerPage{
		"cursor that does not advance": {"": {Items: []contract.TrackerItem{keyedItem("ENG-1", "open")}, NextCursor: &stuck}},
		"oversized page":               {"": {Items: make([]contract.TrackerItem, issuesKeyedPageSize+1)}},
		"missing page":                 {},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newIssuesListFake(pages)
			h, clk, _, _, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
			viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
			issuesKeyedDone(t, h)
			clk.add(30 * time.Second)
			w := do(t, h, authed("/api/issues"))
			v := viewOf[IssuesView](t, w, http.StatusOK)
			if v.Checked || v.Loading || len(v.Issues) != 0 || v.Reason != issuesKeyedReasonFailed || strings.Contains(w.Body.String(), "no such page") {
				t.Fatalf("failed listing %+v", v)
			}
			if lists, _ := fake.count(); lists != 1 {
				t.Fatalf("lists %d", lists)
			}
		})
	}
	// A failure after a good listing keeps the good rows, stale and unstartable.
	fake := newIssuesListFake(onePage(keyedItem("ENG-1", "open")))
	h, clk, _, _, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
	viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	issuesKeyedDone(t, h)
	fake.lmu.Lock()
	fake.err = errors.New("plugin said /Users/someone/secret")
	fake.lmu.Unlock()
	clk.add(time.Minute)
	keyedRows(t, h)
	issuesKeyedDone(t, h)
	w := do(t, h, authed("/api/issues"))
	v := viewOf[IssuesView](t, w, http.StatusOK)
	if !v.Checked || !v.Stale || v.Reason != issuesKeyedReasonFailed || len(v.Issues) != 1 || v.Issues[0].Startable || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("stale keyed view after a failure %+v", v)
	}
}

func TestIssuesKeyedPollReadsMinPollSecondsOrDefaults(t *testing.T) {
	// No manifest can be loaded: the default applies.
	if got := issuesKeyedPoll(t.TempDir(), keyedTestFormula()); got != issuesKeyedTTL {
		t.Fatalf("poll %v", got)
	}
	if got := issuesKeyedPoll(t.TempDir(), formula.Formula{}); got != issuesKeyedTTL {
		t.Fatalf("GitHub Formula poll %v", got)
	}
}

func TestIssuesKeyedGitHubSyncStopsForAPluginLab(t *testing.T) {
	fake := newIssuesListFake(onePage(keyedItem("ENG-1", "open")))
	h, _, gh, _, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
	if repo := h.(*handler).syncRepository(); repo != "" {
		t.Fatalf("syncRepository %q on a plugin Lab", repo)
	}
	s := viewOf[SyncView](t, do(t, h, authed("/api/sync?refresh=1")), http.StatusOK)
	if s.Repository != "" || s.Reason != forgesync.ReasonRepository || s.Checked {
		t.Fatalf("sync on a plugin Lab %+v", s)
	}
	r := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK)
	if r.Available || r.Reason != closedNoRepository {
		t.Fatalf("retire-closed on a plugin Lab %+v", r)
	}
	if gh.calls() != 0 {
		t.Fatalf("a plugin Lab read GitHub %d times", gh.calls())
	}
}

// On a GitHub Lab a keyed ledger is never GitHub Issue 0: the sync never asks
// for it, never checks it and never lists it as closed.
func TestIssuesKeyedMoleculeIsNeverIssueZero(t *testing.T) {
	lab, base := labFixture(t)
	writeRecord(t, lab, keyedRecord(base, "mol-eng-4-a", "ENG-4", "2026-10-24T11:00:00Z", "blocked", ""))
	closed := forgesync.Issue{Number: 0, Found: true, State: forgesync.IssueClosed}
	seven := openSyncIssue(7)
	seven.State = forgesync.IssueClosed
	gh := &syncFetcher{issues: map[int64]forgesync.Issue{0: closed, 7: seven}, prs: map[int64]forgesync.PR{}, listing: forgesync.Listing{Issues: []forgesync.Issue{}}}
	h, _ := syncHandler(t, Options{LabDir: lab}, gh)
	s := viewOf[SyncView](t, do(t, h, authed("/api/sync?refresh=1")), http.StatusOK)
	for _, n := range gh.gotIssues {
		if n < 1 {
			t.Fatalf("the sync asked GitHub for Issue %d: %v", n, gh.gotIssues)
		}
	}
	if d, ok := s.Molecules["mol-eng-4-a"]; !ok || d.Checked {
		t.Fatalf("keyed Molecule state %+v", d)
	}
	r := viewOf[RetireClosedView](t, do(t, h, authed("/api/retire-closed")), http.StatusOK)
	for _, m := range r.Molecules {
		if m.ID == "mol-eng-4-a" || m.Issue == 0 {
			t.Fatalf("a keyed Molecule is listed as a closed GitHub Issue: %+v", r.Molecules)
		}
	}
}

func TestIssuesKeyedServedJSKeysRowsByKey(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	issues := do(t, h, authed("/js/issues.js")).Body.String()
	wantAll(t, "issues.js", issues,
		"function rowKey(row) { return row.key || String(row.number); }",
		"const keys = new Set(items.map((row) => rowKey(row)));",
		"const key = rowKey(row);",
		"let node = existing.get(key);",
		"issueLabel(row)",
		"(keyed ? TRACKER_URL : ISSUE_URL).test(row.url)",
		"start.dataset.key ? startKeyedIntake(start.dataset.key) : startIntake(Number(start.dataset.issue))",
		"$('#drive-run [data-drive=\"key\"]')",
		"r.number > 0) || (typeof r.key === 'string' && r.key !== '')",
		// The numeric texts are unchanged.
		"'#' + row.number",
		"'Can’t start until GitHub state is checked'")
	loop := do(t, h, authed("/js/loop.js")).Body.String()
	wantAll(t, "loop.js", loop,
		"if (loop) showKeyedModes(loop);",
		"loop && Array.isArray(loop.selectors)",
		"startLoop(s.endsWith(':') ? s + value : s, c.max.value);",
		"notRecorded(issueLabel({ key: i.key, number: i.issue }))",
		"'issue ' + issueLabel({ key: i.key }) + ' · planning'",
		"'issue #' + notRecorded(i.issue) + ' · planning'")
	markup := regexp.MustCompile(`innerHTML|insertAdjacentHTML|outerHTML`)
	for _, path := range servedModules {
		js := do(t, h, authed(path)).Body.String()
		if n := strings.Count(js, "\n"); n > 600 {
			t.Errorf("%s has %d lines, over 600", path, n)
		}
		if markup.MatchString(js) {
			t.Errorf("%s renders markup", path)
		}
	}
}
