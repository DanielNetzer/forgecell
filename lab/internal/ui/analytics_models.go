package ui

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"math"
	"sort"
	"time"
)

// The Insights view (#261): how each effective model performs in each Atom role,
// a weekly series, where the time goes and the summary tiles. Every value comes
// from this Lab's ledgers through the same inventory as the rest of the
// analytics; a value no ledger records is null, never zero, and each one carries
// its coverage.

// Atom roles of the by-model table. Meta and Formula-improvement Atoms record no
// usage in Molecule ledgers, so the meta role is reported as unavailable.
const (
	roleAnalysis = "analysis"
	roleCoding   = "coding"
	roleReview   = "review"
)

// Check outcomes of a coding attempt's captured tree.
const (
	checksPassed = "passed"
	checksFailed = "failed"
)

const (
	// maxModelRoleRows bounds the by-model table; attempts past it are counted, not merged.
	maxModelRoleRows = 64
	// maxMedianSamples bounds the values one row keeps for a median; past it the
	// median is unavailable rather than taken from a sample.
	maxMedianSamples = 10000
	// maxWeeks bounds the weekly series to the most recent weeks with data.
	maxWeeks = 12
	// maxSeries bounds the stacked models; the rest are summed as Other models.
	maxSeries = 6
	weekMS    = 7 * 24 * int64(time.Hour/time.Millisecond)
)

// otherModels is the series label of models folded past maxSeries.
const otherModels = "Other models"

// noCodingAttempt is the series label of a finished Molecule that had no coding
// attempt, kept apart from Not recorded, which is a model the ledger left out.
const noCodingAttempt = "No coding attempt"

// maxFutureMS is how far past now a recorded time may lie and still be placed in
// a week; a later one is malformed and counted as undated, so it can neither move
// the chart off the current week nor count toward this week.
const maxFutureMS = 24 * int64(time.Hour/time.Millisecond)

// timeSplitFact is one Molecule's wall-clock split into active Atom time, waiting
// on checks and waiting for you. Verification intervals are carved out of the
// merged Atom intervals, so the three parts sum to the wall-clock.
type timeSplitFact struct {
	ok                      bool
	active, checks, waiting int64
}

// ModelRoleView is one effective model in one Atom role. A nil value was not
// reported by any of the row's attempts; the *Reported counts are its coverage.
type ModelRoleView struct {
	Role          string   `json:"role"`
	Model         string   `json:"model"`
	ModelState    string   `json:"modelState"`
	Attempts      int      `json:"attempts"`
	Molecules     int      `json:"molecules"`
	CostReported  int      `json:"costReported"`
	CostUSD       *float64 `json:"costUsd"`
	CostNotFinite bool     `json:"costNotFinite,omitempty"`
	MedianCostUSD *float64 `json:"medianCostUsd"`
	UsageReported int      `json:"usageReported"`
	TokensIn      *int64   `json:"tokensIn"`
	TokensOut     *int64   `json:"tokensOut"`
	TokensCache   *int64   `json:"tokensCacheRead"`
	TokenOverflow bool     `json:"tokenOverflow,omitempty"`
	WallReported  int      `json:"wallReported"`
	MedianWallMS  *int64   `json:"medianWallMs"`
	// MedianBounded is set when a row held more values than maxMedianSamples.
	MedianBounded bool `json:"medianBounded,omitempty"`
	// Coding: attempts whose captured tree was checked, and those that passed.
	ChecksRan    int      `json:"checksRan"`
	ChecksPassed int      `json:"checksPassed"`
	PassRate     *float64 `json:"passRate"`
	// Review: passes that recorded findings, and the findings raised.
	ReviewsReported int           `json:"reviewsReported"`
	Findings        *FindingsView `json:"findings"`
	// Analysis: attempts that recorded a response, their clarifying questions, and
	// the questions per Molecule over the QuestionMolecules that recorded one.
	QuestionsReported    int      `json:"questionsReported"`
	Questions            *int     `json:"questions"`
	QuestionMolecules    int      `json:"questionMolecules"`
	QuestionsPerMolecule *float64 `json:"questionsPerMolecule"`
}

