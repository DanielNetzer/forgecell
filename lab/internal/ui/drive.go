package ui

import (
	"context"
	"errors"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// The UI drives work through the same engine functions the CLI uses, never a
// shell, and behind the gate write protection (cookie, tab header, exact Origin,
// Fetch Metadata and one strict JSON object):
//
//	POST /api/runs                 {"issue"}                  → molecule.Run (intake) as a job
//	GET  /api/runs/{issue}                                    → the job and its ledger read-back
//	POST /api/loop/start           {"select","maxConcurrent"} → molecule.RunLoop, one per UI
//	POST /api/loop/stop            {}                         → closes Drain; in-flight work finishes
//	GET  /api/loop                                            → the loop and its last report
//	POST /api/molecules/{id}/stop  {}                         → cancels this UI's engine call for it
//
// A started run counts only once a ledger for its Issue appears, and a stop only
// once the ledger no longer reports the Molecule running; the job's own outcome
// never does. Engine error text is never served.

const (
	// maxLoopConcurrent bounds maxConcurrent for a loop started from the UI.
	maxLoopConcurrent = 8
	// maxIssueNumber bounds an Issue number.
	maxIssueNumber = 1<<31 - 1
	// maxSelectBytes bounds the loop selector.
	maxSelectBytes = 256
	// maxLoopItems bounds the report items one loop view carries.
	maxLoopItems = 200
	// maxStateBytes bounds a status or phase copied from a ledger or the report.
	maxStateBytes = 64
)

// DriveActions lists the work this UI starts and stops through the engine.
var DriveActions = []string{"start intake", "loop", "stop Molecule"}

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// repository is s when it is owner/name with neither part a dot segment.
func repository(s string) string {
	owner, name, _ := strings.Cut(s, "/")
	if !repositoryName.MatchString(s) || owner == "." || owner == ".." || name == "." || name == ".." {
		return ""
	}
	return s
}

// driveRoute reports a POST drive path and, for a Molecule stop, its id.
func driveRoute(path string) (id, action string, ok bool) {
	switch path {
	case "/api/runs":
		return "", "run", true
	case "/api/loop/start":
		return "", "loop/start", true
	case "/api/loop/stop":
		return "", "loop/stop", true
	}
	rest, found := strings.CutPrefix(path, "/api/molecules/")
	if !found {
		return "", "", false
	}
	if id, found = strings.CutSuffix(rest, "/stop"); found && !strings.Contains(id, "/") {
		return id, "stop", true
	}
	return "", "", false
}

type runRequest struct {
	Issue int64 `json:"issue"`
}

type loopRequest struct {
	Select        string `json:"select"`
	MaxConcurrent int    `json:"maxConcurrent"`
}

type emptyRequest struct{}

// drive serves one POST drive route after the write protection has passed.
func (h *handler) drive(w http.ResponseWriter, r *http.Request, id, action string) {
	if action == "stop" && !moleculeID.MatchString(id) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "invalid Molecule id")
		return
	}
	if h.opts.Demo {
		h.refuseGate(w, http.StatusNotFound, "sample", "sample data cannot start or stop work; nothing was started")
		return
	}
	var run runRequest
	var loop loopRequest
	var body any = &emptyRequest{}
	switch action {
	case "run":
		body = &run
	case "loop/start":
		body = &loop
	}
	if decodeGate(w, r, body) != nil {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "send one small JSON object with exactly the action's fields")
		return
	}
	switch action {
	case "run":
		h.startRun(w, run)
	case "loop/start":
		h.startLoop(w, loop)
	case "loop/stop":
		h.stopLoop(w)
	default:
		h.stopMolecule(w, id)
	}
}

func runKey(issue int64) string { return "#" + strconv.FormatInt(issue, 10) }

func (h *handler) sourceMissing(w http.ResponseWriter) bool {
	if h.opts.SourceRoot != "" {
		return false
	}
	h.refuseGate(w, http.StatusConflict, "unavailable", "this Lab UI was started without a source checkout; nothing was started")
	return true
}

func (h *handler) belowFloor(w http.ResponseWriter) bool {
	if g := h.opts.Resources; g != nil {
		// The reason can name volumes, so only the category is served.
		if level, _ := g.Check(); level != resources.OK {
			h.refuseGate(w, http.StatusServiceUnavailable, "resources", "the Lab is below a resource floor; nothing was started")
			return true
		}
	}
	return false
}

