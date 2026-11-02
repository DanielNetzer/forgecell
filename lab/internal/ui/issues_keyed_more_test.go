package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// issuesKeyedCacheState is h's keyed listing cache, read under its mutex.
func issuesKeyedCacheState(h http.Handler) (listing *issuesKeyedListing, finished time.Time, failed bool, ttl time.Duration, done chan struct{}) {
	c := &h.(*handler).issuesKeyed
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listing, c.finished, c.failed, c.ttl, c.done
}

// issuesKeyedHold holds every List of fake until the returned channel closes;
// entered receives once a List is held.
func issuesKeyedHold(fake *issuesListFake) (hold, entered chan struct{}) {
	hold, entered = make(chan struct{}), make(chan struct{}, 1)
	fake.lmu.Lock()
	fake.hold, fake.entered = hold, entered
	fake.lmu.Unlock()
	return hold, entered
}

func issuesKeyedSetErr(fake *issuesListFake, err error) {
	fake.lmu.Lock()
	fake.err = err
	fake.lmu.Unlock()
}

func TestIssuesKeyedFormulaChangeDropsOldRowsAndDiscardsALateListing(t *testing.T) {
	for name, late := range map[string]error{"late listing succeeds": nil, "late listing fails": errors.New("plugin failed")} {
		t.Run(name, func(t *testing.T) {
			fake := newIssuesListFake(onePage(keyedItem("ENG-1", "open")))
			h, clk, _, _, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
			viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
			issuesKeyedDone(t, h)
			if _, rows := keyedRows(t, h); len(rows) != 1 {
				t.Fatalf("first identity rows %v", rows)
			}
			// A later listing of the first identity is held while the Formula changes.
			hold, entered := issuesKeyedHold(fake)
			clk.add(time.Minute)
			keyedRows(t, h)
			<-entered
			_, _, _, _, oldDone := issuesKeyedCacheState(h)
			h.(*handler).keyedFormula = func(string) (keyedActiveFormula, bool) {
				return keyedActiveFormula{formula: keyedTestFormula(), identity: "f\x00" + strings.Repeat("c", 64)}, true
			}
			v := viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
			if !v.Keyed || !v.Loading || v.Checked || v.Stale || len(v.Issues) != 0 || v.Reason != "" {
				t.Fatalf("view after the Formula changed %+v", v)
			}
			// The late listing ends and stores nothing for the new identity.
			issuesKeyedSetErr(fake, late)
			close(hold)
			<-oldDone
			if listing, finished, failed, ttl, _ := issuesKeyedCacheState(h); listing != nil || !finished.IsZero() || failed || ttl != 0 {
				t.Fatalf("a late listing was stored: %+v %v %v %v", listing, finished, failed, ttl)
			}
			fake.lmu.Lock()
			fake.hold, fake.err = nil, nil
			fake.lmu.Unlock()
			fake.setPages(onePage(keyedItem("ENG-2", "open")))
			v = viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
			if !v.Loading || len(v.Issues) != 0 || v.Reason != "" {
				t.Fatalf("the new identity's first poll %+v", v)
			}
			issuesKeyedDone(t, h)
			v, rows := keyedRows(t, h)
			if _, old := rows["ENG-1"]; old || len(rows) != 1 || !rows["ENG-2"].Startable || v.Stale {
				t.Fatalf("new identity view %+v", v)
			}
		})
	}
}

// issuesPanicFake is issuesListFake whose List panics while panics is set.
type issuesPanicFake struct {
	*issuesListFake
	mu     sync.Mutex
	panics bool
}

func (f *issuesPanicFake) List(ctx context.Context, selector, cursor string, limit int) (contract.TrackerPage, error) {
	f.mu.Lock()
	panics := f.panics
	f.mu.Unlock()
	if panics {
		f.lmu.Lock()
		f.lists++
		f.lmu.Unlock()
		panic("plugin said /Users/someone/secret")
	}
	return f.issuesListFake.List(ctx, selector, cursor, limit)
}

func (f *issuesPanicFake) setPanics(p bool) {
	f.mu.Lock()
	f.panics = p
	f.mu.Unlock()
}

