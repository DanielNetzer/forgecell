package ui

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// estimateEpoch is a fixed time, so no estimate test reads the clock.
const estimateEpoch = int64(1_790_000_000_000)

// historyEntry is one Molecule of a fixture history: minutes of active Atom
// time; its wall time also holds a long wait for the human at a gate.
type historyEntry struct {
	id, repo, formula, lane string
	minutes                 int64
	costs                   []*float64
	noActive                bool
}

func usdPtr(v float64) *float64 { return &v }

// waitMinutes is the human wait each fixture's wall time adds to its active time.
const waitMinutes = 600

// historyInventory builds an inventory whose i-th entry finished i hours after
// the epoch, so later entries are more recent.
func historyInventory(entries []historyEntry) inventory {
	inv := inventory{facts: map[string]*moleculeFacts{}}
	for i, e := range entries {
		end := estimateEpoch + int64(i+1)*3_600_000
		s := Summary{ID: e.id, Repository: e.repo, FormulaID: e.formula, Lane: e.lane, StartedAt: "set", FinishedAt: "set"}
		f := &moleculeFacts{wallOK: true, wallStart: end - (e.minutes+waitMinutes)*60_000, wallEnd: end, activeOK: !e.noActive, activeMS: e.minutes * 60_000}
		for _, c := range e.costs {
			f.attempts = append(f.attempts, attemptFact{cost: c})
		}
		inv.list.Molecules = append(inv.list.Molecules, s)
		inv.facts[e.id] = f
	}
	return inv
}

