package ui

import (
	"context"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// Keyed Issues (plugins slice 4c). A Lab whose active approved Formula reads
// Issues from a tracker plugin starts, shows and approves its Molecules by the
// tracker's own key, through the same engine calls the CLI uses for
// forgecell run KEY:
//
//	POST /api/runs          {"key"}  → molecule.RunIntakeKeyed (intake) as a job keyed "key:"+key
//	GET  /api/runs?key=KEY           → the job and its ledger read-back
//
// The key is checked with intake.ValidateIssueKey, then with
// intake.ParseIssueKey against the tracker's anchored keyPattern, read once per
// Formula with tracker_describe and cached. Every Issue read goes through
// tracker_get and is recorded on the Molecule (molecule.PluginCallLog). A
// numeric request on such a Lab, and a key on a GitHub Lab, are refused before
// anything starts. Plugin error text is never served.

const (
	// keyedRunJobPrefix starts every keyed run's job key. ":" never appears in
	// runKey's "#N" or in a Molecule id, so the keys never collide.
	keyedRunJobPrefix = "key:"
	// keyedDescribeTimeout bounds one tracker_describe of the drive routes.
	keyedDescribeTimeout = 30 * time.Second
	// keyedMaxReads bounds the ledgers one keyed lookup reads to match a raw key;
	// past it the lookup reports an incomplete inventory.
	keyedMaxReads = 64
	// maxKeyedURLBytes bounds a recorded tracker Issue URL that is linked.
	maxKeyedURLBytes = 2048
)

// keyedRunJob is the job key of a keyed run.
func keyedRunJob(key string) string { return keyedRunJobPrefix + key }

// keyedActiveFormula is the active approved Formula when its intake.source is
// a declared tracker plugin, with the identity its describe is cached under.
type keyedActiveFormula struct {
	formula  formula.Formula
	identity string
}

// loadKeyedFormula reads the active Formula as forgecell run does: a Formula
// that cannot be loaded, is not approved or reads GitHub Issues is not keyed,
// and the numeric path runs as it always has.
func loadKeyedFormula(lab string) (keyedActiveFormula, bool) {
	loaded, err := formula.Load(lab, "")
	if err != nil || !loaded.Approved {
		return keyedActiveFormula{}, false
	}
	if _, ok := loaded.Formula.TrackerPlugin(); !ok {
		return keyedActiveFormula{}, false
	}
	return keyedActiveFormula{formula: loaded.Formula, identity: loaded.Formula.ID + "\x00" + loaded.Approval.SHA256}, true
}

// keyedDescribeCache is the tracker's anchored keyPattern for one Formula
// identity: described once, at the first keyed start or once in the background
// when the Lab can drive, and never again per request. A failed describe is
// cached too, so a poll never starts the plugin again; only a keyed start
// retries it. GET /api/lab reads it under mu alone, which is never held
// through a describe, so a hanging plugin never holds up the Lab view.
type keyedDescribeCache struct {
	// describing is held through one describe, by a keyed start or the
	// background describe only, so the plugin starts at most once at a time.
	describing sync.Mutex
	mu         sync.Mutex
	identity   string
	tried      bool
	pattern    string
	// background is the identity a background describe was started for, and
	// backgroundDone closes when it has finished.
	background     string
	backgroundDone chan struct{}
}

// cached is the stored pattern for identity, and whether it was described.
func (c *keyedDescribeCache) cached(identity string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tried || c.identity != identity {
		return "", false
	}
	return c.pattern, true
}

// keyedLabDir is the Lab's real path, which the adapter needs.
func (h *handler) keyedLabDir() (string, bool) {
	resolved, err := filepath.EvalSymlinks(h.opts.LabDir)
	return resolved, err == nil
}

// keyedPattern is the tracker's anchored keyPattern for f, or false when the
// tracker could not describe a usable one. retry describes again after a
// cached failure. The describing lock is held through the describe, so
// concurrent starts start the plugin at most once; the fields' lock is not.
func (h *handler) keyedPattern(ctx context.Context, f keyedActiveFormula, retry bool) (string, bool) {
	c := &h.keyedPatterns
	c.describing.Lock()
	defer c.describing.Unlock()
	if pattern, tried := c.cached(f.identity); tried && (pattern != "" || !retry) {
		return pattern, pattern != ""
	}
	lab, ok := h.keyedLabDir()
	if !ok {
		return "", false
	}
	call, cancel := context.WithTimeout(ctx, keyedDescribeTimeout)
	defer cancel()
	pattern := ""
	if keyed, plugin, err := h.trackerFor(lab, f.formula, mcpadapter.Options{}); err == nil && plugin && keyed != nil {
		if d, err := keyedDescribe(call, keyed); err == nil {
			pattern = keyedAnchored(d.KeyPattern)
		}
	}
	if pattern == "" && ctx.Err() != nil {
		// A request that went away describes nothing: it is not cached.
		return "", false
	}
	c.mu.Lock()
	c.identity, c.tried, c.pattern = f.identity, true, pattern
	c.mu.Unlock()
	return pattern, pattern != ""
}

// keyedDescribe runs tracker_describe; a panic becomes a failure.
func keyedDescribe(ctx context.Context, keyed tracker.Keyed) (d contract.TrackerDescription, err error) {
	defer func() {
		if recover() != nil {
			err = errGatePanic
		}
	}()
	return keyed.Describe(ctx)
}

// keyedAnchored is the plugin's keyPattern written with ^ and $, as
// intake.ParseIssueKey wants it: the adapter always matches it whole, so an
// unanchored pattern is wrapped as the CLI wraps it. A pattern that is too long,
// not one line of plain text (no control, format, bidi or line separator
// character) or not a regular expression gives "".
func keyedAnchored(p string) string {
	forbidden := func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Bidi_Control)
	}
	if p == "" || len(p) > intake.MaxKeyPatternBytes || !utf8.ValidString(p) || strings.ContainsFunc(p, forbidden) {
		return ""
	}
	if !strings.HasPrefix(p, "^") || !strings.HasSuffix(p, "$") {
		p = "^(?:" + p + ")$"
	}
	if _, err := regexp.Compile(p); err != nil || len(p) > intake.MaxKeyPatternBytes {
		return ""
	}
	return p
}

