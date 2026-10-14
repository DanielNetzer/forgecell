package molecule

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const (
	// maxReviewDiffBytes bounds the diff handed to the reviewer. A larger diff is not
	// truncated: the review is not run and the attempt fails closed.
	maxReviewDiffBytes = 400_000
	// maxReviewAttempts bounds ledger growth; one pass runs per harness attempt.
	maxReviewAttempts  = 5
	reviewTailBytes    = 3000
	maxReviewChecks    = 24
	reviewDetailRunes  = 400
	reviewReasonRunes  = 500
	reviewLedgerNotes  = 10
	reviewLedgerAtoms  = 16
	reviewDiffDeadline = 30 * time.Second
)

// Review attempt statuses. Only "no-high-findings" lets the Molecule continue to the
// human gate, and even then it is evidence, never an approval.
const (
	ReviewNoHighFindings = "no-high-findings"
	ReviewBlocked        = "blocked"
	ReviewFailed         = "failed"
)

// ReviewAttempt is the ledger record of one review pass. It holds sanitized,
// bounded findings and drafts, never the diff and never raw provider output.
type ReviewAttempt struct {
	Ordinal int `json:"ordinal"`
	// HarnessAttemptID is the coding attempt this pass reviewed; one pass runs for each.
	HarnessAttemptID string `json:"harnessAttemptId"`
	PlanDigest       string `json:"planDigest"`
	Tree             string `json:"tree"`
	Binding          string `json:"binding,omitempty"`
	BindingSource    string `json:"bindingSource,omitempty"`
	// SameCommandAsCoding is recorded and shown at the gate; it never decides the run.
	SameCommandAsCoding bool   `json:"sameCommandAsCoding,omitempty"`
	StartedAt           string `json:"startedAt"`
	FinishedAt          string `json:"finishedAt,omitempty"`
	Status              string `json:"status"`
	Reason              string `json:"reason,omitempty"`
	DiffBytes           int    `json:"diffBytes,omitempty"`
	// Redactions counts secret-like values replaced in the request, the diff being the
	// usual source. When it is not zero the reviewer saw incomplete content, and the
	// gate says so.
	Redactions int `json:"redactions,omitempty"`
	// ChangedFiles and ReviewedFiles are counts; the paths themselves live in the diff.
	ChangedFiles  int      `json:"changedFiles,omitempty"`
	ReviewedFiles int      `json:"reviewedFiles,omitempty"`
	High          int      `json:"high"`
	Medium        int      `json:"medium"`
	Low           int      `json:"low"`
	VerifiedSound []string `json:"verifiedSound,omitempty"`

	Findings  []harness.ReviewFinding  `json:"findings,omitempty"`
	FollowUps []harness.ReviewFollowUp `json:"followUps,omitempty"`
	// Diagnostics is absent when no review process ran.
	Diagnostics *process.Diagnostics `json:"diagnostics,omitempty"`
	// Usage records the effective model, tokens and reported cost parsed from the
	// Claude CLI final JSON (#149); additive and omitempty. It is absent for a
	// non-Claude binding, a Claude run that emitted none, and every earlier ledger,
	// all of which read as "unavailable" rather than as zero.
	Usage *harness.Usage `json:"usage,omitempty"`
}

// ReviewDecision tells the caller whether delivery is blocked. A review that found
// no high finding is not an approval and changes no gate.
type ReviewDecision struct {
	Blocked bool
	Status  string
	Detail  string
}

// ReviewInputs are the frozen facts one review pass reviews.
type ReviewInputs struct {
	HarnessAttemptID string
	PlanDigest       string
	Plan             readiness.Plan
	Capture          verification.CaptureResult
	Verified         verification.Result
}

func reviewAtomIndex(r Record) (int, bool) {
	for i, a := range r.Atoms {
		if a.Type == "review" {
			return i, true
		}
	}
	return 0, false
}

func reviewText(s string, limit int) string { return harness.ReviewDisplayText(s, limit) }

// reviewAttemptKey names the coding attempt a review pass belongs to. A continuation
// re-checks retained work without a new harness attempt, so each approved
// continuation is its own attempt.
func reviewAttemptKey(r Record, p readiness.Plan) string {
	key := "none"
	if n := len(r.HarnessAttempts); n > 0 {
		key = r.HarnessAttempts[n-1].ID
	}
	if p.Continuation != "" {
		key += fmt.Sprintf("/continuation-%d", len(r.VerificationAttempts))
	}
	return key
}

