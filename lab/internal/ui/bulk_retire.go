package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// Retire attempts for closed Issues (#243):
//
//	GET  /api/retire-closed                     → the exact list, its count and digest
//	POST /api/retire-closed {"count","digest"}  → molecule.Retire for each listed Molecule
//
// The list holds every Molecule of the active Formula's repository that is not
// retired, not running, allocating or queued, has no gate job of this UI and no
// delivery in progress, and whose Issue GitHub reports closed. Issue state comes
// from the read-only sync cache of /api/issues (#242) and must be current and
// complete; otherwise both routes refuse. The POST has the gate write
// protection and binds the exact list it was shown: the count and the SHA-256 of
// its Molecule ids, recomputed before any retire. Each Molecule is retired
// through the same engine call as the retire route, with a fixed reason, and
// reported on its own with the ledger read-back; a refusal never stops the rest
// and is never silent. The whole retire runs under one deadline: past it, each
// remaining Molecule is reported not attempted.

// BulkRetireAction is the action name /api/lab lists for the bulk retire route.
const BulkRetireAction = "retire closed Issues"

// BulkRetireReason is the fixed retire reason of every bulk retirement.
const BulkRetireReason = "Issue closed; superseded attempt"

// maxBulkRetire bounds the Molecules one bulk retire lists and retires; more are
// left for the next one and reported as truncated.
const maxBulkRetire = 200

// RetireClosedItem is one listed Molecule.
type RetireClosedItem struct {
	ID    string `json:"id"`
	Issue int64  `json:"issue"`
	Title string `json:"title"`
	Lane  string `json:"lane"`
}

// RetireClosedView is GET /api/retire-closed. Available is false when the Issue
// state is unavailable, stale or incomplete; Reason then says why and nothing is
// listed.
type RetireClosedView struct {
	Available bool               `json:"available"`
	Reason    string             `json:"reason,omitempty"`
	Count     int                `json:"count"`
	Truncated bool               `json:"truncated"`
	Digest    string             `json:"digest,omitempty"`
	Molecules []RetireClosedItem `json:"molecules"`
}

// RetireClosedResult is one Molecule's outcome. Committed comes only from the
// ledger read-back recording the retirement.
type RetireClosedResult struct {
	ID        string `json:"id"`
	Committed bool   `json:"committed"`
	Category  string `json:"category,omitempty"`
	Error     string `json:"error,omitempty"`
}

// RetireClosedReport is every accepted POST /api/retire-closed.
// NotAttempted counts the Molecules left when the overall deadline passed;
// each still has its own result.
type RetireClosedReport struct {
	Attempted    int                  `json:"attempted"`
	Retired      int                  `json:"retired"`
	Refused      int                  `json:"refused"`
	NotAttempted int                  `json:"notAttempted"`
	Truncated    bool                 `json:"truncated"`
	Results      []RetireClosedResult `json:"results"`
}

// bulkRetireTimeout bounds one bulk retire inside the server's write timeout.
const bulkRetireTimeout = 40 * time.Second

// retireClosedContext is one bulk retire's overall deadline; tests replace it
// through h.closedDeadline so no test waits on the wall clock.
func (h *handler) retireClosedContext() (context.Context, context.CancelFunc) {
	if h.closedDeadline != nil {
		return h.closedDeadline()
	}
	return context.WithTimeout(h.jobs.ctx, bulkRetireTimeout)
}

type retireClosedRequest struct {
	Count  int    `json:"count"`
	Digest string `json:"digest"`
}

var errClosedState = errors.New("closed Issue state unavailable")

// closedStateReasons are the fixed reasons the list is unavailable.
const (
	closedNoRepository = "this Lab's Formula names no GitHub repository, so Issue state cannot be read"
	closedUnavailable  = "GitHub Issue state could not be read just now; nothing can be retired as closed until it is current"
	closedIncomplete   = "the Lab's ledgers or the Issue states could not all be read; nothing is listed"
)

func listDigest(ids []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(sum[:])
}

