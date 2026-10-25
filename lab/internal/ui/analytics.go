package ui

import (
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AnalyticsView is GET /api/analytics. It is built from the same bounded
// inventory pass as /api/molecules, so for one ETag its counts equal that list's
// and skipped ledgers are left out of both. A missing value is null and reads as
// Not recorded or Unknown, never as zero; each total carries its coverage.
type AnalyticsView struct {
	Sample          bool            `json:"sample"`
	ETag            string          `json:"etag"`
	Complete        bool            `json:"complete"`
	Truncated       bool            `json:"truncated"`
	Diagnostics     []string        `json:"diagnostics"`
	Molecules       int             `json:"molecules"`
	Counts          map[string]int  `json:"counts"`
	Time            TimeView        `json:"time"`
	Models          BreakdownView   `json:"models"`
	Cost            CostView        `json:"cost"`
	CostPerMergedPR UnavailableView `json:"costPerMergedPr"`
}

// DurationView is one time measure summed over the Molecules that recorded it:
// Recorded of Molecules. TotalMS is null when none recorded it or the sum overflowed.
type DurationView struct {
	TotalMS   *int64 `json:"totalMs"`
	Recorded  int    `json:"recorded"`
	Molecules int    `json:"molecules"`
	Overflow  bool   `json:"overflow,omitempty"`
}

// TimeView keeps wall-clock, active Atom and waiting time apart. Within a Molecule,
// overlapping Atom intervals are merged, never summed; the totals are Molecule
// time, and ElapsedMS is the union of the recorded wall-clock intervals.
type TimeView struct {
	WallClock  DurationView `json:"wallClock"`
	ActiveAtom DurationView `json:"activeAtom"`
	Waiting    DurationView `json:"waiting"`
	ElapsedMS  *int64       `json:"elapsedMs"`
	Note       string       `json:"note"`
}

// Group states: a value that is recorded, one recorded but unidentifiable, and
// one absent from the ledger.
const (
	stateRecorded    = "recorded"
	stateUnknown     = "unknown"
	stateNotRecorded = "not-recorded"
)

// ModelGroupView is the harness attempts of one harness binding and model.
type ModelGroupView struct {
	Harness       string   `json:"harness"`
	HarnessState  string   `json:"harnessState"`
	Model         string   `json:"model"`
	ModelState    string   `json:"modelState"`
	Attempts      int      `json:"attempts"`
	CostReported  int      `json:"costReported"`
	CostUSD       *float64 `json:"costUsd"`
	CostNotFinite bool     `json:"costNotFinite,omitempty"`
	CostBasis     string   `json:"costBasis"`
}

// BreakdownView groups every harness attempt exactly once. When the groups exceed
// their bound, Truncated is set and Ungrouped counts the attempts left out.
type BreakdownView struct {
	Attempts  int              `json:"attempts"`
	Groups    []ModelGroupView `json:"groups"`
	Truncated bool             `json:"truncated"`
	Ungrouped int              `json:"ungrouped"`
}

// CostView is the reported cost with its coverage. TotalUSD is null when no
// attempt reported a cost or the sum is not finite.
type CostView struct {
	TotalUSD      *float64 `json:"totalUsd"`
	NotFinite     bool     `json:"notFinite,omitempty"`
	Basis         string   `json:"basis"`
	BasisLabel    string   `json:"basisLabel"`
	Reported      int      `json:"reported"`
	Attempts      int      `json:"attempts"`
	UsageReported int      `json:"usageReported"`
	Coverage      string   `json:"coverage"`
	Note          string   `json:"note"`
}

// UnavailableView is a value the Lab cannot establish; Value is always null.
type UnavailableView struct {
	State  string   `json:"state"`
	Value  *float64 `json:"value"`
	Reason string   `json:"reason"`
}

const (
	// maxModelGroups bounds the breakdown; attempts past it are counted, not merged.
	maxModelGroups = 64
	// maxGroupLabel bounds a harness or model label, in runes.
	maxGroupLabel = 120
)

// moleculeFacts is what analytics needs from one ledger, read with its Summary
// and cached under the same stamp. Times are Unix milliseconds.
type moleculeFacts struct {
	wallOK             bool
	wallStart, wallEnd int64
	activeOK           bool
	activeMS           int64
	attempts           []attemptFact
}

// attemptFact is one harness attempt: its group, whether it recorded usage, and
// its reported cost with a bounded, redacted basis.
type attemptFact struct {
	harness, harnessState string
	model, modelState     string
	usage                 bool
	cost                  *float64
	basis                 string
}

// analyzeRecord derives the analytics facts of one Molecule. Every attempt in
// molecule.UsageSummary appears once, so retries are their own entries.
func analyzeRecord(lab string, r molecule.Record) *moleculeFacts {
	x := newRedactor(lab, r)
	f := &moleculeFacts{}
	start, okStart := parseTime(r.StartedAt)
	end, okEnd := parseTime(r.FinishedAt)
	if okStart && okEnd && end >= start {
		f.wallOK, f.wallStart, f.wallEnd = true, start, end
	}
	f.activeMS, f.activeOK = activeTime(r.Atoms)
	executed := map[string]string{}
	for _, a := range r.Atoms {
		if a.Provenance == nil {
			continue
		}
		for _, act := range a.Provenance.Actions {
			if act.ExecutedBinding != "" {
				executed[act.AttemptID] = act.ExecutedBinding
			}
		}
	}
	usage := molecule.UsageSummary(r)
	coding, review := 0, 0
	for _, u := range usage.Attempts {
		var binding, pinned string
		switch u.Kind {
		case "coding":
			if coding < len(r.HarnessAttempts) {
				binding, pinned = executed[r.HarnessAttempts[coding].ID], r.HarnessAttempts[coding].Model
			}
			coding++
		case "review":
			if review < len(r.ReviewAttempts) {
				binding = r.ReviewAttempts[review].Binding
			}
			review++
		}
		a := attemptFact{usage: u.Reported, cost: u.CostUSD, basis: harness.ReviewDisplayText(x.text(u.CostBasis), maxGroupLabel)}
		a.harness, a.harnessState = groupLabel(x, binding)
		switch {
		case u.Reported && len(u.EffectiveModels) > 0:
			a.model, a.modelState = modelsLabel(x, u.EffectiveModels, u.TruncatedModels)
		case u.Reported:
			// Usage was recorded but named no model.
			a.model, a.modelState = "Unknown", stateUnknown
		case pinned == "model unpinned":
			a.model, a.modelState = "Unknown", stateUnknown
		default:
			a.model, a.modelState = groupLabel(x, pinned)
		}
		f.attempts = append(f.attempts, a)
	}
	return f
}

// groupLabel redacts and then bounds a recorded value (redacting first, so a long
// local path is labelled before truncation can cut it); an absent value is Not recorded
// and one with nothing displayable left is Unknown.
func groupLabel(x redactor, raw string) (string, string) {
	if raw == "" {
		return "Not recorded", stateNotRecorded
	}
	if s := harness.ReviewDisplayText(x.text(raw), maxGroupLabel); s != "" {
		return s, stateRecorded
	}
	return "Unknown", stateUnknown
}

func modelsLabel(x redactor, models []string, truncated int) (string, string) {
	items := []string{}
	for _, m := range models {
		if s := harness.ReviewDisplayText(x.text(m), maxGroupLabel); s != "" {
			items = append(items, s)
		}
	}
	if len(items) == 0 {
		return "Unknown", stateUnknown
	}
	label := harness.ReviewDisplayText(strings.Join(items, ", "), 2*maxGroupLabel)
	if truncated > 0 {
		label += fmt.Sprintf(" (+%d more)", truncated)
	}
	return label, stateRecorded
}

// parseTime reads a recorded RFC 3339 timestamp as Unix milliseconds.
func parseTime(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}

// activeTime is the length of the union of the Atoms' [StartedAt, FinishedAt]
// intervals, so parallel Atoms are counted once. An Atom with no timestamps has
// not run and is skipped; one with a missing, unparseable or reversed timestamp
// makes the measure not recorded rather than understated.
func activeTime(atoms []molecule.Atom) (int64, bool) {
	var spans [][2]int64
	for _, a := range atoms {
		if a.StartedAt == "" && a.FinishedAt == "" {
			continue
		}
		start, okStart := parseTime(a.StartedAt)
		end, okEnd := parseTime(a.FinishedAt)
		if !okStart || !okEnd || end < start {
			return 0, false
		}
		spans = append(spans, [2]int64{start, end})
	}
	if len(spans) == 0 {
		return 0, false
	}
	return unionMS(spans)
}

// unionMS is the total length of the union of spans; ok is false on overflow.
func unionMS(spans [][2]int64) (int64, bool) {
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	var total int64
	ok := true
	cur := spans[0]
	for _, s := range spans[1:] {
		if s[0] > cur[1] {
			total, ok = addMS(total, cur[1]-cur[0], ok)
			cur = s
		} else if s[1] > cur[1] {
			cur[1] = s[1]
		}
	}
	total, ok = addMS(total, cur[1]-cur[0], ok)
	return total, ok
}

func addMS(total, v int64, ok bool) (int64, bool) {
	if !ok || v < 0 || total > math.MaxInt64-v {
		return total, false
	}
	return total + v, true
}

// durationSum accumulates one measure over Molecules with overflow detection.
type durationSum struct {
	view DurationView
	sum  int64
	ok   bool
}

func (d *durationSum) add(v int64, recorded bool) {
	d.view.Molecules++
	if !recorded {
		return
	}
	if d.view.Recorded == 0 {
		d.ok = true
	}
	d.view.Recorded++
	d.sum, d.ok = addMS(d.sum, v, d.ok)
}

func (d durationSum) result() DurationView {
	v := d.view
	switch {
	case v.Recorded > 0 && !d.ok:
		v.Overflow = true
	case v.Recorded > 0:
		sum := d.sum
		v.TotalMS = &sum
	}
	return v
}

// basisSet is the aggregate cost basis: "" when none was carried, the one shared
// basis, or "mixed" when the bases differ or only some carry one.
type basisSet struct {
	first      string
	set, mixed bool
}

func (b *basisSet) add(basis string) {
	switch {
	case !b.set:
		b.first, b.set = basis, true
	case basis != b.first:
		b.mixed = true
	}
}

func (b basisSet) value() string {
	if b.mixed {
		return "mixed"
	}
	return b.first
}

// costSum accumulates reported costs; a sum that is not finite is unavailable.
type costSum struct {
	reported  int
	sum       float64
	notFinite bool
	basis     basisSet
}

func (c *costSum) add(cost *float64, basis string) {
	if cost == nil {
		return
	}
	c.reported++
	c.basis.add(basis)
	next := c.sum + *cost
	if math.IsInf(next, 0) || math.IsNaN(next) {
		c.notFinite = true
		return
	}
	c.sum = next
}

func (c costSum) total() *float64 {
	if c.reported == 0 || c.notFinite {
		return nil
	}
	sum := c.sum
	return &sum
}

type groupKey struct{ harness, harnessState, model, modelState string }

type groupSum struct {
	key      groupKey
	attempts int
	cost     costSum
}

// buildAnalytics derives the analytics of one inventory. It only reads inv, which
// concurrent requests may share.
func buildAnalytics(inv inventory) AnalyticsView {
	list := inv.list
	out := AnalyticsView{Sample: list.Sample, ETag: list.ETag, Complete: list.Complete, Truncated: list.Truncated, Diagnostics: list.Diagnostics, Molecules: len(list.Molecules), Counts: list.Counts}
	if out.Diagnostics == nil {
		out.Diagnostics = []string{}
	}
	var wall, active, waiting durationSum
	var spans [][2]int64
	var cost costSum
	attempts, usageReported := 0, 0
	groups := map[groupKey]*groupSum{}
	var order []*groupSum
	breakdown := BreakdownView{Groups: []ModelGroupView{}}
	for _, m := range list.Molecules {
		f := inv.facts[m.ID]
		if f == nil {
			f = &moleculeFacts{}
		}
		wall.add(f.wallEnd-f.wallStart, f.wallOK)
		active.add(f.activeMS, f.activeOK)
		waitOK := f.wallOK && f.activeOK && f.activeMS <= f.wallEnd-f.wallStart
		waiting.add(f.wallEnd-f.wallStart-f.activeMS, waitOK)
		if f.wallOK {
			spans = append(spans, [2]int64{f.wallStart, f.wallEnd})
		}
		for _, a := range f.attempts {
			attempts++
			if a.usage {
				usageReported++
			}
			cost.add(a.cost, a.basis)
			key := groupKey{a.harness, a.harnessState, a.model, a.modelState}
			g := groups[key]
			if g == nil {
				if len(order) >= maxModelGroups {
					breakdown.Truncated = true
					breakdown.Ungrouped++
					continue
				}
				g = &groupSum{key: key}
				groups[key] = g
				order = append(order, g)
			}
			g.attempts++
			g.cost.add(a.cost, a.basis)
		}
	}
	out.Time = TimeView{WallClock: wall.result(), ActiveAtom: active.result(), Waiting: waiting.result(),
		Note: "Totals sum each Molecule's own measure (Molecule time, not elapsed Lab time). Within a Molecule, overlapping Atom intervals are merged, never summed. Waiting is wall-clock minus active Atom time."}
	if len(spans) > 0 {
		if v, ok := unionMS(spans); ok {
			out.Time.ElapsedMS = &v
		}
	}
	for _, g := range order {
		breakdown.Groups = append(breakdown.Groups, ModelGroupView{Harness: g.key.harness, HarnessState: g.key.harnessState, Model: g.key.model, ModelState: g.key.modelState,
			Attempts: g.attempts, CostReported: g.cost.reported, CostUSD: g.cost.total(), CostNotFinite: g.cost.notFinite, CostBasis: g.cost.label()})
	}
	sort.SliceStable(breakdown.Groups, func(i, j int) bool {
		a, b := breakdown.Groups[i], breakdown.Groups[j]
		if a.Attempts != b.Attempts {
			return a.Attempts > b.Attempts
		}
		if a.Harness != b.Harness {
			return a.Harness < b.Harness
		}
		return a.Model < b.Model
	})
	breakdown.Attempts = attempts
	out.Models = breakdown
	out.Cost = CostView{TotalUSD: cost.total(), NotFinite: cost.notFinite, Basis: cost.basis.value(), BasisLabel: cost.label(), Reported: cost.reported, Attempts: attempts, UsageReported: usageReported,
		Coverage: fmt.Sprintf("cost reported for %d of %d harness attempts", cost.reported, attempts),
		Note:     "Only costs the harness reported are summed. A missing cost is unknown, never zero."}
	out.CostPerMergedPR = UnavailableView{State: "unavailable", Reason: "No verified merge attribution exists; a draft publication receipt does not qualify."}
	return out
}

// label is the display label of the aggregate basis, or unavailable when no
// attempt reported a cost. Bases were bounded and redacted when read.
func (c costSum) label() string {
	if c.reported == 0 {
		return "unavailable"
	}
	return molecule.CostBasisLabel(c.basis.value())
}

// analytics serves GET /api/analytics with the list's ETag.
func (h *handler) analytics(w http.ResponseWriter, r *http.Request) {
	if h.opts.Demo {
		h.json(w, http.StatusOK, demoAnalytics())
		return
	}
	etag := ledgerETag(h.opts.LabDir)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.json(w, http.StatusOK, buildAnalytics(h.inventory(etag)))
}
