package molecule

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func atomicWrite(file string, data []byte) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".ledger-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err = temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, file); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// renderHarnessDiagnostics is the single additive ledger line for one attempt. It
// carries only an enumerated state, byte counts and millisecond offsets, never
// output text. A nil Activity reads as not recorded, which differs from silent.
func renderHarnessDiagnostics(r process.Result) string {
	return renderDiagnostics(r.Diagnostics())
}

// renderDiagnostics is shared by harness and analysis attempts so both read alike.
func renderDiagnostics(d process.Diagnostics) string {
	output := "not recorded"
	if a := d.Activity; a != nil {
		output = fmt.Sprintf("stdout %d B, stderr %d B, last output at: none", a.StdoutBytes, a.StderrBytes)
		if a.LastOutputMS != nil {
			output = fmt.Sprintf("stdout %d B, stderr %d B, last output at: %d ms", a.StdoutBytes, a.StderrBytes, *a.LastOutputMS)
		}
	}
	return fmt.Sprintf("Diagnostics: elapsedMs=%d; terminal state: %s; output observed: %s; reconnect evidence: %s. Observed pipe bytes only: no output is not proof of a stall and output is not proof of progress.\n", d.ElapsedMS, d.TerminalState, output, d.Reconnect)
}
func known(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
func renderAtomProvenance(b *strings.Builder, p *AtomProvenance) {
	if p == nil {
		p = &AtomProvenance{}
	}
	fmt.Fprintf(b, "\nDeclared source: %s\nDeclared binding: %s\nDeclared workflows: %s\nDeclared purpose: %s\nPlanned action: %s\nExecution: %s\n", known(p.DeclaredSource), known(p.DeclaredBinding), known(strings.Join(p.DeclaredWorkflows, ", ")), known(p.DeclaredPurpose), known(p.PlannedAction), known(p.Execution))
	if p.SkipReason != "" {
		fmt.Fprintf(b, "Skip reason: %s\n", p.SkipReason)
	}
	if len(p.Actions) == 0 {
		fmt.Fprintln(b, "Resolved source: unknown\nObserved action/result: unknown\nExternal action/result: unknown\nEvidence references: unknown")
	}
	for _, a := range p.Actions {
		fmt.Fprintf(b, "\nAttempt: %s; plan: %s\nResolved source: %s\nPlanned action: %s\n", known(a.AttemptID), known(a.PlanDigest), known(a.ResolvedSource), known(a.PlannedAction))
		if a.Observed == nil {
			fmt.Fprintln(b, "Observed action/result: unknown\nExternal action/result: unknown\nEvidence references: unknown")
			continue
		}
		obs := a.Observed
		fmt.Fprintf(b, "Observed action/result: %s / %s\nExternal action/result: %s / %s\n", known(obs.Action), known(obs.Result), known(obs.ExternalAction), known(obs.ExternalResult))
		if len(obs.Evidence) == 0 {
			fmt.Fprintln(b, "Evidence references: unknown")
		}
		for _, ref := range obs.Evidence {
			fmt.Fprintf(b, "Evidence reference: pointer %s; file %s; SHA-256 %s\n", known(ref.Pointer), known(ref.Path), known(ref.SHA256))
		}
	}
}
func save(r Record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 5_000_000 {
		return fmt.Errorf("ledger exceeds 5 MB; state was not replaced; inspect retained verification evidence before recovery")
	}
	base := filepath.Join(r.LabDir, "ledgers", r.ID)
	if err = atomicWrite(base+".json", append(data, '\n')); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Molecule %s\n\n- Status: %s\n- Formula: %s\n- Formula SHA-256: %s\n- Issue: %s#%d\n- Workspace: %s\n- Branch: %s\n- Base commit: %s\n\n## Atoms\n", r.ID, r.Status, r.FormulaID, r.FormulaSnapshot.SHA256, r.Issue.Repo, r.Issue.Number, r.Workspace.Path, r.Workspace.Branch, r.Workspace.BaseCommit)
	if r.CheckoutStatus != "" {
		fmt.Fprintf(&b, "Checkout availability: %s\n", r.CheckoutStatus)
	}
	if r.Retired != nil {
		fmt.Fprintf(&b, "\n## Retired\n\n- Retired at: %s\n- Prior status: %s\n- Reason: %s\n\nRetired history: every record is kept; the loop never resumes this Molecule.\n", oneLine(r.Retired.At), oneLine(r.Retired.PriorStatus), oneLine(r.Retired.Reason))
	}
	fmt.Fprintln(&b, "\n## Clarification history (informational; no inherited approval)")
	for _, id := range r.ClarificationPredecessors {
		fmt.Fprintf(&b, "- [%s](%s.md)\n", id, id)
	}
	if r.ClarificationHistoryTruncated {
		fmt.Fprintln(&b, "History discovery truncated by safety bounds; earlier records remain on disk.")
	}
	issueJSON, _ := json.MarshalIndent(r.Issue, "", "  ")
	fmt.Fprintf(&b, "\n## Exact Issue snapshot\n\n```json\n%s\n```\n", issueJSON)
	for _, attempt := range r.AnalysisAttempts {
		raw, _ := json.MarshalIndent(attempt, "", "  ")
		fmt.Fprintf(&b, "\n## Analysis attempt %d\n\n```json\n%s\n```\n", attempt.Ordinal, raw)
		if attempt.Diagnostics != nil {
			fmt.Fprintf(&b, "\n%s", renderDiagnostics(*attempt.Diagnostics))
		}
	}
	for _, a := range r.Atoms {
		fmt.Fprintf(&b, "\n### %s (%s) — %s\n\n%s\n", a.ID, a.Type, a.Status, a.Detail)
		renderAtomProvenance(&b, a.Provenance)
	}
	if r.Analysis != nil {
		fmt.Fprintf(&b, "\n## Intake assessment\n\n%s\n", r.Analysis.Summary)
		for _, q := range r.Analysis.Questions {
			fmt.Fprintf(&b, "\n- Question: %s\n  Reason: %s\n", q.Question, q.Reason)
		}
	}
	if r.Readiness != nil {
		for _, proposal := range r.Readiness.Plans {
			raw, _ := json.MarshalIndent(proposal.Plan, "", "  ")
			fmt.Fprintf(&b, "\n## Readiness revision %d\n\nApproval digest: `%s`\n\n```json\n%s\n```\n", proposal.Plan.Revision, proposal.Digest, raw)
		}
		fmt.Fprintln(&b, "\n## Decisions and attempts")
		for _, event := range r.Readiness.Events {
			fmt.Fprintf(&b, "\n- %s · %s · %s\n", event.At, event.Kind, event.Detail)
		}
	}
	if len(r.PolicyDecisions) > 0 {
		fmt.Fprintln(&b, "\n## Policy decisions (automatic, policy-approved; not a human decision)")
		for _, d := range r.PolicyDecisions {
			fmt.Fprintf(&b, "\n- Approved by: %s (policy-approved)\n  Gate: %s; plan: `%s`; Formula SHA-256: %s; at %s\n", d.ApprovedBy, d.Gate, d.PlanDigest, d.FormulaSHA256, d.At)
			for _, e := range d.Evidence {
				fmt.Fprintf(&b, "  - Evidence: `%s` — %s\n", e.Pointer, e.Observed)
			}
		}
	}
	if len(r.Extensions) > 0 {
		renderExtensions(&b, r)
	}
	for _, a := range r.HarnessAttempts {
		if a.Coding != nil {
			fmt.Fprintf(&b, "\nImplementation outcome: %s\nReason: %s\n", a.Coding.Outcome, a.Coding.Reason)
		} else {
			fmt.Fprintln(&b, "\nImplementation outcome: unknown (historical evidence missing)")
		}
		fmt.Fprintf(&b, "\n## Harness attempt %s\n\nPlan: `%s`\nProvider process OK: %t; exit: %d; timeout: %t; interruption: %t; overflow: %t\nReconciled: %t; uncertainty: %s\nCapture error: %s\nHuman recovery: %t\n", a.ID, a.PlanDigest, a.Result.OK, a.Result.Code, a.Result.TimedOut, a.Result.Interrupted, a.Result.Overflow, a.Result.Reconciled, a.Result.Uncertainty, a.CaptureError, a.Recovered)
		fmt.Fprint(&b, renderHarnessDiagnostics(a.Result))
		if a.Evidence != nil {
			fmt.Fprintf(&b, "Complete bounded result: `%s` (SHA-256 `%s`)\n", a.Evidence.Path, a.Evidence.SHA256)
		}
		if a.Capture != nil {
			fmt.Fprintf(&b, "Tree: `%s`; violations: %s\n", a.Capture.Tree, boundedPaths(a.Capture.Violations, maxListedViolations))
		}
	}
	for _, a := range r.VerificationAttempts {
		fmt.Fprintf(&b, "\n## Verification attempt — %s\n\nPlan: `%s`\nTree: `%s`\nStarted: %s\nFinished: %s\n", a.Status, a.PlanDigest, a.SourceTree, a.StartedAt, a.FinishedAt)
		for _, ref := range a.Observations {
			fmt.Fprintf(&b, "- Observation: `%s` (SHA-256 `%s`)\n", ref.Path, ref.SHA256)
		}
		if a.Result != nil {
			fmt.Fprintf(&b, "- Complete result: `%s` (SHA-256 `%s`)\n", a.Result.Path, a.Result.SHA256)
		}
	}
	for _, a := range r.ReviewAttempts {
		renderReviewAttempt(&b, a)
	}
	if r.Verification != nil {
		fmt.Fprintf(&b, "\n## Independent verification\n\nTree: `%s`\nRequired checks passed: %t\nIndependent acceptance verified: %t\n", r.Verification.SourceTree, r.Verification.RequiredChecksPassed, r.Verification.IndependentAcceptanceVerified)
		b.WriteString(goCacheLines(*r.Verification))
		b.WriteString(selectionLines(*r.Verification))
		b.WriteString(protectedJSONAddedLines(*r.Verification))
		b.WriteString(reviewVerdictLine(r))
		fmt.Fprintf(&b, "\n%s\n", r.Verification.Error)
	}
	if lines := UsageSummaryLines(r); len(lines) > 0 {
		fmt.Fprintln(&b, "\n## Usage and cost (#149; evidence, never an approval)")
		for _, line := range lines {
			fmt.Fprintln(&b, line)
		}
	}
	fmt.Fprintln(&b, "\n## Notes")
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "\n- %s\n", note)
	}
	return atomicWrite(base+".md", []byte(b.String()))
}

