package ui

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func analyticsUsage(model string, cost float64) *harness.Usage {
	input := int64(100)
	return &harness.Usage{Total: harness.TokenCategories{Input: &input}, EffectiveModels: []string{model}, TotalCostUSD: &cost, CostBasis: "list"}
}

// analyticsRecord is a minimal valid ledger of this Lab with fixed timestamps.
func analyticsRecord(lab, id string, number int64, status, started, finished string) molecule.Record {
	return molecule.Record{SchemaVersion: "v0", Kind: "molecule", ID: id, Status: status, FormulaID: "f", Mode: "ticket", StartedAt: started, FinishedAt: finished, LabDir: lab,
		Issue:     molecule.Issue{Repo: "o/r", Number: number, Title: "Issue " + id},
		Workspace: molecule.Workspace{Path: filepath.Join(lab, "workspaces", id), Branch: "forgecell/" + id, Repo: "o/r"}, Notes: []string{}}
}

// analyticsLab writes a fixture Lab of three Molecules plus one unreadable ledger:
//
//   - mol-1 (Draft published): 1h wall-clock with parallel Atoms whose union is 40m
//     (their sum is 50m); a priced analysis attempt, a transient coding attempt with
//     no usage, its priced resume, and a Codex review with no usage;
//   - mol-2 (blocked): unfinished, an Atom with a malformed timestamp, and an
//     unpinned coding attempt with no usage;
//   - mol-3 (needs a decision): a ledger written before #149 with no usage at all,
//     overlapping mol-1 in time.
//
// Every ledger is aged past racyWindow so its ETag is stable.
func analyticsLab(t *testing.T) string {
	t.Helper()
	lab := t.TempDir()
	m1 := analyticsRecord(lab, "mol-1-a", 1, "completed", "2026-10-24T10:00:00Z", "2026-10-24T11:00:00Z")
	m1.Atoms = []molecule.Atom{
		{ID: "intake", Type: "intake", Status: "done", StartedAt: "2026-10-24T10:00:00Z", FinishedAt: "2026-10-24T10:20:00Z"},
		{ID: "code", Type: "harness", Status: "done", StartedAt: "2026-10-24T10:10:00Z", FinishedAt: "2026-10-24T10:30:00Z", Provenance: &molecule.AtomProvenance{Actions: []molecule.AtomAction{
			{AttemptID: "h1", PlannedAction: "edit", StartedAt: "2026-10-24T10:10:00Z", ExecutedBinding: "claude-code"},
			{AttemptID: "h2", PlannedAction: "edit", StartedAt: "2026-10-24T10:15:00Z", ExecutedBinding: "claude-code"},
		}}},
		{ID: "review", Type: "review", Status: "done", StartedAt: "2026-10-24T10:40:00Z", FinishedAt: "2026-10-24T10:50:00Z"},
		{ID: "deliver", Type: "gate", Status: "pending"},
	}
	m1.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "2026-10-24T10:00:00Z", Status: "completed", Usage: analyticsUsage("claude-opus-5-5", 0.10)}}
	m1.HarnessAttempts = []molecule.HarnessAttempt{
		{ID: "h1", PlanDigest: "d", Model: "claude-opus-5-5", Classification: harness.TransientProviderError},
		{ID: "h2", PlanDigest: "d", Model: "claude-opus-5-5", Resumed: "resumed after transient-provider-error (1 of 2)", Usage: analyticsUsage("claude-opus-5-5", 0.20)},
	}
	m1.ReviewAttempts = []molecule.ReviewAttempt{{Ordinal: 0, HarnessAttemptID: "h2", Binding: "codex", StartedAt: "2026-10-24T10:40:00Z", Status: "no-high-findings"}}
	m2 := analyticsRecord(lab, "mol-2-b", 2, "blocked", "2026-10-24T12:00:00Z", "")
	m2.Atoms = []molecule.Atom{{ID: "code", Type: "harness", Status: "failed", StartedAt: "not a time", FinishedAt: "2026-10-24T12:10:00Z"}}
	m2.HarnessAttempts = []molecule.HarnessAttempt{{ID: "h3", PlanDigest: "d", Model: "model unpinned"}}
	m3 := analyticsRecord(lab, "mol-3-c", 3, "waiting", "2026-10-24T10:30:00Z", "2026-10-24T11:30:00Z")
	m3.Readiness = &readiness.State{SchemaVersion: "v1", Phase: "review-waiting"}
	m3.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "2026-10-24T10:30:00Z", Status: "completed"}}
	m3.HarnessAttempts = []molecule.HarnessAttempt{{ID: "h4", PlanDigest: "d"}}
	for _, r := range []molecule.Record{m1, m2, m3} {
		writeRecord(t, lab, r)
		age(t, filepath.Join(lab, "ledgers", r.ID+".json"), time.Hour)
	}
	writeReceipt(t, lab, m1.ID, "published", "OPEN")
	age(t, filepath.Join(lab, "deliveries", m1.ID+".json"), time.Hour)
	bad := filepath.Join(lab, "ledgers", "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	age(t, bad, time.Hour)
	return lab
}