var reviewRevision = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)

// defaultReviewDiff reads the diff from the approved base commit to the captured
// tree. It never truncates: an oversize diff is an error.
func defaultReviewDiff(ctx context.Context, w Workspace, base, tree string) (string, error) {
	if !reviewRevision.MatchString(base) || !reviewRevision.MatchString(tree) {
		return "", fmt.Errorf("the base commit or captured tree is not an exact object id")
	}
	argv := []string{"git", "-c", "core.fsmonitor=false", "-c", "core.pager=cat", "diff", "--no-color", "--no-ext-diff", "--no-textconv", base, tree}
	result := process.Run(ctx, process.Options{Argv: argv, Dir: w.Path, Env: harness.ReviewEnvironment(os.Environ(), nil), Stdin: []byte{}, Timeout: reviewDiffDeadline, MaxOutputBytes: maxReviewDiffBytes + 1})
	if result.Overflow || result.Truncated || len(result.Stdout) > maxReviewDiffBytes {
		return "", fmt.Errorf("the diff exceeds %d bytes; the review was not run and nothing was truncated", maxReviewDiffBytes)
	}
	if !result.OK {
		return "", fmt.Errorf("the diff could not be read: %s", reviewText(result.Error+" "+result.Stderr, reviewDetailRunes))
	}
	return result.Stdout, nil
}

func defaultInvokeReview(ctx context.Context, b formula.Binding, dir string, timeout time.Duration, request map[string]any, withheld []string) process.Result {
	return harness.InvokeReviewBinding(ctx, b.Command, dir, timeout, request, withheld)
}

func tailOf(text string, limit int) string {
	if len(text) > limit {
		text = text[len(text)-limit:]
	}
	return strings.ToValidUTF8(text, "")
}

type reviewCheck struct {
	ID         string   `json:"id"`
	Category   string   `json:"category"`
	View       string   `json:"view"`
	OK         bool     `json:"ok"`
	ExitCode   int      `json:"exitCode"`
	TimedOut   bool     `json:"timedOut,omitempty"`
	Argv       []string `json:"argv"`
	StdoutTail string   `json:"stdoutTail,omitempty"`
	StderrTail string   `json:"stderrTail,omitempty"`
}

func reviewChecks(v verification.Result) (checks []reviewCheck, omitted int) {
	add := func(view string, observations []verification.Observation) {
		for _, o := range observations {
			if len(checks) >= maxReviewChecks {
				omitted++
				continue
			}
			checks = append(checks, reviewCheck{ID: o.ID, Category: o.Category, View: view, OK: o.Result.OK, ExitCode: o.Result.Code, TimedOut: o.Result.TimedOut, Argv: o.Command.Argv, StdoutTail: tailOf(o.Result.Stdout, reviewTailBytes), StderrTail: tailOf(o.Result.Stderr, reviewTailBytes)})
		}
	}
	add("setup", v.Setup)
	add("checks", v.Checks)
	if v.Protected != nil {
		add("protected-setup", v.Protected.Setup)
		add("protected-checks", v.Protected.Checks)
	}
	return checks, omitted
}

type reviewLedgerAtom struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

const reviewOmittedAuthored = "[omitted: text written by the coding harness]"

// minAuthoredMatch is the shortest coding explanation matched inside other text.
// Shorter ones carry no instruction worth hiding and would blank unrelated text.
const minAuthoredMatch = 12