func (h *handler) startRun(w http.ResponseWriter, req runRequest) {
	if req.Issue < 1 || req.Issue > maxIssueNumber {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give the Issue as a positive whole number")
		return
	}
	if h.sourceMissing(w) || h.belowFloor(w) {
		return
	}
	if h.activeMolecule(req.Issue) {
		h.refuseGate(w, http.StatusConflict, "busy", "the ledgers already show a running Molecule for this Issue; nothing was started")
		return
	}
	opts := molecule.Options{LabDir: h.opts.LabDir, SourceRoot: h.opts.SourceRoot, Issue: strconv.FormatInt(req.Issue, 10), Resources: h.opts.Resources}
	run := h.runIssue
	// molecule.Run takes no per-Issue lock and a starting intake has no ledger
	// yet, so a run never starts beside this UI's loop. The loop's mutex covers
	// the check and the registration; startLoop checks runs under it too.
	h.loop.mu.Lock()
	if h.loop.running {
		h.loop.mu.Unlock()
		h.refuseGate(w, http.StatusConflict, "busy", "a loop is running or stopping in this Lab UI; stop it before starting a single run; nothing was started")
		return
	}
	job, err := h.jobs.start(runKey(req.Issue), "run", "", func(ctx context.Context) error {
		_, err := run(ctx, opts)
		return err
	})
	h.loop.mu.Unlock()
	if h.refuseJobStart(w, err, "intake is already running for this Issue from this Lab UI; this submission was not started") {
		return
	}
	h.json(w, http.StatusAccepted, h.runView(req.Issue, job.snapshot(&h.jobs.mu)))
}

func active(status string) bool { return status == "running" || status == "allocating" }

// activeMolecule reports a ledger that shows a running Molecule for issue.
func (h *handler) activeMolecule(issue int64) bool {
	for _, m := range h.list(ledgerETag(h.opts.LabDir)).Molecules {
		if m.Number == issue && active(m.Status) {
			return true
		}
	}
	return false
}

func startedSince(at string, started time.Time) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, at)
	return t, err == nil && !t.Before(started)
}

// runMolecule finds the Molecule a run job started: the earliest ledger for the
// Issue started at or after the job. incomplete reports an inventory that could
// not list every ledger.
func (h *handler) runMolecule(issue int64, started time.Time) (id string, found, incomplete bool) {
	list := h.list(ledgerETag(h.opts.LabDir))
	var first time.Time
	for _, m := range list.Molecules {
		at, ok := startedSince(m.StartedAt, started)
		if m.Number != issue || !ok || !moleculeID.MatchString(m.ID) {
			continue
		}
		if !found || at.Before(first) {
			id, first, found = m.ID, at, true
		}
	}
	return id, found, !list.Complete || list.Truncated
}

