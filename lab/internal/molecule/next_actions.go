package molecule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// Next actions are the ways out of a blocked, failed, interrupted or
// scope-change Molecule that an engine call accepts today. NextActions derives
// them from the ledger with the same preconditions as the engine calls behind
// them (AmendFromLatest, Recover, Retire and intake); it adds no engine
// semantics, records nothing and never approves. The engine checks every
// precondition again under its locks, so an action offered here can still be
// refused when the Lab changed in between, and never the other way round.
const (
	// NextAcceptProtectedFailures is amend --accept-protected-failures.
	NextAcceptProtectedFailures = "accept-protected-failures"
	// NextApproveScopeChange is amend --add-scope PATH --reason TEXT for each path
	// the coding harness requested.
	NextApproveScopeChange = "approve-scope-change"
	// NextRerunChecks is amend --continuation checks-only, after recover
	// --confirm-stopped when RecoverFirst is set.
	NextRerunChecks = "rerun-checks"
	// NextRecover is recover --plan DIGEST --confirm-stopped for an interrupted
	// Molecule whose owner is gone, the loop's recovery proposal.
	NextRecover = "recover"
	// NextFreshIntake is a fresh intake of the Issue (forgecell run ISSUE).
	NextFreshIntake = "fresh-intake"
	// NextRetire is retire --reason TEXT.
	NextRetire = "retire"
)

// MaxNextActionPaths bounds the scope paths one scope-change action offers; a
// request for more is not offered, never cut short.
const MaxNextActionPaths = 8

// maxNextActionQuestions bounds the open questions a fresh intake carries.
const maxNextActionQuestions = 8

// NextAction is one action an engine call accepts today. Digest is the latest
// plan digest the action binds as its parent; it is empty for retire and
// fresh intake. Paths and Reason are the coding harness's scope-change request,
// Deltas the tests accept-protected-failures would declare (set only by a
// preview), and Questions the open questions of a stopped intake. Every text is
// ledger content: a caller redacts and bounds it before showing it.
type NextAction struct {
	Kind         string
	Digest       string
	RecoverFirst bool
	Paths        []string
	Reason       string
	Deltas       []readiness.ProtectedDelta
	Questions    []string
}

// NextActions lists the actions the engine accepts for r today, in a fixed
// order. preview also derives, read-only through git, the exact deltas
// accept-protected-failures would declare; an action whose derivation the
// engine would refuse is then left out. recoveryTimeout is the loop's
// AnalysisTimeout: a ledger written more recently is not proposed for recovery.
func NextActions(ctx context.Context, lab string, r Record, preview bool, recoveryTimeout time.Duration) []NextAction {
	if refuseRetired(r) != nil || !safeLoopID(r.ID) {
		return nil
	}
	lab, err := filepath.Abs(lab)
	if err != nil {
		return nil
	}
	digest, hasPlan := latestReviewDigest(r)
	if r.Status == "running" || r.Status == "allocating" {
		// Only recovery applies to a ledger left running.
		if hasPlan && recoverable(lab, r, recoveryTimeout) {
			return []NextAction{{Kind: NextRecover, Digest: digest}}
		}
		return nil
	}
	var out []NextAction
	approvalLock := pathPresent(filepath.Join(lab, "ledgers", r.ID+".lock"))
	if hasPlan && !approvalLock && amendable(r) {
		latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
		phase := statePhase(r)
		retained := phase == "failed" || phase == "review-waiting" || phase == "interrupted"
		protected := false
		if retained && checksOnlyEligible(r) && protectedFailurePrecheck(r, latest) == nil {
			protected = true
			a := NextAction{Kind: NextAcceptProtectedFailures, Digest: digest}
			ok := true
			if preview {
				a.Deltas, err = protectedFailureDeltas(ctx, r, latest)
				ok = err == nil
			}
			if ok {
				out = append(out, a)
			}
		}
		if phase == "scope-change" {
			if a, ok := scopeChangeAction(r, latest); ok {
				out = append(out, a)
			}
		}
		if phase != "review-waiting" && retained && !protected && checksOnlyEligible(r) && environmentalFailure(r, digest) {
			out = append(out, NextAction{Kind: NextRerunChecks, Digest: digest})
		}
	}
	// A check-pending Molecule that stopped before its checks ran (a resource
	// floor, or the slot) is recovered first, as forgecell recover --plan DIGEST
	// --confirm-stopped does, and then continues checks-only.
	if hasPlan && statePhase(r) == "check-pending" && r.Status == "blocked" && checkPendingRecoverable(lab, r, approvalLock) {
		out = append(out, NextAction{Kind: NextRerunChecks, Digest: digest, RecoverFirst: true})
	}
	if a, ok := freshIntakeAction(r); ok {
		out = append(out, a)
	}
	if retireAllowed(lab, r, approvalLock) {
		out = append(out, NextAction{Kind: NextRetire})
	}
	return out
}