// FindingsView is the review findings raised, by severity.
type FindingsView struct {
	High   int `json:"high"`
	Medium int `json:"medium"`
	Low    int `json:"low"`
}

// ByModelView holds every attempt exactly once: the rows' attempts plus
// Ungrouped equal Attempts.
type ByModelView struct {
	Attempts  int             `json:"attempts"`
	Rows      []ModelRoleView `json:"rows"`
	Truncated bool            `json:"truncated"`
	Ungrouped int             `json:"ungrouped"`
	Meta      UnavailableView `json:"meta"`
	Note      string          `json:"note"`
}

// WeekView is one week (Monday, UTC) of the weekly series. Cost and Finished are
// aligned with WeeklyView.Series; a nil cost had no reported cost that week.
type WeekView struct {
	Start         string     `json:"start"`
	Cost          []*float64 `json:"cost"`
	CostTotal     *float64   `json:"costTotal"`
	CostReported  int        `json:"costReported"`
	CostNotFinite bool       `json:"costNotFinite,omitempty"`
	Finished      []int      `json:"finished"`
	FinishedTotal int        `json:"finishedTotal"`
}

// WeeklyView is reported cost (by attempt start) and Molecules finished (by
// finish time) per week, stacked by model. Truncated marks weeks left out past
// maxWeeks and Folded models summed as Other models; Undated counts costs and
// finished Molecules with no readable time, which no week shows.
type WeeklyView struct {
	Series          []string   `json:"series"`
	Weeks           []WeekView `json:"weeks"`
	Truncated       bool       `json:"truncated"`
	Folded          bool       `json:"folded"`
	UndatedCost     int        `json:"undatedCost"`
	UndatedFinished int        `json:"undatedFinished"`
	Note            string     `json:"note"`
}

// TimeSplitView is where the Molecules' wall-clock went. The three measures share
// one coverage: the Molecules whose wall-clock, Atom and check times all parse.
type TimeSplitView struct {
	Active          DurationView `json:"active"`
	WaitingForYou   DurationView `json:"waitingForYou"`
	WaitingOnChecks DurationView `json:"waitingOnChecks"`
	Note            string       `json:"note"`
}

// CostTileView is a reported cost with its coverage: Reported of Attempts.
type CostTileView struct {
	TotalUSD  *float64 `json:"totalUsd"`
	NotFinite bool     `json:"notFinite,omitempty"`
	Reported  int      `json:"reported"`
	Attempts  int      `json:"attempts"`
}

// RateView is Count of Of, with Value their share; Value is null when Of is 0.
// Molecules is how many Molecules the rate could have covered; Unchecked of them
// are left out of Of because the measure was not recorded for them.
type RateView struct {
	Value     *float64 `json:"value"`
	Count     int      `json:"count"`
	Of        int      `json:"of"`
	Molecules int      `json:"molecules"`
	Unchecked int      `json:"unchecked"`
}

// InsightsSummaryView is the four summary tiles.
type InsightsSummaryView struct {
	WeekStart         string          `json:"weekStart"`
	CostThisWeek      CostTileView    `json:"costThisWeek"`
	CostTotal         CostTileView    `json:"costTotal"`
	MoleculesFinished int             `json:"moleculesFinished"`
	TimeToDraftPR     UnavailableView `json:"timeToDraftPr"`
	FirstTryGreen     RateView        `json:"firstTryGreen"`
}

// PlanningView is planning effort: clarifying questions per Molecule and the
// analysis cost. QuestionsPerMolecule is Questions over the Molecules (of Of
// with an analysis attempt) that recorded an analysis response.
type PlanningView struct {
	Questions            int          `json:"questions"`
	Molecules            int          `json:"molecules"`
	Of                   int          `json:"of"`
	QuestionsPerMolecule *float64     `json:"questionsPerMolecule"`
	Cost                 CostTileView `json:"cost"`
}

