package ui

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDrive replaces the drive engine seams and counts every call, so a test can
// prove a refused or duplicate request never reached the engine. No git, gh or
// harness process starts.
type fakeDrive struct {
	mu    sync.Mutex
	runs  []molecule.Options
	loops []molecule.LoopOptions
	run   func(context.Context, molecule.Options) error
	loop  func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error)
}

func (d *fakeDrive) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.runs) + len(d.loops)
}

func installDrive(h http.Handler, d *fakeDrive) *fakeDrive {
	hh := h.(*handler)
	hh.runIssue = func(ctx context.Context, o molecule.Options) (molecule.Record, error) {
		d.mu.Lock()
		d.runs = append(d.runs, o)
		run := d.run
		d.mu.Unlock()
		if run == nil {
			return molecule.Record{}, errors.New("no run seam")
		}
		return molecule.Record{}, run(ctx, o)
	}
	hh.runLoop = func(ctx context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		d.mu.Lock()
		d.loops = append(d.loops, o)
		loop := d.loop
		d.mu.Unlock()
		if loop == nil {
			return molecule.LoopReport{}, errors.New("no loop seam")
		}
		return loop(ctx, o)
	}
	return d
}

// guardWith is a resource guard over fixed readings, so no real volume is read.
func guardWith(memory uint64) *resources.Guard {
	return &resources.Guard{Floors: resources.DefaultFloors(), Reader: resources.ReaderFunc(func() resources.Readings {
		return resources.Readings{Memory: memory, MemoryKnown: true}
	})}
}

func driveHandler(t *testing.T, lab string, guard *resources.Guard, d *fakeDrive) (http.Handler, *fakeDrive) {
	t.Helper()
	h := testHandler(t, Options{LabDir: lab, SourceRoot: "/src/checkout", Resources: guard})
	return h, installDrive(h, d)
}

// intakeRecord is base as the engine writes it for another Issue.
func intakeRecord(base molecule.Record, id string, issue int64, startedAt, status, phase string) molecule.Record {
	r := base
	r.ID, r.Status, r.StartedAt, r.FinishedAt = id, status, startedAt, ""
	r.Issue.Number, r.Issue.URL = issue, "https://github.com/o/r/issues/"+strconv.FormatInt(issue, 10)
	r.Readiness = nil
	if phase != "" {
		r.Readiness = &readiness.State{SchemaVersion: "v1", Phase: phase}
	}
	return r
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// saveRecord writes a ledger from an engine stub's goroutine, where t.Fatal
// must not be called.
func saveRecord(lab string, r molecule.Record) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(lab, "ledgers", r.ID+".json"), raw, 0600)
}

func viewOf[T any](t *testing.T, w *httptest.ResponseRecorder, code int) T {
	t.Helper()
	var v T
	if w.Code != code {
		t.Fatalf("status %d, want %d: %s", w.Code, code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	noLeak(t, w.Body.String())
	return v
}

func waitLoop(t *testing.T, h http.Handler) {
	t.Helper()
	l := h.(*handler).loop
	l.mu.Lock()
	done := l.done
	l.mu.Unlock()
	if done == nil {
		t.Fatal("no loop")
	}
	<-done
}

func TestUIDriveWriteProtectionRefusesForgedAndMalformedWrites(t *testing.T) {
	lab, r := labFixture(t)
	h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	before := snapshot(t, lab)
	routes := map[string]string{"/api/runs": `{"issue":7}`, "/api/loop/start": `{"select":"oldest","maxConcurrent":1}`, "/api/loop/stop": `{}`, "/api/molecules/" + r.ID + "/stop": `{}`}
	for path, body := range routes {
		for name, forged := range forgedWrites(path, body) {
			if w := do(t, h, forged); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d %s", name, path, w.Code, w.Body)
			}
		}
		for _, req := range map[string]request{
			"no content type": func() request { q := gatePost(path, body); delete(q.header, "Content-Type"); return q }(),
			"unknown field":   gatePost(path, strings.TrimSuffix(body, "}")+`,"approve":true}`),
			"trailing data":   gatePost(path, body+`{}`),
			"oversized":       gatePost(path, strings.TrimSuffix(body, "}")+strings.Repeat(" ", maxGateBody)+`}`),
			"array":           gatePost(path, `[]`),
		} {
			refusalOf(t, do(t, h, req), http.StatusBadRequest, "invalid")
		}
	}
	for _, body := range []string{`{}`, `{"issue":0}`, `{"issue":-1}`, `{"issue":"7"}`, `{"issue":1.5}`, `{"issue":null}`, `{"issue":2147483648}`} {
		refusalOf(t, do(t, h, gatePost("/api/runs", body)), http.StatusBadRequest, "invalid")
	}
	for _, body := range []string{`{"maxConcurrent":1}`, `{"select":"label:","maxConcurrent":1}`, `{"select":"bogus","maxConcurrent":1}`, `{"select":"label:a\nb","maxConcurrent":1}`, `{"select":"label:` + strings.Repeat("a", maxSelectBytes) + `","maxConcurrent":1}`, `{"select":"oldest"}`, `{"select":"oldest","maxConcurrent":0}`, `{"select":"oldest","maxConcurrent":9}`} {
		refusalOf(t, do(t, h, gatePost("/api/loop/start", body)), http.StatusBadRequest, "invalid")
	}
	refusalOf(t, do(t, h, gatePost("/api/molecules/bad.id/stop", `{}`)), http.StatusBadRequest, "invalid")
	if n := d.calls(); n != 0 {
		t.Fatalf("refused writes reached the engine %d times", n)
	}
	if v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK); v.State != "idle" || v.Report != nil || v.Limit != maxLoopConcurrent {
		t.Fatalf("loop %+v", v)
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("a refused write changed the Lab:\n%s\n---\n%s", before, after)
	}
}