// goCacheLines renders the shared Go cache summary of a verification: the go test
// packages served (cached) versus run in each view, and each check's cache state.
// Records that predate the cache fields render nothing.
func goCacheLines(v verification.Result) string {
	var b strings.Builder
	views := []struct {
		name   string
		result *verification.Result
	}{{"Candidate", &v}, {"Protected regression", v.Protected}}
	for _, view := range views {
		if view.result == nil {
			continue
		}
		states := []string{}
		notes := []string{}
		for _, o := range view.result.Checks {
			if o.Cache != "" {
				state := fmt.Sprintf("`%s` cache: %s", o.ID, o.Cache)
				if len(o.GoEnvForwarded) > 0 {
					state += " (policy forwarded " + strings.Join(o.GoEnvForwarded, ", ") + ")"
				}
				states = append(states, state)
			}
			if o.Note != "" {
				notes = append(notes, fmt.Sprintf("`%s` note: %s\n", o.ID, o.Note))
			}
		}
		if len(states) == 0 {
			continue
		}
		total := view.result.CacheTotals()
		fmt.Fprintf(&b, "%s Go test cache: %d packages served (cached), %d run; %s\n", view.name, total.Cached, total.Run, strings.Join(states, "; "))
		b.WriteString(strings.Join(notes, ""))
	}
	return b.String()
}