func TestIssuesKeyedPanickingListFailsSafe(t *testing.T) {
	fake := &issuesPanicFake{issuesListFake: newIssuesListFake(onePage(keyedItem("ENG-1", "open"))), panics: true}
	h, clk, _, _, _ := keyedIssuesHandler(t, fake.issuesListFake, &keyedRuns{})
	h.(*handler).trackerFor = func(string, formula.Formula, mcpadapter.Options) (tracker.Keyed, bool, error) {
		return fake, true, nil
	}
	// A panic before any listing: an empty failed view, and the server serves on.
	viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	issuesKeyedDone(t, h)
	w := do(t, h, authed("/api/issues"))
	v := viewOf[IssuesView](t, w, http.StatusOK)
	if v.Checked || v.Loading || len(v.Issues) != 0 || v.Reason != issuesKeyedReasonFailed || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("view after a panic %+v", v)
	}
	// A good listing, then a panic: the good rows stay, stale and unstartable.
	fake.setPanics(false)
	clk.add(time.Minute)
	keyedRows(t, h)
	issuesKeyedDone(t, h)
	if v, rows := keyedRows(t, h); !v.Checked || v.Stale || !rows["ENG-1"].Startable {
		t.Fatalf("good view %+v", v)
	}
	fake.setPanics(true)
	clk.add(time.Minute)
	keyedRows(t, h)
	issuesKeyedDone(t, h)
	w = do(t, h, authed("/api/issues"))
	v = viewOf[IssuesView](t, w, http.StatusOK)
	if !v.Checked || !v.Stale || v.Reason != issuesKeyedReasonFailed || len(v.Issues) != 1 || v.Issues[0].Startable || !hasNote(v.Issues[0].State.Notes, issuesKeyedNoteStale) || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("stale view after a panic %+v", v)
	}
	if lists, _ := fake.count(); lists != 3 {
		t.Fatalf("lists %d", lists)
	}
}

func TestIssuesKeyedShutdownStopsAHeldListing(t *testing.T) {
	fake := newIssuesListFake(onePage(keyedItem("ENG-1", "open")))
	h, clk, _, _, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
	_, entered := issuesKeyedHold(fake)
	viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	<-entered
	_, _, _, _, done := issuesKeyedCacheState(h)
	// Cancelling the jobs' context ends the held listing; nothing else releases it.
	g := h.(*handler).jobs
	g.cancel()
	<-done
	g.wg.Wait()
	v := viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	if v.Loading || v.Checked || len(v.Issues) != 0 || v.Reason != issuesKeyedReasonFailed {
		t.Fatalf("view after shutdown %+v", v)
	}
	// A stopped server starts no listing, however long it is polled.
	clk.add(issuesKeyedMaxPoll)
	v = viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	if v.Loading || len(v.Issues) != 0 {
		t.Fatalf("view after shutdown and a poll %+v", v)
	}
	if lists, _ := fake.count(); lists != 1 {
		t.Fatalf("a stopped server listed %d times", lists)
	}
	if _, _, _, _, after := issuesKeyedCacheState(h); after != done {
		t.Fatal("a stopped server started a listing")
	}
}

// issuesKeyedSelectorsDone waits for h's background describe to finish.
func issuesKeyedSelectorsDone(t *testing.T, h http.Handler) {
	t.Helper()
	c := &h.(*handler).keyedSelectors
	c.mu.Lock()
	done := c.done
	c.mu.Unlock()
	if done == nil {
		t.Fatal("no background describe was started")
	}
	<-done
}

func TestIssuesKeyedLoopViewDescribesOnceInTheBackground(t *testing.T) {
	fake := newIssuesListFake(nil)
	fake.selectors = []string{"oldest", "label:", "bad\x00", ":", "oldest"}
	h, _, _, _, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
	v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK)
	if !v.Keyed || len(v.Selectors) != 0 {
		t.Fatalf("untried keyed loop view %+v", v)
	}
	issuesKeyedSelectorsDone(t, h)
	for range 2 {
		v = viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK)
		if !v.Keyed || strings.Join(v.Selectors, ",") != "oldest,label:" {
			t.Fatalf("described keyed loop view %+v", v)
		}
	}
	if _, described := fake.count(); described != 1 {
		t.Fatalf("described %d times", described)
	}

	// A failed describe serves no selectors and is not retried per poll.
	failing := newIssuesListFake(nil)
	failing.descErr = errors.New("plugin said /Users/someone/secret")
	h2, _, _, _, _ := keyedIssuesHandler(t, failing, &keyedRuns{})
	viewOf[LoopView](t, do(t, h2, authed("/api/loop")), http.StatusOK)
	issuesKeyedSelectorsDone(t, h2)
	for range 2 {
		w := do(t, h2, authed("/api/loop"))
		if v := viewOf[LoopView](t, w, http.StatusOK); !v.Keyed || len(v.Selectors) != 0 || strings.Contains(w.Body.String(), "secret") {
			t.Fatalf("failed describe view %+v", v)
		}
	}
	if _, described := failing.count(); described != 1 {
		t.Fatalf("a failed describe was retried: %d", described)
	}
}

