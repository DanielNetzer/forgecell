package ui

import (
	"errors"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"net/http"
)

// POST /api/molecules/{id}/retire {"reason"} retires one Molecule through
// molecule.Retire, behind the drive write protection (cookie, tab header, exact
// Origin, Fetch Metadata and one strict JSON object of at most 4 KiB). It is
// synchronous: the engine only appends the retired event under the Molecule
// lock. Committed comes only from the ledger read-back recording that event.
// Engine error text is never served: each refusal maps to a fixed category,
// message and reason. The one bulk retire, of the attempts for closed Issues,
// is bulk_retire.go: it calls the same engine retire for each Molecule and maps
// each refusal through retireRefusals.

// RetireAction is the action name /api/lab lists for the retire route.
const RetireAction = "retire Molecule"

type retireRequest struct {
	Reason string `json:"reason"`
}

// RetireView is every accepted retire response. Committed is true only when the
// ledger read back shows status retired with its retired event.
type RetireView struct {
	MoleculeID string        `json:"moleculeId"`
	Committed  bool          `json:"committed"`
	RetiredAt  string        `json:"retiredAt,omitempty"`
	Canonical  MoleculeState `json:"canonical"`
}

// retireRefusal is one fixed refusal for an engine retire error.
type retireRefusal struct {
	err                       error
	code                      int
	category, message, reason string
}

var retireRefusals = []retireRefusal{
	{molecule.ErrRetireInvalid, http.StatusBadRequest, "invalid", "the reason or Molecule id was refused; nothing was written", "Give a one-line reason of at most 200 characters and submit again"},
	{molecule.ErrRetireMissing, http.StatusNotFound, "missing", "no such Molecule", "No ledger in this Lab has this id; reload the Lab and choose a Molecule from the grid"},
	{molecule.ErrRetireLedger, http.StatusUnprocessableEntity, "refused", "the ledger was refused: it is invalid, unsafe, oversized or not this Lab's; nothing was written", "Inspect it with forgecell ledger; this page never retires a ledger it cannot read safely"},
	{molecule.ErrRetireActive, http.StatusConflict, "active", "the ledger shows this Molecule running, allocating or queued for checks; nothing was written", "Stop it or wait for it to finish, then retire it"},
	{molecule.ErrRetireLocked, http.StatusConflict, "locked", "the Molecule is locked by a loop, another forgecell command or an approval lock; nothing was written", "Wait for the loop or the other forgecell command to finish, then try again; inspect a left-over approval lock with forgecell ledger"},
	{molecule.ErrRetireRetired, http.StatusConflict, "retired", "this Molecule is already retired; nothing was written", "It stays under Retired with every record kept; nothing more is needed"},
	{molecule.ErrRetireDelivery, http.StatusConflict, "delivery", "a delivery is in progress for this Molecule or this Lab; nothing was written", "Finish or reconcile the delivery with forgecell deliver, then retire it"},
	{molecule.ErrRetireWrite, http.StatusInternalServerError, "write-failed", "the ledger could not be written; read it back before trying again", "Read the state back here or with forgecell ledger; nothing is retried"},
}

// refuseRetiredScope refuses a scope approval of a retired Molecule before any
// job starts, and reports whether it did. The engine refuses the same approval
// under the Molecule lock, so this is only the earlier, clearer refusal.
func (h *handler) refuseRetiredScope(w http.ResponseWriter, id, action string) bool {
	if action != "scope" || h.opts.Demo || !moleculeID.MatchString(id) {
		return false
	}
	record, err := h.records(h.opts.LabDir, id)
	if err != nil || (record.Status != molecule.StatusRetired && record.Retired == nil) {
		return false
	}
	h.refuseWith(w, http.StatusConflict, "retired", "this Molecule is retired; nothing was approved", "Start a new Molecule for the Issue; a retired Molecule keeps every record but is never resumed")
	return true
}

func (h *handler) retireMolecule(w http.ResponseWriter, id string, req retireRequest) {
	if _, err := molecule.RetireReason(req.Reason); err != nil {
		h.refuseWith(w, http.StatusBadRequest, "invalid", "give a one-line reason of at most 200 characters; nothing was written", retireRefusals[0].reason)
		return
	}
	if h.jobs.busy(id) {
		h.refuseGate(w, http.StatusConflict, "busy", "a gate job is still running for this Molecule; nothing was written")
		return
	}
	if _, err := h.retire(h.opts.LabDir, id, req.Reason); err != nil {
		for _, r := range retireRefusals {
			if errors.Is(err, r.err) {
				h.refuseWith(w, r.code, r.category, r.message, r.reason)
				return
			}
		}
		h.refuseGate(w, http.StatusUnprocessableEntity, "refused", "the engine refused the request; nothing else was attempted")
		return
	}
	v := RetireView{MoleculeID: id, Canonical: h.moleculeState(id)}
	if record, err := h.records(h.opts.LabDir, id); err == nil && record.Status == molecule.StatusRetired && record.Retired != nil {
		v.Committed, v.RetiredAt = true, stateText(record.Retired.At)
	}
	h.json(w, http.StatusOK, v)
}