// selectionLines renders, per view, one line for each check that declared select:
// `selection: affected-packages (N of M packages)` or `selection: full suite (reason:
// …)`. The reason comes from the record, so it is bounded and stripped of control
// characters. Checks without a selection render nothing.
func selectionLines(v verification.Result) string {
	var b strings.Builder
	views := []struct {
		name   string
		result *verification.Result
	}{{"Candidate", &v}, {"Protected regression", v.Protected}}
	for _, view := range views {
		if view.result == nil {
			continue
		}
		for _, o := range view.result.Checks {
			if o.Selection != nil {
				fmt.Fprintf(&b, "%s `%s` %s\n", view.name, oneLine(o.ID), oneLine(o.Selection.Line()))
			}
		}
	}
	return b.String()
}

// usageAttempt pairs a harness attempt's label with its parsed usage (#149); a nil
// usage reads as "unavailable", never as zero.
type usageAttempt struct {
	kind, id, label string
	usage           *harness.Usage
}

// moleculeUsageAttempts lists every harness attempt of the Molecule (analysis, coding
// and review) in a stable order, each with its usage or nil when unavailable.
func moleculeUsageAttempts(r Record) []usageAttempt {
	var attempts []usageAttempt
	for _, a := range r.AnalysisAttempts {
		attempts = append(attempts, usageAttempt{"analysis", fmt.Sprintf("analysis-%d", a.Ordinal), fmt.Sprintf("analysis attempt %d", a.Ordinal), a.Usage})
	}
	for _, a := range r.HarnessAttempts {
		attempts = append(attempts, usageAttempt{"coding", a.ID, "coding attempt " + usageText(a.ID), a.Usage})
	}
	for _, a := range r.ReviewAttempts {
		attempts = append(attempts, usageAttempt{"review", fmt.Sprintf("review-%d", a.Ordinal), fmt.Sprintf("review attempt review-%d", a.Ordinal), a.Usage})
	}
	return attempts
}