// reviewLedger is a bounded view of the Molecule's own record: Atom states, the
// reviewed coding attempt's outcome class and the latest notes. It carries no paths
// of the Lab and no raw process streams. Text the coding harness wrote about its
// own change, such as its outcome reason, is left out: the reviewer judges the diff,
// not the author's account of it.
func reviewLedger(r Record, harnessAttemptID string) map[string]any {
	var authored []string
	for _, a := range r.HarnessAttempts {
		if a.Coding != nil && len(strings.TrimSpace(a.Coding.Reason)) >= minAuthoredMatch {
			authored = append(authored, a.Coding.Reason)
		}
	}
	tainted := func(text string) bool {
		for _, s := range authored {
			if strings.Contains(text, s) {
				return true
			}
		}
		return false
	}
	atoms := []reviewLedgerAtom{}
	for _, a := range r.Atoms {
		if len(atoms) >= reviewLedgerAtoms {
			break
		}
		detail := reviewText(a.Detail, reviewDetailRunes)
		if a.Type == "harness" || tainted(a.Detail) {
			detail = reviewOmittedAuthored
		}
		atoms = append(atoms, reviewLedgerAtom{ID: a.ID, Type: a.Type, Status: a.Status, Detail: detail})
	}
	view := map[string]any{"molecule": r.ID, "status": r.Status, "atoms": atoms}
	for _, a := range r.HarnessAttempts {
		if a.ID == harnessAttemptID && a.Coding != nil {
			view["codingOutcome"] = map[string]any{"outcome": a.Coding.Outcome}
		}
	}
	notes := []string{}
	for i := max(0, len(r.Notes)-reviewLedgerNotes); i < len(r.Notes); i++ {
		if tainted(r.Notes[i]) {
			notes = append(notes, reviewOmittedAuthored)
			continue
		}
		notes = append(notes, reviewText(r.Notes[i], reviewDetailRunes))
	}
	view["notes"] = notes
	return view
}

// redactTree redacts every string in place and adds the number of values replaced
// to *redactions.
func redactTree(v any, redactions *int) any {
	switch x := v.(type) {
	case string:
		out, n := harness.RedactSecretsCounted(x)
		*redactions += n
		return out
	case []any:
		for i := range x {
			x[i] = redactTree(x[i], redactions)
		}
		return x
	case map[string]any:
		for k, e := range x {
			x[k] = redactTree(e, redactions)
		}
		return x
	}
	return v
}

// expectedReviewedPaths are the changed paths exactly as the reviewer sees them in
// the redacted request, so a path that redaction rewrote can still be matched.
func expectedReviewedPaths(changes []string) []string {
	paths := make([]string, 0, len(changes))
	for _, c := range changes {
		paths = append(paths, harness.RedactSecrets(c))
	}
	return paths
}

// buildReviewRequest assembles what the reviewer sees: the issue, the approved
// plan without credential names, the diff, check results and a bounded ledger
// view. Every string passes the secret redaction rules, and the number of
// redactions is part of the request and returned. The environment of the Lab is
// never part of it.
func buildReviewRequest(r Record, in ReviewInputs, diff string) (map[string]any, int, error) {
	plan := in.Plan
	plan.Policy.CredentialNames = nil
	checks, omitted := reviewChecks(in.Verified)
	raw, err := json.Marshal(map[string]any{
		"kind":                 harness.ReviewKind,
		"moleculeId":           r.ID,
		"harnessAttemptId":     in.HarnessAttemptID,
		"issue":                r.Issue,
		"approvedPlan":         plan,
		"capturedTree":         in.Capture.Tree,
		"changedPaths":         in.Capture.Changes,
		"diff":                 diff,
		"checks":               checks,
		"checksOmitted":        omitted,
		"requiredChecksPassed": in.Verified.RequiredChecksPassed,
		"ledger":               reviewLedger(r, in.HarnessAttemptID),
	})
	if err != nil {
		return nil, 0, err
	}
	var request map[string]any
	if err = json.Unmarshal(raw, &request); err != nil {
		return nil, 0, err
	}
	redactions := 0
	request = redactTree(request, &redactions).(map[string]any)
	request["redactions"] = redactions
	return request, redactions, nil
}