// issuesKeyedReadOnlyHandler is a tracker-plugin Lab started without a source
// checkout.
func issuesKeyedReadOnlyHandler(t *testing.T, fake *issuesListFake) http.Handler {
	t.Helper()
	lab, _ := labFixture(t)
	h, _ := syncHandler(t, Options{LabDir: lab}, &syncFetcher{})
	installKeyedListing(h, fake, &keyedRuns{})
	return h
}

func TestIssuesKeyedReadOnlyLabRunsNoPlugin(t *testing.T) {
	fake := newIssuesListFake(onePage(keyedItem("ENG-1", "open")))
	h := issuesKeyedReadOnlyHandler(t, fake)
	for range 2 {
		v := viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
		if !v.Keyed || v.Loading || v.Checked || v.Stale || len(v.Issues) != 0 || v.Reason != issuesKeyedReasonReadOnly || v.Repository != "o/r" {
			t.Fatalf("read-only keyed view %+v", v)
		}
	}
	// Nor does its loop view describe in the background.
	if v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK); !v.Keyed || len(v.Selectors) != 0 {
		t.Fatalf("read-only keyed loop view %+v", v)
	}
	fake.lmu.Lock()
	prepared := fake.prepared
	fake.lmu.Unlock()
	if lists, described := fake.count(); lists != 0 || described != 0 || prepared != 0 {
		t.Fatalf("a read-only Lab ran the plugin: lists %d, described %d, prepared %d", lists, described, prepared)
	}
	if _, _, _, _, done := issuesKeyedCacheState(h); done != nil {
		t.Fatal("a read-only Lab started a listing")
	}
}