func TestUIDriveRunStartsIntakeAndReadsTheLedgerBack(t *testing.T) {
	lab, base := labFixture(t)
	guard := guardWith(64 << 30)
	written, release := make(chan error, 1), make(chan struct{})
	h, d := driveHandler(t, lab, guard, &fakeDrive{run: func(ctx context.Context, o molecule.Options) error {
		// The stub engine writes the ledger waiting at the scope gate.
		written <- saveRecord(lab, intakeRecord(base, "mol-42-new", 42, now(), "waiting", "scope-waiting"))
		<-release
		return nil
	}})
	// An older ledger for the Issue is never this run's.
	writeRecord(t, lab, intakeRecord(base, "mol-42-old", 42, "2000-01-01T00:00:00Z", "blocked", ""))
	v := viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":42}`)), http.StatusAccepted)
	if v.Issue != 42 || v.State != "running" || v.Canonical.Source != "ledger" || (v.Committed && v.Canonical.MoleculeID != "mol-42-new") {
		t.Fatalf("accepted run %+v", v)
	}
	// A duplicate submission never reaches the engine.
	refusalOf(t, do(t, h, gatePost("/api/runs", `{"issue":42}`)), http.StatusConflict, "busy")
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if n := d.calls(); n != 1 {
		t.Fatalf("engine calls %d", n)
	}
	o := d.runs[0]
	if o.LabDir != lab || o.SourceRoot != "/src/checkout" || o.Issue != "42" || o.Approve != "" || o.Resources != guard {
		t.Fatalf("run options %+v", o)
	}
	v = viewOf[RunJobView](t, do(t, h, authed("/api/runs/42")), http.StatusOK)
	if v.State != "running" || !v.Committed || v.Canonical.MoleculeID != "mol-42-new" || v.Canonical.Phase != "scope-waiting" || v.Canonical.Lane != LaneDecision {
		t.Fatalf("read-back %+v", v)
	}
	close(release)
	waitJob(t, h, runKey(42))
	if v = viewOf[RunJobView](t, do(t, h, authed("/api/runs/42")), http.StatusOK); v.State != "finished" || !v.Committed {
		t.Fatalf("finished run %+v", v)
	}
	// The ledger now shows a Molecule waiting, not running, so a new run may start.
	for path, code := range map[string]int{"/api/runs/07": 400, "/api/runs/x": 400, "/api/runs/0": 400, "/api/runs/43": 404} {
		if w := do(t, h, authed(path)); w.Code != code {
			t.Errorf("GET %s: %d", path, w.Code)
		}
	}
}