func ms(m time.Duration) int64 { return m.Milliseconds() }

func group(v AnalyticsView, harness, model string) (ModelGroupView, bool) {
	for _, g := range v.Models.Groups {
		if g.Harness == harness && g.Model == model {
			return g, true
		}
	}
	return ModelGroupView{}, false
}

// TestAnalyticsReconcilesWithTheLedgers is the fixture-Lab acceptance: lane counts
// equal /api/molecules for the same ETag, time measures merge parallel Atoms and
// keep coverage, each retry is one attempt, attempts with missing usage count in m
// but not n, groups keep Unknown and Not recorded, and nothing is guessed.
func TestAnalyticsReconcilesWithTheLedgers(t *testing.T) {
	lab := analyticsLab(t)
	h := testHandler(t, Options{LabDir: lab})
	lw := do(t, h, authed("/api/molecules"))
	var list MoleculeList
	decode(t, lw, &list)
	w := do(t, h, authed("/api/analytics"))
	raw := w.Body.String()
	var v AnalyticsView
	decode(t, w, &v)
	if etag := w.Header().Get("ETag"); etag == "" || etag != lw.Header().Get("ETag") || v.ETag != etag {
		t.Fatalf("analytics ETag %q, list ETag %q", etag, lw.Header().Get("ETag"))
	}
	if v.Sample || v.Complete || v.Truncated || len(v.Diagnostics) != 1 || v.Molecules != 3 {
		t.Fatalf("inventory %+v", v)
	}
	sum := 0
	for _, lane := range Lanes {
		sum += v.Counts[lane]
		if v.Counts[lane] != list.Counts[lane] {
			t.Errorf("lane %s: analytics %d, list %d", lane, v.Counts[lane], list.Counts[lane])
		}
	}
	if sum != v.Counts["all"] || v.Counts["all"] != 3 || v.Counts[LaneFinished] != 1 || v.Counts[LaneBlocked] != 1 || v.Counts[LaneDecision] != 1 {
		t.Fatalf("counts %v", v.Counts)
	}

	// Wall-clock: mol-1 and mol-3 (1h each); mol-2 never finished.
	if tm := v.Time.WallClock; tm.TotalMS == nil || *tm.TotalMS != ms(2*time.Hour) || tm.Recorded != 2 || tm.Molecules != 3 {
		t.Fatalf("wall-clock %+v", tm)
	}
	// Active: only mol-1, the union 10:00-10:30 plus 10:40-10:50, not the 50m sum.
	if tm := v.Time.ActiveAtom; tm.TotalMS == nil || *tm.TotalMS != ms(40*time.Minute) || tm.Recorded != 1 || tm.Molecules != 3 {
		t.Fatalf("active %+v", tm)
	}
	if tm := v.Time.Waiting; tm.TotalMS == nil || *tm.TotalMS != ms(20*time.Minute) || tm.Recorded != 1 || tm.Molecules != 3 {
		t.Fatalf("waiting %+v", tm)
	}
	// The Lab was busy 10:00-11:30, not the 2h the Molecules sum to.
	if v.Time.ElapsedMS == nil || *v.Time.ElapsedMS != ms(90*time.Minute) {
		t.Fatalf("elapsed %v", v.Time.ElapsedMS)
	}

	// Seven attempts: mol-1 four (h1 and its resume h2 separately), mol-2 one, mol-3 two.
	// Summed at run time, in attempt order, as the ledger costs are.
	first, second := 0.10, 0.20
	wantCost := first + second
	c := v.Cost
	if c.Attempts != 7 || c.Reported != 2 || c.UsageReported != 2 || c.TotalUSD == nil || *c.TotalUSD != wantCost || c.Basis != "list" || c.BasisLabel != "CLI list-price estimate" || c.Coverage != "cost reported for 2 of 7 harness attempts" {
		t.Fatalf("cost %+v", c)
	}
	if v.Models.Attempts != 7 || v.Models.Truncated || v.Models.Ungrouped != 0 || len(v.Models.Groups) != 5 {
		t.Fatalf("breakdown %+v", v.Models)
	}
	attempts, reported := 0, 0
	for _, g := range v.Models.Groups {
		attempts += g.Attempts
		reported += g.CostReported
	}
	if attempts != c.Attempts || reported != c.Reported {
		t.Fatalf("groups hold %d attempts and %d costs, totals %d and %d", attempts, reported, c.Attempts, c.Reported)
	}
	for _, want := range []struct {
		harness, harnessState, model, modelState string
		attempts, costReported                   int
		cost                                     *float64
	}{
		{"claude-code", stateRecorded, "claude-opus-5-5", stateRecorded, 2, 1, ptr(0.20)},
		{"Not recorded", stateNotRecorded, "claude-opus-5-5", stateRecorded, 1, 1, ptr(0.10)},
		{"codex", stateRecorded, "Not recorded", stateNotRecorded, 1, 0, nil},
		{"Not recorded", stateNotRecorded, "Unknown", stateUnknown, 1, 0, nil},
		{"Not recorded", stateNotRecorded, "Not recorded", stateNotRecorded, 2, 0, nil},
	} {
		g, ok := group(v, want.harness, want.model)
		if !ok || g.HarnessState != want.harnessState || g.ModelState != want.modelState || g.Attempts != want.attempts || g.CostReported != want.costReported {
			t.Errorf("group %s/%s = %+v (found %t)", want.harness, want.model, g, ok)
			continue
		}
		if (want.cost == nil) != (g.CostUSD == nil) || want.cost != nil && *g.CostUSD != *want.cost {
			t.Errorf("group %s/%s cost %v", want.harness, want.model, g.CostUSD)
		}
		if want.cost == nil && g.CostBasis != "unavailable" {
			t.Errorf("group %s/%s basis %q", want.harness, want.model, g.CostBasis)
		}
	}

	// Unknown values are null, never zero; cost per merged PR is unavailable.
	if v.CostPerMergedPR.State != "unavailable" || v.CostPerMergedPR.Value != nil || !strings.Contains(raw, `"costPerMergedPr":{"state":"unavailable","value":null`) {
		t.Fatalf("cost per merged PR %+v", v.CostPerMergedPR)
	}
	for _, leak := range []string{lab, "workspaces"} {
		if strings.Contains(raw, leak) {
			t.Errorf("analytics carry %q", leak)
		}
	}

	cached := authed("/api/analytics")
	cached.header = map[string]string{"If-None-Match": v.ETag}
	if w := do(t, h, cached); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("unchanged analytics: %d", w.Code)
	}
}

