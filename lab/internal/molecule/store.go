package molecule

import (
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/ledger"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"io"
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

// usageAttempt pairs a harness attempt's label with its parsed usage (#149); a nil
// usage reads as "unavailable", never as zero.
type usageAttempt struct {
	label string
	usage *harness.Usage
}

// moleculeUsageAttempts lists every harness attempt of the Molecule (analysis, coding
// and review) in a stable order, each with its usage or nil when unavailable.
func moleculeUsageAttempts(r Record) []usageAttempt {
	var attempts []usageAttempt
	for _, a := range r.AnalysisAttempts {
		attempts = append(attempts, usageAttempt{fmt.Sprintf("analysis attempt %d", a.Ordinal), a.Usage})
	}
	for _, a := range r.HarnessAttempts {
		attempts = append(attempts, usageAttempt{"coding attempt " + usageText(a.ID), a.Usage})
	}
	for _, a := range r.ReviewAttempts {
		attempts = append(attempts, usageAttempt{fmt.Sprintf("review attempt review-%d", a.Ordinal), a.Usage})
	}
	return attempts
}

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

func (t tokenTotal) text() string {
	if !t.reported || t.overflow {
		return "unreported"
	}
	return strconv.FormatInt(t.sum, 10)
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
	attempts := moleculeUsageAttempts(r)
	reported := false
	for _, a := range attempts {
		if a.usage != nil {
			reported = true
			break
		}
	}
	if !reported {
		return nil
	}
	var lines []string
	var inputT, outputT, cacheReadT, cacheCreationT tokenTotal
	costSum := 0.0
	costOverflow := false
	costCount, mismatchCount := 0, 0
	models := map[string]bool{}
	var reportedBases []string
	for _, a := range attempts {
		if a.usage == nil {
			lines = append(lines, "- "+a.label+": unavailable")
			continue
		}
		u := a.usage
		inputT.add(u.Total.Input)
		outputT.add(u.Total.Output)
		cacheReadT.add(u.Total.CacheRead)
		cacheCreationT.add(u.Total.CacheCreation)
		for _, m := range u.EffectiveModels {
			models[m] = true
		}
		detail := fmt.Sprintf("effective models %s · tokens input %s, output %s, cache-read %s, cache-creation %s",
			usageModelList(u.EffectiveModels, u.TruncatedModels),
			tokenText(u.Total.Input), tokenText(u.Total.Output), tokenText(u.Total.CacheRead), tokenText(u.Total.CacheCreation))
		if u.TotalCostUSD != nil {
			costCount++
			reportedBases = append(reportedBases, u.CostBasis)
			next := costSum + *u.TotalCostUSD
			if math.IsInf(next, 0) || math.IsNaN(next) {
				costOverflow = true
			} else {
				costSum = next
			}
			detail += " · cost " + formatUSD(*u.TotalCostUSD) + " (" + costBasisLabel(u.CostBasis) + ")"
		} else {
			detail += " · cost unavailable"
		}
		if u.ModelMismatch {
			mismatchCount++
			detail += " · MODEL MISMATCH vs pin (non-blocking)"
		}
		lines = append(lines, "- "+a.label+": "+detail)
	}
	lines = append(lines, fmt.Sprintf("Totals: input %s, output %s, cache-read %s, cache-creation %s tokens across effective models %s",
		inputT.text(), outputT.text(), cacheReadT.text(), cacheCreationT.text(), usageModelList(sortedKeys(models), 0)))
	coverage := fmt.Sprintf("cost reported for %d of %d harness attempts", costCount, len(attempts))
	switch {
	case costCount == 0:
		lines = append(lines, "Reported cost: unavailable — "+coverage)
	case costOverflow:
		lines = append(lines, fmt.Sprintf("Reported cost: unavailable (sum not finite) — %s (%s)", coverage, costBasisLabel(aggregateBasis(reportedBases))))
	default:
		// The basis label sits on the coverage phrase (#149 item 7) and so also labels
		// the dollar figure on this line (#149 review fix 4: never a bare dollar figure).
		lines = append(lines, fmt.Sprintf("Reported cost: %s total — %s (%s)", formatUSD(costSum), coverage, costBasisLabel(aggregateBasis(reportedBases))))
	}
	if mismatchCount > 0 {
		lines = append(lines, fmt.Sprintf("NOTICE: effective models differ from the pinned model for %d of %d harness attempt(s); this is non-blocking and never fails readiness or a gate.", mismatchCount, len(attempts)))
	}
	lines = append(lines, "Note: usage capture covers analysis, coding and review harness attempts; meta/formula-improvement harness usage is deferred to slice B (#149).")
	return lines
}

// History is advisory only. Bounds never broaden approval lookup or mutate predecessors.
func clarificationHistory(lab, repo string, number int64) ([]string, bool, error) {
	dir, err := os.Open(filepath.Join(lab, "ledgers"))
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(4097)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	// No partial directory ordering: oversize inventories have no guessed links.
	if len(names) > 4096 {
		return nil, true, nil
	}
	sort.Strings(names)
	type predecessor struct {
		id string
		at time.Time
	}
	var matches []predecessor
	total, files := int64(0), 0
	truncated := false
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if files >= 256 {
			truncated = true
			break
		}
		files++
		path := filepath.Join(lab, "ledgers", name)
		info, e := os.Lstat(path)
		if e != nil {
			return nil, false, e
		}
		if !info.Mode().IsRegular() || info.Size() > 5_000_000 {
			truncated = true
			continue
		}
		if total+info.Size() > 20_000_000 {
			truncated = true
			break
		}
		f, e := os.Open(path)
		if e != nil {
			return nil, false, e
		}
		raw, e := io.ReadAll(io.LimitReader(f, 5_000_001))
		f.Close()
		if e != nil {
			return nil, false, e
		}
		total += int64(len(raw))
		if len(raw) > 5_000_000 {
			truncated = true
			continue
		}
		header, e := ledger.Decode(raw)
		if e != nil || header.ID+".json" != name {
			continue
		}
		var r Record
		if json.Unmarshal(raw, &r) != nil || r.LabDir != lab || r.Mode != "ticket" ||
			!strings.EqualFold(r.Issue.Repo, repo) || r.Issue.Number != number ||
			!strings.EqualFold(r.Workspace.Repo, repo) {
			continue
		}
		at, e := time.Parse(time.RFC3339Nano, r.StartedAt)
		if e != nil {
			continue
		}
		matches = append(matches, predecessor{r.ID, at})
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