// keyedReader reads the keyed Issue through tracker_get, as forgecell run KEY
// does: a failed call's diagnostics reach only the Molecule's ledger notes.
func keyedReader(keyed tracker.Keyed, calls *molecule.PluginCallLog) func(context.Context, string, string, string) (molecule.Issue, error) {
	return func(ctx context.Context, repo, key, _ string) (molecule.Issue, error) {
		issue, err := keyed.Get(ctx, key)
		if err != nil {
			calls.Fail(err)
			return molecule.Issue{}, err
		}
		state := map[string]string{"open": "OPEN", "closed": "CLOSED"}[issue.State]
		return molecule.Issue{Key: issue.Key, Number: 0, Repo: repo, Title: issue.Title, Body: issue.Body, State: state, URL: issue.URL, UpdatedAt: issue.UpdatedAt}, nil
	}
}

// keyedOptions sets o's keyed Issue reader: the active Formula's tracker
// plugin, every call recorded on the Molecule through a PluginCallLog. o.Issue
// is ignored by a keyed run and cleared.
func (h *handler) keyedOptions(f keyedActiveFormula, o *molecule.Options) bool {
	lab, ok := h.keyedLabDir()
	if !ok {
		return false
	}
	calls := molecule.NewPluginCallLog(lab, h.opts.LabDir)
	keyed, plugin, err := h.trackerFor(lab, f.formula, mcpadapter.Options{Observer: calls.Observe})
	if err != nil || !plugin || keyed == nil {
		return false
	}
	o.Issue, o.ReadIssueKey, o.PluginCalls, o.AllowKeyedApproval = "", keyedReader(keyed, calls), calls, true
	return true
}