// latestReviewDigest is the latest plan digest when it is a SHA-256 digest.
func latestReviewDigest(r Record) (string, bool) {
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		return "", false
	}
	d := r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest
	if len(d) != 64 || strings.Trim(d, "0123456789abcdef") != "" {
		return "", false
	}
	return d, true
}

// pathPresent reports a path that exists or cannot be checked, so an
// uncertain lock always counts as held.
func pathPresent(path string) bool {
	_, err := os.Lstat(path)
	return !errors.Is(err, os.ErrNotExist)
}

// amendable holds the preconditions AmendFromLatest and amendLocked check from
// the ledger alone: a coding checkout, frozen repository evidence, a valid
// verification history with no live check process and a reconciled harness
// outcome.
func amendable(r Record) bool {
	if r.Workspace.Path == "" || (r.CheckoutStatus != "" && r.CheckoutStatus != "available") || r.RepositoryEvidence == nil {
		return false
	}
	if _, err := resolveExecutionRoles(r); err != nil {
		return false
	}
	return ValidateVerificationHistory(r) == nil && confirmVerificationProcessesStopped(r) == nil && retainedWorkCertain(r) == nil
}

// checksOnlyEligible is amendLocked's checks-only rule as the ledger shows it:
// captured completed coding output with no violation.
func checksOnlyEligible(r Record) bool {
	return r.Capture != nil && len(r.Capture.Violations) == 0 && completedCoding(r)
}

// protectedFailurePrecheck is the part of protectedFailureDeltas that reads
// only the ledger and the retained verification result: the latest
// verification of the latest plan and the retained tree failed only in the
// protected view, on required go test checks whose failures parse cleanly.
func protectedFailurePrecheck(r Record, latest readiness.Proposal) error {
	if len(r.VerificationAttempts) == 0 || r.Capture == nil || !reviewRevision.MatchString(r.Capture.Tree) {
		return errors.New("no verification of the retained tree")
	}
	tree := r.Capture.Tree
	a := r.VerificationAttempts[len(r.VerificationAttempts)-1]
	if a.Status != "failed" || a.Result == nil || a.SourceTree != tree || a.PlanDigest != latest.Digest {
		return errors.New("the latest verification is not a failure of the retained tree under the latest plan")
	}
	result, err := readVerificationResult(r, *a.Result)
	if err != nil {
		return err
	}
	if result.SchemaVersion != "v1" || result.SourceTree != tree {
		return errors.New("the latest verification result does not match the retained tree")
	}
	if err = candidateViewPassed(latest.Plan, result, tree); err != nil {
		return err
	}
	failing, err := failingProtectedChecks(latest.Plan, result)
	if err != nil {
		return err
	}
	for _, f := range failing {
		if _, err = verification.ProtectedFailuresForDeltas(f.obs); err != nil {
			return err
		}
	}
	return nil
}

// scopeChangeAction is the scope-change request of the latest coding attempt:
// exact, distinct, valid paths not yet in scope, each of which buildRevision
// (the --add-scope path, which only reads the ledger) accepts.
func scopeChangeAction(r Record, latest readiness.Proposal) (NextAction, bool) {
	if len(r.HarnessAttempts) == 0 {
		return NextAction{}, false
	}
	a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
	if a.Coding == nil || a.Coding.Outcome != "scope-change" || a.PlanDigest != latest.Digest || len(a.Coding.Paths) == 0 || len(a.Coding.Paths) > MaxNextActionPaths {
		return NextAction{}, false
	}
	reason := strings.TrimSpace(a.Coding.Reason)
	if readiness.ValidateOperatorText(reason, maxAmendTextBytes) != nil {
		// The operator edits the reason; a probe reason checks only the paths.
		reason = ""
	}
	probe := AmendRequest{}
	seen := map[string]bool{}
	for _, p := range a.Coding.Paths {
		if readiness.ValidatePath(p) != nil || seen[p] {
			return NextAction{}, false
		}
		seen[p] = true
		probe.AddScope = append(probe.AddScope, ScopeAddition{Path: p, Reason: "Requested by the coding harness"})
	}
	if probe.validate() != nil {
		return NextAction{}, false
	}
	if _, err := buildRevision(r, latest.Plan, probe); err != nil {
		return NextAction{}, false
	}
	return NextAction{Kind: NextApproveScopeChange, Digest: latest.Digest, Paths: append([]string(nil), a.Coding.Paths...), Reason: reason}, true
}