// MoleculeState is a Molecule's state as read back from its ledger.
type MoleculeState struct {
	Source     string `json:"source"`
	MoleculeID string `json:"moleculeId,omitempty"`
	Unreadable bool   `json:"unreadable,omitempty"`
	Incomplete bool   `json:"incomplete,omitempty"`
	Status     string `json:"status,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Lane       string `json:"lane,omitempty"`
}

// stateText is a short one-line value from a ledger or a loop report; anything
// else is dropped rather than served.
func stateText(s string) string {
	if len(s) > maxStateBytes || !utf8.ValidString(s) || strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return ""
	}
	return s
}

func (h *handler) moleculeState(id string) MoleculeState {
	v := MoleculeState{Source: "ledger", MoleculeID: id}
	record, err := h.records(h.opts.LabDir, id)
	if err != nil {
		v.Unreadable = true
		return v
	}
	v.Status, v.Phase, v.Lane = stateText(record.Status), stateText(phase(record)), Lane(record.Status, phase(record), nil)
	return v
}

// RunJobView is GET /api/runs/{issue} and every accepted run response.
// Committed comes only from the ledger read-back: a ledger for the Issue started
// by this job exists.
type RunJobView struct {
	Issue     int64         `json:"issue"`
	State     string        `json:"state"`
	Category  string        `json:"category,omitempty"`
	Error     string        `json:"error,omitempty"`
	Committed bool          `json:"committed"`
	Canonical MoleculeState `json:"canonical"`
}

func (h *handler) runView(issue int64, j jobSnapshot) RunJobView {
	v := RunJobView{Issue: issue, State: j.state, Category: j.category, Error: j.message, Canonical: MoleculeState{Source: "ledger"}}
	id, found, incomplete := h.runMolecule(issue, j.started)
	if !found {
		v.Canonical.Incomplete = incomplete
		return v
	}
	v.Canonical = h.moleculeState(id)
	v.Committed = !v.Canonical.Unreadable
	return v
}

func (h *handler) runStatus(w http.ResponseWriter, rest string) {
	if h.opts.Demo {
		h.refuseGate(w, http.StatusNotFound, "sample", "sample data has no runs")
		return
	}
	issue, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || issue < 1 || issue > maxIssueNumber || strconv.FormatInt(issue, 10) != rest {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give the Issue as a positive whole number")
		return
	}
	j, ok := h.jobs.get(runKey(issue))
	if !ok {
		h.refuseGate(w, http.StatusNotFound, "none", "no run was started for this Issue by this Lab UI")
		return
	}
	h.json(w, http.StatusOK, h.runView(issue, j))
}

// StopView is every accepted Molecule stop. Committed comes only from the
// ledger read-back: the ledger no longer reports the Molecule running.
type StopView struct {
	MoleculeID string        `json:"moleculeId"`
	Requested  bool          `json:"requested"`
	Committed  bool          `json:"committed"`
	Canonical  MoleculeState `json:"canonical"`
}

// stopMolecule cancels the engine call this UI process runs for the Molecule: a
// scope approval job, or the run job whose ledger it is. Cancelling the context
// kills the managed process group and the engine records the stop. Anything
// else is refused and never signalled.
func (h *handler) stopMolecule(w http.ResponseWriter, id string) {
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
	if !active(record.Status) {
		h.refuseGate(w, http.StatusConflict, "not-running", "the ledger does not show this Molecule running; nothing was signalled")
		return
	}
	stopped := h.jobs.cancelRunning(id, []string{"scope"}, func(time.Time) bool { return true })
	if !stopped && record.Issue.Number >= 1 && record.Issue.Number <= maxIssueNumber {
		key := runKey(record.Issue.Number)
		if j, ok := h.jobs.get(key); ok && j.state == "running" {
			if owned, found, _ := h.runMolecule(record.Issue.Number, j.started); found && owned == id {
				stopped = h.jobs.cancelRunning(key, []string{"run"}, j.started.Equal)
			}
		}
	}
	if !stopped {
		h.refuseGate(w, http.StatusConflict, "not-owned", "this Lab UI does not run this Molecule's engine call (a loop, another process or the CLI does); nothing was signalled")
		return
	}
	v := StopView{MoleculeID: id, Requested: true, Canonical: h.moleculeState(id)}
	v.Committed = !v.Canonical.Unreadable && !active(v.Canonical.Status)
	h.json(w, http.StatusAccepted, v)
}

var (
	errLoopBusy = errors.New("a loop is running in this Lab UI")
	errRunsBusy = errors.New("a run is running in this Lab UI")
)

// running reports a running job of kind.
func (g *gateJobs) running(kind string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, j := range g.jobs {
		if j.kind == kind && j.state == "running" {
			return true
		}
	}
	return false
}

// driveLoop is this UI's single loop slot. Drain stops dispatching; cancel, used
// only when the server stops, also cancels the Molecules in flight.
type driveLoop struct {
	mu                       sync.Mutex
	stopped, running, drain  bool
	drained                  chan struct{}
	cancel                   context.CancelFunc
	done                     chan struct{}
	selector                 string
	max                      int
	state, category, message string
	report                   *molecule.LoopReport
}

// start refuses while runsBusy reports a UI run job running; the check and the
// registration hold l.mu, which startRun also holds while it registers a run.
func (l *driveLoop) start(o molecule.LoopOptions, runsBusy func() bool, run func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return errGateStopped
	}
	if l.running {
		return errLoopBusy
	}
	if runsBusy() {
		return errRunsBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	drained, done := make(chan struct{}), make(chan struct{})
	// The stop request closes Drain only; Work is cancelled only with the server.
	o.Drain, o.Work = drained, ctx
	l.running, l.drain, l.drained, l.cancel, l.done = true, false, drained, cancel, done
	l.selector, l.max, l.report = o.Select.String(), o.MaxConcurrent, nil
	l.state, l.category, l.message = "running", "", ""
	go func() {
		defer close(done)
		defer cancel()
		var report molecule.LoopReport
		state, category, message := classify(guarded(ctx, func(ctx context.Context) error {
			var err error
			report, err = run(ctx, o)
			return err
		}))
		l.mu.Lock()
		l.running, l.report = false, &report
		l.state, l.category, l.message = state, category, message
		l.mu.Unlock()
	}()
	return nil
}

// drainLoop closes Drain; it reports false when no loop runs or one is draining.
func (l *driveLoop) drainLoop() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running || l.drain {
		return false
	}
	l.drain = true
	l.state = "draining"
	close(l.drained)
	return true
}

// stop refuses new loops, drains and cancels the running one and waits for it,
// at most grace.
func (l *driveLoop) stop(grace time.Duration) {
	l.mu.Lock()
	l.stopped = true
	done := l.done
	if l.running {
		if !l.drain {
			l.drain = true
			close(l.drained)
		}
		l.cancel()
	}
	l.mu.Unlock()
	if done == nil {
		return
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// stopWork cancels every job and the loop together and waits for them, at most
// grace in all.
func (h *handler) stopWork(grace time.Duration) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); h.jobs.stop(grace) }()
	go func() { defer wg.Done(); h.loop.stop(grace) }()
	wg.Wait()
}

func (h *handler) startLoop(w http.ResponseWriter, req loopRequest) {
	selector, err := intake.ParseSelector(req.Select)
	if len(req.Select) > maxSelectBytes || !validTitle(req.Select) || err != nil {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give select as oldest, label:NAME or milestone:TITLE")
		return
	}
	if req.MaxConcurrent < 1 || req.MaxConcurrent > maxLoopConcurrent {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give maxConcurrent from 1 to "+strconv.Itoa(maxLoopConcurrent))
		return
	}
	if h.sourceMissing(w) || h.belowFloor(w) {
		return
	}
	o := molecule.LoopOptions{Template: molecule.Options{LabDir: h.opts.LabDir, SourceRoot: h.opts.SourceRoot, Resources: h.opts.Resources}, Select: selector, MaxConcurrent: req.MaxConcurrent}
	switch err := h.loop.start(o, func() bool { return h.jobs.running("run") }, h.runLoop); {
	case errors.Is(err, errLoopBusy):
		h.refuseGate(w, http.StatusConflict, "busy", "a loop is already running in this Lab UI; this submission was not started")
	case errors.Is(err, errRunsBusy):
		h.refuseGate(w, http.StatusConflict, "busy", "a single run is in progress in this Lab UI; wait for it or stop it before starting the loop; nothing was started")
	case err != nil:
		h.refuseGate(w, http.StatusServiceUnavailable, "stopped", "the Lab UI is stopping; nothing was started")
	default:
		h.json(w, http.StatusAccepted, h.loopView())
	}
}

func (h *handler) stopLoop(w http.ResponseWriter) {
	if !h.loop.drainLoop() {
		h.refuseGate(w, http.StatusConflict, "not-running", "no loop is running, or it is already stopping; nothing was changed")
		return
	}
	h.json(w, http.StatusAccepted, h.loopView())
}

func (h *handler) loopStatus(w http.ResponseWriter) {
	if h.opts.Demo {
		h.refuseGate(w, http.StatusNotFound, "sample", "sample data has no loop")
		return
	}
	h.json(w, http.StatusOK, h.loopView())
}

// LoopView is GET /api/loop. State is idle, running, draining, or the finished
// loop's outcome; Report is the last finished loop's final pass as the engine
// reported it from the ledgers.
type LoopView struct {
	State         string          `json:"state"`
	Category      string          `json:"category,omitempty"`
	Error         string          `json:"error,omitempty"`
	Select        string          `json:"select,omitempty"`
	MaxConcurrent int             `json:"maxConcurrent,omitempty"`
	Limit         int             `json:"maxConcurrentLimit"`
	Report        *LoopReportView `json:"report,omitempty"`
}

type LoopReportView struct {
	Repository  string         `json:"repository,omitempty"`
	Passes      int            `json:"passes"`
	Failed      bool           `json:"failed"`
	NonProgress int            `json:"nonProgress"`
	Truncated   bool           `json:"truncated"`
	Items       []LoopItemView `json:"items"`
}

type LoopItemView struct {
	Repository string `json:"repository,omitempty"`
	Issue      int64  `json:"issue"`
	MoleculeID string `json:"moleculeId,omitempty"`
	State      string `json:"state,omitempty"`
	Status     string `json:"status,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Started    bool   `json:"started"`
	Approved   bool   `json:"approved"`
	Failed     bool   `json:"failed"`
}

