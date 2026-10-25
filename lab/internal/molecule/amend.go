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
	unlockLoop, err := lockMolecule(lab, id)
	if err != nil {
		return r, lockedByLoop(err)
	}
	defer unlockLoop()
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
	if err = retainedWorkCertain(r); err != nil {
		return r, err
	}
	previous := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	if err = refuseSelectionInputChange(previous, next); err != nil {
		return r, err
	}
	// Lifecycle fields are runtime-owned; the submitted payload defines the actual
	// proposed scope/check changes, which remain visible under the new digest.
	next.SchemaVersion = previous.SchemaVersion
	next.MoleculeID = id
	next.Revision = previous.Revision + 1
	next.ParentDigest = parent
	// The coding allowlist is runtime-owned like the lifecycle fields: derive it
	// from the amended checks and scope so a submitted payload can neither smuggle nor drop it.
	var allowlistNote string
	if next.CodingAllowlist, allowlistNote, err = harness.DeriveCodingAllowlistNoted(r.FormulaSnapshot.YAML, next.Analysis.Checks, next.Analysis.Scope...); err != nil {
		return r, err
	}
	// Protected exemptions are runtime-owned too: only the evaluation runner sets them,
	// so an amendment keeps the previous ones that remain in the amended scope.
	next.ProtectedExemptPaths = nil
	amendedScope := map[string]bool{}
	for _, s := range next.Analysis.Scope {
		amendedScope[s.Path] = true
	}
	for _, file := range previous.ProtectedExemptPaths {
		if amendedScope[file] {
			next.ProtectedExemptPaths = append(next.ProtectedExemptPaths, file)
		}
	}
	approvedFormula, err := formula.Parse([]byte(r.FormulaSnapshot.YAML))
	if err != nil {
		return r, err
	}
	codingBinding, _ := approvedFormula.RoleBinding(formula.CodingHarness)
	if err = codingBinding.CheckTimeout(next.HarnessTimeoutMS); err != nil {
		return r, err
	}
	if r.RepositoryEvidence == nil {
		return r, fmt.Errorf("missing frozen repository evidence")
	}
	if err = r.RepositoryEvidence.ValidateScope(next.Analysis.Scope); err != nil {
		return r, err
	}
	// Newness is derived from the frozen tree, never from the submitted payload.
	next.NewFiles = readiness.CitedNewFiles(next.Analysis.Checks, readiness.NewScopeFiles(next.Analysis.Scope, r.RepositoryEvidence.Files))
	if err = next.ValidateDirectDeltaChecks(); err != nil {
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
	if note, ok := newFilesNote(next); ok {
		r.Notes = append(r.Notes, note)
	}
	if allowlistNote != "" {
		r.Notes = append(r.Notes, allowlistNote)
	}
	return r, save(r)
}

// refuseSelectionInputChange keeps a hand-written amendment from approving a check
// selection through a check input it supplied itself: only evaluation intake
// freezes the evaluation-plan input that evaluationPlanDeclaresSelect trusts. An
// amendment may neither add nor alter that input, and once the previous plan has a
// non-baseline check declaring select, its check inputs and evidence are fixed.
func refuseSelectionInputChange(previous, next readiness.Plan) error {
	// Nil and empty lists are the same list, as they are after a JSON round trip.
	same := func(a, b any) bool {
		va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
		if va.Len() != vb.Len() {
			return false
		}
		for i := 0; i < va.Len(); i++ {
			if !reflect.DeepEqual(va.Index(i).Interface(), vb.Index(i).Interface()) {
				return false
			}
		}
		return true
	}
	evaluationPlan := func(p readiness.Plan) []readiness.FileEvidence {
		var out []readiness.FileEvidence
		for _, input := range p.CheckInputs {
			if input.ID == evaluationPlanCheckInput {
				out = append(out, input)
			}
		}
		return out
	}
	if !same(evaluationPlan(previous), evaluationPlan(next)) {
		return fmt.Errorf("an amendment cannot add, change or remove the frozen %s check input; only an evaluation run freezes it", evaluationPlanCheckInput)
	}
	baseline := map[string]bool{}
	for _, id := range previous.FormulaBaseline {
		baseline[id] = true
	}
	for _, c := range previous.Analysis.Checks {
		if c.Select == "" || baseline[c.ID] {
			continue
		}
		if !same(previous.CheckInputs, next.CheckInputs) || !same(previous.Evidence, next.Evidence) {
			return fmt.Errorf("check %s declares select through the frozen evaluation plan, so an amendment must keep the check inputs and evidence unchanged", c.ID)
		}
		break
	}
	return nil
}