func ptr(v float64) *float64 { return &v }

// TestAnalyticsNotRecordedWhenNothingIsKnown: a Molecule with no times and no
// attempts gives null totals with 0-of-1 coverage, never zero.
func TestAnalyticsNotRecordedWhenNothingIsKnown(t *testing.T) {
	lab := t.TempDir()
	writeRecord(t, lab, analyticsRecord(lab, "mol-9-z", 9, "running", "2026-10-24T10:00:00Z", ""))
	w := do(t, testHandler(t, Options{LabDir: lab}), authed("/api/analytics"))
	raw := w.Body.String()
	var v AnalyticsView
	decode(t, w, &v)
	for name, m := range map[string]DurationView{"wall": v.Time.WallClock, "active": v.Time.ActiveAtom, "waiting": v.Time.Waiting} {
		if m.TotalMS != nil || m.Recorded != 0 || m.Molecules != 1 {
			t.Errorf("%s %+v", name, m)
		}
	}
	if v.Time.ElapsedMS != nil || v.Cost.TotalUSD != nil || v.Cost.Attempts != 0 || v.Cost.BasisLabel != "unavailable" || len(v.Models.Groups) != 0 {
		t.Fatalf("analytics %+v", v)
	}
	if !strings.Contains(raw, `"totalMs":null`) || !strings.Contains(raw, `"totalUsd":null`) {
		t.Fatalf("unknown values are not null: %s", raw)
	}
}