func TestUIDriveRunRefusalsFailSafe(t *testing.T) {
	lab, base := labFixture(t)
	writeRecord(t, lab, intakeRecord(base, "mol-9-cli", 9, now(), "running", ""))
	h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{run: func(context.Context, molecule.Options) error {
		return errors.New("git failed in /Users/someone/src with ghp_secret")
	}})
	// The ledgers already show a running Molecule for the Issue.
	refusalOf(t, do(t, h, gatePost("/api/runs", `{"issue":9}`)), http.StatusConflict, "busy")
	// Engine error text, which names local paths, is never served.
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":11}`)), http.StatusAccepted)
	waitJob(t, h, runKey(11))
	if v := viewOf[RunJobView](t, do(t, h, authed("/api/runs/11")), http.StatusOK); v.State != "refused" || v.Category != "refused" || v.Committed || v.Reason != categoryReasons["refused"] {
		t.Fatalf("refused run %+v", v)
	}
	// A registry full of running jobs refuses; finished jobs are evicted first.
	hh := h.(*handler)
	hold := make(chan struct{})
	for i := 0; i < maxGateJobs; i++ {
		if _, err := hh.jobs.start("hold-"+strconv.Itoa(i), "scope", "", func(ctx context.Context) error { <-hold; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	refusalOf(t, do(t, h, gatePost("/api/runs", `{"issue":12}`)), http.StatusServiceUnavailable, "busy")
	close(hold)
	if n := d.calls(); n != 1 {
		t.Fatalf("engine calls %d", n)
	}
	// Below a resource floor, without a source checkout, and for sample data
	// nothing starts.
	low, _ := driveHandler(t, lab, guardWith(1<<30), d)
	refusalOf(t, do(t, low, gatePost("/api/runs", `{"issue":12}`)), http.StatusServiceUnavailable, "resources")
	refusalOf(t, do(t, low, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`)), http.StatusServiceUnavailable, "resources")
	bare := testHandler(t, Options{LabDir: lab})
	installDrive(bare, d)
	refusalOf(t, do(t, bare, gatePost("/api/runs", `{"issue":12}`)), http.StatusConflict, "unavailable")
	refusalOf(t, do(t, bare, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`)), http.StatusConflict, "unavailable")
	demo := testHandler(t, Options{Demo: true})
	installDrive(demo, d)
	for _, path := range []string{"/api/runs", "/api/loop/start", "/api/loop/stop", "/api/molecules/m1/stop"} {
		refusalOf(t, do(t, demo, gatePost(path, `{}`)), http.StatusNotFound, "sample")
	}
	for _, path := range []string{"/api/loop", "/api/runs/1"} {
		refusalOf(t, do(t, demo, authed(path)), http.StatusNotFound, "sample")
	}
	if n := d.calls(); n != 1 {
		t.Fatalf("refused writes reached the engine: %d calls", n)
	}
}

func TestUIDriveLoopStartsOnceAndStopsGracefully(t *testing.T) {
	lab, _ := labFixture(t)
	guard := guardWith(64 << 30)
	running, workAlive := make(chan struct{}), make(chan bool, 1)
	h, d := driveHandler(t, lab, guard, &fakeDrive{loop: func(ctx context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		close(running)
		<-o.Drain
		// The stop request drains; Molecules in flight keep their context.
		workAlive <- o.Work.Err() == nil && ctx.Err() == nil
		return molecule.LoopReport{Repository: "o/r", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{
			{Repository: "o/r", Issue: 5, MoleculeID: "mol-5-a", Started: true, State: molecule.LoopWaiting, Status: "waiting", Phase: "scope-waiting", Reason: "lock /Users/someone/.lab/lock"},
			{Repository: "../x", Issue: 6, MoleculeID: "../evil", State: "bad state\n", Reason: "ghp_secret"},
		}}}}, nil
	}})
	v := viewOf[LoopView](t, do(t, h, gatePost("/api/loop/start", `{"select":"label:ready","maxConcurrent":2}`)), http.StatusAccepted)
	if v.State != "running" || v.Select != "label:ready" || v.MaxConcurrent != 2 {
		t.Fatalf("started loop %+v", v)
	}
	<-running
	refusalOf(t, do(t, h, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`)), http.StatusConflict, "busy")
	if n := d.calls(); n != 1 {
		t.Fatalf("engine calls %d", n)
	}
	o := d.loops[0]
	if o.Template.LabDir != lab || o.Template.SourceRoot != "/src/checkout" || o.Template.Resources != guard || o.Template.Issue != "" || o.Select.String() != "label:ready" || o.MaxConcurrent != 2 || o.Once || o.Drain == nil || o.Work == nil {
		t.Fatalf("loop options %+v", o)
	}
	if v = viewOf[LoopView](t, do(t, h, gatePost("/api/loop/stop", `{}`)), http.StatusAccepted); v.State != "draining" {
		t.Fatalf("stopping loop %+v", v)
	}
	refusalOf(t, do(t, h, gatePost("/api/loop/stop", `{}`)), http.StatusConflict, "not-running")
	if !<-workAlive {
		t.Fatal("the stop request cancelled work in flight")
	}
	waitLoop(t, h)
	w := do(t, h, authed("/api/loop"))
	v = viewOf[LoopView](t, w, http.StatusOK)
	if v.State != "finished" || v.Report == nil || v.Report.Repository != "o/r" || v.Report.Passes != 1 || len(v.Report.Items) != 2 {
		t.Fatalf("finished loop %+v", v)
	}
	if got := v.Report.Items[0]; got != (LoopItemView{Repository: "o/r", Issue: 5, MoleculeID: "mol-5-a", State: "waiting", Status: "waiting", Phase: "scope-waiting", Started: true}) {
		t.Fatalf("item %+v", got)
	}
	if got := v.Report.Items[1]; got.Repository != "" || got.MoleculeID != "" || got.State != "" {
		t.Fatalf("an invalid item was served: %+v", got)
	}
	if strings.Contains(w.Body.String(), "lock") || strings.Contains(w.Body.String(), "evil") {
		t.Fatalf("loop reasons were served: %s", w.Body)
	}
	refusalOf(t, do(t, h, gatePost("/api/loop/stop", `{}`)), http.StatusConflict, "not-running")
}

func TestUIDriveStopsOnlyAMoleculeThisUIRuns(t *testing.T) {
	lab, base := labFixture(t)
	writeRecord(t, lab, intakeRecord(base, "mol-9-cli", 9, now(), "running", ""))
	live := make(chan error, 1)
	cancelled, finished := make(chan struct{}), make(chan error, 1)
	h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{run: func(ctx context.Context, o molecule.Options) error {
		r := intakeRecord(base, "mol-42-live", 42, now(), "running", "")
		live <- saveRecord(lab, r)
		<-ctx.Done()
		close(cancelled)
		// The engine records the stopped intake and returns.
		r.Status, r.FinishedAt = "blocked", now()
		finished <- saveRecord(lab, r)
		return nil
	}})
	// The crash leftover and the CLI's ledger are refused before any job runs.
	refusalOf(t, do(t, h, gatePost("/api/molecules/mol-9-cli/stop", `{}`)), http.StatusConflict, "not-owned")
	refusalOf(t, do(t, h, gatePost("/api/molecules/mol-1-none/stop", `{}`)), http.StatusNotFound, "missing")
	refusalOf(t, do(t, h, gatePost("/api/molecules/"+base.ID+"/stop", `{}`)), http.StatusConflict, "not-running")
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":42}`)), http.StatusAccepted)
	if err := <-live; err != nil {
		t.Fatal(err)
	}
	// An older ledger for the same Issue, left running by a crash, is not this
	// run's Molecule.
	writeRecord(t, lab, intakeRecord(base, "mol-42-old", 42, "2000-01-01T00:00:00Z", "running", ""))
	refusalOf(t, do(t, h, gatePost("/api/molecules/mol-42-old/stop", `{}`)), http.StatusConflict, "not-owned")
	refusalOf(t, do(t, h, gatePost("/api/molecules/mol-9-cli/stop", `{}`)), http.StatusConflict, "not-owned")
	select {
	case <-cancelled:
		t.Fatal("a refused stop signalled the run")
	default:
	}
	v := viewOf[StopView](t, do(t, h, gatePost("/api/molecules/mol-42-live/stop", `{}`)), http.StatusAccepted)
	if v.MoleculeID != "mol-42-live" || !v.Requested || v.Canonical.Source != "ledger" {
		t.Fatalf("stop %+v", v)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	waitJob(t, h, runKey(42))
	// The stop is committed once the ledger no longer shows the Molecule running.
	if r := viewOf[RunJobView](t, do(t, h, authed("/api/runs/42")), http.StatusOK); !r.Committed || r.Canonical.MoleculeID != "mol-42-live" || r.Canonical.Status != "blocked" {
		t.Fatalf("read-back after stop %+v", r)
	}
	refusalOf(t, do(t, h, gatePost("/api/molecules/mol-42-live/stop", `{}`)), http.StatusConflict, "not-running")
	if n := d.calls(); n != 1 {
		t.Fatalf("engine calls %d", n)
	}
}