// AttemptUsage is one harness attempt's recorded usage (#200). Reported is false when
// the attempt carried no usage object; then every other usage field is empty, which
// reads as unavailable, never as zero. A nil CostUSD or token category was not
// reported. Model ids and the basis are provider data, kept as recorded: a renderer
// bounds them.
type AttemptUsage struct {
	// Kind is analysis, coding or review; ID is the attempt's id (analysis-N, the
	// coding attempt id, review-N); Label is its bounded summary-line label.
	Kind            string                  `json:"kind"`
	ID              string                  `json:"id"`
	Label           string                  `json:"label"`
	Reported        bool                    `json:"reported"`
	CostUSD         *float64                `json:"costUsd"`
	CostBasis       string                  `json:"costBasis,omitempty"`
	Tokens          harness.TokenCategories `json:"tokens"`
	EffectiveModels []string                `json:"effectiveModels,omitempty"`
	TruncatedModels int                     `json:"truncatedModels,omitempty"`
	ModelMismatch   bool                    `json:"modelMismatch,omitempty"`
}

// UsageCoverage is "n of m": N attempts reported the value out of M attempts.
type UsageCoverage struct {
	Reported int `json:"reported"`
	Attempts int `json:"attempts"`
}

// MoleculeUsage is the structured usage and cost summary of one Molecule (#200), the
// data UsageSummaryLines renders. Attempts lists every analysis, coding and review
// attempt in the summary order. A nil token total was reported by no attempt or
// overflowed; CostUSD is nil when no attempt reported a cost or when the sum is not
// finite (CostNotFinite). CostBasis is the aggregate basis of the cost-reporting
// attempts ("" none, "mixed" when they differ).
type MoleculeUsage struct {
	Attempts      []AttemptUsage          `json:"attempts"`
	UsageReported int                     `json:"usageReported"`
	Tokens        harness.TokenCategories `json:"tokens"`
	Models        []string                `json:"models"`
	CostUSD       *float64                `json:"costUsd"`
	CostNotFinite bool                    `json:"costNotFinite,omitempty"`
	CostBasis     string                  `json:"costBasis,omitempty"`
	CostCoverage  UsageCoverage           `json:"costCoverage"`
	Mismatches    int                     `json:"mismatches"`
}

// UsageSummary returns the structured usage and cost summary of r (#200). Each
// attempt appears once, so a retried attempt is its own entry and an attempt with no
// usage counts in the coverage total but not as reported.
func UsageSummary(r Record) MoleculeUsage {
	attempts := moleculeUsageAttempts(r)
	out := MoleculeUsage{Attempts: make([]AttemptUsage, 0, len(attempts)), Models: []string{}}
	var inputT, outputT, cacheReadT, cacheCreationT tokenTotal
	costSum := 0.0
	models := map[string]bool{}
	var reportedBases []string
	for _, a := range attempts {
		entry := AttemptUsage{Kind: a.kind, ID: a.id, Label: a.label}
		if u := a.usage; u != nil {
			entry.Reported = true
			out.UsageReported++
			entry.Tokens = harness.TokenCategories{Input: copyInt(u.Total.Input), Output: copyInt(u.Total.Output), CacheRead: copyInt(u.Total.CacheRead), CacheCreation: copyInt(u.Total.CacheCreation)}
			entry.EffectiveModels = append([]string(nil), u.EffectiveModels...)
			entry.TruncatedModels, entry.ModelMismatch, entry.CostBasis = u.TruncatedModels, u.ModelMismatch, u.CostBasis
			inputT.add(u.Total.Input)
			outputT.add(u.Total.Output)
			cacheReadT.add(u.Total.CacheRead)
			cacheCreationT.add(u.Total.CacheCreation)
			for _, m := range u.EffectiveModels {
				models[m] = true
			}
			if u.TotalCostUSD != nil {
				cost := *u.TotalCostUSD
				entry.CostUSD = &cost
				out.CostCoverage.Reported++
				reportedBases = append(reportedBases, u.CostBasis)
				next := costSum + cost
				if math.IsInf(next, 0) || math.IsNaN(next) {
					out.CostNotFinite = true
				} else {
					costSum = next
				}
			}
			if u.ModelMismatch {
				out.Mismatches++
			}
		}
		out.Attempts = append(out.Attempts, entry)
	}
	out.CostCoverage.Attempts = len(attempts)
	out.Tokens = harness.TokenCategories{Input: inputT.value(), Output: outputT.value(), CacheRead: cacheReadT.value(), CacheCreation: cacheCreationT.value()}
	out.Models = sortedKeys(models)
	if out.CostCoverage.Reported > 0 {
		out.CostBasis = aggregateBasis(reportedBases)
		if !out.CostNotFinite {
			out.CostUSD = &costSum
		}
	}
	return out
}

