package ui

import (
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// racyWindow matches the ledger header cache: a file modified this recently is
// never cached, since a coarse clock could give a later write the same stamp.
const racyWindow = 2 * time.Second

// fileStamp identifies one version of a file without reading it.
type fileStamp struct {
	present     bool
	dev, ino    uint64
	size, mtime int64
}

// stamp reports a file's stamp; ok is false when the file cannot be stamped or
// was modified within racyWindow, and then nothing about it is cached.
func stamp(path string) (s fileStamp, ok bool) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return s, true
	}
	if err != nil {
		return s, false
	}
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat || time.Since(info.ModTime()) < racyWindow {
		return s, false
	}
	return fileStamp{present: true, dev: uint64(st.Dev), ino: st.Ino, size: info.Size(), mtime: info.ModTime().UnixNano()}, true
}

// summaryKey is a Molecule's ledger and receipt stamps: a Summary depends on
// nothing else in the Lab.
type summaryKey struct{ ledger, receipt fileStamp }

type cachedSummary struct {
	key     summaryKey
	summary Summary
}

// listCache keeps each Molecule's Summary while its ledger and receipt are
// unchanged, and lets concurrent requests for one ETag share a single build.
type listCache struct {
	mu      sync.Mutex
	entries map[string]cachedSummary
	flight  *listFlight
}

// listFlight is one list build in progress; waiters counts requests sharing it.
type listFlight struct {
	etag    string
	done    chan struct{}
	waiters int
	out     MoleculeList
}

func newListCache() *listCache { return &listCache{entries: map[string]cachedSummary{}} }

func (c *listCache) get(id string, key summaryKey) (Summary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[id]
	return e.summary, ok && e.key == key
}

// replace keeps only the Molecules the latest build saw.
func (c *listCache) replace(entries map[string]cachedSummary) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = entries
}

// list returns the Molecule list for etag. A request arriving while a build for
// the same ETag runs waits for that build instead of reading the Lab again.
func (h *handler) list(etag string) MoleculeList {
	c := h.cache
	c.mu.Lock()
	if f := c.flight; f != nil && f.etag == etag {
		f.waiters++
		c.mu.Unlock()
		<-f.done
		return f.out
	}
	f := &listFlight{etag: etag, done: make(chan struct{})}
	c.flight = f
	c.mu.Unlock()
	f.out = h.build(etag)
	c.mu.Lock()
	if c.flight == f {
		c.flight = nil
	}
	c.mu.Unlock()
	close(f.done)
	return f.out
}

// build reads the Lab once, reusing the cached Summary of every Molecule whose
// ledger and receipt stamps are unchanged. Stamps are taken before reading, so a
// write during the read is cached under the older stamp and re-read next time.
func (h *handler) build(etag string) MoleculeList {
	lab := h.opts.LabDir
	out := MoleculeList{ETag: etag, Molecules: []Summary{}}
	seen := map[string]cachedSummary{}
	inv := molecule.ListLedgers(lab, func(id string) error {
		ledger, ledgerOK := stamp(filepath.Join(lab, "ledgers", id+".json"))
		receipt, receiptOK := stamp(filepath.Join(lab, "deliveries", id+".json"))
		key, cacheable := summaryKey{ledger, receipt}, ledgerOK && receiptOK && ledger.present
		if cacheable {
			if s, ok := h.cache.get(id, key); ok {
				out.Molecules = append(out.Molecules, s)
				seen[id] = cachedSummary{key, s}
				return nil
			}
		}
		r, err := h.records(lab, id)
		if err != nil {
			return err
		}
		s := summarize(lab, r, h.receipts)
		out.Molecules = append(out.Molecules, s)
		if cacheable {
			seen[id] = cachedSummary{key, s}
		}
		return nil
	})
	h.cache.replace(seen)
	x := newRedactor(lab, molecule.Record{})
	out.Complete, out.Truncated, out.Diagnostics = inv.Complete, inv.Truncated, x.texts(inv.Diagnostics)
	sort.SliceStable(out.Molecules, func(i, j int) bool {
		a, b := out.Molecules[i], out.Molecules[j]
		if a.StartedAt != b.StartedAt {
			return a.StartedAt < b.StartedAt
		}
		return a.ID < b.ID
	})
	out.Counts = counts(out.Molecules)
	return out
}