// reportedCost is a usage line's cost as analytics counts it. A negative cost is
// malformed and is rejected here, at ingest, so it reads as not reported in both
// the coverage and every total.
func reportedCost(c *float64) *float64 {
	if c == nil || *c < 0 {
		return nil
	}
	v := *c
	return &v
}

// attemptIndex pairs a usage entry of one kind with the ledger attempt it
// describes: by its attempt id when exactly one attempt of that kind carries it,
// otherwise by its ordinal among the entries of that kind. Never by position in
// the whole list, so a missing or extra attempt of one kind shifts no other kind.
type attemptIndex struct {
	byID    map[string]int
	ordinal int
	n       int
}

func newAttemptIndex(ids []string) *attemptIndex {
	x := &attemptIndex{byID: map[string]int{}, n: len(ids)}
	for i, id := range ids {
		if _, dup := x.byID[id]; dup {
			x.byID[id] = -1
			continue
		}
		x.byID[id] = i
	}
	return x
}

// next is the index of the attempt the next entry of this kind, with id, describes.
func (x *attemptIndex) next(id string) (int, bool) {
	k := x.ordinal
	x.ordinal++
	if i, ok := x.byID[id]; ok && i >= 0 && id != "" {
		return i, true
	}
	return k, k < x.n
}

// enrichFacts adds the by-model facts of r to the attempts analyzeRecord derived,
// pairing each with its ledger attempt by kind and attempt id. An attempt of an
// unknown kind, or one with no ledger attempt to pair with, keeps no role, so it
// is counted as ungrouped rather than merged into a row.
func enrichFacts(r molecule.Record, f *moleculeFacts) {
	f.split = splitTime(r, f)
	trees := checkedTrees(r.VerificationAttempts)
	actions := map[string]molecule.AtomAction{}
	for _, a := range r.Atoms {
		if a.Provenance == nil {
			continue
		}
		for _, act := range a.Provenance.Actions {
			if _, seen := actions[act.AttemptID]; !seen && act.AttemptID != "" {
				actions[act.AttemptID] = act
			}
		}
	}
	ids := func(n int, id func(int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = id(i)
		}
		return out
	}
	analysis := newAttemptIndex(ids(len(r.AnalysisAttempts), func(i int) string { return fmt.Sprintf("analysis-%d", r.AnalysisAttempts[i].Ordinal) }))
	coding := newAttemptIndex(ids(len(r.HarnessAttempts), func(i int) string { return r.HarnessAttempts[i].ID }))
	review := newAttemptIndex(ids(len(r.ReviewAttempts), func(i int) string { return fmt.Sprintf("review-%d", r.ReviewAttempts[i].Ordinal) }))
	for k := range f.attempts {
		x := &f.attempts[k]
		switch x.kind {
		case roleAnalysis:
			i, ok := analysis.next(x.id)
			if !ok {
				continue
			}
			a := r.AnalysisAttempts[i]
			x.role = roleAnalysis
			x.startMS, x.startOK = parseTime(a.StartedAt)
			x.wallMS, x.wallOK = interval(a.StartedAt, a.FinishedAt)
			if !x.wallOK && a.Diagnostics != nil && a.Diagnostics.ElapsedMS > 0 {
				x.wallMS, x.wallOK = a.Diagnostics.ElapsedMS, true
			}
			usageFacts(x, a.Usage)
			if a.Response != nil {
				x.questionsOK, x.questions = true, len(a.Response.Questions)
			}
		case roleCoding:
			i, ok := coding.next(x.id)
			if !ok {
				continue
			}
			a := r.HarnessAttempts[i]
			x.role = roleCoding
			act, ok := actions[a.ID]
			if ok {
				x.startMS, x.startOK = parseTime(act.StartedAt)
			}
			switch {
			case a.Result.ElapsedMS > 0:
				x.wallMS, x.wallOK = a.Result.ElapsedMS, true
			case ok && act.Observed != nil:
				x.wallMS, x.wallOK = interval(act.StartedAt, act.Observed.FinishedAt)
			}
			usageFacts(x, a.Usage)
			if a.Capture != nil && a.Capture.Tree != "" {
				t := trees[a.Capture.Tree]
				x.checks, x.firstCheck = t.any, t.first
			}
		case roleReview:
			i, ok := review.next(x.id)
			if !ok {
				continue
			}
			a := r.ReviewAttempts[i]
			x.role = roleReview
			x.startMS, x.startOK = parseTime(a.StartedAt)
			x.wallMS, x.wallOK = interval(a.StartedAt, a.FinishedAt)
			if !x.wallOK && a.Diagnostics != nil && a.Diagnostics.ElapsedMS > 0 {
				x.wallMS, x.wallOK = a.Diagnostics.ElapsedMS, true
			}
			usageFacts(x, a.Usage)
			if (a.Status == molecule.ReviewNoHighFindings || a.Status == molecule.ReviewBlocked) && a.High >= 0 && a.Medium >= 0 && a.Low >= 0 {
				x.findingsOK, x.high, x.medium, x.low = true, a.High, a.Medium, a.Low
			}
		}
	}
}