// checked is s when it is a valid Molecule id.
func checked(s string) string {
	if moleculeID.MatchString(s) {
		return s
	}
	return ""
}

// loopView copies the bounded, validated parts of the report; reasons, which
// can name local paths and lock files, are never served.
func (h *handler) loopView() LoopView {
	l := h.loop
	l.mu.Lock()
	defer l.mu.Unlock()
	v := LoopView{State: l.state, Category: l.category, Error: l.message, Select: l.selector, MaxConcurrent: l.max, Limit: maxLoopConcurrent}
	if v.State == "" {
		v.State = "idle"
	}
	if r := l.report; r != nil {
		rv := &LoopReportView{Repository: repository(r.Repository), Passes: len(r.Passes), Failed: r.Failed(), NonProgress: r.NonProgress(), Items: []LoopItemView{}}
		if n := len(r.Passes); n > 0 {
			for i, item := range r.Passes[n-1].Items {
				if i == maxLoopItems {
					rv.Truncated = true
					break
				}
				rv.Items = append(rv.Items, LoopItemView{Repository: repository(item.Repository), Issue: item.Issue, MoleculeID: checked(item.MoleculeID), State: stateText(item.State), Status: stateText(item.Status), Phase: stateText(item.Phase), Started: item.Started, Approved: item.Approved, Failed: item.Failed})
			}
		}
		v.Report = rv
	}
	return v
}