// TestAnalyticsBoundsFailSafe: groups past the bound are counted as ungrouped and
// mark the breakdown truncated, a cost sum that is not finite is unavailable, and
// a duration sum that overflows is unavailable instead of wrapping.
func TestAnalyticsBoundsFailSafe(t *testing.T) {
	inv := inventory{facts: map[string]*moleculeFacts{}}
	const n = maxModelGroups + 6
	huge := math.MaxFloat64
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("m%d", i)
		inv.list.Molecules = append(inv.list.Molecules, Summary{ID: id})
		f := &moleculeFacts{attempts: []attemptFact{{harness: "h", harnessState: stateRecorded, model: fmt.Sprintf("model-%d", i), modelState: stateRecorded, usage: true, cost: &huge, basis: "list"}}}
		if i < 2 {
			f.wallOK, f.wallStart, f.wallEnd = true, 0, math.MaxInt64
		}
		inv.facts[id] = f
	}
	v := buildAnalytics(inv)
	if len(v.Models.Groups) != maxModelGroups || !v.Models.Truncated || v.Models.Ungrouped != 6 || v.Models.Attempts != n {
		t.Fatalf("breakdown: %d groups, truncated %t, ungrouped %d, attempts %d", len(v.Models.Groups), v.Models.Truncated, v.Models.Ungrouped, v.Models.Attempts)
	}
	if v.Cost.TotalUSD != nil || !v.Cost.NotFinite || v.Cost.Reported != n {
		t.Fatalf("cost %+v", v.Cost)
	}
	if w := v.Time.WallClock; w.TotalMS != nil || !w.Overflow || w.Recorded != 2 || w.Molecules != n {
		t.Fatalf("wall-clock %+v", w)
	}
	if v.Diagnostics == nil {
		t.Fatal("diagnostics must be a list")
	}
}

// TestAnalyticsActiveTimeRefusesBadAtomTimes: a reversed, partial or unparseable
// Atom interval makes active time not recorded; Atoms that never ran are skipped.
func TestAnalyticsActiveTimeRefusesBadAtomTimes(t *testing.T) {
	ok := molecule.Atom{StartedAt: "2026-10-24T10:00:00Z", FinishedAt: "2026-10-24T10:05:00Z"}
	inside := molecule.Atom{StartedAt: "2026-10-24T10:01:00Z", FinishedAt: "2026-10-24T10:02:00Z"}
	if got, recorded := activeTime([]molecule.Atom{ok, inside, {Status: "pending"}}); !recorded || got != ms(5*time.Minute) {
		t.Fatalf("nested interval: %d %t", got, recorded)
	}
	for name, bad := range map[string]molecule.Atom{
		"reversed":    {StartedAt: "2026-10-24T10:05:00Z", FinishedAt: "2026-10-24T10:00:00Z"},
		"unfinished":  {StartedAt: "2026-10-24T10:05:00Z"},
		"unparseable": {StartedAt: "yesterday", FinishedAt: "2026-10-24T10:00:00Z"},
	} {
		if _, recorded := activeTime([]molecule.Atom{ok, bad}); recorded {
			t.Errorf("%s Atom was recorded", name)
		}
	}
	if _, recorded := activeTime([]molecule.Atom{{Status: "pending"}}); recorded {
		t.Error("Atoms that never ran recorded an active time")
	}
}