// environmentalFailure reports a verification that failed for reasons other
// than the retained code: it never started under the latest plan, it was
// interrupted, the Lab ran below a resource floor, a required check timed
// out, or every check passed and a later step (review, harness or provider)
// failed. A check that failed on its own is not environmental.
func environmentalFailure(r Record, digest string) bool {
	var last *VerificationAttempt
	for i := range r.VerificationAttempts {
		if r.VerificationAttempts[i].PlanDigest == digest {
			last = &r.VerificationAttempts[i]
		}
	}
	if last == nil || last.Status != "failed" {
		// Never started, interrupted, or passed with a later step failing.
		return true
	}
	v := r.Verification
	if v == nil {
		return true
	}
	if strings.HasPrefix(v.Error, "resources-exhausted") {
		return true
	}
	for _, view := range []*verification.Result{v, v.Protected} {
		if view == nil {
			continue
		}
		for _, obs := range view.Checks {
			if obs.Command.Required && !obs.Result.OK && obs.Result.TimedOut {
				return true
			}
		}
	}
	return v.RequiredChecksPassed && v.Error == "" && (v.Protected == nil || v.Protected.RequiredChecksPassed)
}

// checkPendingRecoverable holds Recover's preconditions for a stopped
// check-pending Molecule: completed coding output, a reconciled harness
// attempt with durable stopped-process evidence, a valid verification history
// and no approval or recovery lock left behind.
func checkPendingRecoverable(lab string, r Record, approvalLock bool) bool {
	if approvalLock || pathPresent(filepath.Join(lab, "ledgers", r.ID+".recovery-lock")) || !amendable(r) || !checksOnlyEligible(r) {
		return false
	}
	a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
	return a.Evidence != nil && a.Result.Reconciled
}

// recoverable is the loop's recovery proposal for a ledger left running: a
// plan and a reconciled harness attempt to name, not written for longer than
// timeout, and no live owner of its approval lock.
func recoverable(lab string, r Record, timeout time.Duration) bool {
	if pathPresent(filepath.Join(lab, "ledgers", r.ID+".recovery-lock")) {
		return false
	}
	if ValidateVerificationHistory(r) != nil {
		return false
	}
	_, ok := currentRecoveryProposal(lab, r.ID, timeout)
	return ok
}

// freshIntakeAction is a fresh intake for an intake that stopped on open
// questions or after its supplemental evidence passes were exhausted.
func freshIntakeAction(r Record) (NextAction, bool) {
	if r.Readiness != nil || (r.Status != "blocked" && r.Status != "failed") || r.Issue.Number < 1 {
		return NextAction{}, false
	}
	if r.Analysis != nil && len(r.Analysis.Questions) > 0 {
		a := NextAction{Kind: NextFreshIntake}
		for i, q := range r.Analysis.Questions {
			if i == maxNextActionQuestions {
				break
			}
			a.Questions = append(a.Questions, q.Question)
		}
		return a, true
	}
	if n := len(r.Notes); n > 0 && strings.HasPrefix(r.Notes[n-1], "Supplemental evidence passes exhausted") {
		return NextAction{Kind: NextFreshIntake}, true
	}
	return NextAction{}, false
}

// retireAllowed holds Retire's preconditions that a read can predict: not
// running, allocating or queued, no approval lock, no Lab delivery lock and a
// delivery receipt that is absent or published.
func retireAllowed(lab string, r Record, approvalLock bool) bool {
	if approvalLock || r.Status == "running" || r.Status == "allocating" || pathPresent(filepath.Join(lab, ".delivery-lock")) {
		return false
	}
	state, found, err := retireReceiptState(lab, r.ID)
	return err == nil && (!found || state == "published")
}

// RetireDeliveryInProgress reports a Molecule whose delivery Retire would
// refuse: the Lab's delivery lock exists, or its receipt is unreadable,
// prepared or committed.
func RetireDeliveryInProgress(lab, id string) bool {
	if !safeLoopID(id) || pathPresent(filepath.Join(lab, ".delivery-lock")) {
		return true
	}
	state, found, err := retireReceiptState(lab, id)
	return err != nil || (found && state != "published")
}