// usageFacts copies an attempt's token totals, and its reported duration when no
// recorded interval gave one. Negative values are malformed and read as not
// reported.
func usageFacts(x *attemptFact, u *harness.Usage) {
	if u == nil {
		return
	}
	x.tokens = harness.TokenCategories{Input: nonNegative(u.Total.Input), Output: nonNegative(u.Total.Output), CacheRead: nonNegative(u.Total.CacheRead)}
	if !x.wallOK && u.DurationMS != nil && *u.DurationMS >= 0 {
		x.wallMS, x.wallOK = *u.DurationMS, true
	}
}

func nonNegative(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	c := *v
	return &c
}

// treeChecks is what the verification attempts on one source tree concluded. any
// is passed when any attempt passed, or failed when attempts finished and none
// passed. first is the outcome of the earliest attempt alone: passed or failed as
// it concluded, failed when it did not conclude but the tree was checked again,
// and "" when it is the only attempt and has not concluded.
type treeChecks struct {
	any, first string
}

// checkedTrees maps each verified source tree to its treeChecks. The earliest
// attempt is the one with the earliest start when the starts compared are
// readable; ties and unreadable starts keep ledger order, the order attempts are
// appended in.
func checkedTrees(attempts []molecule.VerificationAttempt) map[string]treeChecks {
	out := map[string]treeChecks{}
	type earliest struct {
		status  string
		startOK bool
		start   int64
		runs    int
	}
	firsts := map[string]*earliest{}
	for _, v := range attempts {
		if v.SourceTree == "" {
			continue
		}
		t := out[v.SourceTree]
		switch v.Status {
		case checksPassed:
			t.any = checksPassed
		case checksFailed:
			if t.any == "" {
				t.any = checksFailed
			}
		}
		out[v.SourceTree] = t
		start, ok := parseTime(v.StartedAt)
		e := firsts[v.SourceTree]
		switch {
		case e == nil:
			firsts[v.SourceTree] = &earliest{status: v.Status, startOK: ok, start: start, runs: 1}
			continue
		case ok && e.startOK && start < e.start:
			e.status, e.start = v.Status, start
		}
		e.runs++
	}
	for tree, e := range firsts {
		t := out[tree]
		switch {
		case e.status == checksPassed || e.status == checksFailed:
			t.first = e.status
		case e.runs > 1:
			t.first = checksFailed
		}
		out[tree] = t
	}
	return out
}

