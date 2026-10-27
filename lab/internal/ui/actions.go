package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// "Why it's blocked · What you can do" (#243). GET /api/molecules/{id}/actions
// lists the next actions molecule.NextActions derives from the ledger, with
// the blocking reason bounded and redacted; it previews, read-only through
// git, the exact tests accept-protected-failures would declare. Each action is
// a POST with the gate write protection (cookie, tab header, exact Origin,
// Fetch Metadata and one strict JSON object of at most 4 KiB), taking the
// exact plan digest it was shown:
//
//	POST /api/molecules/{id}/actions/accept-failures {"digest","deltas"}
//	     → molecule.AmendFromLatest{AcceptProtectedFailures} (amend --accept-protected-failures)
//	POST /api/molecules/{id}/actions/scope-change    {"digest","paths","reason"}
//	     → molecule.AmendFromLatest{AddScope} (amend --add-scope PATH --reason TEXT)
//	POST /api/molecules/{id}/actions/rerun-checks    {"digest","confirmStopped"}
//	     → molecule.Recover first when the action says so, then
//	       molecule.AmendFromLatest{Continuation: checks-only}
//	POST /api/molecules/{id}/actions/recover         {"digest","confirmStopped"}
//	     → molecule.Recover(confirmedStopped)
//
// None of them approves: a revision they record waits for its own exact
// approval through the scope gate. An action the engine would refuse today is
// refused before the engine, the engine refuses a stale digest under its lock,
// and committed comes only from the ledger read-back. Accept-failures also
// sends the DeltasDigest of the tests it listed and is refused as stale when
// the tests derived again differ; scope-change sends the ledger's exact paths.
// When re-run checks recovered first and the amend after it fails, the refusal
// is recovered-not-amended: the recovery stays recorded. Fresh intake reuses POST
// /api/runs and retire the retire route; demo refuses every action.

// UnblockAction is the action name /api/lab lists for the next-action routes.
const UnblockAction = "unblock Molecule"

// Action kinds, the last path segment of each POST route.
const (
	ActionAcceptFailures = "accept-failures"
	ActionScopeChange    = "scope-change"
	ActionRerunChecks    = "rerun-checks"
	ActionRecover        = "recover"
	ActionFreshIntake    = "fresh-intake"
	ActionRetire         = "retire"
)

// actionKinds maps each POST route's kind to its engine next action.
var actionKinds = map[string]string{
	ActionAcceptFailures: molecule.NextAcceptProtectedFailures,
	ActionScopeChange:    molecule.NextApproveScopeChange,
	ActionRerunChecks:    molecule.NextRerunChecks,
	ActionRecover:        molecule.NextRecover,
}

// maxActionText bounds a shown path, test name or question in runes.
const maxActionText = 300

// ActionsView is GET /api/molecules/{id}/actions.
type ActionsView struct {
	MoleculeID string `json:"moleculeId"`
	// Blocking is why the Molecule stopped, bounded and redacted; "" when nothing
	// blocks it.
	Blocking string           `json:"blocking,omitempty"`
	Actions  []NextActionView `json:"actions"`
	// NotOffered are actions the engine would accept that this page cannot offer
	// exactly, each with the plain reason.
	NotOffered []NotOfferedView `json:"notOffered"`
}

