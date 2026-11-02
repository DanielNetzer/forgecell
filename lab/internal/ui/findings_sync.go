package ui

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
)

// Background findings collection. Each GET /api/sync may start one pass, at
// most once per findingsSyncMinPoll, on the jobs' context, so it stops with
// the server. A pass collects, one at a time and oldest collection first, at
// most findingsSyncMaxPerPass published Molecules whose ledger-snapshot
// Formula declares reviewers and that have no complete findings round on the
// change request's current head, or whose newest round is incomplete or
// holds a pending or errored reviewer. Each Molecule is collected at most once
// per max(60s, the largest needs.minPollSeconds of its reviewer plugins); a
// failed collection, or one whose newest round is incomplete or holds an
// errored reviewer, doubles that wait for each further consecutive failure, up
// to one hour, as the keyed Issues view does. A Lab UI started without a source checkout runs nothing.
// The pass never proposes, dismisses or pushes.

const (
	// findingsSyncMinPoll is the shortest wait between passes and between two
	// collections of one Molecule.
	findingsSyncMinPoll = 60 * time.Second
	// findingsSyncMaxPerPass bounds the collections of one pass.
	findingsSyncMaxPerPass = 8
	// findingsSyncMaxScan bounds the published Molecules one pass reads.
	findingsSyncMaxScan = 500
	// findingsSyncTimeout bounds one collection.
	findingsSyncTimeout = 5 * time.Minute
)

// findingsSyncEntry is one Molecule's collection history in this Lab UI:
// when its latest background collection ended, whether it failed, and the
// wait stored after it (issuesKeyedBackoff).
type findingsSyncEntry struct {
	finished time.Time
	failed   bool
	ttl      time.Duration
}

// findingsSyncState is the background pass's state. mu is never held through
// a collection.
type findingsSyncState struct {
	mu         sync.Mutex
	entries    map[string]*findingsSyncEntry
	running    bool
	collecting string
	lastPass   time.Time
	// done is closed when the latest pass ends.
	done chan struct{}
}

func (s *findingsSyncState) collectingID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.collecting
}

// findingsSyncPoll is the collection wait of f's reviewers: findingsSyncMinPoll,
// or the largest pinned reviewer manifest's needs.minPollSeconds when that is
// longer. A manifest that cannot be loaded adds nothing.
func findingsSyncPoll(lab string, f formula.Formula) time.Duration {
	poll := findingsSyncMinPoll
	for _, r := range f.Reviewers() {
		for _, p := range f.Plugins {
			if p.Name != r.Plugin {
				continue
			}
			m, _, err := plugin.Load(lab, p.Manifest, p.SHA256)
			if err != nil {
				continue
			}
			if d := time.Duration(m.Needs.MinPollSeconds) * time.Second; d > poll && d <= issuesKeyedMaxPoll {
				poll = d
			}
		}
	}
	return poll
}