// refuseNumericOnKeyedLab refuses Start intake by number in a Lab whose Issues
// are tracker keys, before GitHub is read; an invalid number is left to
// startRun, so its refusal is unchanged.
func (h *handler) refuseNumericOnKeyedLab(w http.ResponseWriter, issue int64) bool {
	if issue < 1 || issue > maxIssueNumber {
		return false
	}
	if _, keyed := h.keyedFormula(h.opts.LabDir); !keyed {
		return false
	}
	h.refuseGate(w, http.StatusConflict, "keyed", "this Lab's Issues are tracker keys; start with key; nothing was started")
	return true
}

// startKeyedRun starts intake for a tracker key. The refusals that need no
// plugin come first, so a start that cannot run never starts the plugin.
func (h *handler) startKeyedRun(ctx context.Context, w http.ResponseWriter, req runRequest) {
	if req.Issue != 0 {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "send the Issue number or the Issue key, not both")
		return
	}
	f, keyed := h.keyedFormula(h.opts.LabDir)
	if !keyed {
		h.refuseGate(w, http.StatusConflict, "not-keyed", "this Lab's active Formula reads GitHub Issues, so intake takes an Issue number, not a key; nothing was started")
		return
	}
	if intake.ValidateIssueKey(req.Key) != nil {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give the Issue key as one line of at most 128 characters")
		return
	}
	if h.sourceMissing(w) || h.belowFloor(w) {
		return
	}
	if busy, incomplete := h.activeKeyedMolecule(req.Key); incomplete {
		h.refuseGate(w, http.StatusConflict, "busy", "the ledgers for this Issue could not all be read, so a running Molecule cannot be ruled out; nothing was started")
		return
	} else if busy {
		h.refuseGate(w, http.StatusConflict, "busy", "the ledgers already show a running Molecule for this Issue; nothing was started")
		return
	}
	// A duplicate submission is refused before the plugin is prepared; the job
	// registry still refuses one that races past this check.
	if h.jobs.busy(keyedRunJob(req.Key)) {
		h.refuseGate(w, http.StatusConflict, "busy", "intake is already running for this Issue from this Lab UI; this submission was not started")
		return
	}
	pattern, ok := h.keyedPattern(ctx, f, true)
	if !ok {
		h.refuseGate(w, http.StatusConflict, "tracker", "the tracker plugin could not describe its Issue keys; nothing was started")
		return
	}
	if _, err := intake.ParseIssueKey(req.Key, pattern); err != nil {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "the key does not match this tracker's Issue keys; nothing was started")
		return
	}
	opts := molecule.Options{LabDir: h.opts.LabDir, SourceRoot: h.opts.SourceRoot, Resources: h.opts.Resources}
	if !h.keyedOptions(f, &opts) {
		h.refuseGate(w, http.StatusConflict, "tracker", "the tracker plugin could not be prepared; nothing was started")
		return
	}
	run, key := h.runKeyedIssue, req.Key
	// As startRun: never beside this UI's own loop, under the loop's mutex.
	h.loop.mu.Lock()
	if h.loop.running {
		h.loop.mu.Unlock()
		h.refuseGate(w, http.StatusConflict, "busy", "a loop is running or stopping in this Lab UI; stop it before starting a single run; nothing was started")
		return
	}
	job, err := h.jobs.start(keyedRunJob(key), "run", "", func(ctx context.Context) error {
		_, err := run(ctx, opts, key)
		return err
	})
	h.loop.mu.Unlock()
	if h.refuseJobStart(w, err, "intake is already running for this Issue from this Lab UI; this submission was not started") {
		return
	}
	h.json(w, http.StatusAccepted, h.keyedRunView(key, job.snapshot(&h.jobs.mu)))
}

