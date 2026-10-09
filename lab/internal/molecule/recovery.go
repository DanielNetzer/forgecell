package molecule

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Recover records an uncertain outcome without executing checks or another
// harness. Explicit confirmation concerns all descendants, not just the Lab PID.
func Recover(ctx context.Context, lab, id, digest string, confirmedStopped bool) (Record, error) {
	var empty Record
	if !confirmedStopped {
		return empty, fmt.Errorf("first stop the Lab and its child processes, then use --confirm-stopped; recovery never retries coding")
	}
	absolute, err := filepath.Abs(lab)
	if err != nil {
		return empty, err
	}
	lab = absolute
	r, err := findPending(lab, digest)
	if err != nil {
		return r, err
	}
	if r.ID != id {
		return r, fmt.Errorf("Molecule and plan mismatch")
	}
	recoveryLock := filepath.Join(lab, "ledgers", r.ID+".recovery-lock")
	if err = os.Mkdir(recoveryLock, 0700); err != nil {
		return r, fmt.Errorf("recovery already active: %w", err)
	}
	defer os.Remove(recoveryLock)
	// Retain the original execution lock throughout recovery. It prevents an
	// approval from racing a recovery, and remains as evidence for interrupted work.
	lock := filepath.Join(lab, "ledgers", r.ID+".lock")
	created := false
	if err = os.Mkdir(lock, 0700); err == nil {
		created = true
	} else if !os.IsExist(err) {
		return r, err
	}
	if created {
		defer os.Remove(lock)
	}
	owner, err := os.ReadFile(filepath.Join(lock, "owner"))
	if err == nil {
		pid, e := strconv.Atoi(strings.TrimSpace(string(owner)))
		if e != nil || pid < 1 {
			return r, fmt.Errorf("invalid lock owner; inspect manually")
		}
		if e = confirmProcessExited(pid); e != nil {
			return r, e
		}
	} else if !os.IsNotExist(err) {
		return r, err
	} else if !created {
		return r, fmt.Errorf("ownerless execution lock cannot be safely recovered; an approval may still be starting")
	}
	r, err = findPending(lab, digest)
	if err != nil {
		return r, err
	}
	// A stopped Lab PID or a human assertion cannot replace a lost process
	// result. In particular, a crashed harness may still have live descendants.
	if len(r.HarnessAttempts) > 0 {
		a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
		if a.Evidence == nil || !a.Result.Reconciled {
			return r, fmt.Errorf("missing durable stopped-process evidence; recovery refused")
		}
	} else {
		return r, fmt.Errorf("legacy or interrupted coding has no durable process reconciliation evidence")
	}
	if err = ValidateVerificationHistory(r); err != nil {
		return r, err
	}
	if err = confirmVerificationProcessesStopped(r); err != nil {
		return r, err
	}
	p := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	paths := []string{}
	for _, s := range p.Analysis.Scope {
		paths = append(paths, s.Path)
	}
	capture, err := verification.Capture(ctx, r.Workspace.Path, p.Inputs.BaseCommit, paths, p.Artifacts)
	if err != nil {
		return r, err
	}
	// A crash after saving Result but before selecting the failure phase must
	// preserve the known failure, rather than rewriting it as an unknown result.
	if statePhase(r) == "coding" {
		a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
		raw, e := os.ReadFile(filepath.Join(r.LabDir, a.Evidence.Path))
		if e != nil {
			return r, e
		}
		var outcome process.Result
		if e = json.Unmarshal(raw, &outcome); e != nil {
			return r, e
		}
		coding := harness.CodingOutcomeFromResult(outcome.Stdout)
		if a.Coding != nil {
			coding = *a.Coding
		}
		phase, detail := "unknown", "Recovered coding with uncertain implementation; exact review required."
		if !outcome.OK {
			phase = "failed"
			detail = harnessOutcomeSummary(outcome, *a.Evidence)
		} else if a.Coding != nil && a.Capture != nil && a.CaptureError == "" && len(a.CaptureEvidence) > 0 && a.Capture.Tree == capture.Tree && artifactHash(*a.Capture) == artifactHash(capture) && len(a.Capture.Violations) == 0 && len(capture.Violations) == 0 && ((coding.Outcome == "completed" && len(a.Capture.Changes) > 0) || (coding.Outcome == "no-change" && p.AllowNoChange && len(a.Capture.Changes) == 0)) {
			phase = "check-pending"
			detail = "Recovered durable validated coding capture; explicit successor approval required before verification."
		} else if coding.Outcome == "blocked" || coding.Outcome == "unknown" || coding.Outcome == "scope-change" {
			phase = coding.Outcome
			detail = coding.Reason
		}
		{
			state, e := r.Readiness.FinishAttempt(phase, detail, stamp())
			if e != nil {
				return r, e
			}
			r.Readiness = &state
			atom(&r, 2, "failed", "Recovered "+phase+" coding outcome: "+detail+" Original output retained in immutable evidence.")
			if phase == "check-pending" {
				atom(&r, 2, "done", coding.Outcome+": "+coding.Reason)
			}
			r.Atoms[2].ExitCode = &outcome.Code
			r.Atoms[2].ElapsedMS = outcome.ElapsedMS
		}
	}
	state, err := r.Readiness.Interrupt(digest, "Human confirmed all processes stopped. Retained observed tree: "+capture.Tree+"; violations: "+strings.Join(capture.Violations, ", ")+". Original known harness outcome retained; no automatic retry or publication.", stamp())
	if err != nil {
		return r, err
	}
	state.Events[len(state.Events)-1].Tree = capture.Tree
	if len(capture.Violations) > 0 {
		state.Phase = "scope-violation"
		state.Events = append(state.Events, readiness.Event{Kind: "attempt-scope-violation", Digest: digest, At: stamp(), Tree: capture.Tree, Detail: "Recovery observed out-of-scope writes; exact existing-diff review is required."})
	}
	r.Readiness = &state
	for i := range r.VerificationAttempts {
		if r.VerificationAttempts[i].Status == "started" {
			r.VerificationAttempts[i].Status = "interrupted"
			r.VerificationAttempts[i].FinishedAt = stamp()
		}
	}
	if len(r.HarnessAttempts) > 0 {
		a := &r.HarnessAttempts[len(r.HarnessAttempts)-1]
		// Never reconcile changed source into an exact failed-attempt identity.
		if a.Capture != nil && a.CaptureError == "" && (a.Capture.Tree != capture.Tree || artifactHash(*a.Capture) != artifactHash(capture)) {
			return r, fmt.Errorf("failed-attempt tree or artifacts changed before recovery")
		}
		if a.CaptureError != "" || a.Capture == nil {
			ref, e := historyFile(r, fmt.Sprintf("harness-%s-recovery-%d.json", a.ID, len(a.CaptureEvidence)), capture)
			if e != nil {
				return r, e
			}
			a.CaptureEvidence = append(a.CaptureEvidence, ref)
			a.Capture = &capture
			a.CaptureError = ""
		}
		// A later plan may expand scope. Keep the original capture projection
		// bound to its immutable evidence; Recovery records the current view.
		a.Recovered = true
	}
	r.Capture = &capture
	r.Recovery = &capture
	r.Captures = append(r.Captures, capture)
	r, err = finishStopped(r, "Interrupted work preserved. Approval consumed; explicit amended plan review is required before any new attempt.")
	if err != nil {
		return r, err
	}
	// State is durably interrupted before retiring its stopped owner's lock.
	if err = os.Remove(filepath.Join(lock, "owner")); err != nil && !os.IsNotExist(err) {
		return r, err
	}
	if err = os.Remove(lock); err != nil {
		return r, err
	}
	return r, nil
}
