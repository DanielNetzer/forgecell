package evaluation

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CohortAttempt is one recorded (task, variant, repetition) attempt. It embeds the
// existing single-task Attempt so every per-attempt field, verification tree and
// isolated-Lab record is retained exactly as today, with the cohort coordinates
// recorded alongside. A directory holding a complete, valid CohortAttempt counts as
// done on resume.
type CohortAttempt struct {
	Task       string `json:"task"`
	Variant    string `json:"variant"`
	Repetition int    `json:"repetition"`
	Attempt
}

// RateDenominators states the n behind every rate: the cohort shape and the number of
// attempts the rate is computed over. The report never claims significance; it reports
// counts and rates with their n so a reviewer reads them as feasibility evidence only.
type RateDenominators struct {
	Tasks       int `json:"tasks"`
	Repetitions int `json:"repetitions"`
	Attempts    int `json:"attempts"`
}

// VariantSummary aggregates one variant across the whole cohort. Counts and rates only.
type VariantSummary struct {
	Name          string `json:"name"`
	FormulaSHA256 string `json:"formulaSha256"`

	Attempts             int `json:"attempts"`
	Completions          int `json:"completions"`
	HiddenAcceptancePass int `json:"hiddenAcceptancePass"`
	RequiredChecksPass   int `json:"requiredChecksPass"`
	ScopeViolations      int `json:"scopeViolations"`
	ScopeChanges         int `json:"scopeChanges"`
	Amendments           int `json:"amendments"`
	Timeouts             int `json:"timeouts"`
	Failures             int `json:"failures"`

	ElapsedMedianMS int64 `json:"elapsedMedianMs"`
	ElapsedP90MS    int64 `json:"elapsedP90Ms"`
	HarnessMedianMS int64 `json:"harnessMedianMs"`
	HarnessP90MS    int64 `json:"harnessP90Ms"`

	// ReportedCostUSD is the total reported cost over the attempts that reported any;
	// nil when none reported (never rendered as zero). CostCoverage is the "N of M"
	// coverage and CostBasis labels the figure (for example a CLI list-price estimate).
	ReportedCostUSD    *float64  `json:"reportedCostUsd"`
	CostCoverage       string    `json:"costCoverage"`
	CostBasis          string    `json:"costBasis,omitempty"`
	CostPerAcceptedUSD *float64  `json:"costPerAcceptedOutcomeUsd"`
	EffectiveModels    []string  `json:"effectiveModels"`
	ReviewFindings     *Severity `json:"reviewFindingsBySeverity,omitempty"`

	N RateDenominators `json:"n"`
}

// Severity holds review-Atom finding counts by severity, summed across attempts. It is
// present only when the Formula has a review Atom that recorded findings.
type Severity struct {
	High   int `json:"high"`
	Medium int `json:"medium"`
	Low    int `json:"low"`
}

// TaskVariantOutcome is one cell of the per-task × variant table.
type TaskVariantOutcome struct {
	Variant              string `json:"variant"`
	Attempts             int    `json:"attempts"`
	Completions          int    `json:"completions"`
	HiddenAcceptancePass int    `json:"hiddenAcceptancePass"`
	RequiredChecksPass   int    `json:"requiredChecksPass"`
	Timeouts             int    `json:"timeouts"`
	Failures             int    `json:"failures"`
}

type TaskOutcome struct {
	Task     string               `json:"task"`
	Variants []TaskVariantOutcome `json:"variants"`
}

// CohortReport is the side-by-side result written to report.json and rendered to
// report.md. Counts and rates only, with no significance claims; every rate states its
// n (tasks, repetitions and the attempts it was computed over).
type CohortReport struct {
	SchemaVersion    int    `json:"schemaVersion"`
	ActivationDigest string `json:"activationDigest,omitempty"`
	Status           string `json:"status"`
	Hypothesis       string `json:"hypothesis"`
	PrimaryOutcome   string `json:"primaryOutcome"`
	Tasks            int    `json:"tasks"`
	Repetitions      int    `json:"repetitions"`
	StartedAt        string `json:"startedAt"`
	FinishedAt       string `json:"finishedAt,omitempty"`
	// Stopped names the stop rule that ended the run early, empty when the cohort ran
	// to completion.
	Stopped  string           `json:"stopped,omitempty"`
	Variants []VariantSummary `json:"variants"`
	PerTask  []TaskOutcome    `json:"perTask"`
	Attempts []CohortAttempt  `json:"attempts"`
	Notes    []string         `json:"notes"`
}