// interval is the length of a recorded [start, end] interval.
func interval(start, end string) (int64, bool) {
	s, okS := parseTime(start)
	e, okE := parseTime(end)
	if !okS || !okE || e < s {
		return 0, false
	}
	return e - s, true
}

// splitTime splits a finished Molecule's wall-clock. Every Atom that ran and every
// verification attempt must have a readable interval inside the wall-clock, or the
// split is not recorded rather than understated.
func splitTime(r molecule.Record, f *moleculeFacts) timeSplitFact {
	if !f.wallOK {
		return timeSplitFact{}
	}
	var all, checks [][2]int64
	inside := func(start, end string) bool {
		s, okS := parseTime(start)
		e, okE := parseTime(end)
		if !okS || !okE || e < s || s < f.wallStart || e > f.wallEnd {
			return false
		}
		all = append(all, [2]int64{s, e})
		return true
	}
	for _, a := range r.Atoms {
		if a.StartedAt == "" && a.FinishedAt == "" {
			continue
		}
		if !inside(a.StartedAt, a.FinishedAt) {
			return timeSplitFact{}
		}
	}
	for _, v := range r.VerificationAttempts {
		if !inside(v.StartedAt, v.FinishedAt) {
			return timeSplitFact{}
		}
		checks = append(checks, all[len(all)-1])
	}
	if len(all) == 0 {
		return timeSplitFact{}
	}
	busy, ok := unionMS(all)
	if !ok {
		return timeSplitFact{}
	}
	var checked int64
	if len(checks) > 0 {
		if checked, ok = unionMS(checks); !ok {
			return timeSplitFact{}
		}
	}
	wall := f.wallEnd - f.wallStart
	if busy > wall || checked > busy {
		return timeSplitFact{}
	}
	return timeSplitFact{ok: true, active: busy - checked, checks: checked, waiting: wall - busy}
}

// tokenSum is one token category over a row, with overflow detection.
type tokenSum struct {
	sum                int64
	reported, overflow bool
}

func (t *tokenSum) add(v *int64) {
	if v == nil {
		return
	}
	t.reported = true
	if t.overflow || *v < 0 || t.sum > math.MaxInt64-*v {
		t.overflow = true
		return
	}
	t.sum += *v
}

func (t tokenSum) value() *int64 {
	if !t.reported || t.overflow {
		return nil
	}
	v := t.sum
	return &v
}

type roleKey struct{ role, model, modelState string }

type roleSum struct {
	key                 roleKey
	attempts, molecules int
	last                int
	usage               int
	cost                costSum
	costs               []float64
	walls               []int64
	bounded             bool
	in, out, cache      tokenSum
	checksRan, passed   int
	reviews             int
	findings            FindingsView
	questionsReported   int
	questions           int
	qLast, qMolecules   int
}

// add counts a, an attempt of the index-th listed Molecule.
func (s *roleSum) add(a attemptFact, index int) {
	s.attempts++
	if s.last != index {
		s.last = index
		s.molecules++
	}
	if a.usage {
		s.usage++
		s.in.add(a.tokens.Input)
		s.out.add(a.tokens.Output)
		s.cache.add(a.tokens.CacheRead)
	}
	s.cost.add(a.cost, a.basis)
	if a.cost != nil && !math.IsNaN(*a.cost) && !math.IsInf(*a.cost, 0) {
		if len(s.costs) < maxMedianSamples {
			s.costs = append(s.costs, *a.cost)
		} else {
			s.bounded = true
		}
	}
	if a.wallOK {
		if len(s.walls) < maxMedianSamples {
			s.walls = append(s.walls, a.wallMS)
		} else {
			s.bounded = true
		}
	}
	switch a.checks {
	case checksPassed:
		s.checksRan++
		s.passed++
	case checksFailed:
		s.checksRan++
	}
	if a.findingsOK {
		s.reviews++
		s.findings.High += a.high
		s.findings.Medium += a.medium
		s.findings.Low += a.low
	}
	if a.questionsOK {
		s.questionsReported++
		s.questions += a.questions
		if s.qLast != index {
			s.qLast = index
			s.qMolecules++
		}
	}
}

