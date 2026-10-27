package ui

import (
	"context"
	"sync"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// Diff size states: measured from the captured tree, nothing captured yet, or
// not measurable now (no workspace, an invalid id or a git failure).
const (
	DiffMeasured    = "measured"
	DiffNone        = "none"
	DiffUnavailable = "unavailable"
)

// DiffSizeView is the "+412 −96 in 4 files" line of the review and decision
// cards: git diff --numstat of the Molecule's captured tree against its base
// commit, computed on demand by the engine and never taken from ledger text.
// Truncated means a bound stopped the count, so the numbers are a lower bound.
type DiffSizeView struct {
	State     string `json:"state"`
	Files     int    `json:"files,omitempty"`
	Added     int64  `json:"added,omitempty"`
	Removed   int64  `json:"removed,omitempty"`
	Binary    int    `json:"binary,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

const (
	// maxDiffCache bounds the sizes kept; the oldest is dropped first.
	maxDiffCache = 128
	// diffSizeTimeout bounds one measurement inside the server's write timeout.
	diffSizeTimeout = 25 * time.Second
)

// diffCache keeps each measured size by base commit and tree: those object ids
// name the exact content, so a size never goes stale. Failures are not kept.
type diffCache struct {
	mu      sync.Mutex
	entries map[string]molecule.DiffStat
	order   []string
}

func newDiffCache() *diffCache { return &diffCache{entries: map[string]molecule.DiffStat{}} }

func (c *diffCache) get(key string) (molecule.DiffStat, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.entries[key]
	return s, ok
}

func (c *diffCache) put(key string, s molecule.DiffStat) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		return
	}
	for len(c.order) >= maxDiffCache {
		delete(c.entries, c.order[0])
		c.order = c.order[1:]
	}
	c.entries[key] = s
	c.order = append(c.order, key)
}

// diffBase is the base commit the captured tree is measured against: the latest
// plan's frozen base, else the workspace's.
func diffBase(r molecule.Record) string {
	if r.Readiness != nil && len(r.Readiness.Plans) > 0 {
		if b := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.Inputs.BaseCommit; b != "" {
			return b
		}
	}
	return r.Workspace.BaseCommit
}

// diffSize measures the record's captured tree, from the cache when this exact
// base and tree were measured before. Nothing captured, or a capture with no
// change, is DiffNone without running git.
func (h *handler) diffSize(ctx context.Context, r molecule.Record) DiffSizeView {
	if r.Capture == nil || r.Capture.Tree == "" || len(r.Capture.Changes) == 0 {
		return DiffSizeView{State: DiffNone}
	}
	base, tree := diffBase(r), r.Capture.Tree
	key := base + ":" + tree
	s, ok := h.diffs.get(key)
	if !ok {
		ctx, cancel := context.WithTimeout(ctx, diffSizeTimeout)
		defer cancel()
		var err error
		if s, err = h.numstat(ctx, h.opts.LabDir, r.Workspace.Path, base, tree); err != nil {
			// The error is a fixed engine category; it is not served.
			return DiffSizeView{State: DiffUnavailable}
		}
		h.diffs.put(key, s)
	}
	return DiffSizeView{State: DiffMeasured, Files: s.Files, Added: s.Added, Removed: s.Removed, Binary: s.Binary, Truncated: s.Truncated}
}