// retainedWorkCertain refuses an amendment while the latest harness outcome or
// capture is unreconciled.
func retainedWorkCertain(r Record) error {
	if len(r.HarnessAttempts) > 0 {
		a := r.HarnessAttempts[len(r.HarnessAttempts)-1]
		if a.Evidence == nil || !a.Result.Reconciled || a.CaptureError != "" {
			return fmt.Errorf("harness outcome or capture is uncertain; reconcile before amendment")
		}
	}
	if len(r.HarnessAttempts) == 0 && statePhase(r) == "interrupted" && !completedCoding(r) {
		return fmt.Errorf("legacy interrupted coding lacks durable process reconciliation evidence")
	}
	return nil
}

// errFormulaChanged is the refusal every Formula difference other than a
// launcher-only rebind keeps.
var errFormulaChanged = fmt.Errorf("issue or Formula changed; refresh intake")

// RebaseFormula carries the pending or retained plan of a Molecule to the active
// Formula when that Formula differs from the Molecule's snapshot only in release
// launcher paths. The successor revision keeps scope, checks, acceptance,
// artifacts and the retained tree; it rebinds only the Formula and binding
// identities and the derived coding allowlist, and waits for exact approval.
// Completed coding output continues as checks-only. Any other Formula difference
// is refused with the refresh-intake message and leaves the ledger unchanged.
func RebaseFormula(ctx context.Context, lab, id, parent string) (Record, error) {
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
	unlockLoop, err := lockMolecule(lab, id)
	if err != nil {
		return r, lockedByLoop(err)
	}
	defer unlockLoop()
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
	if err = retainedWorkCertain(r); err != nil {
		return r, err
	}
	// Load refuses a Formula write lock, an unfinished activation and any active
	// Formula without a complete, current exact-byte approval.
	loaded, err := formula.Load(lab, r.FormulaID)
	if err != nil {
		return r, err
	}
	if !loaded.Approved || loaded.Formula.ID != r.FormulaID {
		return r, errFormulaChanged
	}
	if loaded.Formula.SHA256 == r.FormulaSnapshot.SHA256 {
		return r, fmt.Errorf("active Formula equals the Molecule snapshot; nothing to rebase")
	}
	if formula.LauncherOnlyChange([]byte(r.FormulaSnapshot.YAML), []byte(loaded.Formula.YAML)) != nil {
		return r, errFormulaChanged
	}
	previous := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	raw, err := json.Marshal(previous)
	if err != nil {
		return r, err
	}
	var next readiness.Plan
	if err = json.Unmarshal(raw, &next); err != nil {
		return r, err
	}
	next.Revision = previous.Revision + 1
	next.ParentDigest = parent
	pending := statePhase(r) == "scope-waiting"
	if pending {
		// The adoption decision binds the old digest; it is not carried across.
		if previous.Continuation == "adopt-failed-tree" {
			return r, fmt.Errorf("Formula rebase cannot carry a pending failed-tree adoption; propose the adoption again after rebasing")
		}
	} else {
		if !completedCoding(r) {
			return r, fmt.Errorf("Formula rebase carries a pending plan or captured completed coding output only")
		}
		next.Continuation, next.AdoptAttempt = "checks-only", ""
	}
	codingBinding, _ := loaded.Formula.ExecutionBinding(formula.Coding)
	if err = codingBinding.CheckTimeout(next.HarnessTimeoutMS); err != nil {
		return r, err
	}
	next.Inputs.FormulaSHA256 = loaded.Formula.SHA256
	if next.Inputs.BindingSHA256, err = bindingHash(codingBinding, r.Workspace.SourceRoot, r.Workspace.BaseCommit); err != nil {
		return r, err
	}
	var allowlistNote string
	if next.CodingAllowlist, allowlistNote, err = harness.DeriveCodingAllowlistNoted(loaded.Formula.YAML, next.Analysis.Checks, next.Analysis.Scope...); err != nil {
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
	if len(capture.Violations) > 0 {
		return r, fmt.Errorf("unrecorded workspace violation; do not convert it to approved scope")
	}
	if previous.Revision > 1 && (capture.Tree != previous.PausedTree || (previous.PausedArtifactsSHA256 != "" && artifactHash(capture) != previous.PausedArtifactsSHA256)) {
		return r, fmt.Errorf("pending workspace changed; Formula rebase requires the retained tree and artifacts")
	}
	if previous.Revision == 1 && pending && len(capture.Changes) > 0 {
		return r, fmt.Errorf("pending workspace changed; Formula rebase requires the retained tree and artifacts")
	}
	if next.Continuation == "checks-only" && (r.Capture == nil || r.Capture.Tree != capture.Tree || artifactHash(capture) != artifactHash(*r.Capture) || !completedCoding(r)) {
		return r, fmt.Errorf("checks-only requires unchanged scope and captured completed coding output")
	}
	next.PausedTree = capture.Tree
	next.PausedArtifactsSHA256 = artifactHash(capture)
	state, err := r.Readiness.RebaseFormula(next, loaded.Approval.DecisionID, stamp())
	if err != nil {
		return r, err
	}
	if err = archiveLegacyVerification(&r); err != nil {
		return r, err
	}
	r.Readiness = &state
	// The replaced snapshot stays in the ledger, so the chain can be audited offline.
	r.PriorFormulaSnapshots = append(append([]PriorFormulaSnapshot(nil), r.PriorFormulaSnapshots...), PriorFormulaSnapshot{Snapshot: r.FormulaSnapshot, PlanDigest: latestPlanDigest(state), Decision: loaded.Approval.DecisionID})
	r.FormulaSnapshot = Snapshot{YAML: loaded.Formula.YAML, SHA256: loaded.Formula.SHA256}
	if err = ValidateFormulaRebases(r); err != nil {
		return r, err
	}
	if _, err = resolveExecutionRoles(r); err != nil {
		return r, err
	}
	r.Verification = nil
	if r.Capture != nil && len(r.Captures) == 0 {
		r.Captures = append(r.Captures, *r.Capture)
	}
	r.Capture = &capture
	r.Captures = append(r.Captures, capture)
	r.Status = "waiting"
	r.FinishedAt = ""
	atom(&r, roles.atoms[formula.ScopeApproval], "waiting", "Formula rebase requires exact approval; retained work has not been rerun.")
	r.Notes = append(r.Notes, fmt.Sprintf("Formula rebase r%d retains tree %s under Formula SHA-256 %s (activating decision %s). Compare immutable plans in the ledger before approval.", next.Revision, capture.Tree, loaded.Formula.SHA256, loaded.Approval.DecisionID))
	if allowlistNote != "" {
		r.Notes = append(r.Notes, allowlistNote)
	}
	// Recheck the activation immediately before the ledger write.
	if err = formula.Revalidate(lab, loaded); err != nil {
		return r, err
	}
	return r, save(r)
}

func latestPlanDigest(s readiness.State) string {
	if len(s.Plans) == 0 {
		return ""
	}
	return s.Plans[len(s.Plans)-1].Digest
}

// maxFormulaRebases bounds the rebase chain a ledger may carry.
const maxFormulaRebases = 64

// ValidateFormulaRebases re-checks, from the ledger alone, every FormulaRebased
// boundary: the retained prior snapshot is intact and is the boundary's FormulaFrom,
// it names the rebased plan and the event's activating decision, the replacing
// snapshot (the next prior one, or the current snapshot) is FormulaTo, and the
// change between them is launcher-only. Rebases recorded before prior snapshots
// were kept have none, so the snapshots match the latest rebase events; the
// earlier boundaries keep only the event lineage, which current() checks.
func ValidateFormulaRebases(r Record) error {
	if r.Readiness == nil {
		if len(r.PriorFormulaSnapshots) > 0 {
			return fmt.Errorf("prior Formula snapshots without readiness history")
		}
		return nil
	}
	if len(r.PriorFormulaSnapshots) == 0 {
		return nil
	}
	events, err := r.Readiness.FormulaRebases()
	if err != nil {
		return err
	}
	if len(events) > maxFormulaRebases || len(r.PriorFormulaSnapshots) > len(events) {
		return fmt.Errorf("prior Formula snapshots do not match the rebase events")
	}
	events = events[len(events)-len(r.PriorFormulaSnapshots):]
	for i, e := range events {
		prior := r.PriorFormulaSnapshots[i]
		next := r.FormulaSnapshot
		if i+1 < len(events) {
			next = r.PriorFormulaSnapshots[i+1].Snapshot
		}
		if prior.PlanDigest != e.Digest || prior.Decision != e.Decision || prior.Snapshot.SHA256 != e.FormulaFrom || next.SHA256 != e.FormulaTo ||
			fmt.Sprintf("%x", sha256.Sum256([]byte(prior.Snapshot.YAML))) != prior.Snapshot.SHA256 || fmt.Sprintf("%x", sha256.Sum256([]byte(next.YAML))) != next.SHA256 {
			return fmt.Errorf("Formula rebase %d does not match its retained snapshots", i+1)
		}
		if formula.LauncherOnlyChange([]byte(prior.Snapshot.YAML), []byte(next.YAML)) != nil {
			return fmt.Errorf("Formula rebase %d is not a launcher-only change", i+1)
		}
	}
	return nil
}

func artifactHash(c verification.CaptureResult) string {
	raw, _ := json.Marshal(c.Artifacts)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