func TestUIDriveStopsAScopeApprovalJob(t *testing.T) {
	lab, r, digest := gateFixture(t)
	h, _ := driveHandler(t, lab, nil, &fakeDrive{})
	started := make(chan struct{})
	installEngine(h, &fakeEngine{scope: func(ctx context.Context, o molecule.Options) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}})
	jobOf(t, do(t, h, gatePost("/api/molecules/"+r.ID+"/scope", `{"digest":"`+digest+`"}`)), http.StatusAccepted)
	<-started
	// The approved Molecule is coding.
	r.Status = "running"
	writeRecord(t, lab, r)
	if v := viewOf[StopView](t, do(t, h, gatePost("/api/molecules/"+r.ID+"/stop", `{}`)), http.StatusAccepted); !v.Requested || v.Committed || v.Canonical.Status != "running" {
		t.Fatalf("stop %+v", v)
	}
	waitJob(t, h, r.ID)
	if s, _ := h.(*handler).jobs.get(r.ID); s.state != "interrupted" {
		t.Fatalf("scope job %+v", s)
	}
}

// A refused run or loop names the next step for the engine error it recognises,
// and never serves the engine text, its paths, environment values or tokens.
func TestUIDriveRefusalsNameTheNextStepWithoutLeaks(t *testing.T) {
	lab, _ := labFixture(t)
	secret := " in " + filepath.Join(lab, "ledgers") + " with ANTHROPIC_API_KEY=sk-ant-" + strings.Repeat("b", 30) + " ghp_" + strings.Repeat("C", 36)
	auth := "Harness not authenticated: run its login command (claude auth login for Claude Code, codex login for Codex, agent login for Cursor), then start again"
	h, _ := driveHandler(t, lab, nil, &fakeDrive{
		run: func(context.Context, molecule.Options) error {
			return errors.New("analysis harness failed: authentication_error" + secret)
		},
		loop: func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
			return molecule.LoopReport{}, errors.New("the approved Formula's intake.repo changed from o/r to acme/widgets during the run; restart the loop" + secret)
		},
	})
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":11}`)), http.StatusAccepted)
	waitJob(t, h, runKey(11))
	w := do(t, h, authed("/api/runs/11"))
	if v := viewOf[RunJobView](t, w, http.StatusOK); v.State != "refused" || v.Reason != auth {
		t.Fatalf("refused run %+v", v)
	}
	noLeak(t, w.Body.String(), lab, "sk-ant-", "ANTHROPIC_API_KEY", "authentication_error")
	viewOf[LoopView](t, do(t, h, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`)), http.StatusAccepted)
	waitLoop(t, h)
	w = do(t, h, authed("/api/loop"))
	if v := viewOf[LoopView](t, w, http.StatusOK); v.State != "refused" || v.Category != "refused" || v.Reason != "This Lab's Formula now targets acme/widgets; restart the loop" {
		t.Fatalf("refused loop %+v", v)
	}
	noLeak(t, w.Body.String(), lab, "sk-ant-", "ANTHROPIC_API_KEY")
	// Every refusal the UI makes itself carries its category's next step.
	refusalOf(t, do(t, h, gatePost("/api/loop/stop", `{}`)), http.StatusConflict, "not-running")
	refusalOf(t, do(t, h, gatePost("/api/runs", `{"issue":0}`)), http.StatusBadRequest, "invalid")
	refusalOf(t, do(t, h, authed("/api/runs/12")), http.StatusNotFound, "none")
}

// molecule.Run takes no per-Issue lock, so a UI run and the UI loop never run
// together: a running run refuses the loop.
func TestUIDriveRunThenLoopIsRefused(t *testing.T) {
	lab, _ := labFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	h, d := driveHandler(t, lab, nil, &fakeDrive{
		run: func(ctx context.Context, o molecule.Options) error {
			close(started)
			<-release
			return nil
		},
		loop: func(ctx context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
			<-o.Drain
			return molecule.LoopReport{}, nil
		},
	})
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":5}`)), http.StatusAccepted)
	<-started
	refusalOf(t, do(t, h, gatePost("/api/loop/start", `{"select":"label:ready","maxConcurrent":1}`)), http.StatusConflict, "busy")
	if n := d.calls(); n != 1 {
		t.Fatalf("engine calls %d", n)
	}
	if v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK); v.State != "idle" {
		t.Fatalf("loop %+v", v)
	}
	// Once the run has finished the loop may start.
	close(release)
	waitJob(t, h, runKey(5))
	viewOf[LoopView](t, do(t, h, gatePost("/api/loop/start", `{"select":"label:ready","maxConcurrent":1}`)), http.StatusAccepted)
	viewOf[LoopView](t, do(t, h, gatePost("/api/loop/stop", `{}`)), http.StatusAccepted)
	waitLoop(t, h)
	if n := d.calls(); n != 2 {
		t.Fatalf("engine calls %d", n)
	}
}

