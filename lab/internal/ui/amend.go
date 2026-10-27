package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// POST /api/molecules/{id}/amend {"digest","addAcceptance","addScope"} is
// Request changes: it records a successor revision of the plan waiting for
// approval through molecule.AmendFromLatest, the engine path of forgecell amend
// --add-acceptance and --add-scope PATH --reason TEXT, without --run. It never
// approves: the new revision waits for its own exact approval through the scope
// gate. It has the gate write protection (cookie, tab header, exact Origin,
// Fetch Metadata and one strict JSON object of at most 4 KiB), refuses a digest
// that is not the one waiting the way the scope gate does, and serves only fixed
// refusal categories. Committed comes only from the ledger read-back recording
// a successor of exactly the submitted digest.

// AmendAction is the action name /api/lab lists for the Request changes route.
const AmendAction = "request plan changes"

const (
	// maxUIAmendAdditions bounds the acceptance notes and the scope paths of one
	// request; the engine allows more, the form sends one of each at most.
	maxUIAmendAdditions = 8
	// maxUIAmendText bounds one acceptance note or scope reason in bytes.
	maxUIAmendText = 1000
	// amendTimeout keeps an amendment inside the server's write timeout.
	amendTimeout = 45 * time.Second
)

type amendScope struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type amendRequest struct {
	Digest        string       `json:"digest"`
	AddAcceptance []string     `json:"addAcceptance"`
	AddScope      []amendScope `json:"addScope"`
}

// engineAmend is the CLI's amend request for the same additions: --add-scope
// PATH --reason TEXT pairs and --add-acceptance TEXT, in order, and nothing else
// (no note, continuation, accepted tree or --run).
func (q amendRequest) engineAmend() molecule.AmendRequest {
	var req molecule.AmendRequest
	for _, s := range q.AddScope {
		req.AddScope = append(req.AddScope, molecule.ScopeAddition{Path: s.Path, Reason: s.Reason})
	}
	req.AddAcceptance = append(req.AddAcceptance, q.AddAcceptance...)
	return req
}

// valid checks what the engine would refuse anyway, so a malformed request never
// reaches it: at least one addition, bounded counts and text, exact repository
// paths and no path twice.
func (q amendRequest) valid() bool {
	if len(q.AddAcceptance)+len(q.AddScope) == 0 || len(q.AddAcceptance) > maxUIAmendAdditions || len(q.AddScope) > maxUIAmendAdditions {
		return false
	}
	for _, a := range q.AddAcceptance {
		if readiness.ValidateOperatorText(a, maxUIAmendText) != nil {
			return false
		}
	}
	seen := map[string]bool{}
	for _, s := range q.AddScope {
		if readiness.ValidatePath(s.Path) != nil || readiness.ValidateOperatorText(s.Reason, maxUIAmendText) != nil || seen[s.Path] {
			return false
		}
		seen[s.Path] = true
	}
	return true
}

// AmendView is every accepted Request changes response. Committed is true only
// when the ledger read back holds a revision whose parent is the submitted digest
// and waits for approval; Digest and Revision are that revision's.
type AmendView struct {
	MoleculeID string        `json:"moleculeId"`
	Committed  bool          `json:"committed"`
	Revision   int           `json:"revision,omitempty"`
	Digest     string        `json:"digest,omitempty"`
	Canonical  MoleculeState `json:"canonical"`
}

// amendRefusal is one fixed refusal for engine error text that names its cause;
// its reason is categoryReasons[category].
type amendRefusal struct {
	parts             []string
	code              int
	category, message string
}

var amendRefusals = []amendRefusal{
	{[]string{"already in scope", "acceptance item already present"}, http.StatusConflict, "duplicate", "the plan already lists that path or acceptance note; nothing was recorded"},
	{[]string{"unsafe repository path", "needs evidence for its parent component", readiness.InScopeDefinitionMessage}, http.StatusUnprocessableEntity, "scope-refused", "the engine refused that scope path: it is unsafe, a frozen check definition, or no frozen manifest covers it; nothing was recorded"},
	{[]string{"coding checkout unavailable"}, http.StatusConflict, "checkout", "the coding checkout is unavailable; nothing was recorded"},
	{[]string{"is locked", "lock could not be taken"}, http.StatusConflict, "locked", "the Molecule is locked by another command or a loop; nothing was recorded"},
}

// amendStaleMessage is the stale refusal of Request changes, before or inside the
// engine lock: the plan shown is no longer the latest.
const amendStaleMessage = "a newer plan exists: this plan digest is not the one waiting for approval; reload the Molecule and review the current plan"