func copyInt(v *int64) *int64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// CostBasisLabel is the bounded display label of a cost basis; it is never empty.
func CostBasisLabel(basis string) string { return costBasisLabel(basis) }

// usageText renders a provider-supplied usage string (a model id or basis) as one
// bounded, control-free token, since it is data from the CLI's final JSON.
func usageText(s string) string { return harness.ReviewDisplayText(s, 120) }

// formatUSD renders a cost deterministically. Cost is reported only; it is never
// estimated or invented.
func formatUSD(v float64) string { return fmt.Sprintf("$%.6f", v) }

// tokenText renders a nullable token category: an unreported value is shown as
// "unreported", never as zero (#149).
func tokenText(v *int64) string {
	if v == nil {
		return "unreported"
	}
	return strconv.FormatInt(*v, 10)
}

// costBasisLabel renders a cost basis for display; it never returns empty, so a dollar
// figure always carries a basis label (#149). "" means no basis was reported. A "list"
// basis is always a CLI list-price estimate and never an actual charge.
func costBasisLabel(basis string) string {
	switch basis {
	case "":
		return "basis unavailable"
	case "list":
		return "CLI list-price estimate"
	case "mixed":
		return "mixed cost basis; see per-attempt line items"
	case "unavailable":
		return "unavailable"
	default:
		return "cost basis " + usageText(basis)
	}
}

// aggregateBasis is the single non-empty basis every cost-reporting attempt shares. It
// is "mixed" when the reported bases differ or some carry one and some do not, and ""
// (rendered "basis unavailable") when none carried a basis.
func aggregateBasis(reported []string) string {
	distinct := map[string]bool{}
	anyEmpty := false
	for _, b := range reported {
		if b == "" {
			anyEmpty = true
			continue
		}
		distinct[b] = true
	}
	switch {
	case len(distinct) == 0:
		return ""
	case len(distinct) == 1 && !anyEmpty:
		for b := range distinct {
			return b
		}
	}
	return "mixed"
}