// A running or draining loop refuses a run.
func TestUIDriveLoopThenRunIsRefused(t *testing.T) {
	lab, _ := labFixture(t)
	running, finish := make(chan struct{}), make(chan struct{})
	h, d := driveHandler(t, lab, nil, &fakeDrive{
		run: func(ctx context.Context, o molecule.Options) error { return nil },
		loop: func(ctx context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
			close(running)
			<-o.Drain
			<-finish
			return molecule.LoopReport{}, nil
		},
	})
	viewOf[LoopView](t, do(t, h, gatePost("/api/loop/start", `{"select":"label:ready","maxConcurrent":1}`)), http.StatusAccepted)
	<-running
	refusalOf(t, do(t, h, gatePost("/api/runs", `{"issue":5}`)), http.StatusConflict, "busy")
	if v := viewOf[LoopView](t, do(t, h, gatePost("/api/loop/stop", `{}`)), http.StatusAccepted); v.State != "draining" {
		t.Fatalf("loop %+v", v)
	}
	refusalOf(t, do(t, h, gatePost("/api/runs", `{"issue":5}`)), http.StatusConflict, "busy")
	if n := d.calls(); n != 1 {
		t.Fatalf("engine calls %d", n)
	}
	if w := do(t, h, authed("/api/runs/5")); w.Code != http.StatusNotFound {
		t.Fatalf("a refused run was registered: %d %s", w.Code, w.Body)
	}
	// Once the loop has finished a run may start.
	close(finish)
	waitLoop(t, h)
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":5}`)), http.StatusAccepted)
	waitJob(t, h, runKey(5))
	if n := d.calls(); n != 2 {
		t.Fatalf("engine calls %d", n)
	}
}

