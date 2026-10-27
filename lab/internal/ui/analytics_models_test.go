package ui

import (
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// modelUsage is a usage line for model with input, output and cache-read tokens
// and, when cost is not nil, a reported list-price cost.
func modelUsage(model string, cost *float64, in, out, cache int64) *harness.Usage {
	return &harness.Usage{Total: harness.TokenCategories{Input: &in, Output: &out, CacheRead: &cache}, EffectiveModels: []string{model}, TotalCostUSD: cost, CostBasis: "list"}
}

// insightsFixture is a fixture Lab of three Molecules with several models and
// roles, analysed record by record with fixed RFC 3339 times:
//
//   - mol-a (Draft published, finished 2026-10-27): an Opus analysis that asked
//     two clarifying questions ($0.10), an Opus coding attempt whose tree passed
//     ($0.50, 10 min) and a Sonnet review that raised 0 high, 1 medium, 2 low ($0.05);
//   - mol-b (blocked, week of 2026-10-19): an Opus analysis with no cost and no
//     questions, a Sonnet coding attempt whose tree failed ($1.00, 20 min), its
//     Sonnet retry with no usage whose tree passed, and a failed review with no
//     usage and no binding model;
//   - mol-c (running): nothing recorded.
func insightsFixture(t *testing.T) (string, inventory) {
	t.Helper()
	lab := t.TempDir()
	a := analyticsRecord(lab, "mol-a-1", 1, "completed", "2026-10-27T09:00:00Z", "2026-10-27T11:00:00Z")
	a.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "2026-10-27T09:00:00Z", FinishedAt: "2026-10-27T09:01:00Z", Status: "completed",
		Response: &readiness.Analysis{Questions: make([]readiness.Question, 2)}, Usage: modelUsage("claude-opus-5-5", ptr(0.10), 100, 10, 1000)}}
	a.Atoms = []molecule.Atom{
		{ID: "intake", Type: "intake", Status: "done", StartedAt: "2026-10-27T09:00:00Z", FinishedAt: "2026-10-27T09:10:00Z"},
		{ID: "code", Type: "harness", Status: "done", StartedAt: "2026-10-27T09:10:00Z", FinishedAt: "2026-10-27T09:40:00Z", Provenance: &molecule.AtomProvenance{Actions: []molecule.AtomAction{
			{AttemptID: "h1", PlannedAction: "edit", StartedAt: "2026-10-27T09:10:00Z", ExecutedBinding: "claude-code"}}}},
	}
	a.HarnessAttempts = []molecule.HarnessAttempt{{ID: "h1", PlanDigest: "d", Model: "claude-opus-5-5", Result: process.Result{ElapsedMS: ms(10 * time.Minute)},
		Capture: &verification.CaptureResult{Tree: "tree-a"}, Usage: modelUsage("claude-opus-5-5", ptr(0.50), 300, 30, 3000)}}
	a.VerificationAttempts = []molecule.VerificationAttempt{{PlanDigest: "d", SourceTree: "tree-a", StartedAt: "2026-10-27T09:30:00Z", FinishedAt: "2026-10-27T09:40:00Z", Status: "passed"}}
	a.ReviewAttempts = []molecule.ReviewAttempt{{Ordinal: 0, HarnessAttemptID: "h1", Binding: "claude-review", StartedAt: "2026-10-27T09:45:00Z", FinishedAt: "2026-10-27T09:47:00Z",
		Status: molecule.ReviewNoHighFindings, Medium: 1, Low: 2, Usage: modelUsage("claude-sonnet-5-5", ptr(0.05), 50, 5, 500)}}

	b := analyticsRecord(lab, "mol-b-2", 2, "blocked", "2026-10-20T09:00:00Z", "")
	b.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "2026-10-20T09:00:00Z", FinishedAt: "2026-10-20T09:03:00Z", Status: "completed",
		Response: &readiness.Analysis{}, Usage: modelUsage("claude-opus-5-5", nil, 200, 20, 2000)}}
	b.Atoms = []molecule.Atom{{ID: "code", Type: "harness", Status: "failed", Provenance: &molecule.AtomProvenance{Actions: []molecule.AtomAction{
		{AttemptID: "h2", PlannedAction: "edit", StartedAt: "2026-10-20T09:10:00Z"},
		{AttemptID: "h3", PlannedAction: "edit", StartedAt: "2026-10-20T10:00:00Z", Observed: &molecule.AtomObservation{FinishedAt: "2026-10-20T10:30:00Z"}}}}}}
	b.HarnessAttempts = []molecule.HarnessAttempt{
		{ID: "h2", PlanDigest: "d", Model: "claude-sonnet-5-5", Result: process.Result{ElapsedMS: ms(20 * time.Minute)}, Capture: &verification.CaptureResult{Tree: "tree-b"}, Usage: modelUsage("claude-sonnet-5-5", ptr(1.00), 400, 40, 4000)},
		{ID: "h3", PlanDigest: "d", Model: "claude-sonnet-5-5", Capture: &verification.CaptureResult{Tree: "tree-c"}},
	}
	b.VerificationAttempts = []molecule.VerificationAttempt{
		{PlanDigest: "d", SourceTree: "tree-b", StartedAt: "2026-10-20T09:40:00Z", FinishedAt: "2026-10-20T09:50:00Z", Status: "failed"},
		{PlanDigest: "d", SourceTree: "tree-c", StartedAt: "2026-10-20T10:30:00Z", FinishedAt: "2026-10-20T10:40:00Z", Status: "passed"},
	}
	b.ReviewAttempts = []molecule.ReviewAttempt{{Ordinal: 0, HarnessAttemptID: "h3", StartedAt: "2026-10-20T10:45:00Z", Status: molecule.ReviewFailed, High: 3}}

	c := analyticsRecord(lab, "mol-c-3", 3, "running", "2026-10-27T12:00:00Z", "")
	inv := inventory{facts: map[string]*moleculeFacts{}}
	for _, s := range []struct {
		r    molecule.Record
		lane string
	}{{a, LaneFinished}, {b, LaneBlocked}, {c, LaneRunning}} {
		inv.list.Molecules = append(inv.list.Molecules, Summary{ID: s.r.ID, Lane: s.lane, StartedAt: s.r.StartedAt, FinishedAt: s.r.FinishedAt})
		inv.facts[s.r.ID] = analyzeRecord(lab, s.r)
	}
	inv.list.Counts = counts(inv.list.Molecules)
	return lab, inv
}