// completed reports whether a harness Atom finished for the attempt.
func completed(a Attempt) bool {
	for _, atom := range a.Molecule.Atoms {
		if atom.Type == "harness" && atom.Status == "done" {
			return true
		}
	}
	return false
}

// timedOut reports whether any harness attempt of the Molecule hit its own timeout or
// the parent deadline. A superseded earlier timeout still counts as a timeout here,
// which is deliberate: a timed-out attempt is recorded, never silently retried away.
func timedOut(a Attempt) bool {
	for _, h := range a.Molecule.HarnessAttempts {
		if h.Result.TimedOut || h.Result.ParentDeadline {
			return true
		}
	}
	return false
}

// scopeChange reports whether a coding attempt returned the scope-change outcome.
func scopeChange(a Attempt) bool {
	for _, h := range a.Molecule.HarnessAttempts {
		if h.Coding != nil && h.Coding.Outcome == "scope-change" {
			return true
		}
	}
	return false
}

// scopeViolation reports whether the captured output left the frozen scope. It is only
// meaningful once a workspace existed; an attempt that never produced one is not a
// scope violation.
func scopeViolation(a Attempt) bool {
	return a.Molecule.Workspace.Path != "" && !a.ScopeOK
}

// requiredChecksPassed reads the per-check verification observations: every check other
// than the hidden acceptance check passed and verification itself completed. It is
// distinct from the hidden-acceptance pass (Attempt.Correct), which also requires the
// acceptance check to pass.
func requiredChecksPassed(a Attempt) bool {
	if a.Verification.Error != "" {
		return false
	}
	for _, o := range a.Checks {
		if o.Command.Name == "acceptance" {
			continue
		}
		if !o.Result.OK {
			return false
		}
	}
	return completed(a)
}

// amendments proxies the amendment count from the readiness plan history: each proposal
// beyond the first is an amendment. A run with no readiness history, or exactly one
// proposal, reports zero.
func amendments(a Attempt) int {
	if a.Molecule.Readiness == nil || len(a.Molecule.Readiness.Plans) <= 1 {
		return 0
	}
	return len(a.Molecule.Readiness.Plans) - 1
}

// reportedCost sums the reported cost (#149 usage) across the attempt's harness
// attempts. It returns nil when no harness attempt reported a cost, so a cost is never
// rendered as zero; list-price estimates are included and labelled by basis.
func reportedCost(a Attempt) *float64 {
	reported := false
	total := 0.0
	for _, h := range a.Molecule.HarnessAttempts {
		if h.Usage != nil && h.Usage.TotalCostUSD != nil {
			reported = true
			total += *h.Usage.TotalCostUSD
		}
	}
	if !reported {
		return nil
	}
	return &total
}