// findingsSyncKick starts one background pass over the listed Molecules when
// none runs and the last one started at least findingsSyncMinPoll ago. It
// never waits. Only published, live Molecules are passed on.
func (h *handler) findingsSyncKick(list []Summary) {
	s := h.findingsSync
	if s == nil || h.opts.Demo || h.opts.SourceRoot == "" {
		return
	}
	var ids []string
	for _, m := range list {
		if m.Lane != LaneRetired && m.Delivery == DeliveryLabel(&DeliveryState{State: "published"}) && !m.Inbox.Merged && len(ids) < findingsSyncMaxScan {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := h.findingsNow()
	if s.running || (!s.lastPass.IsZero() && now.Sub(s.lastPass) < findingsSyncMinPoll) {
		return
	}
	g := h.jobs
	g.mu.Lock()
	if g.ctx.Err() != nil {
		g.mu.Unlock()
		return
	}
	g.wg.Add(1)
	g.mu.Unlock()
	done := make(chan struct{})
	s.running, s.lastPass, s.done = true, now, done
	go func() {
		defer g.wg.Done()
		defer close(done)
		defer func() {
			s.mu.Lock()
			s.running, s.collecting = false, ""
			s.mu.Unlock()
		}()
		h.findingsSyncPass(g.ctx, ids)
	}()
}

// findingsSyncDue is one Molecule a pass may collect.
type findingsSyncDue struct {
	id       string
	f        formula.Formula
	last     time.Time
	interval time.Duration
}

// findingsSyncPass collects the due Molecules among ids, one at a time.
func (h *handler) findingsSyncPass(ctx context.Context, ids []string) {
	s := h.findingsSync
	lab := h.opts.LabDir
	now := h.findingsNow()
	var due []findingsSyncDue
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		r, err := h.records(lab, id)
		if err != nil {
			continue
		}
		f, ok := findingsSnapshot(r)
		if !ok {
			continue
		}
		receipt, err := delivery.ReadFixRoundReceipt(lab, id)
		if err != nil || !findingsLive(receipt) {
			continue
		}
		record, err := delivery.ReadFindings(lab, id)
		if err != nil {
			continue
		}
		head, _ := delivery.ReceiptHead(receipt)
		if !findingsSyncWanted(record, head) {
			continue
		}
		interval := max(findingsSyncMinPoll, h.findingsPoll(lab, f))
		last := findingsNewestCollection(record)
		wait := interval
		s.mu.Lock()
		if e := s.entries[id]; e != nil {
			wait = max(wait, e.ttl)
			if e.finished.After(last) {
				last = e.finished
			}
		}
		s.mu.Unlock()
		if !last.IsZero() && now.Sub(last) < wait {
			continue
		}
		due = append(due, findingsSyncDue{id: id, f: f, last: last, interval: interval})
	}
	sort.SliceStable(due, func(i, j int) bool {
		if !due[i].last.Equal(due[j].last) {
			return due[i].last.Before(due[j].last)
		}
		return due[i].id < due[j].id
	})
	if len(due) > findingsSyncMaxPerPass {
		due = due[:findingsSyncMaxPerPass]
	}
	for _, d := range due {
		if ctx.Err() != nil {
			return
		}
		if h.jobs.busy(d.id) || h.jobs.busy(findingsJobKey(d.id)) || h.jobs.busy(fixRoundJobKey(d.id)) {
			continue
		}
		s.mu.Lock()
		s.collecting = d.id
		s.mu.Unlock()
		cctx, cancel := context.WithTimeout(ctx, findingsSyncTimeout)
		var collected delivery.FindingsRecord
		err := guarded(cctx, func(ctx context.Context) error {
			var err error
			collected, err = h.findingsCollect(ctx, lab, d.id, d.f)
			return err
		})
		cancel()
		failed := err != nil || findingsSyncFailedRound(collected)
		h.findingsForget(d.id)
		s.mu.Lock()
		s.collecting = ""
		if ctx.Err() != nil {
			// A stopping server's interrupted collection is no failure to back off.
			s.mu.Unlock()
			return
		}
		e := s.entries[d.id]
		if e == nil {
			e = &findingsSyncEntry{}
			s.entries[d.id] = e
		}
		e.ttl = issuesKeyedBackoff(d.interval, e.ttl, e.failed, failed)
		e.finished, e.failed = h.findingsNow(), failed
		s.mu.Unlock()
	}
}

// findingsSyncFailedRound reports a collection whose newest round is cut by a
// bound or holds a reviewer that errored: it backs off as a failure does.
func findingsSyncFailedRound(record delivery.FindingsRecord) bool {
	n := len(record.Rounds)
	if n == 0 {
		return false
	}
	newest := record.Rounds[n-1]
	if !newest.Complete {
		return true
	}
	for _, e := range newest.Reviewers {
		if e.Status == delivery.FindingsError {
			return true
		}
	}
	return false
}

// findingsSyncWanted reports a findings record that needs a collection: no
// complete round on head, or a newest round that is incomplete or holds a
// pending or errored reviewer.
func findingsSyncWanted(record delivery.FindingsRecord, head string) bool {
	n := len(record.Rounds)
	if n == 0 {
		return true
	}
	newest := record.Rounds[n-1]
	if !newest.Complete {
		return true
	}
	for _, e := range newest.Reviewers {
		if e.Status == delivery.FindingsPending || e.Status == delivery.FindingsError {
			return true
		}
	}
	for i := n - 1; i >= 0; i-- {
		if record.Rounds[i].Complete && record.Rounds[i].Commit == head {
			return false
		}
	}
	return true
}

// findingsNewestCollection is the newest round's collection time, or zero.
func findingsNewestCollection(record delivery.FindingsRecord) time.Time {
	if n := len(record.Rounds); n > 0 {
		if t, err := time.Parse(time.RFC3339, record.Rounds[n-1].CollectedAt); err == nil {
			return t
		}
	}
	return time.Time{}
}