// keyedRecords visits the ledgers that may be keyed Molecules of key, by the
// id molecule.RunKeyed gives them, and reads each to match its raw Key: the
// summary's key is display text and never matched. It reads at most
// keyedMaxReads ledgers; incomplete reports an inventory that could not list
// or read every candidate.
func (h *handler) keyedRecords(key string, visit func(Summary, molecule.Record)) (incomplete bool) {
	list := h.list(ledgerETag(h.opts.LabDir))
	prefix := "mol-" + molecule.Issue{Key: key}.KeySlug() + "-"
	reads := 0
	for _, m := range list.Molecules {
		if m.Number != 0 || m.Key == "" || !moleculeID.MatchString(m.ID) || !strings.HasPrefix(m.ID, prefix) {
			continue
		}
		if reads == keyedMaxReads {
			return true
		}
		reads++
		record, err := h.records(h.opts.LabDir, m.ID)
		if err != nil {
			incomplete = true
			continue
		}
		if record.Issue.IsKeyed() && record.Issue.Key == key {
			visit(m, record)
		}
	}
	return incomplete || !list.Complete || list.Truncated
}

// activeKeyedMolecule reports a ledger that shows a running Molecule for key.
// An inventory that could not list or read every candidate (more than
// keyedMaxReads of them, say) counts as busy, so a running keyed Molecule is
// never missed; incomplete says which.
func (h *handler) activeKeyedMolecule(key string) (busy, incomplete bool) {
	found := false
	incomplete = h.keyedRecords(key, func(_ Summary, r molecule.Record) {
		found = found || active(r.Status)
	})
	return found || incomplete, incomplete && !found
}

// keyedRunMolecule is runMolecule for a key: the earliest ledger for it
// started at or after the job.
func (h *handler) keyedRunMolecule(key string, started time.Time) (id string, found, incomplete bool) {
	var first time.Time
	incomplete = h.keyedRecords(key, func(m Summary, _ molecule.Record) {
		at, ok := startedSince(m.StartedAt, started)
		if ok && (!found || at.Before(first)) {
			id, first, found = m.ID, at, true
		}
	})
	return id, found, incomplete
}

func (h *handler) keyedRunView(key string, j jobSnapshot) RunJobView {
	v := RunJobView{Key: key, State: j.state, Category: j.category, Error: j.message, Reason: j.reason, Canonical: MoleculeState{Source: "ledger"}}
	id, found, incomplete := h.keyedRunMolecule(key, j.started)
	if !found {
		v.Canonical.Incomplete = incomplete
		return v
	}
	v.Canonical = h.moleculeState(id)
	v.Committed = !v.Canonical.Unreadable
	return v
}

// runKeyedStatus serves GET /api/runs?key=KEY: a key may hold characters that
// do not fit a path segment, so it is a query parameter.
func (h *handler) runKeyedStatus(w http.ResponseWriter, r *http.Request) {
	if h.opts.Demo {
		h.refuseGate(w, http.StatusNotFound, "sample", "sample data has no runs")
		return
	}
	query := r.URL.Query()
	keys := query["key"]
	if len(query) != 1 || len(keys) != 1 || intake.ValidateIssueKey(keys[0]) != nil {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give one Issue key as ?key=")
		return
	}
	j, ok := h.jobs.get(keyedRunJob(keys[0]))
	if !ok || j.kind != "run" {
		h.refuseGate(w, http.StatusNotFound, "none", "no run was started for this Issue by this Lab UI")
		return
	}
	h.json(w, http.StatusOK, h.keyedRunView(keys[0], j))
}

// keyedRunner reports that this Lab UI's keyed run job, still running, started
// record; owned is runner's ownership check, which may also cancel the job.
func (h *handler) keyedRunner(record molecule.Record, owned func(string, []string, func(time.Time) bool) bool) bool {
	if !record.Issue.IsKeyed() || intake.ValidateIssueKey(record.Issue.Key) != nil {
		return false
	}
	key := keyedRunJob(record.Issue.Key)
	j, ok := h.jobs.get(key)
	if !ok || j.state != "running" {
		return false
	}
	id, found, _ := h.keyedRunMolecule(record.Issue.Key, j.started)
	return found && id == record.ID && owned(key, []string{"run"}, j.started.Equal)
}

