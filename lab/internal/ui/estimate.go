package ui

import (
	"fmt"
	"math"
	"sort"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// Estimates ("usually 30–45 min", "~$4") come only from this Lab's own
// history: the median and interquartile range of the active Atom time (the
// analytics activeMS, not wall time including time waiting for you) and the
// reported cost of the most recent comparable finished Molecules. Comparable
// means the same repository and Formula id, in the Finished lane (a published
// draft receipt) and with its active time recorded; the Decision, Running,
// Blocked, Other and Retired lanes never count. An estimate is hidden below
// minEstimateBasis Molecules, always names its basis, and is never zero-filled:
// cost uses only attempts that reported one and says when coverage is partial.
const (
	// estimateWindow is how many of the most recent comparable Molecules count.
	estimateWindow = 20
	// minEstimateBasis is the fewest comparable Molecules an estimate needs.
	minEstimateBasis = 5
)

// EstimateView is the estimate shown beside a decision. Shown is false, with
// Time and Cost absent, when the basis is too small.
type EstimateView struct {
	Shown     bool           `json:"shown"`
	Molecules int            `json:"molecules"`
	Basis     string         `json:"basis"`
	Time      *TimeRangeView `json:"time,omitempty"`
	Cost      *CostRangeView `json:"cost,omitempty"`
	Note      string         `json:"note"`
}

// TimeRangeView is the active-time quartiles in milliseconds: Low is the first
// quartile, High the third.
type TimeRangeView struct {
	LowMS    int64 `json:"lowMs"`
	MedianMS int64 `json:"medianMs"`
	HighMS   int64 `json:"highMs"`
}

// CostRangeView is the reported-cost quartiles in USD over the Molecules that
// reported a cost. Partial means some attempts of the basis reported none, so
// the cost undercounts them; Coverage says how many.
type CostRangeView struct {
	LowUSD    float64 `json:"lowUsd"`
	MedianUSD float64 `json:"medianUsd"`
	HighUSD   float64 `json:"highUsd"`
	Molecules int     `json:"molecules"`
	Partial   bool    `json:"partial"`
	Coverage  string  `json:"coverage"`
}

// estimateNote is said with every shown estimate: it is history, not a promise.
const estimateNote = "From this Lab's own history; not a promise."

// comparableTo reports a Summary that counts toward an estimate for a Molecule
// of repo and formulaID, other than self.
func comparableTo(s Summary, f *moleculeFacts, repo, formulaID, self string) bool {
	return s.ID != self && s.Repository == repo && s.FormulaID == formulaID && repo != "" && formulaID != "" && f != nil && f.activeOK && f.activeMS >= 0 && s.Lane == LaneFinished
}

// estimateBasis is the basis label of an estimate over n Molecules.
func estimateBasis(n int) string {
	return fmt.Sprintf("based on your last %d finished Molecules · active time, not time waiting for you", n)
}

// estimate derives the estimate for self from one inventory. It only reads inv.
func estimate(inv inventory, repo, formulaID, self string) EstimateView {
	type sample struct {
		id    string
		end   int64
		facts *moleculeFacts
	}
	var all []sample
	for _, s := range inv.list.Molecules {
		f := inv.facts[s.ID]
		if comparableTo(s, f, repo, formulaID, self) {
			// Recency is the finish time; one without a recorded wall time sorts oldest.
			end := int64(math.MinInt64)
			if f.wallOK {
				end = f.wallEnd
			}
			all = append(all, sample{id: s.ID, end: end, facts: f})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].end != all[j].end {
			return all[i].end > all[j].end
		}
		return all[i].id < all[j].id
	})
	if len(all) > estimateWindow {
		all = all[:estimateWindow]
	}
	n := len(all)
	if n < minEstimateBasis {
		return EstimateView{Molecules: n, Note: fmt.Sprintf("Estimates appear once this Lab has published drafts for %d comparable Molecules.", minEstimateBasis)}
	}
	active := make([]float64, 0, n)
	var costs []float64
	attempts, reported := 0, 0
	for _, s := range all {
		active = append(active, float64(s.facts.activeMS))
		sum, seen := 0.0, false
		for _, a := range s.facts.attempts {
			attempts++
			if a.cost == nil || math.IsNaN(*a.cost) || math.IsInf(*a.cost, 0) || *a.cost < 0 {
				continue
			}
			reported++
			sum, seen = sum+*a.cost, true
		}
		if seen && !math.IsInf(sum, 0) {
			costs = append(costs, sum)
		}
	}
	v := EstimateView{Shown: true, Molecules: n, Basis: estimateBasis(n), Note: estimateNote}
	low, mid, high := quartiles(active)
	v.Time = &TimeRangeView{LowMS: int64(math.Round(low)), MedianMS: int64(math.Round(mid)), HighMS: int64(math.Round(high))}
	if len(costs) >= minEstimateBasis {
		low, mid, high := quartiles(costs)
		v.Cost = &CostRangeView{LowUSD: low, MedianUSD: mid, HighUSD: high, Molecules: len(costs), Partial: reported < attempts || len(costs) < n,
			Coverage: fmt.Sprintf("cost reported for %d of %d harness attempts", reported, attempts)}
	}
	return v
}

// quartiles is the first quartile, median and third quartile of values by linear
// interpolation between closest ranks. values is not empty.
func quartiles(values []float64) (float64, float64, float64) {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	at := func(q float64) float64 {
		pos := q * float64(len(sorted)-1)
		lo := int(math.Floor(pos))
		hi := int(math.Ceil(pos))
		return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
	}
	return at(0.25), at(0.5), at(0.75)
}

// StatsView is GET /api/molecules/{id}/stats: the diff size and the estimate a
// decision card shows. Both are read-only; the diff size runs git diff
// --numstat on demand through the engine, and the estimate reads the same
// bounded inventory as /api/molecules.
type StatsView struct {
	Diff     DiffSizeView `json:"diff"`
	Estimate EstimateView `json:"estimate"`
}

func (h *handler) stats(r molecule.Record) StatsView {
	inv := h.inventory(ledgerETag(h.opts.LabDir))
	return StatsView{Diff: h.diffSize(h.jobs.ctx, r), Estimate: estimate(inv, r.Issue.Repo, r.FormulaID, r.ID)}
}