// fixedNow is Wednesday of the week of 2026-10-26.
var fixedNow = time.Date(2026, 10, 28, 15, 0, 0, 0, time.UTC)

func modelRow(v AnalyticsView, role, model string) (ModelRoleView, bool) {
	for _, r := range v.ByModel.Rows {
		if r.Role == role && r.Model == model {
			return r, true
		}
	}
	return ModelRoleView{}, false
}

func i64(v int64) *int64 { return &v }

func eqF(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || math.Abs(*a-*b) < 1e-9) }
func eqI(a, b *int64) bool   { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// TestInsightsByModelRowsAggregate: one row per effective model and Atom role,
// every attempt counted exactly once, cost and tokens summed with their coverage,
// medians per attempt, and the coding, review and analysis outcomes.
func TestInsightsByModelRowsAggregate(t *testing.T) {
	_, inv := insightsFixture(t)
	v := buildAnalyticsAt(inv, fixedNow)
	by := v.ByModel
	if by.Attempts != 7 || by.Truncated || by.Ungrouped != 0 || len(by.Rows) != 5 {
		t.Fatalf("by model %+v", by)
	}
	sum := 0
	for _, r := range by.Rows {
		sum += r.Attempts
	}
	if sum != by.Attempts || by.Attempts != v.Cost.Attempts {
		t.Fatalf("rows hold %d attempts, total %d, cost %d", sum, by.Attempts, v.Cost.Attempts)
	}
	// Default order: analysis, coding, review; then attempts, then model.
	var order []string
	for _, r := range by.Rows {
		order = append(order, r.Role+"/"+r.Model)
	}
	if want := "analysis/claude-opus-5-5 coding/claude-sonnet-5-5 coding/claude-opus-5-5 review/Not recorded review/claude-sonnet-5-5"; strings.Join(order, " ") != want {
		t.Errorf("order %q", order)
	}

	for _, want := range []struct {
		role, model             string
		attempts, molecules     int
		costReported, usage     int
		cost, median            *float64
		in, out, cache          *int64
		wallReported            int
		wall                    *int64
		checksRan, checksPassed int
		passRate                *float64
	}{
		{roleAnalysis, "claude-opus-5-5", 2, 2, 1, 2, ptr(0.10), ptr(0.10), i64(300), i64(30), i64(3000), 2, i64(ms(2 * time.Minute)), 0, 0, nil},
		{roleCoding, "claude-opus-5-5", 1, 1, 1, 1, ptr(0.50), ptr(0.50), i64(300), i64(30), i64(3000), 1, i64(ms(10 * time.Minute)), 1, 1, ptr(1)},
		{roleCoding, "claude-sonnet-5-5", 2, 1, 1, 1, ptr(1.00), ptr(1.00), i64(400), i64(40), i64(4000), 2, i64(ms(25 * time.Minute)), 2, 1, ptr(0.5)},
		{roleReview, "claude-sonnet-5-5", 1, 1, 1, 1, ptr(0.05), ptr(0.05), i64(50), i64(5), i64(500), 1, i64(ms(2 * time.Minute)), 0, 0, nil},
		{roleReview, "Not recorded", 1, 1, 0, 0, nil, nil, nil, nil, nil, 0, nil, 0, 0, nil},
	} {
		r, ok := modelRow(v, want.role, want.model)
		if !ok {
			t.Errorf("no row %s/%s", want.role, want.model)
			continue
		}
		if r.Attempts != want.attempts || r.Molecules != want.molecules || r.CostReported != want.costReported || r.UsageReported != want.usage || r.WallReported != want.wallReported || r.ChecksRan != want.checksRan || r.ChecksPassed != want.checksPassed {
			t.Errorf("%s/%s counts %+v", want.role, want.model, r)
		}
		if !eqF(r.CostUSD, want.cost) || !eqF(r.MedianCostUSD, want.median) || !eqF(r.PassRate, want.passRate) {
			t.Errorf("%s/%s cost %v median %v pass %v", want.role, want.model, r.CostUSD, r.MedianCostUSD, r.PassRate)
		}
		if !eqI(r.TokensIn, want.in) || !eqI(r.TokensOut, want.out) || !eqI(r.TokensCache, want.cache) || !eqI(r.MedianWallMS, want.wall) {
			t.Errorf("%s/%s tokens %v %v %v wall %v", want.role, want.model, r.TokensIn, r.TokensOut, r.TokensCache, r.MedianWallMS)
		}
	}
	if r, _ := modelRow(v, roleReview, "claude-sonnet-5-5"); r.ReviewsReported != 1 || r.Findings == nil || *r.Findings != (FindingsView{High: 0, Medium: 1, Low: 2}) {
		t.Errorf("review findings %+v", r)
	}
	// A failed review raised no findings it could report, whatever it recorded.
	if r, _ := modelRow(v, roleReview, "Not recorded"); r.ReviewsReported != 0 || r.Findings != nil {
		t.Errorf("failed review %+v", r)
	}
	if r, _ := modelRow(v, roleAnalysis, "claude-opus-5-5"); r.QuestionsReported != 2 || r.Questions == nil || *r.Questions != 2 || r.QuestionMolecules != 2 || !eqF(r.QuestionsPerMolecule, ptr(1)) {
		t.Errorf("analysis questions %+v", r)
	}
	if v.ByModel.Meta.State != "unavailable" || v.ByModel.Meta.Value != nil || v.ByModel.Meta.Reason == "" {
		t.Errorf("meta %+v", v.ByModel.Meta)
	}
}

// TestInsightsSummaryPlanningWeeklyAndTime: the tiles, planning effort, the weekly
// series and the time split reconcile with the fixture.
func TestInsightsSummaryPlanningWeeklyAndTime(t *testing.T) {
	_, inv := insightsFixture(t)
	v := buildAnalyticsAt(inv, fixedNow)
	s := v.Summary
	if s.WeekStart != "2026-10-26" || s.MoleculesFinished != 1 {
		t.Fatalf("summary %+v", s)
	}
	// This week: mol-a's three attempts and mol-c's none; mol-b ran the week before.
	if w := s.CostThisWeek; w.Reported != 3 || w.Attempts != 3 || !eqF(w.TotalUSD, ptr(0.65)) {
		t.Errorf("cost this week %+v", w)
	}
	if c := s.CostTotal; c.Reported != 4 || c.Attempts != 7 || !eqF(c.TotalUSD, ptr(1.65)) || !eqF(c.TotalUSD, v.Cost.TotalUSD) {
		t.Errorf("cost total %+v", c)
	}
	if d := s.TimeToDraftPR; d.State != "unavailable" || d.Value != nil {
		t.Errorf("time to draft PR %+v", d)
	}
	// mol-a passed on its first attempt, mol-b only on its retry; mol-c never coded.
	if f := s.FirstTryGreen; f.Count != 1 || f.Of != 2 || f.Molecules != 2 || !eqF(f.Value, ptr(0.5)) {
		t.Errorf("first-try green %+v", f)
	}
	if p := v.Planning; p.Questions != 2 || p.Molecules != 2 || p.Of != 2 || !eqF(p.QuestionsPerMolecule, ptr(1)) || p.Cost.Reported != 1 || p.Cost.Attempts != 2 || !eqF(p.Cost.TotalUSD, ptr(0.10)) {
		t.Errorf("planning %+v", p)
	}

	w := v.Weekly
	if len(w.Weeks) != 2 || w.Weeks[0].Start != "2026-10-19" || w.Weeks[1].Start != "2026-10-26" || w.Truncated || w.Folded || w.UndatedCost != 0 || w.UndatedFinished != 0 {
		t.Fatalf("weekly %+v", w)
	}
	// Opus: three dated costs and one finished Molecule; Sonnet: two dated costs.
	if strings.Join(w.Series, ",") != "claude-opus-5-5,claude-sonnet-5-5" {
		t.Fatalf("series %q", w.Series)
	}
	if wk := w.Weeks[0]; wk.CostReported != 1 || !eqF(wk.CostTotal, ptr(1.00)) || wk.Cost[0] != nil || !eqF(wk.Cost[1], ptr(1.00)) || wk.FinishedTotal != 0 {
		t.Errorf("week 1 %+v", wk)
	}
	if wk := w.Weeks[1]; wk.CostReported != 3 || !eqF(wk.CostTotal, ptr(0.65)) || !eqF(wk.Cost[0], ptr(0.60)) || !eqF(wk.Cost[1], ptr(0.05)) || wk.FinishedTotal != 1 || wk.Finished[0] != 1 {
		t.Errorf("week 2 %+v", wk)
	}

	// mol-a: 2h wall-clock; Atoms 09:00-09:40 with checks 09:30-09:40 inside.
	ts := v.TimeSplit
	if ts.Active.Recorded != 1 || ts.Active.Molecules != 3 || !eqI(ts.Active.TotalMS, i64(ms(30*time.Minute))) || !eqI(ts.WaitingOnChecks.TotalMS, i64(ms(10*time.Minute))) || !eqI(ts.WaitingForYou.TotalMS, i64(ms(80*time.Minute))) {
		t.Errorf("time split %+v", ts)
	}
}

// TestInsightsMissingValuesAreNullNeverZero: values no ledger records are JSON
// null, with coverage counts that say so, and nothing local leaks.
func TestInsightsMissingValuesAreNullNeverZero(t *testing.T) {
	lab, inv := insightsFixture(t)
	v := buildAnalyticsAt(inv, fixedNow)
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		ByModel struct {
			Rows []map[string]json.RawMessage `json:"rows"`
		} `json:"byModel"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	for _, r := range probe.ByModel.Rows {
		if string(r["model"]) != `"Not recorded"` {
			continue
		}
		for _, field := range []string{"costUsd", "medianCostUsd", "tokensIn", "tokensOut", "tokensCacheRead", "medianWallMs", "passRate", "findings", "questions"} {
			if string(r[field]) != "null" {
				t.Errorf("%s = %s, want null", field, r[field])
			}
		}
		if string(r["costReported"]) != "0" || string(r["attempts"]) != "1" {
			t.Errorf("coverage %s of %s", r["costReported"], r["attempts"])
		}
	}
	if !strings.Contains(string(raw), `"timeToDraftPr":{"state":"unavailable","value":null`) {
		t.Error("time to draft PR is not null")
	}
	for _, leak := range []string{lab, "workspaces"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("insights carry %q", leak)
		}
	}

	// A Lab with nothing recorded: every new measure is null with 0-of-N coverage.
	empty := inventory{facts: map[string]*moleculeFacts{}}
	empty.list.Molecules = []Summary{{ID: "m", Lane: LaneRunning}}
	e := buildAnalyticsAt(empty, fixedNow)
	if e.Summary.CostThisWeek.TotalUSD != nil || e.Summary.CostTotal.TotalUSD != nil || e.Summary.FirstTryGreen.Value != nil || e.Planning.QuestionsPerMolecule != nil || e.Planning.Cost.TotalUSD != nil ||
		e.TimeSplit.Active.TotalMS != nil || e.TimeSplit.Active.Molecules != 1 || len(e.ByModel.Rows) != 0 || len(e.Weekly.Weeks) != 0 {
		t.Fatalf("empty insights %+v", e)
	}
	raw, _ = json.Marshal(e)
	for _, want := range []string{`"rows":[]`, `"weeks":[]`, `"series":[]`, `"value":null`, `"questionsPerMolecule":null`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("empty insights lack %s", want)
		}
	}
}

// TestInsightsBoundsFailSafe: rows past the bound are counted as ungrouped, token
// sums that overflow and costs that are not finite are null with a flag, weeks
// past the bound are dropped and flagged, models past the series bound are
// folded, and an unreadable time split is not recorded.
func TestInsightsBoundsFailSafe(t *testing.T) {
	inv := inventory{facts: map[string]*moleculeFacts{}}
	const n = maxModelRoleRows + 5
	huge, big := math.MaxFloat64, int64(math.MaxInt64-1)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("m%03d", i)
		inv.list.Molecules = append(inv.list.Molecules, Summary{ID: id})
		start := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * 7 * 24 * time.Hour).UnixMilli()
		a := attemptFact{harness: "h", harnessState: stateRecorded, model: fmt.Sprintf("model-%02d", i), modelState: stateRecorded, usage: true, cost: &huge, basis: "list",
			role: roleCoding, startOK: true, startMS: start, tokens: harness.TokenCategories{Input: &big}}
		inv.facts[id] = &moleculeFacts{attempts: []attemptFact{a, a}}
	}
	v := buildAnalyticsAt(inv, fixedNow)
	by := v.ByModel
	if len(by.Rows) != maxModelRoleRows || !by.Truncated || by.Ungrouped != 2*5 || by.Attempts != 2*n {
		t.Fatalf("rows %d truncated %t ungrouped %d attempts %d", len(by.Rows), by.Truncated, by.Ungrouped, by.Attempts)
	}
	r := by.Rows[0]
	if r.CostUSD != nil || !r.CostNotFinite || r.TokensIn != nil || !r.TokenOverflow || r.CostReported != 2 {
		t.Fatalf("row %+v", r)
	}
	w := v.Weekly
	if len(w.Weeks) != maxWeeks || !w.Truncated || !w.Folded || len(w.Series) != maxSeries+1 || w.Series[maxSeries] != otherModels {
		t.Fatalf("weekly: %d weeks truncated %t folded %t series %q", len(w.Weeks), w.Truncated, w.Folded, w.Series)
	}
	for _, wk := range w.Weeks {
		if wk.CostTotal != nil || !wk.CostNotFinite || len(wk.Cost) != len(w.Series) || len(wk.Finished) != len(w.Series) {
			t.Fatalf("week %+v", wk)
		}
	}
	if v.Summary.CostTotal.TotalUSD != nil || !v.Summary.CostTotal.NotFinite {
		t.Fatalf("total %+v", v.Summary.CostTotal)
	}

	// An attempt enrichFacts could not place is counted, never merged into a row.
	odd := inventory{facts: map[string]*moleculeFacts{"x": {attempts: []attemptFact{{model: "m", modelState: stateRecorded}}}}}
	odd.list.Molecules = []Summary{{ID: "x"}}
	if o := buildAnalyticsAt(odd, fixedNow).ByModel; o.Attempts != 1 || len(o.Rows) != 0 || !o.Truncated || o.Ungrouped != 1 {
		t.Fatalf("unplaced attempt %+v", o)
	}

	// A check that never finished, or an Atom outside the wall-clock, leaves the
	// split not recorded rather than understated.
	lab := t.TempDir()
	base := analyticsRecord(lab, "mol-s", 1, "completed", "2026-10-27T09:00:00Z", "2026-10-27T10:00:00Z")
	base.Atoms = []molecule.Atom{{ID: "code", StartedAt: "2026-10-27T09:00:00Z", FinishedAt: "2026-10-27T09:30:00Z"}}
	if f := analyzeRecord(lab, base); !f.split.ok || f.split.active != ms(30*time.Minute) || f.split.waiting != ms(30*time.Minute) {
		t.Fatalf("split %+v", f.split)
	}
	unfinished := base
	unfinished.VerificationAttempts = []molecule.VerificationAttempt{{SourceTree: "t", StartedAt: "2026-10-27T09:10:00Z", Status: "started"}}
	outside := base
	outside.Atoms = []molecule.Atom{{ID: "code", StartedAt: "2026-10-27T08:00:00Z", FinishedAt: "2026-10-27T09:30:00Z"}}
	for name, r := range map[string]molecule.Record{"unfinished check": unfinished, "outside": outside} {
		if f := analyzeRecord(lab, r); f.split.ok {
			t.Errorf("%s: split recorded %+v", name, f.split)
		}
	}
}

// TestInsightsServedAndDemoInventNothing: /api/analytics carries the Insights
// fields from the ledgers with no local path, and sample mode invents no value.
func TestInsightsServedAndDemoInventNothing(t *testing.T) {
	lab := t.TempDir()
	r := analyticsRecord(lab, "mol-7-g", 7, "completed", "2026-10-24T10:00:00Z", "2026-10-24T11:00:00Z")
	r.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "2026-10-24T10:00:00Z", FinishedAt: "2026-10-24T10:02:00Z", Status: "completed", Usage: modelUsage(filepath.Join(lab, "model"), nil, 1, 2, 3)}}
	writeRecord(t, lab, r)
	w := do(t, testHandler(t, Options{LabDir: lab}), authed("/api/analytics"))
	raw := w.Body.String()
	var v AnalyticsView
	decode(t, w, &v)
	if len(v.ByModel.Rows) != 1 || v.ByModel.Rows[0].Role != roleAnalysis || v.ByModel.Rows[0].CostUSD != nil || v.ByModel.Rows[0].CostReported != 0 || !eqI(v.ByModel.Rows[0].MedianWallMS, i64(ms(2*time.Minute))) {
		t.Fatalf("served rows %+v", v.ByModel.Rows)
	}
	if strings.Contains(raw, lab) || !strings.Contains(v.ByModel.Rows[0].Model, "<lab>") {
		t.Fatalf("served model label %q", v.ByModel.Rows[0].Model)
	}

	var sample AnalyticsView
	decode(t, do(t, testHandler(t, Options{Demo: true}), authed("/api/analytics")), &sample)
	if !sample.Sample || len(sample.ByModel.Rows) != 0 || sample.ByModel.Attempts != 0 || len(sample.Weekly.Weeks) != 0 || sample.Summary.CostTotal.TotalUSD != nil ||
		sample.Summary.CostThisWeek.TotalUSD != nil || sample.Summary.FirstTryGreen.Value != nil || sample.TimeSplit.Active.TotalMS != nil || sample.Planning.QuestionsPerMolecule != nil {
		t.Fatalf("demo insights invented values: %+v", sample)
	}
}

// inventoryOf analyses records, each in its lane, as one inventory.
func inventoryOf(lab string, lanes []string, records ...molecule.Record) inventory {
	inv := inventory{facts: map[string]*moleculeFacts{}}
	for i, r := range records {
		inv.list.Molecules = append(inv.list.Molecules, Summary{ID: r.ID, Lane: lanes[i], StartedAt: r.StartedAt, FinishedAt: r.FinishedAt})
		inv.facts[r.ID] = analyzeRecord(lab, r)
	}
	inv.list.Counts = counts(inv.list.Molecules)
	return inv
}

// coded is a record with one coding attempt per {id, start, tree}, listed in the
// given (ledger) order, and the given checks.
func coded(lab, id string, n int64, attempts [][3]string, checks ...molecule.VerificationAttempt) molecule.Record {
	r := analyticsRecord(lab, id, n, "blocked", "2026-10-27T08:00:00Z", "")
	var actions []molecule.AtomAction
	for _, a := range attempts {
		actions = append(actions, molecule.AtomAction{AttemptID: a[0], PlannedAction: "edit", StartedAt: a[1]})
		r.HarnessAttempts = append(r.HarnessAttempts, molecule.HarnessAttempt{ID: a[0], PlanDigest: "d", Model: "m", Capture: &verification.CaptureResult{Tree: a[2]}})
	}
	r.Atoms = []molecule.Atom{{ID: "code", Type: "harness", Status: "failed", Provenance: &molecule.AtomProvenance{Actions: actions}}}
	r.VerificationAttempts = checks
	return r
}

func check(tree, start, status string) molecule.VerificationAttempt {
	return molecule.VerificationAttempt{PlanDigest: "d", SourceTree: tree, StartedAt: start, Status: status}
}

// TestInsightsFirstTryGreenNeedsACheckedFirstAttempt: only a Molecule whose
// first coding attempt (by start time) was checked is in the rate, judged by the
// earliest check of that tree; the others are coverage, never a failure.
func TestInsightsFirstTryGreenNeedsACheckedFirstAttempt(t *testing.T) {
	lab := t.TempDir()
	// First attempt unchecked, its retry passed: left out of the rate.
	unchecked := coded(lab, "mol-u", 1, [][3]string{{"h1", "2026-10-27T09:00:00Z", "t1"}, {"h2", "2026-10-27T10:00:00Z", "t2"}}, check("t2", "2026-10-27T10:30:00Z", "passed"))
	// The same tree failed, then passed on a re-run: not a first-try pass.
	rerun := coded(lab, "mol-r", 2, [][3]string{{"h1", "2026-10-27T09:00:00Z", "t3"}}, check("t3", "2026-10-27T09:30:00Z", "failed"), check("t3", "2026-10-27T09:50:00Z", "passed"))
	// Listed out of start order: the earlier-started attempt passed.
	reordered := coded(lab, "mol-o", 3, [][3]string{{"h9", "2026-10-27T11:00:00Z", "t5"}, {"h1", "2026-10-27T09:00:00Z", "t4"}}, check("t4", "2026-10-27T09:30:00Z", "passed"), check("t5", "2026-10-27T11:30:00Z", "failed"))
	// Checks listed out of start order: the earliest check of the tree failed.
	late := coded(lab, "mol-v", 4, [][3]string{{"h1", "2026-10-27T09:00:00Z", "t6"}}, check("t6", "2026-10-27T10:00:00Z", "passed"), check("t6", "2026-10-27T09:30:00Z", "failed"))
	// Two attempts, one with no readable start: which ran first is unknown.
	unknown := coded(lab, "mol-k", 5, [][3]string{{"h1", "", "t7"}, {"h2", "2026-10-27T09:00:00Z", "t8"}}, check("t7", "2026-10-27T09:30:00Z", "passed"), check("t8", "2026-10-27T09:30:00Z", "passed"))
	lanes := []string{LaneBlocked, LaneBlocked, LaneBlocked, LaneBlocked, LaneBlocked}
	v := buildAnalyticsAt(inventoryOf(lab, lanes, unchecked, rerun, reordered, late, unknown), fixedNow)
	if f := v.Summary.FirstTryGreen; f.Molecules != 5 || f.Unchecked != 2 || f.Of != 3 || f.Count != 1 || !eqF(f.Value, ptr(1.0/3)) {
		t.Fatalf("first-try green %+v", f)
	}
	// The row still counts a re-run tree that passed as passed on that attempt.
	if r, ok := modelRow(v, roleCoding, "m"); !ok || r.ChecksRan != 7 || r.ChecksPassed != 6 {
		t.Fatalf("coding row %+v", r)
	}

	trees := checkedTrees([]molecule.VerificationAttempt{check("a", "", "started"), check("b", "", "started"), check("b", "", "passed")})
	if trees["a"] != (treeChecks{}) || trees["b"] != (treeChecks{any: checksPassed, first: checksFailed}) {
		t.Fatalf("trees %+v", trees)
	}
}

// TestInsightsPairsUsageByAttemptID: a usage entry is paired with its ledger
// attempt by kind and attempt id, never by its position in the whole list.
func TestInsightsPairsUsageByAttemptID(t *testing.T) {
	lab := t.TempDir()
	r := analyticsRecord(lab, "mol-p", 1, "completed", "2026-10-27T09:00:00Z", "2026-10-27T11:00:00Z")
	r.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "2026-10-27T09:00:00Z", FinishedAt: "2026-10-27T09:04:00Z", Response: &readiness.Analysis{Questions: make([]readiness.Question, 3)}}}
	r.HarnessAttempts = []molecule.HarnessAttempt{
		{ID: "h1", Result: process.Result{ElapsedMS: ms(time.Minute)}},
		{ID: "h2", Result: process.Result{ElapsedMS: ms(7 * time.Minute)}},
	}
	r.ReviewAttempts = []molecule.ReviewAttempt{{Ordinal: 0, StartedAt: "2026-10-27T10:00:00Z", FinishedAt: "2026-10-27T10:05:00Z", Status: molecule.ReviewBlocked, High: 2}}
	f := &moleculeFacts{attempts: []attemptFact{
		{kind: roleReview, id: "review-0"},
		{kind: roleCoding, id: "h2"},
		{kind: "meta", id: "x"},
		{kind: roleCoding, id: "h1"},
		{kind: roleAnalysis, id: "analysis-1"},
		{kind: roleCoding, id: "h-missing"},
	}}
	enrichFacts(r, f)
	a := f.attempts
	if a[0].role != roleReview || !a[0].findingsOK || a[0].high != 2 || a[0].wallMS != ms(5*time.Minute) {
		t.Errorf("review %+v", a[0])
	}
	if a[1].role != roleCoding || a[1].wallMS != ms(7*time.Minute) || a[3].role != roleCoding || a[3].wallMS != ms(time.Minute) {
		t.Errorf("coding %+v %+v", a[1], a[3])
	}
	if a[2].role != "" || a[5].role != "" {
		t.Errorf("unpaired attempts placed: %+v %+v", a[2], a[5])
	}
	if a[4].role != roleAnalysis || !a[4].questionsOK || a[4].questions != 3 || a[4].wallMS != ms(4*time.Minute) {
		t.Errorf("analysis %+v", a[4])
	}

	// Duplicate ids fall back to the ordinal within the kind.
	dup := r
	dup.HarnessAttempts = []molecule.HarnessAttempt{{ID: "h", Result: process.Result{ElapsedMS: 1}}, {ID: "h", Result: process.Result{ElapsedMS: 2}}}
	g := &moleculeFacts{attempts: []attemptFact{{kind: roleCoding, id: "h"}, {kind: roleCoding, id: "h"}}}
	enrichFacts(dup, g)
	if g.attempts[0].wallMS != 1 || g.attempts[1].wallMS != 2 {
		t.Errorf("duplicate ids %+v", g.attempts)
	}

	// Through analyzeRecord, every attempt is placed under its own kind.
	if facts := analyzeRecord(lab, r); len(facts.attempts) != 4 || facts.attempts[0].role != roleAnalysis || facts.attempts[1].role != roleCoding || facts.attempts[3].role != roleReview {
		t.Errorf("analysed %+v", facts.attempts)
	}
}

// TestInsightsRejectsNegativeCost: a negative cost is not reported, so the
// coverage and every total agree.
func TestInsightsRejectsNegativeCost(t *testing.T) {
	lab := t.TempDir()
	r := analyticsRecord(lab, "mol-n", 1, "running", "2026-10-27T09:00:00Z", "")
	r.AnalysisAttempts = []molecule.AnalysisAttempt{
		{Ordinal: 1, StartedAt: "2026-10-27T09:00:00Z", Usage: modelUsage("m", ptr(-5), 1, 1, 1)},
		{Ordinal: 2, StartedAt: "2026-10-27T09:10:00Z", Usage: modelUsage("m", ptr(0.25), 1, 1, 1)},
	}
	v := buildAnalyticsAt(inventoryOf(lab, []string{LaneRunning}, r), fixedNow)
	row, ok := modelRow(v, roleAnalysis, "m")
	if !ok || row.CostReported != 1 || !eqF(row.CostUSD, ptr(0.25)) || !eqF(row.MedianCostUSD, ptr(0.25)) || row.UsageReported != 2 {
		t.Fatalf("row %+v", row)
	}
	if c := v.Summary.CostTotal; c.Reported != 1 || c.Attempts != 2 || !eqF(c.TotalUSD, ptr(0.25)) {
		t.Errorf("total %+v", c)
	}
	if c := v.Cost; c.Reported != 1 || !eqF(c.TotalUSD, ptr(0.25)) || c.Coverage != "cost reported for 1 of 2 harness attempts" {
		t.Errorf("cost %+v", c)
	}
	if p := v.Planning.Cost; p.Reported != 1 || !eqF(p.TotalUSD, ptr(0.25)) {
		t.Errorf("planning %+v", p)
	}
}

// TestInsightsWeeklyAnchoredOnTheCurrentWeek: a time far in the future is
// undated, so it cannot empty the chart; the series ends at the current week; a
// finished Molecule with no coding attempt has its own label.
func TestInsightsWeeklyAnchoredOnTheCurrentWeek(t *testing.T) {
	lab, inv := insightsFixture(t)
	future := analyticsRecord(lab, "mol-f-9", 9, "completed", "9999-01-04T09:00:00Z", "9999-01-04T10:00:00Z")
	future.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "9999-01-04T09:00:00Z", FinishedAt: "9999-01-04T09:01:00Z", Usage: modelUsage("claude-opus-5-5", ptr(9), 1, 1, 1)}}
	inv.list.Molecules = append(inv.list.Molecules, Summary{ID: future.ID, Lane: LaneFinished, StartedAt: future.StartedAt, FinishedAt: future.FinishedAt})
	inv.facts[future.ID] = analyzeRecord(lab, future)
	inv.list.Counts = counts(inv.list.Molecules)
	v := buildAnalyticsAt(inv, fixedNow)
	w := v.Weekly
	if len(w.Weeks) != 2 || w.Weeks[0].Start != "2026-10-19" || w.Weeks[1].Start != "2026-10-26" || w.Truncated || w.UndatedCost != 1 || w.UndatedFinished != 1 {
		t.Fatalf("weekly %+v", w)
	}
	if c := v.Summary.CostThisWeek; c.Reported != 3 || c.Attempts != 3 || !eqF(c.TotalUSD, ptr(0.65)) {
		t.Errorf("this week %+v", c)
	}
	// Still in the all-time total: its cost is reported, only its week is not.
	if c := v.Summary.CostTotal; c.Reported != 5 || !eqF(c.TotalUSD, ptr(10.65)) {
		t.Errorf("total %+v", c)
	}

	// Old data only: the series still ends at the current week, with distinct
	// labels for a finished Molecule that never coded and one whose coding
	// attempt's model the ledger left out.
	old := analyticsRecord(lab, "mol-o-1", 1, "completed", "2026-09-07T09:00:00Z", "2026-09-07T10:00:00Z")
	old.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "2026-09-07T09:00:00Z"}}
	unmodelled := analyticsRecord(lab, "mol-o-2", 2, "completed", "2026-09-08T09:00:00Z", "2026-09-08T10:00:00Z")
	unmodelled.HarnessAttempts = []molecule.HarnessAttempt{{ID: "h1"}}
	// Within a day of now: placed, not undated.
	soon := analyticsRecord(lab, "mol-o-3", 3, "completed", "2026-10-28T20:00:00Z", "2026-10-29T03:00:00Z")
	o := buildAnalyticsAt(inventoryOf(lab, []string{LaneFinished, LaneFinished, LaneFinished}, old, unmodelled, soon), fixedNow).Weekly
	if len(o.Weeks) != 8 || o.Weeks[0].Start != "2026-09-07" || o.Weeks[7].Start != "2026-10-26" || o.UndatedFinished != 0 || o.Weeks[7].FinishedTotal != 1 {
		t.Fatalf("old weekly %+v", o)
	}
	if strings.Join(o.Series, ",") != "No coding attempt,Not recorded" {
		t.Fatalf("series %q", o.Series)
	}
}

// TestInsightsServedWeekFollowsTheClock: the served "this week" comes from the
// injected clock, so a request after a week boundary reports the new week.
func TestInsightsServedWeekFollowsTheClock(t *testing.T) {
	lab := t.TempDir()
	r := analyticsRecord(lab, "mol-w-1", 1, "running", "2026-10-25T23:00:00Z", "")
	r.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "2026-10-25T23:00:00Z", Usage: modelUsage("m", ptr(0.5), 1, 1, 1)}}
	writeRecord(t, lab, r)
	defer func(c func() time.Time) { analyticsNow = c }(analyticsNow)
	h := testHandler(t, Options{LabDir: lab})
	for _, want := range []struct {
		now  time.Time
		week string
		cost *float64
	}{
		{time.Date(2026, 10, 25, 23, 59, 0, 0, time.UTC), "2026-10-19", ptr(0.5)},
		{time.Date(2026, 10, 26, 0, 1, 0, 0, time.UTC), "2026-10-26", nil},
	} {
		analyticsNow = func() time.Time { return want.now }
		var v AnalyticsView
		decode(t, do(t, h, authed("/api/analytics")), &v)
		if v.Summary.WeekStart != want.week || !eqF(v.Summary.CostThisWeek.TotalUSD, want.cost) {
			t.Errorf("at %s: week %s cost %v", want.now, v.Summary.WeekStart, v.Summary.CostThisWeek.TotalUSD)
		}
	}
}

// TestInsightsETagTurnsWithTheWeek: the week is part of the ETag, so a poll after
// a week boundary is never answered 304 with last week's tiles, and the list and
// analytics tags still agree on either side of it.
func TestInsightsETagTurnsWithTheWeek(t *testing.T) {
	lab := t.TempDir()
	r := analyticsRecord(lab, "mol-w-2", 2, "running", "2026-10-25T23:00:00Z", "")
	r.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, StartedAt: "2026-10-25T23:00:00Z", Usage: modelUsage("m", ptr(0.5), 1, 1, 1)}}
	writeRecord(t, lab, r)
	age(t, filepath.Join(lab, "ledgers", r.ID+".json"), time.Hour)
	defer func(c func() time.Time) { analyticsNow = c }(analyticsNow)
	h := testHandler(t, Options{LabDir: lab})
	tags := func() (string, string) {
		t.Helper()
		return do(t, h, authed("/api/molecules")).Header().Get("ETag"), do(t, h, authed("/api/analytics")).Header().Get("ETag")
	}

	analyticsNow = func() time.Time { return time.Date(2026, 10, 25, 23, 59, 0, 0, time.UTC) }
	list, before := tags()
	if before == "" || list != before {
		t.Fatalf("list %q and analytics %q tags differ", list, before)
	}
	// Later in the same week the tag holds, so an unchanged poll is still 304.
	analyticsNow = func() time.Time { return time.Date(2026, 10, 25, 23, 59, 30, 0, time.UTC) }
	cached := authed("/api/analytics")
	cached.header = map[string]string{"If-None-Match": before}
	if w := do(t, h, cached); w.Code != http.StatusNotModified {
		t.Fatalf("same-week poll: %d", w.Code)
	}

	// Past the boundary the old tag no longer matches and the new week is served.
	analyticsNow = func() time.Time { return time.Date(2026, 10, 26, 0, 1, 0, 0, time.UTC) }
	w := do(t, h, cached)
	if w.Code != http.StatusOK {
		t.Fatalf("poll after the week turned: %d", w.Code)
	}
	var v AnalyticsView
	decode(t, w, &v)
	if v.Summary.WeekStart != "2026-10-26" || v.Summary.CostThisWeek.TotalUSD != nil || v.ETag == before {
		t.Fatalf("after the boundary: week %s cost %v tag %q", v.Summary.WeekStart, v.Summary.CostThisWeek.TotalUSD, v.ETag)
	}
	list, after := tags()
	if after == before || list != after || v.ETag != after {
		t.Fatalf("tags after the boundary: list %q analytics %q body %q (before %q)", list, after, v.ETag, before)
	}
}