// renderReviewAttempt writes one review pass into the Markdown ledger. Scalars are
// cut and stripped of control characters; the findings sit in a JSON block, which
// escapes whatever text remains.
func renderReviewAttempt(b *strings.Builder, a ReviewAttempt) {
	fmt.Fprintf(b, "\n## Adversarial review pass %d — %s\n\nHarness attempt: `%s`\nPlan: `%s`\nTree: `%s`\n", a.Ordinal, reviewText(a.Status, 40), reviewText(a.HarnessAttemptID, 200), reviewText(a.PlanDigest, 100), reviewText(a.Tree, 100))
	if a.Binding != "" {
		fmt.Fprintf(b, "Review binding: %s (%s)\n", reviewText(a.Binding, 120), reviewText(a.BindingSource, 60))
	}
	if a.SameCommandAsCoding {
		fmt.Fprintln(b, "NOTICE: the review binding runs the same command as the coding binding, so the coding harness reviewed its own change.")
	}
	if a.Redactions > 0 {
		fmt.Fprintf(b, "NOTICE: %d secret-like value(s) were redacted from the review request, so the reviewer's view was incomplete.\n", a.Redactions)
	}
	if a.ChangedFiles > 0 {
		fmt.Fprintf(b, "Coverage: the reviewer listed %d file(s) as reviewed for %d changed path(s).\n", a.ReviewedFiles, a.ChangedFiles)
	}
	fmt.Fprintf(b, "Findings: %d high, %d medium, %d low. Evidence only, never an approval.\n", a.High, a.Medium, a.Low)
	if a.Reason != "" {
		fmt.Fprintf(b, "Reason: %s\n", reviewText(a.Reason, reviewReasonRunes))
	}
	raw, _ := json.MarshalIndent(a, "", "  ")
	fmt.Fprintf(b, "\n```json\n%s\n```\n", raw)
}

// reviewVerdictLine is the review's line in the verification summary of the
// Markdown ledger, so passing required checks is never read as deliverable when the
// review blocked. It is empty when no review ran.
func reviewVerdictLine(r Record) string {
	if len(r.ReviewAttempts) == 0 {
		return ""
	}
	last := r.ReviewAttempts[len(r.ReviewAttempts)-1]
	if last.Status == ReviewNoHighFindings {
		return fmt.Sprintf("Adversarial review: no high finding (%d medium, %d low); evidence only, never an approval\n", last.Medium, last.Low)
	}
	return fmt.Sprintf("Adversarial review BLOCKED delivery (%s, %d high): passing the required checks does not make this change deliverable\n", reviewText(last.Status, 40), last.High)
}

func reviewCounts(findings []harness.ReviewFinding) (high, medium, low int) {
	for _, f := range findings {
		switch f.Severity {
		case "high":
			high++
		case "medium":
			medium++
		case "low":
			low++
		}
	}
	return high, medium, low
}