// issuesKeyedPinnedTracker writes a tracker manifest with the given
// needs.minPollSeconds into lab, pinned with a placeholder server under the
// test's temporary directory, and returns a Formula that pins it. Nothing runs
// the server.
func issuesKeyedPinnedTracker(t *testing.T, lab string, poll int) formula.Formula {
	t.Helper()
	script := []byte("#!/bin/sh\n")
	path := filepath.Join(plugintest.TempDir(t), "server")
	if err := os.WriteFile(path, script, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(script)
	server := pinned.File{Path: path, SHA256: hex.EncodeToString(sum[:])}
	var m map[string]any
	if err := json.Unmarshal(plugintest.Manifest(t, server, "tracker", "good"), &m); err != nil {
		t.Fatal(err)
	}
	m["needs"].(map[string]any)["minPollSeconds"] = poll
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	f := keyedTestFormula()
	at := filepath.Join(lab, filepath.FromSlash(f.Plugins[0].Manifest))
	if err := os.MkdirAll(filepath.Dir(at), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(at, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	f.Plugins[0].SHA256 = hex.EncodeToString(digest[:])
	return f
}

func TestIssuesKeyedPollReadsAPinnedManifest(t *testing.T) {
	lab := plugintest.TempDir(t)
	if got := issuesKeyedPoll(lab, issuesKeyedPinnedTracker(t, lab, 120)); got != 2*time.Minute {
		t.Fatalf("minPollSeconds 120 gave %v", got)
	}
	if got := issuesKeyedPoll(lab, issuesKeyedPinnedTracker(t, lab, 60)); got != issuesKeyedTTL {
		t.Fatalf("minPollSeconds 60 gave %v", got)
	}
	if got := issuesKeyedPoll(lab, issuesKeyedPinnedTracker(t, lab, 3600)); got != issuesKeyedMaxPoll {
		t.Fatalf("minPollSeconds 3600 gave %v", got)
	}
	// A manifest whose bytes differ from the pin gives the default.
	f := issuesKeyedPinnedTracker(t, lab, 120)
	f.Plugins[0].SHA256 = strings.Repeat("0", 64)
	if got := issuesKeyedPoll(lab, f); got != issuesKeyedTTL {
		t.Fatalf("an unpinned manifest gave %v", got)
	}
}

func TestIssuesKeyedMinPollSecondsIsTheListingTTL(t *testing.T) {
	fake := newIssuesListFake(onePage(keyedItem("ENG-1", "open")))
	h, clk, _, lab, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
	f := issuesKeyedPinnedTracker(t, lab, 120)
	// Only the Formula seam is replaced; the cache time is read from the pinned
	// manifest by the default issuesKeyedPoll.
	h.(*handler).keyedFormula = func(string) (keyedActiveFormula, bool) {
		return keyedActiveFormula{formula: f, identity: "f\x00" + f.Plugins[0].SHA256}, true
	}
	viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	issuesKeyedDone(t, h)
	if _, _, _, ttl, _ := issuesKeyedCacheState(h); ttl != 2*time.Minute {
		t.Fatalf("listing TTL %v", ttl)
	}
	clk.add(2*time.Minute - time.Second)
	if v, _ := keyedRows(t, h); v.Stale || len(v.Issues) != 1 {
		t.Fatalf("cached view %+v", v)
	}
	if lists, _ := fake.count(); lists != 1 {
		t.Fatalf("a poll within minPollSeconds listed: %d", lists)
	}
	clk.add(time.Second)
	keyedRows(t, h)
	issuesKeyedDone(t, h)
	if lists, _ := fake.count(); lists != 2 {
		t.Fatalf("no listing once minPollSeconds passed: %d", lists)
	}
}

func TestIssuesKeyedFailuresBackOffAndASuccessResets(t *testing.T) {
	fake := newIssuesListFake(onePage(keyedItem("ENG-1", "open")))
	issuesKeyedSetErr(fake, errors.New("plugin failed"))
	h, clk, _, _, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
	viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	issuesKeyedDone(t, h)
	want, lists := issuesKeyedTTL, 1
	for want < issuesKeyedMaxPoll || lists < 9 {
		if _, _, failed, ttl, _ := issuesKeyedCacheState(h); !failed || ttl != want {
			t.Fatalf("after %d failures: failed %v, TTL %v, want %v", lists, failed, ttl, want)
		}
		// No listing starts before the stored TTL has passed.
		clk.add(want - time.Second)
		keyedRows(t, h)
		if got, _ := fake.count(); got != lists {
			t.Fatalf("a poll within the backed-off TTL %v listed", want)
		}
		clk.add(time.Second)
		keyedRows(t, h)
		issuesKeyedDone(t, h)
		lists++
		if got, _ := fake.count(); got != lists {
			t.Fatalf("no listing once the TTL %v passed", want)
		}
		want = min(2*want, issuesKeyedMaxPoll)
	}
	// A good listing resets the TTL, and the next failure starts from it again.
	issuesKeyedSetErr(fake, nil)
	clk.add(issuesKeyedMaxPoll)
	keyedRows(t, h)
	issuesKeyedDone(t, h)
	if _, _, failed, ttl, _ := issuesKeyedCacheState(h); failed || ttl != issuesKeyedTTL {
		t.Fatalf("after a success: failed %v, TTL %v", failed, ttl)
	}
	issuesKeyedSetErr(fake, errors.New("plugin failed"))
	clk.add(issuesKeyedTTL)
	keyedRows(t, h)
	issuesKeyedDone(t, h)
	if _, _, failed, ttl, _ := issuesKeyedCacheState(h); !failed || ttl != issuesKeyedTTL {
		t.Fatalf("first failure after a success: failed %v, TTL %v", failed, ttl)
	}
}

func TestIssuesKeyedBackoffIsBounded(t *testing.T) {
	for _, c := range []struct {
		ttl, prev          time.Duration
		prevFailed, failed bool
		want               time.Duration
	}{
		{issuesKeyedTTL, 0, false, false, issuesKeyedTTL},
		{issuesKeyedTTL, 0, false, true, issuesKeyedTTL},
		{issuesKeyedTTL, 5 * time.Minute, true, false, issuesKeyedTTL},
		{issuesKeyedTTL, issuesKeyedTTL, true, true, 2 * issuesKeyedTTL},
		{issuesKeyedTTL, 0, true, true, issuesKeyedTTL},
		{2 * time.Minute, issuesKeyedTTL, true, true, 2 * time.Minute},
		{issuesKeyedTTL, issuesKeyedMaxPoll/2 + time.Second, true, true, issuesKeyedMaxPoll},
		{issuesKeyedTTL, issuesKeyedMaxPoll, true, true, issuesKeyedMaxPoll},
		{issuesKeyedTTL, 1 << 62, true, true, issuesKeyedMaxPoll},
	} {
		if got := issuesKeyedBackoff(c.ttl, c.prev, c.prevFailed, c.failed); got != c.want {
			t.Errorf("issuesKeyedBackoff(%v, %v, %v, %v) = %v, want %v", c.ttl, c.prev, c.prevFailed, c.failed, got, c.want)
		}
	}
}

func TestIssuesKeyedTotalCountsOnlyOpenItems(t *testing.T) {
	total := int64(40)
	fake := newIssuesListFake(map[string]contract.TrackerPage{"": {Items: []contract.TrackerItem{keyedItem("ENG-1", "open"), keyedItem("ENG-2", "closed"), keyedItem("ENG-3", "open")}, Total: &total}})
	h, _, _, _, _ := keyedIssuesHandler(t, fake, &keyedRuns{})
	viewOf[IssuesView](t, do(t, h, authed("/api/issues")), http.StatusOK)
	issuesKeyedDone(t, h)
	v, rows := keyedRows(t, h)
	if v.Truncated || v.Note != "" || v.Total != 2 || v.Shown != 2 || len(rows) != 2 {
		t.Fatalf("a whole listing with a tracker Total %+v", v)
	}
}