// NotOfferedView is one action left out, with why, in plain words.
type NotOfferedView struct {
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

// scopePathsNotShown is why a scope change whose paths cannot be shown exactly
// is not offered: the POST sends the paths it showed, and they must be the
// ledger's own.
const scopePathsNotShown = "A requested path cannot be shown exactly as the coding agent wrote it, so this page does not offer to approve it; review the request in the Molecule's ledger."

// NextActionView is one offered action. Digest is the plan digest its POST must
// send back; ConfirmStopped means the POST needs the operator's explicit
// "I confirm it is stopped". Paths are the ledger's exact paths, which the
// POST sends back unchanged. DeltasDigest binds accept-failures to exactly the
// tests listed: its POST sends it back.
type NextActionView struct {
	Kind           string      `json:"kind"`
	Label          string      `json:"label"`
	Copy           string      `json:"copy"`
	Digest         string      `json:"digest,omitempty"`
	RecoverFirst   bool        `json:"recoverFirst,omitempty"`
	ConfirmStopped bool        `json:"confirmStopped,omitempty"`
	Paths          []string    `json:"paths,omitempty"`
	Reason         string      `json:"reason,omitempty"`
	Deltas         []DeltaView `json:"deltas,omitempty"`
	DeltasDigest   string      `json:"deltasDigest,omitempty"`
	Questions      []string    `json:"questions,omitempty"`
	Issue          int64       `json:"issue,omitempty"`
	IssueURL       string      `json:"issueUrl,omitempty"`
}

// DeltasDigest is the SHA-256 of the exact protected deltas an
// accept-failures preview lists, in order: package, test, check and declaring
// file of each, as the engine derives them.
func DeltasDigest(deltas []readiness.ProtectedDelta) string {
	raw, err := json.Marshal(deltas)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// shownExactly reports paths each of which is shown as itself, so the POST can
// send back the ledger's exact path.
func shownExactly(x redactor, paths []string) bool {
	for _, p := range paths {
		if actionText(x, p) != p {
			return false
		}
	}
	return true
}

// DeltaView is one test accept-protected-failures declares: its package, test,
// the check it failed on and the in-scope test file that declares it.
type DeltaView struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	Check   string `json:"check"`
	File    string `json:"file"`
}

var actionLabels = map[string][2]string{
	ActionAcceptFailures: {"Accept by-design test failures", "Only protected regression tests failed, and the new code passed every check. Accepting records a new version of the plan that expects exactly these failures; you approve it separately."},
	ActionScopeChange:    {"Approve the requested scope change", "The coding agent asked to change more files. This records a new version of the plan with these paths; you approve it separately."},
	ActionRerunChecks:    {"Re-run checks on the same code", "Coding finished and its output is kept, but the checks did not complete for reasons outside the code. This records a checks-only version of the plan; you approve it separately."},
	ActionRecover:        {"Recover this Molecule", "The process that ran it is gone. Recovery records the interruption and keeps the work; it never retries coding."},
	ActionFreshIntake:    {"Start a fresh intake", "Intake stopped before a plan. Answer the questions in the Issue on GitHub, then start a fresh intake."},
	ActionRetire:         {"Retire", "Retire it from the Danger zone below if it is no longer needed."},
}

func deltaViews(x redactor, deltas []readiness.ProtectedDelta) []DeltaView {
	out := make([]DeltaView, 0, len(deltas))
	for _, d := range deltas {
		out = append(out, DeltaView{Package: actionText(x, d.Package), Test: actionText(x, d.Test), Check: actionText(x, d.CheckID), File: actionText(x, d.ScopePath)})
	}
	return out
}

func actionText(x redactor, s string) string {
	return harness.ReviewDisplayText(x.text(redactSecrets(s)), maxActionText)
}

// analysisTimeout is the Molecule's Formula, whose AnalysisTimeout is the
// loop's recovery bound; an unreadable snapshot gives the default bound.
func analysisTimeout(r molecule.Record) (f formula.Formula) {
	if parsed, err := formula.Parse([]byte(r.FormulaSnapshot.YAML)); err == nil {
		f = parsed
	}
	return f
}

// nextActions is the engine's next actions for record, as this Lab UI offers
// them: none while a gate job of this UI runs for it, recovery only for work no
// one here runs, and a fresh intake only with a source checkout.
func (h *handler) nextActions(ctx context.Context, record molecule.Record, preview bool) []molecule.NextAction {
	if h.jobs.busy(record.ID) {
		return nil
	}
	var out []molecule.NextAction
	for _, a := range h.deriveActions(ctx, h.opts.LabDir, record, preview, molecule.AnalysisTimeout(analysisTimeout(record))) {
		switch {
		case a.Kind == molecule.NextRecover && h.runner(record, false) != RunnerOther:
		case a.Kind == molecule.NextFreshIntake && h.opts.SourceRoot == "":
		default:
			out = append(out, a)
		}
	}
	return out
}

func uiKind(kind string) string {
	for k, v := range actionKinds {
		if v == kind {
			return k
		}
	}
	switch kind {
	case molecule.NextFreshIntake:
		return ActionFreshIntake
	case molecule.NextRetire:
		return ActionRetire
	}
	return ""
}

func (h *handler) actionsView(ctx context.Context, record molecule.Record) ActionsView {
	x := newRedactor(h.opts.LabDir, record)
	v := ActionsView{MoleculeID: record.ID, Actions: []NextActionView{}, NotOffered: []NotOfferedView{}}
	if Lane(record.Status, phase(record), nil) == LaneBlocked {
		v.Blocking = blockedReason(record)
	}
	for _, a := range h.nextActions(ctx, record, true) {
		kind := uiKind(a.Kind)
		if kind == "" {
			continue
		}
		text := actionLabels[kind]
		av := NextActionView{Kind: kind, Label: text[0], Copy: text[1], Digest: a.Digest, RecoverFirst: a.RecoverFirst}
		switch kind {
		case ActionRecover:
			av.ConfirmStopped = true
		case ActionRerunChecks:
			av.ConfirmStopped = a.RecoverFirst
			if a.RecoverFirst {
				av.Copy = "Checks never ran: the Lab stopped before them, for example below a resource floor. Forgecell recovers the Molecule first, then records a checks-only version of the plan; you approve it separately."
			}
		case ActionScopeChange:
			if !shownExactly(x, a.Paths) {
				v.NotOffered = append(v.NotOffered, NotOfferedView{Kind: kind, Label: text[0], Reason: scopePathsNotShown})
				continue
			}
			av.Paths = append(av.Paths, a.Paths...)
			av.Reason = actionText(x, a.Reason)
		case ActionAcceptFailures:
			av.Deltas, av.DeltasDigest = deltaViews(x, a.Deltas), DeltasDigest(a.Deltas)
		case ActionFreshIntake:
			for _, q := range a.Questions {
				av.Questions = append(av.Questions, actionText(x, q))
			}
			av.Issue, av.IssueURL = record.Issue.Number, issueURL(record.Issue)
		}
		v.Actions = append(v.Actions, av)
	}
	return v
}

// actionRequest is every action POST. Deltas is the DeltasDigest of the tests
// accept-failures listed, and only that action takes it.
type actionRequest struct {
	Digest         string   `json:"digest"`
	Deltas         string   `json:"deltas"`
	Paths          []string `json:"paths"`
	Reason         string   `json:"reason"`
	ConfirmStopped bool     `json:"confirmStopped"`
}

// ActionEngineRequest is the engine amend request of an action that records a
// revision, exactly the request forgecell amend builds for the same flags,
// with ExpectParent set to the shown digest: --accept-protected-failures,
// --add-scope PATH --reason TEXT for each path, or --continuation checks-only.
func ActionEngineRequest(kind, digest string, paths []string, reason string) (molecule.AmendRequest, bool) {
	req := molecule.AmendRequest{ExpectParent: digest}
	switch kind {
	case ActionAcceptFailures:
		req.AcceptProtectedFailures = true
	case ActionScopeChange:
		for _, p := range paths {
			req.AddScope = append(req.AddScope, molecule.ScopeAddition{Path: p, Reason: reason})
		}
	case ActionRerunChecks:
		req.Continuation = "checks-only"
	default:
		return req, false
	}
	return req, true
}

// valid checks the request's shape for kind before anything is read: only
// the fields that kind takes, bounded.
func (q actionRequest) valid(kind string) bool {
	if !sha256Hex.MatchString(q.Digest) {
		return false
	}
	if (kind == ActionAcceptFailures) != (q.Deltas != "") || (q.Deltas != "" && !sha256Hex.MatchString(q.Deltas)) {
		return false
	}
	switch kind {
	case ActionScopeChange:
		if len(q.Paths) == 0 || len(q.Paths) > molecule.MaxNextActionPaths || q.ConfirmStopped || readiness.ValidateOperatorText(q.Reason, maxUIAmendText) != nil {
			return false
		}
		for _, p := range q.Paths {
			if readiness.ValidatePath(p) != nil {
				return false
			}
		}
		return true
	case ActionRecover:
		return len(q.Paths) == 0 && q.Reason == ""
	default:
		return len(q.Paths) == 0 && q.Reason == "" && (kind == ActionRerunChecks || !q.ConfirmStopped)
	}
}

// ActionResultView is every accepted action response. Committed is true only
// when the ledger read back records the action: a revision whose parent is the
// submitted digest waiting for approval, or for recovery the interruption.
type ActionResultView struct {
	MoleculeID string        `json:"moleculeId"`
	Kind       string        `json:"kind"`
	Committed  bool          `json:"committed"`
	Recovered  bool          `json:"recovered,omitempty"`
	Revision   int           `json:"revision,omitempty"`
	Digest     string        `json:"digest,omitempty"`
	Changes    string        `json:"changes,omitempty"`
	Deltas     []DeltaView   `json:"deltas,omitempty"`
	Canonical  MoleculeState `json:"canonical"`
}

// moleculeAction serves one POST action after the write protection passed and
// the ledger was read.
func (h *handler) moleculeAction(w http.ResponseWriter, r *http.Request, id, kind string) {
	var req actionRequest
	if decodeGate(w, r, &req) != nil || !req.valid(kind) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "send one small JSON object with exactly the action's fields and the plan digest shown")
		return
	}
	if kind != ActionScopeChange {
		req.Paths = nil
	}
	// One amendment or recovery at a time from this UI, so the action checked
	// here is the one the engine performs.
	h.amendMu.Lock()
	defer h.amendMu.Unlock()
	record, err := h.records(h.opts.LabDir, id)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		h.refuseGate(w, http.StatusNotFound, "missing", "no such Molecule")
		return
	default:
		h.refuseGate(w, http.StatusUnprocessableEntity, "refused", "the ledger was refused: it is invalid, unsafe, oversized or not this Lab's")
		return
	}
	if h.refuseRetiredAmend(w, record) {
		return
	}
	if h.jobs.busy(record.ID) {
		h.refuseGate(w, http.StatusConflict, "busy", "a gate job is still running for this Molecule; nothing was recorded")
		return
	}
	ctx, cancel := context.WithTimeout(h.jobs.ctx, amendTimeout)
	defer cancel()
	// Accept-failures derives its tests again, read-only, so the POST binds the
	// exact list it was shown, not only the plan digest.
	var offered *molecule.NextAction
	for _, a := range h.nextActions(ctx, record, kind == ActionAcceptFailures) {
		if a.Kind == actionKinds[kind] {
			a := a
			offered = &a
			break
		}
	}
	if offered == nil || (kind == ActionScopeChange && !shownExactly(newRedactor(h.opts.LabDir, record), offered.Paths)) {
		h.refuseGate(w, http.StatusConflict, "not-offered", "the engine would refuse this action in the Molecule's current state; nothing was recorded")
		return
	}
	if offered.Digest != req.Digest || (kind == ActionScopeChange && !reflect.DeepEqual(offered.Paths, req.Paths)) {
		h.refuseGate(w, http.StatusConflict, "stale", amendStaleMessage)
		return
	}
	if kind == ActionAcceptFailures && (len(offered.Deltas) == 0 || DeltasDigest(offered.Deltas) != req.Deltas) {
		h.refuseGate(w, http.StatusConflict, "stale", "the failing tests changed since they were shown: a newer verification failed differently; reload the Molecule and review the tests listed now; nothing was recorded")
		return
	}
	if (kind == ActionRecover || offered.RecoverFirst) && !req.ConfirmStopped {
		h.refuseGate(w, http.StatusBadRequest, "confirm-stopped", "confirm that every Lab process for this Molecule has stopped; nothing was recorded")
		return
	}
	v := ActionResultView{MoleculeID: record.ID, Kind: kind}
	if kind == ActionRecover || offered.RecoverFirst {
		if _, err := guardedRecover(ctx, h.recover, h.opts.LabDir, record.ID, req.Digest); err != nil {
			h.amendRefused(w, err)
			return
		}
		v.Recovered = h.recovered(record.ID, req.Digest)
		if kind == ActionRecover {
			v.Committed = v.Recovered
			v.Canonical = h.moleculeState(record.ID)
			h.json(w, http.StatusOK, v)
			return
		}
	}
	engineReq, _ := ActionEngineRequest(kind, req.Digest, req.Paths, req.Reason)
	if _, err := guardedAmend(ctx, h.amend, h.opts.LabDir, record.ID, engineReq); err != nil {
		if offered.RecoverFirst {
			// The recovery is recorded already; only the amend after it failed.
			h.refuseGate(w, http.StatusConflict, "recovered-not-amended", "recovery was recorded, but no new plan version exists: the checks-only amend after it was refused; reload the Molecule")
			return
		}
		h.amendRefused(w, err)
		return
	}
	read := h.amendView(record.ID, req.Digest)
	v.Committed, v.Revision, v.Digest, v.Canonical = read.Committed, read.Revision, read.Digest, read.Canonical
	if after, err := h.records(h.opts.LabDir, record.ID); v.Committed && err == nil && len(after.Readiness.Plans) > 1 {
		plans := after.Readiness.Plans
		next, parent := plans[len(plans)-1].Plan, plans[len(plans)-2].Plan
		v.Changes = harness.ReviewDisplayText(molecule.AmendDiffSummary(parent, next), maxReasonRunes)
		if kind == ActionAcceptFailures {
			v.Deltas = deltaViews(newRedactor(h.opts.LabDir, after), next.ExpectedProtectedDeltas)
		}
	}
	h.json(w, http.StatusOK, v)
}

// recovered reads the ledger back: the Molecule is no longer running and its
// latest plan, still digest, is interrupted.
func (h *handler) recovered(id, digest string) bool {
	r, err := h.records(h.opts.LabDir, id)
	if err != nil || active(r.Status) || r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		return false
	}
	p := phase(r)
	return r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest == digest && (p == "interrupted" || p == "scope-violation")
}

// guardedRecover runs the engine's recovery with the operator's confirmation;
// a panic becomes a refusal.
func guardedRecover(ctx context.Context, run func(context.Context, string, string, string, bool) (molecule.Record, error), lab, id, digest string) (r molecule.Record, err error) {
	defer func() {
		if recover() != nil {
			err = errGatePanic
		}
	}()
	return run(ctx, lab, id, digest, true)
}

// actionRoute reports a POST action path, its Molecule id and its kind.
func actionRoute(path string) (id, kind string, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/molecules/")
	if !found {
		return "", "", false
	}
	id, kind, found = strings.Cut(rest, "/actions/")
	if !found || strings.Contains(id, "/") || actionKinds[kind] == "" {
		return "", "", false
	}
	return id, kind, true
}