// runReviewAtom runs the review Atom once for one harness attempt and records the
// pass. Every failure, including an unresolvable binding, an oversize diff, a
// provider failure and malformed output, is recorded and blocks delivery; no
// failure is read as "no findings". The coding binding is never substituted. A
// record error is returned as an error.
func runReviewAtom(ctx context.Context, o Options, r Record, f formula.Formula, in ReviewInputs) (Record, ReviewDecision, error) {
	idx, ok := reviewAtomIndex(r)
	if !ok {
		return r, ReviewDecision{}, fmt.Errorf("the Molecule has no review Atom")
	}
	if len(r.ReviewAttempts) >= maxReviewAttempts {
		return r, ReviewDecision{}, fmt.Errorf("review history capacity reached; no review process was started")
	}
	for _, a := range r.ReviewAttempts {
		if a.HarnessAttemptID == in.HarnessAttemptID {
			return r, ReviewDecision{}, fmt.Errorf("a review pass already ran for harness attempt %s; a second pass is refused", in.HarnessAttemptID)
		}
	}
	atomID := r.Atoms[idx].ID
	attemptID := fmt.Sprintf("review-%d", len(r.ReviewAttempts))
	attempt := ReviewAttempt{Ordinal: len(r.ReviewAttempts), HarnessAttemptID: in.HarnessAttemptID, PlanDigest: in.PlanDigest, Tree: in.Capture.Tree, StartedAt: stamp()}
	record := func(status, reason string) (Record, ReviewDecision, error) {
		attempt.Status, attempt.Reason, attempt.FinishedAt = status, reviewText(reason, reviewReasonRunes), stamp()
		r.ReviewAttempts = append(r.ReviewAttempts, attempt)
		blocked := status != ReviewNoHighFindings
		detail := fmt.Sprintf("Adversarial review %s: %d high, %d medium, %d low finding(s). This is evidence, never an approval.", status, attempt.High, attempt.Medium, attempt.Low)
		if attempt.Reason != "" {
			detail += " " + attempt.Reason
		}
		if blocked {
			atom(&r, idx, "failed", detail+" Delivery is blocked.")
		} else {
			atom(&r, idx, "done", detail)
		}
		observeAtomAction(&r, atomID, attemptID, "run-adversarial-review", status, ProvenanceEvidence{Pointer: fmt.Sprintf("/reviewAttempts/%d", attempt.Ordinal)})
		if err := save(r); err != nil {
			return r, ReviewDecision{}, err
		}
		return r, ReviewDecision{Blocked: blocked, Status: status, Detail: detail}, nil
	}
	startAtomAction(&r, atomID, attemptID, in.PlanDigest, "review-binding", "run-adversarial-review")
	resolution, configured, err := onboarding.ResolveReviewBinding(f)
	if err == nil && !configured {
		err = fmt.Errorf("no review binding is configured: %s", onboarding.ReviewMissingBindingInstruction)
	}
	if err != nil {
		return record(ReviewFailed, "Review not run, no process started: "+err.Error())
	}
	attempt.Binding, attempt.BindingSource, attempt.SameCommandAsCoding = resolution.Binding.Binding, resolution.Source, resolution.SameCommandAsCoding
	recordExecutedBinding(&r, atomID, attemptID, resolution.Binding.Binding)
	diffFor := o.ReviewDiff
	if diffFor == nil {
		diffFor = defaultReviewDiff
	}
	diff, err := diffFor(ctx, r.Workspace, r.Workspace.BaseCommit, in.Capture.Tree)
	if err != nil {
		return record(ReviewFailed, "Review not run: "+err.Error())
	}
	if len(diff) > maxReviewDiffBytes {
		return record(ReviewFailed, fmt.Sprintf("Review not run: the diff exceeds %d bytes and was not truncated", maxReviewDiffBytes))
	}
	attempt.DiffBytes = len(diff)
	attempt.ChangedFiles = len(in.Capture.Changes)
	if len(in.Capture.Changes) > harness.MaxReviewedFiles {
		return record(ReviewFailed, fmt.Sprintf("Review not run: %d changed paths exceed the limit of %d a review can list as reviewed", len(in.Capture.Changes), harness.MaxReviewedFiles))
	}
	request, redactions, err := buildReviewRequest(r, in, strings.ToValidUTF8(diff, ""))
	if err != nil {
		return record(ReviewFailed, "Review not run: "+err.Error())
	}
	attempt.Redactions = redactions
	request = harness.WithModel(request, resolution.Binding.Model)
	invoke := o.InvokeReview
	if invoke == nil {
		invoke = defaultInvokeReview
	}
	result := invoke(ctx, resolution.Binding, r.Workspace.SourceRoot, time.Duration(resolution.Binding.TimeoutMS)*time.Millisecond, request, in.Plan.Policy.CredentialNames)
	attempt.Diagnostics = resultDiagnostics(result)
	// Effective model, tokens and reported cost for the review attempt (#149). A
	// non-Claude binding or a run that emitted none records no usage (unavailable).
	attempt.Usage = harness.AttemptUsage(resolution.Binding.Command, result.RawStdout, resolution.Binding.Model)
	if !result.OK {
		reason := "Review process failed: " + result.Error
		if result.TimedOut || result.ParentDeadline {
			reason = "Review timed out: " + result.Error
		}
		return record(ReviewFailed, reason)
	}
	outcome, err := harness.DecodeReview([]byte(result.Stdout))
	if err != nil {
		return record(ReviewFailed, harness.InvalidReviewPrefix+err.Error())
	}
	clean := harness.SanitizeReview(outcome)
	attempt.Findings, attempt.VerifiedSound = clean.Findings, clean.VerifiedSound
	attempt.High, attempt.Medium, attempt.Low = reviewCounts(clean.Findings)
	attempt.FollowUps = harness.ReviewFollowUps(clean.Findings)
	attempt.ReviewedFiles = len(outcome.ReviewedFiles)
	if missing := harness.MissingReviewedFiles(expectedReviewedPaths(in.Capture.Changes), outcome.ReviewedFiles); len(missing) > 0 {
		return record(ReviewFailed, fmt.Sprintf("Review incomplete: reviewedFiles omits %d of %d changed path(s), first %q; a partial review cannot clear delivery.", len(missing), len(in.Capture.Changes), reviewText(missing[0], 120)))
	}
	if attempt.High > 0 {
		return record(ReviewBlocked, fmt.Sprintf("%d high finding(s) block delivery.", attempt.High))
	}
	return record(ReviewNoHighFindings, "")
}