// keyedPassKey is a keyed Issue's key in a loop pass, beside issueKey's
// repo#N; the "\x00key:" separator never appears in one.
func keyedPassKey(repo, key string) string { return strings.ToLower(repo) + "\x00key:" + key }

// approveKeyedScope approves the exact pending digest of a keyed Molecule
// through molecule.RunKeyed with AllowKeyedApproval and the tracker reader,
// as forgecell run KEY --approve DIGEST does. The engine resolves the pending
// keyed Molecule by its raw Key and binds the digest exactly as by number.
func (h *handler) approveKeyedScope(w http.ResponseWriter, r molecule.Record, opts molecule.Options, digest string) {
	key := r.Issue.Key
	if intake.ValidateIssueKey(key) != nil {
		h.refuseGate(w, http.StatusUnprocessableEntity, "refused", "the ledger's Issue key cannot be approved from this page; nothing was approved")
		return
	}
	f, keyed := h.keyedFormula(h.opts.LabDir)
	if !keyed {
		h.refuseGate(w, http.StatusConflict, "not-keyed", "this Lab's active Formula no longer reads Issues from a tracker plugin; nothing was approved")
		return
	}
	if !h.keyedOptions(f, &opts) {
		h.refuseGate(w, http.StatusConflict, "tracker", "the tracker plugin could not be prepared; nothing was approved")
		return
	}
	run := h.runKeyedScope
	h.startJob(w, r.ID, "scope", digest, func(ctx context.Context) error {
		_, err := run(ctx, opts, key)
		return err
	})
}

// KeyedIntakeView is GET /api/lab's intake when the active Formula reads Issues
// from a tracker plugin: the drive panel then asks for the tracker's Issue key.
// KeyPattern is the tracker's anchored keyPattern, as text, or absent until it
// is known or when the tracker could not describe one; the client then checks
// only the key's length and control characters.
type KeyedIntakeView struct {
	Keyed      bool   `json:"keyed"`
	KeyPattern string `json:"keyPattern,omitempty"`
}

// keyedIntake is the intake GET /api/lab serves, or nil for a GitHub Lab. It
// never starts the plugin and never waits on a describe: the pattern is the
// cached one, absent until it is known. A Lab that can drive (SourceRoot set)
// starts one background describe per Formula identity the first time it is
// asked.
func (h *handler) keyedIntake() *KeyedIntakeView {
	f, keyed := h.keyedFormula(h.opts.LabDir)
	if !keyed {
		return nil
	}
	c := &h.keyedPatterns
	pattern, tried := c.cached(f.identity)
	if !tried && h.opts.SourceRoot != "" {
		h.keyedDescribeInBackground(f)
	}
	return &KeyedIntakeView{Keyed: true, KeyPattern: pattern}
}

// keyedDescribeInBackground starts the one background describe for f's
// identity, on the jobs' context so it stops with the server; a second call
// for the same identity starts nothing.
func (h *handler) keyedDescribeInBackground(f keyedActiveFormula) {
	c := &h.keyedPatterns
	c.mu.Lock()
	if c.background == f.identity {
		c.mu.Unlock()
		return
	}
	done := make(chan struct{})
	c.background, c.backgroundDone = f.identity, done
	c.mu.Unlock()
	g := h.jobs
	g.mu.Lock()
	if g.ctx.Err() != nil {
		g.mu.Unlock()
		close(done)
		return
	}
	g.wg.Add(1)
	g.mu.Unlock()
	go func() {
		defer g.wg.Done()
		defer close(done)
		h.keyedPattern(g.ctx, f, false)
	}()
}