func (s *roleSum) view() ModelRoleView {
	v := ModelRoleView{Role: s.key.role, Model: s.key.model, ModelState: s.key.modelState, Attempts: s.attempts, Molecules: s.molecules,
		CostReported: s.cost.reported, CostUSD: s.cost.total(), CostNotFinite: s.cost.notFinite, UsageReported: s.usage,
		TokensIn: s.in.value(), TokensOut: s.out.value(), TokensCache: s.cache.value(), TokenOverflow: s.in.overflow || s.out.overflow || s.cache.overflow,
		WallReported: len(s.walls), MedianBounded: s.bounded, ChecksRan: s.checksRan, ChecksPassed: s.passed, ReviewsReported: s.reviews, QuestionsReported: s.questionsReported}
	if !s.bounded {
		v.MedianCostUSD = medianFloat(s.costs)
		v.MedianWallMS = medianInt(s.walls)
	}
	if s.key.role == roleCoding && s.checksRan > 0 {
		rate := float64(s.passed) / float64(s.checksRan)
		v.PassRate = &rate
	}
	if s.key.role == roleReview && s.reviews > 0 {
		f := s.findings
		v.Findings = &f
	}
	if s.key.role == roleAnalysis && s.questionsReported > 0 {
		q, per := s.questions, float64(s.questions)/float64(s.qMolecules)
		v.Questions, v.QuestionMolecules, v.QuestionsPerMolecule = &q, s.qMolecules, &per
	}
	return v
}

func medianFloat(values []float64) *float64 {
	if len(values) == 0 {
		return nil
	}
	v := append([]float64(nil), values...)
	sort.Float64s(v)
	m := v[len(v)/2]
	if len(v)%2 == 0 {
		m = v[len(v)/2-1]/2 + m/2
	}
	return &m
}

func medianInt(values []int64) *int64 {
	if len(values) == 0 {
		return nil
	}
	v := append([]int64(nil), values...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	m := v[len(v)/2]
	if len(v)%2 == 0 {
		lo := v[len(v)/2-1]
		m = lo + (m-lo)/2
	}
	return &m
}

var roleOrder = map[string]int{roleAnalysis: 0, roleCoding: 1, roleReview: 2}

// weekStart is the Monday 00:00 UTC that starts the week of ms.
func weekStart(ms int64) int64 {
	t := time.UnixMilli(ms).UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	back := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -back).UnixMilli()
}

func weekLabel(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }

// cell is one week and series of the weekly series.
type cell struct {
	week   int64
	series string
}