// attemptModels and attemptBases collect the effective models and cost bases a single
// attempt reported.
func attemptModels(a Attempt) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range a.Molecule.HarnessAttempts {
		if h.Usage == nil {
			continue
		}
		for _, m := range h.Usage.EffectiveModels {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// reviewSeverity sums review-Atom findings by severity for one attempt; the second
// return is whether any review pass was recorded at all.
func reviewSeverity(a Attempt) (Severity, bool) {
	var s Severity
	found := false
	for _, r := range a.Molecule.ReviewAttempts {
		found = true
		s.High += r.High
		s.Medium += r.Medium
		s.Low += r.Low
	}
	return s, found
}

// median returns the median of the samples (integer average of the two middle values
// for an even count), or zero for no samples.
func median(values []int64) int64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	s := append([]int64{}, values...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// p90 returns the 90th percentile by the nearest-rank method, or zero for no samples.
func p90(values []int64) int64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	s := append([]int64{}, values...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	rank := int(math.Ceil(0.9*float64(n))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= n {
		rank = n - 1
	}
	return s[rank]
}

// buildCohortReport computes the side-by-side report from the recorded attempts. It
// makes no significance claim: it reports counts and rates with their n.
func buildCohortReport(base CohortReport, variantNames []string, variantHashes map[string]string, taskNames []string, attempts []CohortAttempt) CohortReport {
	r := base
	r.Attempts = attempts

	byVariant := map[string][]CohortAttempt{}
	byTaskVariant := map[string]map[string][]CohortAttempt{}
	for _, a := range attempts {
		byVariant[a.Variant] = append(byVariant[a.Variant], a)
		if byTaskVariant[a.Task] == nil {
			byTaskVariant[a.Task] = map[string][]CohortAttempt{}
		}
		byTaskVariant[a.Task][a.Variant] = append(byTaskVariant[a.Task][a.Variant], a)
	}

	r.Variants = nil
	for _, name := range variantNames {
		group := byVariant[name]
		s := VariantSummary{Name: name, FormulaSHA256: variantHashes[name], CostCoverage: "0 of 0", EffectiveModels: []string{}}
		var elapsed, harness []int64
		var costTotal float64
		costReported := 0
		bases := map[string]bool{}
		models := map[string]bool{}
		var sev Severity
		sevSeen := false
		for _, ca := range group {
			a := ca.Attempt
			s.Attempts++
			if completed(a) {
				s.Completions++
			}
			if a.Correct {
				s.HiddenAcceptancePass++
			}
			if requiredChecksPassed(a) {
				s.RequiredChecksPass++
			}
			if scopeViolation(a) {
				s.ScopeViolations++
			}
			if scopeChange(a) {
				s.ScopeChanges++
			}
			s.Amendments += amendments(a)
			if timedOut(a) {
				s.Timeouts++
			}
			if !a.Correct {
				s.Failures++
			}
			elapsed = append(elapsed, a.ElapsedMS)
			harness = append(harness, a.HarnessMS)
			if c := reportedCost(a); c != nil {
				costReported++
				costTotal += *c
			}
			for _, h := range a.Molecule.HarnessAttempts {
				if h.Usage != nil && h.Usage.CostBasis != "" {
					bases[h.Usage.CostBasis] = true
				}
			}
			for _, m := range attemptModels(a) {
				models[m] = true
			}
			if as, ok := reviewSeverity(a); ok {
				sevSeen = true
				sev.High += as.High
				sev.Medium += as.Medium
				sev.Low += as.Low
			}
		}
		s.ElapsedMedianMS, s.ElapsedP90MS = median(elapsed), p90(elapsed)
		s.HarnessMedianMS, s.HarnessP90MS = median(harness), p90(harness)
		s.CostCoverage = fmt.Sprintf("%d of %d", costReported, s.Attempts)
		if costReported > 0 {
			total := costTotal
			s.ReportedCostUSD = &total
			if s.HiddenAcceptancePass > 0 {
				per := total / float64(s.HiddenAcceptancePass)
				s.CostPerAcceptedUSD = &per
			}
			switch {
			case len(bases) == 1:
				for b := range bases {
					s.CostBasis = b
				}
			case len(bases) > 1:
				s.CostBasis = "mixed"
			}
		}
		for m := range models {
			s.EffectiveModels = append(s.EffectiveModels, m)
		}
		sort.Strings(s.EffectiveModels)
		if sevSeen {
			sc := sev
			s.ReviewFindings = &sc
		}
		s.N = RateDenominators{Tasks: base.Tasks, Repetitions: base.Repetitions, Attempts: s.Attempts}
		r.Variants = append(r.Variants, s)
	}

	r.PerTask = nil
	for _, task := range taskNames {
		to := TaskOutcome{Task: task}
		for _, name := range variantNames {
			group := byTaskVariant[task][name]
			cell := TaskVariantOutcome{Variant: name}
			for _, ca := range group {
				a := ca.Attempt
				cell.Attempts++
				if completed(a) {
					cell.Completions++
				}
				if a.Correct {
					cell.HiddenAcceptancePass++
				}
				if requiredChecksPassed(a) {
					cell.RequiredChecksPass++
				}
				if timedOut(a) {
					cell.Timeouts++
				}
				if !a.Correct {
					cell.Failures++
				}
			}
			to.Variants = append(to.Variants, cell)
		}
		r.PerTask = append(r.PerTask, to)
	}
	return r
}

func rate(num, den int) string {
	if den == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(num)/float64(den))
}

func msOrNA(v int64, n int) string {
	if n == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%d ms", v)
}

func costOrUnreported(c *float64) string {
	if c == nil {
		return "unreported"
	}
	return fmt.Sprintf("$%.6f", *c)
}

// renderCohortMarkdown renders the side-by-side report. Counts and rates only, with no
// significance claims; every rate states its n next to it.
func renderCohortMarkdown(r CohortReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Cohort evaluation report\n\n")
	fmt.Fprintf(&b, "Status: %s\n\n", r.Status)
	if r.Stopped != "" {
		fmt.Fprintf(&b, "Stopped early: %s\n\n", r.Stopped)
	}
	fmt.Fprintf(&b, "Hypothesis: %s\n\n", r.Hypothesis)
	fmt.Fprintf(&b, "Primary outcome: %s\n\n", r.PrimaryOutcome)
	fmt.Fprintf(&b, "Cohort shape: %d task(s) × %d repetition(s) per variant.\n\n", r.Tasks, r.Repetitions)
	fmt.Fprintf(&b, "**Counts and rates only. No significance or causal claims. Every rate states its n.**\n\n")

	for _, v := range r.Variants {
		n := fmt.Sprintf("n=%d attempts over %d task(s) × %d rep(s)", v.N.Attempts, v.N.Tasks, v.N.Repetitions)
		fmt.Fprintf(&b, "## Variant %s\n\n", v.Name)
		fmt.Fprintf(&b, "- Formula SHA-256: `%s`\n", v.FormulaSHA256)
		fmt.Fprintf(&b, "- Attempts: %d; completions: %d (%s; %s)\n", v.Attempts, v.Completions, rate(v.Completions, v.Attempts), n)
		fmt.Fprintf(&b, "- Hidden-acceptance pass: %d (%s; %s)\n", v.HiddenAcceptancePass, rate(v.HiddenAcceptancePass, v.Attempts), n)
		fmt.Fprintf(&b, "- Required-checks pass: %d (%s; %s)\n", v.RequiredChecksPass, rate(v.RequiredChecksPass, v.Attempts), n)
		fmt.Fprintf(&b, "- Scope violations: %d; scope-change: %d; amendments: %d; timeouts: %d; failures: %d (%s)\n", v.ScopeViolations, v.ScopeChanges, v.Amendments, v.Timeouts, v.Failures, n)
		fmt.Fprintf(&b, "- Elapsed: median %s, p90 %s; harness: median %s, p90 %s (%s)\n", msOrNA(v.ElapsedMedianMS, v.Attempts), msOrNA(v.ElapsedP90MS, v.Attempts), msOrNA(v.HarnessMedianMS, v.Attempts), msOrNA(v.HarnessP90MS, v.Attempts), n)
		basis := v.CostBasis
		if basis == "" {
			basis = "basis unavailable"
		}
		fmt.Fprintf(&b, "- Reported cost: %s total (coverage %s; %s)\n", costOrUnreported(v.ReportedCostUSD), v.CostCoverage, basis)
		fmt.Fprintf(&b, "- Reported cost per accepted outcome: %s\n", costOrUnreported(v.CostPerAcceptedUSD))
		models := "unavailable"
		if len(v.EffectiveModels) > 0 {
			models = strings.Join(v.EffectiveModels, ", ")
		}
		fmt.Fprintf(&b, "- Effective models: %s\n", models)
		if v.ReviewFindings != nil {
			fmt.Fprintf(&b, "- Review findings: %d high, %d medium, %d low\n", v.ReviewFindings.High, v.ReviewFindings.Medium, v.ReviewFindings.Low)
		}
		fmt.Fprintf(&b, "\n")
	}

	fmt.Fprintf(&b, "## Per-task outcomes\n\n")
	fmt.Fprintf(&b, "Each cell shows hidden-acceptance pass / attempts for the task × variant (over %d rep(s)).\n\n", r.Repetitions)
	fmt.Fprintf(&b, "| task |")
	for _, v := range r.Variants {
		fmt.Fprintf(&b, " %s |", v.Name)
	}
	fmt.Fprintf(&b, "\n| --- |")
	for range r.Variants {
		fmt.Fprintf(&b, " --- |")
	}
	fmt.Fprintf(&b, "\n")
	for _, to := range r.PerTask {
		fmt.Fprintf(&b, "| %s |", to.Task)
		for _, cell := range to.Variants {
			fmt.Fprintf(&b, " %d/%d (to %d, fail %d) |", cell.HiddenAcceptancePass, cell.Attempts, cell.Timeouts, cell.Failures)
		}
		fmt.Fprintf(&b, "\n")
	}
	fmt.Fprintf(&b, "\n")
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "> %s\n", note)
	}
	return b.String()
}

// saveCohortReport writes report.json atomically and report.md beside it.
func saveCohortReport(dir string, r CohortReport) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".report-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, "report.json")); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "report.md"), []byte(renderCohortMarkdown(r)), 0600)
}