func comparableEntries(n int, minutes func(int) int64, costs func(int) []*float64) []historyEntry {
	var out []historyEntry
	for i := 1; i <= n; i++ {
		out = append(out, historyEntry{id: fmt.Sprintf("m%02d", i), repo: "o/r", formula: "f", lane: LaneFinished, minutes: minutes(i), costs: costs(i)})
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// With enough history the estimate is the interquartile range and median of the
// comparable Molecules' active Atom time (never their wall time, which holds the
// wait for you) and reported cost, labelled with its basis and never a promise.
func TestEstimateWithEnoughHistory(t *testing.T) {
	entries := comparableEntries(12, func(i int) int64 { return int64(i) * 10 }, func(i int) []*float64 { return []*float64{usdPtr(float64(i))} })
	v := estimate(historyInventory(entries), "o/r", "f", "self")
	if !v.Shown || v.Molecules != 12 || v.Basis != "based on your last 12 finished Molecules · active time, not time waiting for you" || v.Note != estimateNote {
		t.Fatalf("estimate %+v", v)
	}
	// Active minutes 10..120: quartiles 37.5, 65 and 92.5 minutes.
	if v.Time == nil || v.Time.LowMS != 2_250_000 || v.Time.MedianMS != 3_900_000 || v.Time.HighMS != 5_550_000 {
		t.Fatalf("time %+v", v.Time)
	}
	if c := v.Cost; c == nil || !near(c.LowUSD, 3.75) || !near(c.MedianUSD, 6.5) || !near(c.HighUSD, 9.25) || c.Partial || c.Molecules != 12 || c.Coverage != "cost reported for 12 of 12 harness attempts" {
		t.Fatalf("cost %+v", v.Cost)
	}
	// More recent Decision-lane and Other-lane Molecules, far slower, change nothing.
	mixed := append([]historyEntry(nil), entries...)
	for i := range 6 {
		mixed = append(mixed, historyEntry{id: fmt.Sprintf("d%02d", i), repo: "o/r", formula: "f", lane: LaneDecision, minutes: 5000, costs: []*float64{usdPtr(99)}},
			historyEntry{id: fmt.Sprintf("o%02d", i), repo: "o/r", formula: "f", lane: LaneOther, minutes: 5000, costs: []*float64{usdPtr(99)}})
	}
	if m := estimate(historyInventory(mixed), "o/r", "f", "self"); m.Molecules != 12 || *m.Time != *v.Time || *m.Cost != *v.Cost {
		t.Fatalf("Decision or Other lane counted: %+v %+v", m, m.Time)
	}
	// Only the most recent estimateWindow comparable Molecules count: the older
	// ones here are far slower and would move the median.
	window := comparableEntries(estimateWindow+5, func(i int) int64 {
		if i <= 5 {
			return 1000
		}
		return 10
	}, func(int) []*float64 { return []*float64{usdPtr(1)} })
	if v := estimate(historyInventory(window), "o/r", "f", "self"); v.Molecules != estimateWindow || v.Time.HighMS != 600_000 || v.Basis != estimateBasis(estimateWindow) {
		t.Fatalf("window: %+v %+v", v, v.Time)
	}
}

// Fewer than minEstimateBasis comparable Molecules hide the estimate entirely:
// only the Finished lane (a published draft receipt) counts, so Molecules of
// another repository or Formula, in the Decision, Running, Blocked, Other or
// Retired lane, without recorded active time, and the Molecule itself never do.
func TestEstimateHiddenWithTooLittleHistory(t *testing.T) {
	entries := comparableEntries(minEstimateBasis-1, func(int) int64 { return 30 }, func(int) []*float64 { return []*float64{usdPtr(2)} })
	for _, e := range []historyEntry{
		{id: "other-repo", repo: "o/x", formula: "f", lane: LaneFinished, minutes: 30},
		{id: "other-formula", repo: "o/r", formula: "g", lane: LaneFinished, minutes: 30},
		{id: "decision", repo: "o/r", formula: "f", lane: LaneDecision, minutes: 30},
		{id: "blocked", repo: "o/r", formula: "f", lane: LaneBlocked, minutes: 30},
		{id: "running", repo: "o/r", formula: "f", lane: LaneRunning, minutes: 30},
		{id: "retired", repo: "o/r", formula: "f", lane: LaneRetired, minutes: 30},
		{id: "other", repo: "o/r", formula: "f", lane: LaneOther, minutes: 30},
		{id: "no-active-time", repo: "o/r", formula: "f", lane: LaneFinished, minutes: 30, noActive: true},
		{id: "self", repo: "o/r", formula: "f", lane: LaneFinished, minutes: 30},
	} {
		entries = append(entries, e)
	}
	v := estimate(historyInventory(entries), "o/r", "f", "self")
	if v.Shown || v.Molecules != minEstimateBasis-1 || v.Time != nil || v.Cost != nil || v.Basis != "" {
		t.Fatalf("estimate shown on too little history: %+v", v)
	}
	if v := estimate(inventory{facts: map[string]*moleculeFacts{}}, "o/r", "f", "self"); v.Shown || v.Molecules != 0 {
		t.Fatalf("empty history: %+v", v)
	}
	// An unknown repository or Formula id matches nothing.
	if v := estimate(historyInventory(comparableEntries(8, func(int) int64 { return 5 }, func(int) []*float64 { return nil })), "", "f", "self"); v.Shown {
		t.Fatalf("no repository: %+v", v)
	}
}

// Cost uses only attempts that reported one and says when coverage is partial;
// with fewer than minEstimateBasis Molecules reporting a cost, only the time is
// shown. A missing cost is never zero-filled.
func TestEstimatePartialCostCoverageIsLabelled(t *testing.T) {
	entries := comparableEntries(6, func(int) int64 { return 20 }, func(i int) []*float64 { return []*float64{usdPtr(float64(i)), nil} })
	v := estimate(historyInventory(entries), "o/r", "f", "self")
	if c := v.Cost; !v.Shown || c == nil || !c.Partial || c.Molecules != 6 || c.Coverage != "cost reported for 6 of 12 harness attempts" || !near(c.MedianUSD, 3.5) {
		t.Fatalf("partial cost %+v", v.Cost)
	}
	few := comparableEntries(6, func(int) int64 { return 20 }, func(i int) []*float64 {
		if i <= 3 {
			return []*float64{usdPtr(9)}
		}
		return []*float64{nil, usdPtr(math.NaN()), usdPtr(-1)}
	})
	v = estimate(historyInventory(few), "o/r", "f", "self")
	if !v.Shown || v.Time == nil || v.Cost != nil {
		t.Fatalf("cost shown from too few reports: %+v", v)
	}
	if lo, mid, hi := quartiles([]float64{4}); lo != 4 || mid != 4 || hi != 4 {
		t.Fatalf("one value: %v %v %v", lo, mid, hi)
	}
}

// numstatSeam counts diff measurements and returns stat or err.
type numstatSeam struct {
	mu    sync.Mutex
	calls [][4]string
	stat  molecule.DiffStat
	err   error
}

func (s *numstatSeam) install(h http.Handler) {
	h.(*handler).numstat = func(_ context.Context, lab, workspace, base, tree string) (molecule.DiffStat, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, [4]string{lab, workspace, base, tree})
		return s.stat, s.err
	}
}

func (s *numstatSeam) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// GET /api/molecules/{id}/stats measures the captured tree against the plan's
// base through the engine once per tree, serves only the counts, and reports a
// failed measurement as unavailable without caching it. It needs the tab
// session and serves nothing for sample data.
func TestUIStatsMeasuresTheCapturedTreeOncePerTree(t *testing.T) {
	lab, r, _ := viewFixture(t)
	tree := strings.Repeat("7", 40)
	r.Capture = &verification.CaptureResult{Tree: tree, Changes: []string{"lab/internal/ui/web/app.js"}}
	writeRecord(t, lab, r)
	h := testHandler(t, Options{LabDir: lab})
	seam := &numstatSeam{stat: molecule.DiffStat{Files: 4, Added: 412, Removed: 96, Binary: 1}}
	seam.install(h)
	path := "/api/molecules/" + r.ID + "/stats"
	for range 2 {
		v := viewOf[StatsView](t, do(t, h, authed(path)), http.StatusOK)
		if v.Diff != (DiffSizeView{State: DiffMeasured, Files: 4, Added: 412, Removed: 96, Binary: 1}) || v.Estimate.Shown {
			t.Fatalf("stats %+v", v)
		}
	}
	if seam.count() != 1 || seam.calls[0] != [4]string{lab, r.Workspace.Path, r.Workspace.BaseCommit, tree} {
		t.Fatalf("numstat calls %q", seam.calls)
	}
	// A truncated measurement says so.
	h2 := testHandler(t, Options{LabDir: lab})
	(&numstatSeam{stat: molecule.DiffStat{Files: 10000, Added: 1, Truncated: true}}).install(h2)
	if v := viewOf[StatsView](t, do(t, h2, authed(path)), http.StatusOK); !v.Diff.Truncated || v.Diff.State != DiffMeasured {
		t.Fatalf("truncated %+v", v.Diff)
	}
	// A failure is unavailable and measured again next time.
	h3 := testHandler(t, Options{LabDir: lab})
	failing := &numstatSeam{err: molecule.ErrDiffStatFailed}
	failing.install(h3)
	for range 2 {
		w := do(t, h3, authed(path))
		if v := viewOf[StatsView](t, w, http.StatusOK); v.Diff != (DiffSizeView{State: DiffUnavailable}) {
			t.Fatalf("failed measurement %+v", v.Diff)
		}
		noLeak(t, w.Body.String())
	}
	if failing.count() != 2 {
		t.Fatalf("a failed measurement was cached: %d calls", failing.count())
	}
	// Nothing captured, or no change, runs no git at all.
	r.Capture = &verification.CaptureResult{Tree: tree}
	writeRecord(t, lab, r)
	h4 := testHandler(t, Options{LabDir: lab})
	none := &numstatSeam{err: errors.New("must not run")}
	none.install(h4)
	if v := viewOf[StatsView](t, do(t, h4, authed(path)), http.StatusOK); v.Diff.State != DiffNone || none.count() != 0 {
		t.Fatalf("no change measured: %+v after %d calls", v.Diff, none.count())
	}
	// The tab session is required, and sample data has no stats.
	noTab := authed(path)
	noTab.header = map[string]string{sessionHeader: ""}
	if w := do(t, h4, noTab); w.Code != http.StatusForbidden {
		t.Fatalf("stats without the tab session: %d", w.Code)
	}
	if w := do(t, testHandler(t, Options{Demo: true}), authed("/api/molecules/demo-101/stats")); w.Code != http.StatusNotFound {
		t.Fatalf("sample stats: %d", w.Code)
	}
}

// The diff cache is bounded: the oldest size is dropped first.
func TestUIDiffCacheIsBounded(t *testing.T) {
	c := newDiffCache()
	for i := 0; i < maxDiffCache+10; i++ {
		c.put(fmt.Sprint(i), molecule.DiffStat{Files: i})
	}
	if len(c.entries) != maxDiffCache || len(c.order) != maxDiffCache {
		t.Fatalf("cache holds %d", len(c.entries))
	}
	if _, ok := c.get("0"); ok {
		t.Fatal("the oldest size was kept")
	}
	if s, ok := c.get(fmt.Sprint(maxDiffCache + 9)); !ok || s.Files != maxDiffCache+9 {
		t.Fatalf("the newest size is %+v %v", s, ok)
	}
}