// usageModelList renders effective model ids, optionally with a "+N more" suffix for
// entries dropped by the parser's bound on untrusted output.
func usageModelList(models []string, truncated int) string {
	var base string
	if len(models) == 0 {
		base = "none reported"
	} else {
		items := make([]string, len(models))
		for i, m := range models {
			items[i] = usageText(m)
		}
		base = strings.Join(items, ", ")
	}
	if truncated > 0 {
		base += fmt.Sprintf(" (+%d more)", truncated)
	}
	return base
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// tokenTotal accumulates one token category across attempts with overflow detection,
// so a total never wraps. A category no attempt reported, or one whose sum overflowed,
// renders "unreported" rather than zero (#149).
type tokenTotal struct {
	sum      int64
	reported bool
	overflow bool
}

func (t *tokenTotal) add(v *int64) {
	if v == nil {
		return
	}
	t.reported = true
	if t.overflow {
		return
	}
	next := t.sum + *v
	if next < t.sum {
		t.overflow = true
		return
	}
	t.sum = next
}

// value is the total, or nil when no attempt reported the category or it overflowed.
func (t tokenTotal) value() *int64 {
	if !t.reported || t.overflow {
		return nil
	}
	sum := t.sum
	return &sum
}

// UsageSummaryLines renders the per-Molecule usage and cost summary (#149) shared by
// the ledger Markdown and presentRun: one line per harness attempt (its effective
// models, token categories and reported cost, or an explicit "unavailable"), the token
// totals by category, the union of effective models, a coverage line "cost reported
// for N of M harness attempts", the reported cost total, and a prominent non-blocking
// mismatch notice when any attempt's effective models differ from its pin. It returns
// nil when no attempt has reported usage yet, so nothing is shown then. The notice is
// evidence only: it never fails readiness or a gate. Meta/formula-improvement harness
// usage is not captured in v1 (deferred to slice B) and so is never part of these
// totals.
func UsageSummaryLines(r Record) []string {
	s := UsageSummary(r)
	if s.UsageReported == 0 {
		return nil
	}
	var lines []string
	for _, a := range s.Attempts {
		if !a.Reported {
			lines = append(lines, "- "+a.Label+": unavailable")
			continue
		}
		detail := fmt.Sprintf("effective models %s · tokens input %s, output %s, cache-read %s, cache-creation %s",
			usageModelList(a.EffectiveModels, a.TruncatedModels),
			tokenText(a.Tokens.Input), tokenText(a.Tokens.Output), tokenText(a.Tokens.CacheRead), tokenText(a.Tokens.CacheCreation))
		if a.CostUSD != nil {
			detail += " · cost " + formatUSD(*a.CostUSD) + " (" + costBasisLabel(a.CostBasis) + ")"
		} else {
			detail += " · cost unavailable"
		}
		if a.ModelMismatch {
			detail += " · MODEL MISMATCH vs pin (non-blocking)"
		}
		lines = append(lines, "- "+a.Label+": "+detail)
	}
	lines = append(lines, fmt.Sprintf("Totals: input %s, output %s, cache-read %s, cache-creation %s tokens across effective models %s",
		tokenText(s.Tokens.Input), tokenText(s.Tokens.Output), tokenText(s.Tokens.CacheRead), tokenText(s.Tokens.CacheCreation), usageModelList(s.Models, 0)))
	coverage := fmt.Sprintf("cost reported for %d of %d harness attempts", s.CostCoverage.Reported, s.CostCoverage.Attempts)
	switch {
	case s.CostCoverage.Reported == 0:
		lines = append(lines, "Reported cost: unavailable — "+coverage)
	case s.CostNotFinite:
		lines = append(lines, fmt.Sprintf("Reported cost: unavailable (sum not finite) — %s (%s)", coverage, costBasisLabel(s.CostBasis)))
	default:
		// The basis label sits on the coverage phrase (#149 item 7) and so also labels
		// the dollar figure on this line (#149 review fix 4: never a bare dollar figure).
		lines = append(lines, fmt.Sprintf("Reported cost: %s total — %s (%s)", formatUSD(*s.CostUSD), coverage, costBasisLabel(s.CostBasis)))
	}
	if s.Mismatches > 0 {
		lines = append(lines, fmt.Sprintf("NOTICE: effective models differ from the pinned model for %d of %d harness attempt(s); this is non-blocking and never fails readiness or a gate.", s.Mismatches, len(s.Attempts)))
	}
	lines = append(lines, "Note: usage capture covers analysis, coding and review harness attempts; meta/formula-improvement harness usage is deferred to slice B (#149).")
	return lines
}

// History is advisory only. Bounds never broaden approval lookup or mutate predecessors.
func clarificationHistory(lab, repo string, number int64) ([]string, bool, error) {
	scan := scanLedgers(lab, nil)
	if scan.Missing {
		return nil, false, nil
	}
	// No partial directory ordering: oversize inventories have no guessed links.
	if scan.Problem != "" && scan.Truncated && len(scan.Files) == 0 {
		return nil, true, nil
	}
	if scan.Problem != "" && !scan.Truncated {
		return nil, false, errors.New(scan.Problem)
	}
	type predecessor struct {
		id string
		at time.Time
	}
	var matches []predecessor
	truncated := scan.Truncated
	for _, f := range scan.Files {
		switch f.Kind {
		case ledger.ProblemNone:
		case ledger.ProblemUnreadable:
			return nil, false, errors.New(f.Problem)
		case ledger.ProblemUnsafe, ledger.ProblemOversized:
			truncated = true
			continue
		default:
			continue
		}
		h := f.Header
		if h.LabDir != lab || h.Mode != "ticket" ||
			!strings.EqualFold(h.Issue.Repo, repo) || h.Issue.Number != number ||
			h.Workspace == nil || !strings.EqualFold(h.Workspace.Repo, repo) {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, h.StartedAt)
		if e != nil {
			continue
		}
		matches = append(matches, predecessor{h.ID, at})
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].at.Equal(matches[j].at) {
			return matches[i].id < matches[j].id
		}
		return matches[i].at.Before(matches[j].at)
	})
	ids := make([]string, 0, len(matches))
	for _, p := range matches {
		ids = append(ids, p.id)
	}
	return ids, truncated, nil
}
