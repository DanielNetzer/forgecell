package ui

import (
	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"net/http"
	"strings"
)

// SyncView is GET /api/sync: the one sync state of every listed Molecule, keyed
// by Molecule id, with the GitHub state's age. Without GitHub state every
// Molecule keeps its ledger lane, labelled not checked; nothing is guessed.
type SyncView struct {
	Sample     bool   `json:"sample"`
	Repository string `json:"repository"`
	Freshness
	// StatesTruncated means more Issues and pull requests were wanted than one
	// sync reads; the rest are not checked.
	StatesTruncated bool                              `json:"statesTruncated"`
	Molecules       map[string]forgesync.DisplayState `json:"molecules"`
	// Counts tallies the buckets of the non-retired Molecules.
	Counts map[string]int `json:"counts"`
	// NeedsYou is every Molecule whose state needs you (bucket decision or
	// blocked), so no surface re-derives it.
	NeedsYou map[string]bool `json:"needsYou"`
}

// needsYou reports a state the counts header files under "need you".
func needsYou(d forgesync.DisplayState) bool {
	return d.Bucket == forgesync.BucketDecision || d.Bucket == forgesync.BucketBlocked
}

func (h *handler) syncStates(w http.ResponseWriter, r *http.Request) {
	if h.opts.Demo {
		h.json(w, http.StatusOK, demoSync())
		return
	}
	repo := h.syncRepository()
	out := SyncView{Repository: repo, Molecules: map[string]forgesync.DisplayState{}, Counts: map[string]int{}, NeedsYou: map[string]bool{}}
	work := h.syncWork(repo)
	var res forgesync.Result
	if repo == "" {
		res.Reason = forgesync.ReasonRepository
	} else {
		res = h.forge.cache.Get(r.Context(), work.want, refresh(r))
	}
	out.Freshness = freshness(res)
	snap := res.Snapshot
	var prs map[int64]forgesync.PR
	if snap != nil {
		prs, out.StatesTruncated = snap.PRs, snap.StatesTruncated
	}
	for _, s := range work.list.Molecules {
		m, ok := work.inputs[s.ID]
		var issue *forgesync.Issue
		if ok && strings.EqualFold(s.Repository, repo) {
			issue = snap.Issue(s.Number)
		} else {
			// Another repository's Molecule is never checked against this one.
			m = forgesync.Molecule{ID: s.ID, Lane: s.Lane, StartedAt: s.StartedAt}
		}
		d := forgesync.ForMolecule(issue, prs, m)
		if out.Stale {
			d.Startable = false
			d.Notes = append(d.Notes, forgesync.NoteStale)
		}
		out.Molecules[s.ID] = d
		if d.Bucket != forgesync.BucketRetired {
			out.Counts[d.Bucket]++
		}
		if needsYou(d) {
			out.NeedsYou[s.ID] = true
		}
	}
	h.json(w, http.StatusOK, out)
}