// refuseRetiredAmend refuses Request changes on a retired Molecule before the
// engine, which refuses it too, and reports whether it did.
func (h *handler) refuseRetiredAmend(w http.ResponseWriter, r molecule.Record) bool {
	if r.Status != molecule.StatusRetired && r.Retired == nil {
		return false
	}
	h.refuseWith(w, http.StatusConflict, "retired", "this Molecule is retired; nothing was recorded", "Start a new Molecule for the Issue; a retired Molecule keeps every record but is never resumed")
	return true
}

func (h *handler) amendPlan(w http.ResponseWriter, r molecule.Record, req amendRequest) {
	if !sha256Hex.MatchString(req.Digest) || !req.valid() {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give the exact plan digest shown and an acceptance note or a scope path with a one-line reason")
		return
	}
	if h.refuseRetiredAmend(w, r) {
		return
	}
	// One amendment at a time from this UI, so the waiting digest checked here is
	// the parent the engine amends.
	h.amendMu.Lock()
	defer h.amendMu.Unlock()
	record, err := h.records(h.opts.LabDir, r.ID)
	if err != nil {
		h.refuseGate(w, http.StatusUnprocessableEntity, "refused", "the ledger was refused: it is invalid, unsafe, oversized or not this Lab's")
		return
	}
	if h.refuseRetiredAmend(w, record) {
		return
	}
	if digest, pending := pendingDigest(record); !pending || req.Digest != digest {
		h.refuseGate(w, http.StatusConflict, "stale", amendStaleMessage)
		return
	}
	if h.jobs.busy(record.ID) {
		h.refuseGate(w, http.StatusConflict, "busy", "a gate job is still running for this Molecule; nothing was recorded")
		return
	}
	ctx, cancel := context.WithTimeout(h.jobs.ctx, amendTimeout)
	defer cancel()
	// The engine rechecks the shown digest under its lock, so a revision another
	// command recorded after the check above is refused as stale too.
	engineReq := req.engineAmend()
	engineReq.ExpectParent = req.Digest
	if _, err := guardedAmend(ctx, h.amend, h.opts.LabDir, record.ID, engineReq); err != nil {
		h.amendRefused(w, err)
		return
	}
	h.json(w, http.StatusOK, h.amendView(record.ID, req.Digest))
}

// guardedAmend runs the engine call; a panic becomes a refusal.
func guardedAmend(ctx context.Context, amend func(context.Context, string, string, molecule.AmendRequest) (molecule.Record, error), lab, id string, req molecule.AmendRequest) (r molecule.Record, err error) {
	defer func() {
		if recover() != nil {
			err = errGatePanic
		}
	}()
	return amend(ctx, lab, id, req)
}

// amendRefused maps an engine error to a fixed category; its text is never served.
func (h *handler) amendRefused(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		h.refuseGate(w, http.StatusServiceUnavailable, "interrupted", "the request was interrupted before the engine finished; read the Molecule back before trying again")
		return
	case errors.Is(err, molecule.ErrMoleculeRetired):
		h.refuseWith(w, http.StatusConflict, "retired", "this Molecule is retired; nothing was recorded", "Start a new Molecule for the Issue; a retired Molecule keeps every record but is never resumed")
		return
	case errors.Is(err, molecule.ErrStalePlan):
		h.refuseGate(w, http.StatusConflict, "stale", amendStaleMessage)
		return
	}
	text := err.Error()
	for _, a := range amendRefusals {
		for _, p := range a.parts {
			if strings.Contains(text, p) {
				h.refuseGate(w, a.code, a.category, a.message)
				return
			}
		}
	}
	h.refuseGate(w, http.StatusUnprocessableEntity, "refused", "the engine refused the request; nothing else was attempted")
}

// amendView reads the ledger back: committed once the latest plan is a successor
// of parent and waits for approval.
func (h *handler) amendView(id, parent string) AmendView {
	v := AmendView{MoleculeID: id, Canonical: h.moleculeState(id)}
	record, err := h.records(h.opts.LabDir, id)
	if err != nil || record.Readiness == nil || len(record.Readiness.Plans) == 0 {
		return v
	}
	latest := record.Readiness.Plans[len(record.Readiness.Plans)-1]
	if digest, pending := pendingDigest(record); pending && digest == latest.Digest && latest.Plan.ParentDigest == parent && sha256Hex.MatchString(latest.Digest) {
		v.Committed, v.Revision, v.Digest = true, latest.Plan.Revision, latest.Digest
	}
	return v
}