// TestAnalyticsLabelsAreBoundedAndRedacted: provider and ledger values become
// single-line, bounded labels with local paths redacted.
func TestAnalyticsLabelsAreBoundedAndRedacted(t *testing.T) {
	lab := t.TempDir()
	r := analyticsRecord(lab, "mol-5-e", 5, "completed", "", "")
	r.ReviewAttempts = []molecule.ReviewAttempt{{Ordinal: 0, Binding: "bind\n\x1b[31m" + filepath.Join(lab, "x") + strings.Repeat("y", 500), Usage: analyticsUsage("model‮"+strings.Repeat("z", 500), 1)}}
	f := analyzeRecord(lab, r)
	if len(f.attempts) != 1 {
		t.Fatalf("attempts %+v", f.attempts)
	}
	a := f.attempts[0]
	for _, label := range []string{a.harness, a.model} {
		if strings.ContainsAny(label, "\n\x1b‮") || strings.Contains(label, lab) || len([]rune(label)) > 2*maxGroupLabel {
			t.Errorf("label %q", label)
		}
	}
	if !strings.Contains(a.harness, "<lab>") || a.harnessState != stateRecorded || a.modelState != stateRecorded {
		t.Fatalf("attempt %+v", a)
	}
}

// TestAnalyticsIsGuardedReadOnlyAndSampleOnlyInDemo: the endpoint needs the cookie
// and the tab header, refuses foreign origins and non-GET methods, never writes
// the Lab, and serves sample analytics only in demo mode.
func TestAnalyticsIsGuardedReadOnlyAndSampleOnlyInDemo(t *testing.T) {
	lab := analyticsLab(t)
	h := testHandler(t, Options{LabDir: lab})
	before := snapshot(t, lab)
	cookieOnly := authed("/api/analytics")
	cookieOnly.header = map[string]string{sessionHeader: ""}
	foreign := authed("/api/analytics")
	foreign.origin = "http://evil.example"
	crossSite := authed("/api/analytics")
	crossSite.site = "cross-site"
	for name, r := range map[string]request{"no cookie": {path: "/api/analytics", header: map[string]string{sessionHeader: theSession}}, "cookie only": cookieOnly, "foreign origin": foreign, "cross-site": crossSite} {
		if w := do(t, h, r); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		r := authed("/api/analytics")
		r.method = method
		if w := do(t, h, r); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", method, w.Code)
		}
	}
	do(t, h, authed("/api/analytics"))
	if after := snapshot(t, lab); after != before {
		t.Fatalf("analytics changed the Lab:\n%s\n---\n%s", before, after)
	}

	var sample AnalyticsView
	decode(t, do(t, testHandler(t, Options{Demo: true}), authed("/api/analytics")), &sample)
	demo := demoList()
	if !sample.Sample || sample.Molecules != demoCount || sample.Counts["all"] != demo.Counts["all"] || sample.Counts[LaneBlocked] != demo.Counts[LaneBlocked] {
		t.Fatalf("demo analytics %+v", sample)
	}
	if sample.Time.WallClock.TotalMS != nil || sample.Time.WallClock.Molecules != demoCount || sample.Cost.TotalUSD != nil || sample.Cost.Attempts != 0 || sample.CostPerMergedPR.Value != nil {
		t.Fatalf("demo analytics invented values: %+v", sample)
	}
	var real AnalyticsView
	decode(t, do(t, h, authed("/api/analytics")), &real)
	if real.Sample {
		t.Fatal("real mode served sample analytics")
	}
}

// TestAnalyticsOverviewRendersTextOnly: the overview fetches /api/analytics with
// the tab session, shows unknown values as text, and the page keeps a container
// and the Unavailable cost-per-merged-PR card.
func TestAnalyticsOverviewRendersTextOnly(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	js := do(t, h, authed("/app.js")).Body.String()
	for _, want := range []string{
		"api('/api/analytics'",
		"token !== state.analyticsToken",
		"if (previous !== state.etag) loadAnalytics();",
		"$('#analytics')",
		"'Not recorded'",
		"'Unknown'",
		"Unknown is not zero.",
		"renderAnalytics();",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	page := do(t, h, authed("/")).Body.String()
	if !strings.Contains(page, `<article id="analytics" class="card">`) || !strings.Contains(page, "COST PER MERGED PR") {
		t.Fatal("index.html lacks the analytics container or the cost-per-merged-PR card")
	}
}