// retireClosedList computes the exact list from the ledgers and the current
// Issue state.
func (h *handler) retireClosedList(r *http.Request) (RetireClosedView, error) {
	v := RetireClosedView{Molecules: []RetireClosedItem{}}
	repo := h.syncRepository()
	if repo == "" {
		v.Reason = closedNoRepository
		return v, errClosedState
	}
	work := h.syncWork(repo)
	res := h.forge.get(r.Context(), work.want, false)
	if !res.Current() || res.Snapshot == nil {
		v.Reason = closedUnavailable
		return v, errClosedState
	}
	if res.Snapshot.StatesTruncated || !work.list.Complete || work.list.Truncated {
		v.Reason = closedIncomplete
		return v, errClosedState
	}
	var items []RetireClosedItem
	for _, s := range work.list.Molecules {
		if !strings.EqualFold(s.Repository, repo) || s.Lane == LaneRetired || active(s.Status) || !moleculeID.MatchString(s.ID) {
			continue
		}
		issue := res.Snapshot.Issue(s.Number)
		if issue == nil || !issue.Found || issue.State != forgesync.IssueClosed {
			continue
		}
		if h.jobs.busy(s.ID) || molecule.RetireDeliveryInProgress(h.opts.LabDir, s.ID) {
			continue
		}
		items = append(items, RetireClosedItem{ID: s.ID, Issue: s.Number, Title: bounded(s.Title), Lane: s.Lane})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	if len(items) > maxBulkRetire {
		items, v.Truncated = items[:maxBulkRetire], true
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	v.Available, v.Count, v.Molecules = true, len(items), append(v.Molecules, items...)
	v.Digest = listDigest(ids)
	return v, nil
}

func (h *handler) retireClosedStatus(w http.ResponseWriter, r *http.Request) {
	if h.opts.Demo {
		h.refuseGate(w, http.StatusNotFound, "sample", "sample data cannot be retired")
		return
	}
	v, _ := h.retireClosedList(r)
	h.json(w, http.StatusOK, v)
}

// retireClosed serves the POST after the write protection and the demo check.
func (h *handler) retireClosed(w http.ResponseWriter, r *http.Request, req retireClosedRequest) {
	if !sha256Hex.MatchString(req.Digest) || req.Count < 1 || req.Count > maxBulkRetire {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give the exact count and list digest shown")
		return
	}
	list, err := h.retireClosedList(r)
	if err != nil {
		h.refuseGate(w, http.StatusConflict, "issue-state", list.Reason+"; nothing was retired")
		return
	}
	if list.Count != req.Count || list.Digest != req.Digest {
		h.refuseGate(w, http.StatusConflict, "stale", "the list of Molecules for closed Issues changed since it was shown; nothing was retired")
		return
	}
	report := RetireClosedReport{Truncated: list.Truncated, Results: []RetireClosedResult{}}
	ctx, cancel := h.retireClosedContext()
	defer cancel()
	for _, it := range list.Molecules {
		res := RetireClosedResult{ID: it.ID}
		// Past the overall deadline every remaining Molecule is reported, never
		// dropped.
		if ctx.Err() != nil {
			res.Category, res.Error = "not-attempted", "the bulk retire reached its time bound first; it was not attempted and is still listed"
			report.NotAttempted++
			report.Results = append(report.Results, res)
			continue
		}
		report.Attempted++
		if h.jobs.busy(it.ID) {
			res.Category, res.Error = "busy", "a gate job is running for this Molecule; it was not retired"
		} else if _, err := h.retire(h.opts.LabDir, it.ID, BulkRetireReason); err != nil {
			res.Category, res.Error = "refused", "the engine refused the request"
			for _, rr := range retireRefusals {
				if errors.Is(err, rr.err) {
					res.Category, res.Error = rr.category, rr.message
					break
				}
			}
		} else if record, err := h.records(h.opts.LabDir, it.ID); err == nil && record.Status == molecule.StatusRetired && record.Retired != nil {
			res.Committed = true
		} else {
			res.Category, res.Error = "unreadable", "the ledger read back does not record the retirement"
		}
		if res.Committed {
			report.Retired++
		} else {
			report.Refused++
		}
		report.Results = append(report.Results, res)
	}
	h.json(w, http.StatusOK, report)
}
