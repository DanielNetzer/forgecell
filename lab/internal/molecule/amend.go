package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
	"os"
	"path/filepath"
	"reflect"
)

// Amend records a human-authored successor proposal. Approval remains a separate
// operation and rechecks issue, Formula, binding and paused workspace identities.
func Amend(ctx context.Context, lab, id, parent string, next readiness.Plan, acceptTree string) (Record, error) {
	var empty Record
	lab, err := filepath.Abs(lab)
	if err != nil {
		return empty, err
	}
	r, err := findPending(lab, parent)
	if err != nil {
		return r, err
	}
	if _, err = resolveExecutionRoles(r); err != nil {
		return r, err
	}
	if r.ID != id {
		return r, fmt.Errorf("Molecule and parent plan mismatch")
	}
	lock := filepath.Join(lab, "ledgers", id+".lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return r, fmt.Errorf("Molecule is locked: %w", err)
	}
	defer os.Remove(lock)
	r, err = findPending(lab, parent)
	if err != nil {
		return r, err
	}
	roles, err := resolveExecutionRoles(r)
	if err != nil {
		return r, err
	}
	if err = ValidateVerificationHistory(r); err != nil {
		return r, err
	}
	if err = confirmVerificationProcessesStopped(r); err != nil {
		return r, err
	}
	if len(r.HarnessAttempts) > 0 {
		a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
		if a.Evidence == nil || !a.Result.Reconciled || a.CaptureError != "" {
			return r, fmt.Errorf("harness outcome or capture is uncertain; reconcile before amendment")
		}
	}
	if len(r.HarnessAttempts) == 0 && statePhase(r) == "interrupted" && !completedCoding(r) {
		return r, fmt.Errorf("legacy interrupted coding lacks durable process reconciliation evidence")
	}
	previous := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	// Lifecycle fields are runtime-owned; the submitted payload defines the actual
	// proposed scope/check changes, which remain visible under the new digest.
	next.SchemaVersion = previous.SchemaVersion
	next.MoleculeID = id
	next.Revision = previous.Revision + 1
	next.ParentDigest = parent
	// The coding allowlist is runtime-owned like the lifecycle fields: derive it
	// from the amended checks so a submitted payload can neither smuggle nor drop it.
	if next.CodingAllowlist, err = harness.DeriveCodingAllowlist(r.FormulaSnapshot.YAML, next.Analysis.Checks); err != nil {
		return r, err
	}
	if r.RepositoryEvidence == nil {
		return r, fmt.Errorf("missing frozen repository evidence")
	}
	if err = r.RepositoryEvidence.ValidateScope(next.Analysis.Scope); err != nil {
		return r, err
	}
	allowed := []string{}
	for _, s := range previous.Analysis.Scope {
		allowed = append(allowed, s.Path)
	}
	capture, err := verification.Capture(ctx, r.Workspace.Path, previous.Inputs.BaseCommit, allowed, previous.Artifacts)
	if err != nil {
		return r, err
	}
	nextCapture, err := verification.Capture(ctx, r.Workspace.Path, previous.Inputs.BaseCommit, allowed, next.Artifacts)
	if err != nil {
		return r, err
	}
	if nextCapture.Tree != capture.Tree {
		return r, fmt.Errorf("artifact amendment would reclassify existing source bytes; review or restore the existing state explicitly")
	}
	if next.Continuation == "checks-only" {
		pending := statePhase(r) == "scope-waiting"
		if (!pending && statePhase(r) != "failed" && statePhase(r) != "review-waiting" && statePhase(r) != "interrupted") || r.Capture == nil || r.Capture.Tree != capture.Tree || len(capture.Violations) > 0 || !reflect.DeepEqual(next.Analysis.Scope, previous.Analysis.Scope) || !completedCoding(r) {
			return r, fmt.Errorf("checks-only requires unchanged scope and captured completed coding output")
		}
		if pending {
			// Correct only the unexecuted proposal. Never recapture drift into a
			// fresh approval identity or turn pending coding into checks-only.
			if previous.Continuation != "checks-only" || next.AllowNoChange != previous.AllowNoChange ||
				capture.Tree != previous.PausedTree ||
				next.PausedTree != previous.PausedTree ||
				next.PausedArtifactsSHA256 != previous.PausedArtifactsSHA256 ||
				!reflect.DeepEqual(next.Inputs, previous.Inputs) ||
				!reflect.DeepEqual(next.Artifacts, previous.Artifacts) ||
				artifactHash(capture) != artifactHash(*r.Capture) ||
				(previous.PausedArtifactsSHA256 != "" && artifactHash(capture) != previous.PausedArtifactsSHA256) {
				return r, fmt.Errorf("pending checks-only amendment requires unchanged frozen inputs, paused tree and artifacts")
			}
		}
	}
	if next.Continuation == "adopt-failed-tree" {
		if !failedTreeEligible(r, next.AdoptAttempt) {
			return r, fmt.Errorf("failed-tree adoption requires the latest reconciled failed attempt")
		}
		a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
		if capture.Tree != a.Capture.Tree || artifactHash(capture) != artifactHash(*a.Capture) || artifactHash(nextCapture) != artifactHash(capture) {
			return r, fmt.Errorf("failed-tree adoption requires unchanged exact tree and artifacts")
		}
		if statePhase(r) != "interrupted" && statePhase(r) != "scope-violation" {
			return r, fmt.Errorf("recover the failed attempt before proposing adoption")
		}
		approved := []string{}
		for _, path := range next.Analysis.Scope {
			approved = append(approved, path.Path)
		}
		adopted, err := verification.Capture(ctx, r.Workspace.Path, previous.Inputs.BaseCommit, approved, next.Artifacts)
		if err != nil || len(adopted.Violations) > 0 {
			return r, fmt.Errorf("adopted tree exceeds successor exact scope")
		}
	}
	next.PausedTree = capture.Tree
	next.PausedArtifactsSHA256 = artifactHash(nextCapture)
	state := *r.Readiness
	if len(capture.Violations) > 0 || state.Phase == "scope-violation" {
		if state.Phase != "scope-violation" {
			return r, fmt.Errorf("unrecorded workspace violation; do not convert it to approved scope")
		}
		if acceptTree != capture.Tree {
			return r, fmt.Errorf("existing scope violation requires separate --accept-existing-tree %s after reviewing its diff", capture.Tree)
		}
		state, err = state.ReviewExistingDiff(parent, capture.Tree, stamp())
		if err != nil {
			return r, err
		}
	} else if acceptTree != "" {
		return r, fmt.Errorf("no violating diff to accept")
	}
	state, err = state.Amend(next, stamp())
	if err != nil {
		return r, err
	}
	if err = archiveLegacyVerification(&r); err != nil {
		return r, err
	}
	if next.Continuation == "adopt-failed-tree" {
		state.Events = append(state.Events, readiness.Event{Kind: "failed-tree-adopted", Digest: state.Plans[len(state.Plans)-1].Digest, At: stamp(), Tree: capture.Tree, Detail: next.AdoptAttempt})
		// The old violation remains on the failed attempt and in Captures history.
		paths := []string{}
		for _, s := range next.Analysis.Scope {
			paths = append(paths, s.Path)
		}
		capture, err = verification.Capture(ctx, r.Workspace.Path, previous.Inputs.BaseCommit, paths, next.Artifacts)
		if err != nil {
			return r, err
		}
	}
	r.Readiness = &state
	r.Verification = nil
	if r.Capture != nil && len(r.Captures) == 0 {
		r.Captures = append(r.Captures, *r.Capture)
	}
	r.Capture = &capture
	r.Captures = append(r.Captures, capture)
	r.Status = "waiting"
	r.FinishedAt = ""
	atom(&r, roles.atoms[formula.ScopeApproval], "waiting", "Amended scope requires exact approval; retained work has not been rerun.")
	r.Notes = append(r.Notes, fmt.Sprintf("Amendment r%d retains tree %s. Compare immutable plans in the ledger before approval.", next.Revision, capture.Tree))
	return r, save(r)
}

func artifactHash(c verification.CaptureResult) string {
	raw, _ := json.Marshal(c.Artifacts)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