// Stopping the server cancels run jobs, drains and cancels the loop, and refuses
// new work.
func TestUIDriveServerStopCancelsJobsAndDrainsTheLoop(t *testing.T) {
	lab, _ := labFixture(t)
	runStarted, loopStarted := make(chan struct{}), make(chan struct{})
	drained := make(chan bool, 1)
	h, d := driveHandler(t, lab, nil, &fakeDrive{
		run: func(ctx context.Context, o molecule.Options) error {
			close(runStarted)
			<-ctx.Done()
			return ctx.Err()
		},
		loop: func(ctx context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
			close(loopStarted)
			<-o.Work.Done()
			select {
			case <-o.Drain:
				drained <- true
			default:
				drained <- false
			}
			return molecule.LoopReport{}, nil
		},
	})
	// A run and the loop never run together, so the run gets its own UI.
	hr, _ := driveHandler(t, lab, nil, d)
	viewOf[RunJobView](t, do(t, hr, gatePost("/api/runs", `{"issue":3}`)), http.StatusAccepted)
	viewOf[LoopView](t, do(t, h, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`)), http.StatusAccepted)
	<-runStarted
	<-loopStarted
	hr.(*handler).stopWork(jobStopGrace)
	h.(*handler).stopWork(jobStopGrace)
	if s, _ := hr.(*handler).jobs.get(runKey(3)); s.state != "interrupted" {
		t.Fatalf("run job %+v", s)
	}
	if !<-drained {
		t.Fatal("the loop was not drained before its work was cancelled")
	}
	if v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK); v.State != "finished" {
		t.Fatalf("loop %+v", v)
	}
	refusalOf(t, do(t, h, gatePost("/api/runs", `{"issue":4}`)), http.StatusServiceUnavailable, "stopped")
	refusalOf(t, do(t, h, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`)), http.StatusServiceUnavailable, "stopped")
	if n := d.calls(); n != 2 {
		t.Fatalf("engine calls %d", n)
	}
}