// buildInsights adds the Insights fields to out from inv. now only decides which
// week is "this week".
func buildInsights(out *AnalyticsView, inv inventory, now time.Time) {
	list := inv.list
	rows := map[roleKey]*roleSum{}
	var order []*roleSum
	by := ByModelView{Rows: []ModelRoleView{}}
	nowMS := now.UnixMilli()
	thisWeek := weekStart(nowMS)
	dated := func(ms int64, ok bool) bool { return ok && ms <= nowMS+maxFutureMS }
	var weekCost, totalCost, planCost costSum
	weekAttempts, planAttempts := 0, 0
	first := RateView{}
	planning := PlanningView{}
	var active, checks, waiting durationSum
	costCells := map[cell]*costSum{}
	finishedCells := map[cell]int{}
	weight := map[string]int{}
	weeks := map[int64]bool{}
	weekly := WeeklyView{Series: []string{}, Weeks: []WeekView{}}
	for n, m := range list.Molecules {
		f := inv.facts[m.ID]
		if f == nil {
			f = &moleculeFacts{}
		}
		active.add(f.split.active, f.split.ok)
		checks.add(f.split.checks, f.split.ok)
		waiting.add(f.split.waiting, f.split.ok)
		// The first coding attempt is the earliest started one; with an attempt
		// whose start is unreadable among several, which ran first is unknown.
		var firstCoding *attemptFact
		codings, firstKnown := 0, true
		analysed, responded, questions := false, false, 0
		lastModel := noCodingAttempt
		for k := range f.attempts {
			a := f.attempts[k]
			by.Attempts++
			totalCost.add(a.cost, a.basis)
			if dated(a.startMS, a.startOK) && weekStart(a.startMS) == thisWeek {
				weekAttempts++
				weekCost.add(a.cost, a.basis)
			}
			if a.cost != nil {
				if dated(a.startMS, a.startOK) {
					w := weekStart(a.startMS)
					weeks[w] = true
					c := costCells[cell{w, a.model}]
					if c == nil {
						c = &costSum{}
						costCells[cell{w, a.model}] = c
					}
					c.add(a.cost, a.basis)
					weight[a.model]++
				} else {
					weekly.UndatedCost++
				}
			}
			switch a.role {
			case roleCoding:
				codings++
				firstKnown = firstKnown && a.startOK
				if firstCoding == nil || a.startOK && firstCoding.startOK && a.startMS < firstCoding.startMS {
					firstCoding = &f.attempts[k]
				}
				lastModel = a.model
			case roleAnalysis:
				analysed = true
				planAttempts++
				planCost.add(a.cost, a.basis)
				if a.questionsOK {
					responded = true
					questions += a.questions
				}
			}
			if a.role == "" {
				// enrichFacts could not place the attempt.
				by.Truncated = true
				by.Ungrouped++
				continue
			}
			key := roleKey{a.role, a.model, a.modelState}
			s := rows[key]
			if s == nil {
				if len(order) >= maxModelRoleRows {
					by.Truncated = true
					by.Ungrouped++
					continue
				}
				s = &roleSum{key: key, last: -1, qLast: -1}
				rows[key] = s
				order = append(order, s)
			}
			s.add(a, n)
		}
		if codings > 0 {
			// Only a Molecule whose first coding attempt was checked is in the
			// rate, judged by the earliest check of that attempt's tree; the rest
			// are its coverage gap, never a first-try failure.
			first.Molecules++
			switch {
			case codings > 1 && !firstKnown, firstCoding.firstCheck == "":
				first.Unchecked++
			default:
				first.Of++
				if firstCoding.firstCheck == checksPassed {
					first.Count++
				}
			}
		}
		if analysed {
			planning.Of++
			if responded {
				planning.Molecules++
				planning.Questions += questions
			}
		}
		if m.Lane == LaneFinished {
			if end, ok := parseTime(m.FinishedAt); dated(end, ok) {
				w := weekStart(end)
				weeks[w] = true
				finishedCells[cell{w, lastModel}]++
				weight[lastModel]++
			} else {
				weekly.UndatedFinished++
			}
		}
	}
	for _, s := range order {
		by.Rows = append(by.Rows, s.view())
	}
	sort.SliceStable(by.Rows, func(i, j int) bool {
		a, b := by.Rows[i], by.Rows[j]
		if a.Role != b.Role {
			return roleOrder[a.Role] < roleOrder[b.Role]
		}
		if a.Attempts != b.Attempts {
			return a.Attempts > b.Attempts
		}
		return a.Model < b.Model
	})
	by.Meta = UnavailableView{State: "unavailable", Reason: "Meta and Formula-improvement Atoms record no usage in Molecule ledgers."}
	by.Note = "One row per effective model and Atom role. A missing value is not reported, never zero."
	out.ByModel = by

	out.Weekly = weeklySeries(weekly, thisWeek, weeks, weight, costCells, finishedCells)

	out.TimeSplit = TimeSplitView{Active: active.result(), WaitingOnChecks: checks.result(), WaitingForYou: waiting.result(),
		Note: "Per Molecule, overlapping Atom intervals are merged and check runs are carved out of them; waiting for you is the rest of the wall-clock."}

	if first.Of > 0 {
		v := float64(first.Count) / float64(first.Of)
		first.Value = &v
	}
	out.Summary = InsightsSummaryView{WeekStart: weekLabel(thisWeek),
		CostThisWeek:      CostTileView{TotalUSD: weekCost.total(), NotFinite: weekCost.notFinite, Reported: weekCost.reported, Attempts: weekAttempts},
		CostTotal:         CostTileView{TotalUSD: totalCost.total(), NotFinite: totalCost.notFinite, Reported: totalCost.reported, Attempts: by.Attempts},
		MoleculesFinished: list.Counts[LaneFinished],
		TimeToDraftPR:     UnavailableView{State: "unavailable", Reason: "The delivery receipt records no publication time."},
		FirstTryGreen:     first}
	if planning.Molecules > 0 {
		v := float64(planning.Questions) / float64(planning.Molecules)
		planning.QuestionsPerMolecule = &v
	}
	planning.Cost = CostTileView{TotalUSD: planCost.total(), NotFinite: planCost.notFinite, Reported: planCost.reported, Attempts: planAttempts}
	out.Planning = planning
}

// weeklySeries lays the cells out as consecutive weeks ending at the current week
// (or the next, for a time within maxFutureMS of now), at most maxWeeks of them,
// stacked by the maxSeries heaviest models and Other models. Weeks before the
// window are dropped and flagged as truncated.
func weeklySeries(w WeeklyView, thisWeek int64, weeks map[int64]bool, weight map[string]int, costs map[cell]*costSum, finished map[cell]int) WeeklyView {
	w.Note = "Cost is placed in the week its attempt started; a Molecule in the week it finished, under the model of its last coding attempt. A time more than a day after now is counted as undated."
	if len(weeks) == 0 {
		return w
	}
	lo, hi := thisWeek, thisWeek
	for k := range weeks {
		lo, hi = min(lo, k), max(hi, k)
	}
	if hi-lo >= maxWeeks*weekMS {
		w.Truncated = true
		lo = hi - (maxWeeks-1)*weekMS
	}
	models := make([]string, 0, len(weight))
	for m := range weight {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		if weight[models[i]] != weight[models[j]] {
			return weight[models[i]] > weight[models[j]]
		}
		return models[i] < models[j]
	})
	slot := map[string]int{}
	for i, m := range models {
		if i < maxSeries {
			slot[m] = i
			w.Series = append(w.Series, m)
			continue
		}
		if !w.Folded {
			w.Folded = true
			w.Series = append(w.Series, otherModels)
		}
		slot[m] = maxSeries
	}
	for start := lo; start <= hi; start += weekMS {
		wk := WeekView{Start: weekLabel(start), Cost: make([]*float64, len(w.Series)), Finished: make([]int, len(w.Series))}
		sums := make([]costSum, len(w.Series))
		var total costSum
		for _, m := range models {
			if c := costs[cell{start, m}]; c != nil {
				merge(&sums[slot[m]], c)
				merge(&total, c)
			}
			n := finished[cell{start, m}]
			wk.Finished[slot[m]] += n
			wk.FinishedTotal += n
		}
		for i := range sums {
			wk.Cost[i] = sums[i].total()
		}
		wk.CostTotal, wk.CostReported, wk.CostNotFinite = total.total(), total.reported, total.notFinite
		w.Weeks = append(w.Weeks, wk)
	}
	return w
}

// merge adds the reported costs of c to s.
func merge(s, c *costSum) {
	s.reported += c.reported
	s.notFinite = s.notFinite || c.notFinite
	next := s.sum + c.sum
	if math.IsInf(next, 0) || math.IsNaN(next) {
		s.notFinite = true
		return
	}
	s.sum = next
}
