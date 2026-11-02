package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// keyedFake is a fake tracker plugin behind the trackerFor seam: it counts
// every call, so a test can prove a refused request never started the plugin.
// No plugin process starts and nothing reaches the network.
type keyedFake struct {
	mu          sync.Mutex
	pattern     string
	describeErr error
	describes   int
	gets        []string
	prepared    []string
	observed    int
	// hang, when set, holds every describe until it is closed or the call's
	// context ends; entered receives once a describe is held.
	hang    chan struct{}
	entered chan struct{}
}

func (k *keyedFake) Describe(ctx context.Context) (contract.TrackerDescription, error) {
	k.mu.Lock()
	k.describes++
	hang, entered := k.hang, k.entered
	k.mu.Unlock()
	if hang != nil {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-hang:
		case <-ctx.Done():
			return contract.TrackerDescription{}, ctx.Err()
		}
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.describeErr != nil {
		return contract.TrackerDescription{}, k.describeErr
	}
	return contract.TrackerDescription{DisplayName: "Fake tracker", KeyPattern: k.pattern, States: []string{"open", "closed"}}, nil
}

// keyedBackgroundDone waits for h's background describe to finish.
func keyedBackgroundDone(t *testing.T, h http.Handler) {
	t.Helper()
	c := &h.(*handler).keyedPatterns
	c.mu.Lock()
	done := c.backgroundDone
	c.mu.Unlock()
	if done == nil {
		t.Fatal("no background describe was started")
	}
	<-done
}

func (k *keyedFake) List(context.Context, string, string, int) (contract.TrackerPage, error) {
	return contract.TrackerPage{}, errors.New("not listed in this slice")
}

func (k *keyedFake) Get(_ context.Context, key string) (contract.TrackerIssue, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gets = append(k.gets, key)
	return contract.TrackerIssue{Key: key, Title: "Fix " + key, Body: "body", State: "open", URL: "https://tracker.example/browse/" + key, UpdatedAt: "2026-11-01T00:00:00Z"}, nil
}

func (k *keyedFake) States(context.Context, []string) (contract.TrackerStates, error) {
	return contract.TrackerStates{}, errors.New("not read in this slice")
}

func (k *keyedFake) counts() (describes, gets, prepared int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.describes, len(k.gets), len(k.prepared)
}

// keyedRuns replaces the keyed engine seams and records every call.
type keyedRuns struct {
	mu     sync.Mutex
	runs   []molecule.Options
	keys   []string
	scopes []molecule.Options
	scoped []string
	run    func(context.Context, molecule.Options, string) error
	scope  func(context.Context, molecule.Options, string) error
}

func (k *keyedRuns) calls() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.runs) + len(k.scopes)
}

// keyedTestFormula is an approved Formula whose intake.source is a tracker
// plugin; it is never written, the keyedFormula seam serves it.
func keyedTestFormula() formula.Formula {
	return formula.Formula{ID: "f", Intake: formula.Intake{Source: "tracker", Repo: "o/r"}, Plugins: []formula.PluginRef{{Name: "tracker", Manifest: "plugins/tracker/manifest.json", SHA256: strings.Repeat("a", 64)}}}
}

// installKeyed makes h a tracker-plugin Lab served by fake, with the keyed
// engine calls replaced by runs.
func installKeyed(h http.Handler, fake *keyedFake, runs *keyedRuns) {
	hh := h.(*handler)
	hh.keyedFormula = func(string) (keyedActiveFormula, bool) {
		return keyedActiveFormula{formula: keyedTestFormula(), identity: "f\x00" + strings.Repeat("b", 64)}, true
	}
	hh.trackerFor = func(lab string, f formula.Formula, o mcpadapter.Options) (tracker.Keyed, bool, error) {
		fake.mu.Lock()
		fake.prepared = append(fake.prepared, lab)
		if o.Observer != nil {
			fake.observed++
		}
		fake.mu.Unlock()
		if _, ok := f.TrackerPlugin(); !ok {
			return nil, false, nil
		}
		return fake, true, nil
	}
	hh.runKeyedIssue = func(ctx context.Context, o molecule.Options, key string) (molecule.Record, error) {
		runs.mu.Lock()
		runs.runs, runs.keys = append(runs.runs, o), append(runs.keys, key)
		run := runs.run
		runs.mu.Unlock()
		if run == nil {
			return molecule.Record{}, errors.New("no keyed run seam")
		}
		return molecule.Record{}, run(ctx, o, key)
	}
	hh.runKeyedScope = func(ctx context.Context, o molecule.Options, key string) (molecule.Record, error) {
		runs.mu.Lock()
		runs.scopes, runs.scoped = append(runs.scopes, o), append(runs.scoped, key)
		scope := runs.scope
		runs.mu.Unlock()
		if scope == nil {
			return molecule.Record{}, errors.New("no keyed scope seam")
		}
		return molecule.Record{}, scope(ctx, o, key)
	}
}

// keyedRecord is base as molecule.RunKeyed writes it for key.
func keyedRecord(base molecule.Record, id, key, startedAt, status, phase string) molecule.Record {
	r := intakeRecord(base, id, 0, startedAt, status, phase)
	r.Issue.Key, r.Issue.Number, r.Issue.URL = key, 0, "https://tracker.example/browse/"+key
	return r
}

// keyedRefusalOf checks a refusal's category and fixed reason, with no local
// path or secret in the body.
func keyedRefusalOf(t *testing.T, h http.Handler, body string, code int, category string) {
	t.Helper()
	refusalOf(t, do(t, h, gatePost("/api/runs", body)), code, category)
}

func TestUIKeyedRunStartsIntakeThroughTheTrackerAndReadsTheLedgerBack(t *testing.T) {
	lab, base := labFixture(t)
	guard := guardWith(64 << 30)
	h, d := driveHandler(t, lab, guard, &fakeDrive{})
	fake := &keyedFake{pattern: `^ENG-[0-9]+$`}
	read := make(chan molecule.Issue, 1)
	written, release := make(chan error, 1), make(chan struct{})
	runs := &keyedRuns{run: func(ctx context.Context, o molecule.Options, key string) error {
		// The engine reads the keyed Issue only through the reader it was given.
		issue, err := o.ReadIssueKey(ctx, "o/r", key, "")
		if err != nil {
			return err
		}
		read <- issue
		written <- saveRecord(lab, keyedRecord(base, "mol-eng-42-new", key, now(), "waiting", "scope-waiting"))
		<-release
		return nil
	}}
	installKeyed(h, fake, runs)
	// An older ledger for the key is never this run's.
	writeRecord(t, lab, keyedRecord(base, "mol-eng-42-old", "ENG-42", "2000-01-01T00:00:00Z", "blocked", ""))
	v := viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"key":"ENG-42"}`)), http.StatusAccepted)
	if v.Key != "ENG-42" || v.Issue != 0 || v.State != "running" || v.Canonical.Source != "ledger" {
		t.Fatalf("accepted keyed run %+v", v)
	}
	// A duplicate submission never reaches the engine.
	keyedRefusalOf(t, h, `{"key":"ENG-42"}`, http.StatusConflict, "busy")
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	issue := <-read
	if issue.Key != "ENG-42" || issue.Number != 0 || issue.Repo != "o/r" || issue.State != "OPEN" || issue.URL != "https://tracker.example/browse/ENG-42" {
		t.Fatalf("keyed reader %+v", issue)
	}
	v = viewOf[RunJobView](t, do(t, h, authed("/api/runs?key=ENG-42")), http.StatusOK)
	if v.Key != "ENG-42" || v.State != "running" || !v.Committed || v.Canonical.MoleculeID != "mol-eng-42-new" || v.Canonical.Phase != "scope-waiting" {
		t.Fatalf("keyed read-back %+v", v)
	}
	close(release)
	waitJob(t, h, keyedRunJob("ENG-42"))
	if v = viewOf[RunJobView](t, do(t, h, authed("/api/runs?key=ENG-42")), http.StatusOK); v.State != "finished" || !v.Committed {
		t.Fatalf("finished keyed run %+v", v)
	}
	if runs.calls() != 1 || d.calls() != 0 {
		t.Fatalf("keyed engine calls %d, numeric engine calls %d", runs.calls(), d.calls())
	}
	o, key := runs.runs[0], runs.keys[0]
	if key != "ENG-42" || o.LabDir != lab || o.SourceRoot != "/src/checkout" || o.Resources != guard || o.Issue != "" || o.Approve != "" || o.PluginCalls == nil || o.ReadIssueKey == nil || !o.AllowKeyedApproval {
		t.Fatalf("keyed run options %+v key %q", o, key)
	}
	// The keyPattern was described once, for the first start, and the Issue read
	// went through the tracker with every call observed for the Molecule.
	if describes, gets, _ := fake.counts(); describes != 1 || gets != 1 || fake.observed != 1 {
		t.Fatalf("describes %d, gets %d, observed trackers %d", describes, gets, fake.observed)
	}
	// The keyed job key never collides with a numeric run's, and the numeric
	// status route is unchanged.
	if keyedRunJob("42") == runKey(42) {
		t.Fatal("keyed and numeric job keys collide")
	}
	for path, code := range map[string]int{"/api/runs": 404, "/api/runs?key=": 400, "/api/runs?key=ENG-1&key=ENG-2": 400, "/api/runs?key=ENG-42&x=1": 400, "/api/runs?key=ENG-7": 404, "/api/runs/42": 404, "/api/runs?key=" + strings.Repeat("A", 129): 400} {
		if w := do(t, h, authed(path)); w.Code != code {
			t.Errorf("GET %s: %d %s", path, w.Code, w.Body)
		}
	}
	demo := testHandler(t, Options{Demo: true})
	refusalOf(t, do(t, demo, authed("/api/runs?key=ENG-42")), http.StatusNotFound, "sample")
}

func TestUIKeyedRunRefusalsStartNothing(t *testing.T) {
	lab, base := labFixture(t)
	h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	fake := &keyedFake{pattern: `^ENG-[0-9]+$`}
	runs := &keyedRuns{}
	// The ledgers show a running Molecule for ENG-9.
	writeRecord(t, lab, keyedRecord(base, "mol-eng-9-cli", "ENG-9", now(), "running", ""))
	before := snapshot(t, lab)

	// A GitHub Lab refuses a key, and the plugin is never prepared.
	keyedRefusalOf(t, h, `{"key":"ENG-1"}`, http.StatusConflict, "not-keyed")
	installKeyed(h, fake, runs)

	// Both fields, a key that is not one line of at most 128 runes, and a key that
	// does not match the tracker's pattern are invalid.
	for _, body := range []string{`{"issue":7,"key":"ENG-1"}`, `{"key":"ENG\n1"}`, `{"key":"` + strings.Repeat("E", 129) + `"}`, `{"key":"ENG‮1"}`, `{"key":7}`} {
		keyedRefusalOf(t, h, body, http.StatusBadRequest, "invalid")
	}
	if describes, _, prepared := fake.counts(); describes != 0 || prepared != 0 {
		t.Fatalf("an invalid key started the plugin: %d describes, %d prepared", describes, prepared)
	}
	for _, body := range []string{`{"key":"eng-1"}`, `{"key":"ENG-1 "}`, `{"key":"xENG-1"}`, `{"key":"ENG-"}`} {
		keyedRefusalOf(t, h, body, http.StatusBadRequest, "invalid")
	}
	// A numeric request on a tracker-plugin Lab is refused before GitHub is
	// read; an invalid number keeps its numeric refusal.
	keyedRefusalOf(t, h, `{"issue":7}`, http.StatusConflict, "keyed")
	if w := do(t, h, gatePost("/api/runs", `{"issue":7}`)); !strings.Contains(w.Body.String(), "this Lab's Issues are tracker keys; start with key") {
		t.Fatalf("numeric refusal %s", w.Body)
	}
	for _, body := range []string{`{}`, `{"issue":null}`, `{"issue":0}`, `{"key":""}`, `{"issue":-1}`} {
		keyedRefusalOf(t, h, body, http.StatusBadRequest, "invalid")
	}
	// A running Molecule for the key, matched by its raw Key, refuses a start.
	keyedRefusalOf(t, h, `{"key":"ENG-9"}`, http.StatusConflict, "busy")
	if runs.calls() != 0 || d.calls() != 0 {
		t.Fatalf("refused keyed starts reached the engine: %d keyed, %d numeric", runs.calls(), d.calls())
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("a refused keyed start changed the Lab:\n%s\n---\n%s", before, after)
	}
}

// More candidate ledgers for a key than one lookup reads is an incomplete
// scan: it counts as busy, so a running keyed Molecule past the bound is never
// missed, and nothing starts.
func TestUIKeyedRunIncompleteLedgerScanIsBusy(t *testing.T) {
	lab, base := labFixture(t)
	h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	fake := &keyedFake{pattern: `^ENG-[0-9]+$`}
	runs := &keyedRuns{run: func(context.Context, molecule.Options, string) error { return nil }}
	installKeyed(h, fake, runs)
	for i := 0; i <= keyedMaxReads; i++ {
		writeRecord(t, lab, keyedRecord(base, fmt.Sprintf("mol-eng-3-%03d", i), "ENG-3", "2000-01-01T00:00:00Z", "blocked", ""))
	}
	w := do(t, h, gatePost("/api/runs", `{"key":"ENG-3"}`))
	refusalOf(t, w, http.StatusConflict, "busy")
	if !strings.Contains(w.Body.String(), "cannot be ruled out") {
		t.Fatalf("incomplete scan refusal %s", w.Body)
	}
	if describes, _, prepared := fake.counts(); describes != 0 || prepared != 0 || runs.calls() != 0 || d.calls() != 0 {
		t.Fatalf("an incomplete scan started work: %d describes, %d prepared, %d keyed, %d numeric", describes, prepared, runs.calls(), d.calls())
	}
	// Within the bound, the same finished ledgers do not block a start.
	other := keyedRecord(base, "mol-eng-4-old", "ENG-4", "2000-01-01T00:00:00Z", "blocked", "")
	writeRecord(t, lab, other)
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"key":"ENG-4"}`)), http.StatusAccepted)
	waitJob(t, h, keyedRunJob("ENG-4"))
	if runs.calls() != 1 {
		t.Fatalf("keyed runs %d, want 1", runs.calls())
	}
}

func TestUIKeyedRunTrackerFailuresFailSafe(t *testing.T) {
	lab, _ := labFixture(t)
	h, _ := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	fake := &keyedFake{pattern: `^ENG-[0-9]+$`, describeErr: errors.New("plugin failed in /Users/someone/lab with ghp_secret")}
	runs := &keyedRuns{run: func(context.Context, molecule.Options, string) error {
		return errors.New("tracker_get failed in /Users/someone/lab with ghp_secret")
	}}
	installKeyed(h, fake, runs)
	// A tracker that cannot describe its keys refuses with a fixed reason; plugin
	// text is never served.
	keyedRefusalOf(t, h, `{"key":"ENG-1"}`, http.StatusConflict, "tracker")
	// The Lab view caches that failure: a poll never starts the plugin again.
	for i := 0; i < 3; i++ {
		v := viewOf[LabView](t, do(t, h, authed("/api/lab")), http.StatusOK)
		if v.Intake == nil || !v.Intake.Keyed || v.Intake.KeyPattern != "" {
			t.Fatalf("intake %+v", v.Intake)
		}
	}
	if describes, _, _ := fake.counts(); describes != 1 {
		t.Fatalf("describes %d, want 1", describes)
	}
	// A keyed start describes again; once it succeeds the pattern is cached.
	fake.mu.Lock()
	fake.describeErr = nil
	fake.mu.Unlock()
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"key":"ENG-1"}`)), http.StatusAccepted)
	waitJob(t, h, keyedRunJob("ENG-1"))
	v := viewOf[RunJobView](t, do(t, h, authed("/api/runs?key=ENG-1")), http.StatusOK)
	if v.State != "refused" || v.Category != "refused" || v.Committed || v.Reason != categoryReasons["refused"] {
		t.Fatalf("refused keyed run %+v", v)
	}
	viewOf[LabView](t, do(t, h, authed("/api/lab")), http.StatusOK)
	if describes, _, _ := fake.counts(); describes != 2 {
		t.Fatalf("describes %d, want 2", describes)
	}
	// Without a source checkout or below a resource floor nothing starts, and the
	// plugin is not described for it.
	bare := testHandler(t, Options{LabDir: lab})
	installKeyed(bare, fake, runs)
	keyedRefusalOf(t, bare, `{"key":"ENG-2"}`, http.StatusConflict, "unavailable")
	low, _ := driveHandler(t, lab, guardWith(1<<30), &fakeDrive{})
	installKeyed(low, fake, runs)
	keyedRefusalOf(t, low, `{"key":"ENG-2"}`, http.StatusServiceUnavailable, "resources")
	if describes, _, _ := fake.counts(); describes != 2 || runs.calls() != 1 {
		t.Fatalf("describes %d, keyed runs %d", describes, runs.calls())
	}
}

// The Molecule a keyed run job started is this UI's to stop, matched by its
// raw Key: another keyed Molecule whose key only slugs the same is not.
func TestUIKeyedRunIsOwnedByItsRawKey(t *testing.T) {
	lab, base := labFixture(t)
	h, _ := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	written := make(chan error, 1)
	runs := &keyedRuns{run: func(ctx context.Context, _ molecule.Options, key string) error {
		written <- saveRecord(lab, keyedRecord(base, "mol-eng-5-run", key, now(), "running", ""))
		<-ctx.Done()
		return ctx.Err()
	}}
	installKeyed(h, &keyedFake{pattern: `^(?i:ENG)-[0-9]+$`}, runs)
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"key":"ENG-5"}`)), http.StatusAccepted)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	// Same slug, another raw key: a terminal's Molecule, not this run's.
	writeRecord(t, lab, keyedRecord(base, "mol-eng-5-twin", "eng-5", now(), "running", ""))
	for id, want := range map[string]string{"mol-eng-5-run": RunnerRun, "mol-eng-5-twin": RunnerOther} {
		d := viewOf[Detail](t, do(t, h, authed("/api/molecules/"+id)), http.StatusOK)
		if d.View == nil || d.View.Control == nil || d.View.Control.Runner != want || d.View.Control.Stoppable != (want == RunnerRun) {
			t.Fatalf("%s control %+v, want runner %s", id, d.View, want)
		}
	}
	refusalOf(t, do(t, h, gatePost("/api/molecules/mol-eng-5-twin/stop", `{}`)), http.StatusConflict, "not-owned")
	if v := viewOf[StopView](t, do(t, h, gatePost("/api/molecules/mol-eng-5-run/stop", `{}`)), http.StatusAccepted); !v.Requested {
		t.Fatalf("stop %+v", v)
	}
	waitJob(t, h, keyedRunJob("ENG-5"))
	if v := viewOf[RunJobView](t, do(t, h, authed("/api/runs?key=ENG-5")), http.StatusOK); v.State != "interrupted" {
		t.Fatalf("stopped keyed run %+v", v)
	}
}

func TestUIKeyedPatternIsAnchoredAndBounded(t *testing.T) {
	for in, want := range map[string]string{
		`^ENG-[0-9]+$`:           `^ENG-[0-9]+$`,
		`ENG-[0-9]+`:             `^(?:ENG-[0-9]+)$`,
		`^a|b$`:                  `^a|b$`,
		``:                       ``,
		`(`:                      ``,
		"ENG\n[0-9]+":            ``,
		"ENG‮[0-9]+":             ``,
		strings.Repeat("a", 251): ``,
	} {
		if got := keyedAnchored(in); got != want {
			t.Errorf("keyedAnchored(%q) = %q, want %q", in, got, want)
		}
	}
}

// The served JS names every Issue through core.js issueLabel (its tracker key
// when it has one), links a keyed Issue only to its recorded https tracker URL,
// posts {key} for a keyed start and keeps every numeric text as it was.
func TestUIServedJSNamesKeyedIssuesWithIssueLabel(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	core := do(t, h, authed("/js/core.js")).Body.String()
	wantAll(t, "core.js", core,
		"export function issueLabel(m) {",
		"if (m && typeof m.key === 'string' && m.key) return m.key;",
		"return m && Number.isInteger(m.number) ? '#' + m.number : '';",
		"export const TRACKER_URL = /^https:\\/\\/",
		"(issue.key ? TRACKER_URL : ISSUE_URL).test(url)")
	numbered := regexp.MustCompile(`'#' \+ (?:m|d\.issue|row)\.number|'Issue #' \+|' #' \+ d\.issue|'Selected #'`)
	for _, path := range []string{"/js/inbox.js", "/js/insights.js", "/js/work.js", "/js/molecule.js", "/js/molecule-decision.js", "/js/molecule-timeline.js"} {
		js := do(t, h, authed(path)).Body.String()
		if !strings.Contains(js, "issueLabel(") || !regexp.MustCompile(`(?m)^import \{[^}]*\bissueLabel\b[^}]*\} from './core\.js';$`).MatchString(js) {
			t.Errorf("%s does not name Issues with issueLabel", path)
		}
		if m := numbered.FindString(js); m != "" {
			t.Errorf("%s still builds %q from the number", path, m)
		}
	}
	// Any text ending in "#" joined to a Molecule's number, with a prefix such as
	// ': #' or 'MOLECULE #', in every served module. The Issues view's GitHub
	// rows (row.number) and PR numbers are numbers by design; issueLabel itself is
	// the one place a Molecule's number is written with "#".
	prefixed := regexp.MustCompile(`#['"]\s*\+\s*(?:m|d|s|x|mol|molecule|d\.issue|m\.issue|issue)\.number\b`)
	for _, path := range servedModules {
		js := do(t, h, authed(path)).Body.String()
		if path == "/js/core.js" {
			js = strings.Replace(js, "return m && Number.isInteger(m.number) ? '#' + m.number : '';", "", 1)
		}
		if m := prefixed.FindString(js); m != "" {
			t.Errorf("%s still builds %q from a Molecule's number", path, m)
		}
	}
	timeline := do(t, h, authed("/js/molecule-timeline.js")).Body.String()
	wantAll(t, "molecule-timeline.js", timeline, "externalLink(d.issue.url, d.issue.key ? TRACKER_URL : ISSUE_URL, 'Issue ' + label + ' ↗')")
	actions := do(t, h, authed("/js/molecule-actions.js")).Body.String()
	wantAll(t, "molecule-actions.js", actions,
		"if (a.kind === 'fresh-intake') { path = '/api/runs'; body = { issue: a.issue }; }",
		"if (a.kind === 'fresh-intake' && freshKey(a)) body = { key: a.issueKey };",
		"TRACKER_URL.test(a.issueUrl)")
	loop := do(t, h, authed("/js/loop.js")).Body.String()
	wantAll(t, "loop.js", loop,
		"'Issue key'", "placeholder: 'ENG-123…'", "name: 'key', maxlength: '128'",
		"const job = await submitDrive('/api/runs', JSON.stringify({ key: drive.runKey }));",
		"api('/api/runs?key=' + encodeURIComponent(drive.runKey))",
		"if (drive.keyed) { startKeyedRun(drive.controls.key.value); return; }",
		"drive.keyed = Boolean(intake && intake.keyed === true);",
		"'Keys match ' + drive.keyPattern",
		"showKeyed(lab.intake);",
		// The numeric form is unchanged.
		"const job = await submitDrive('/api/runs', JSON.stringify({ issue: drive.runIssue }));",
		"el('label', { class: 'field-label', for: 'drive-issue' }, 'Issue number'), issue)")
	if submit := moduleFunction(t, loop, "submitRun"); !strings.Contains(submit, "startRun(drive.controls.issue.value);") {
		t.Errorf("submitRun changed: %s", submit)
	}
	for _, js := range []string{core, timeline, actions, loop} {
		if regexp.MustCompile(`innerHTML|insertAdjacentHTML|outerHTML`).MatchString(js) {
			t.Error("a module renders markup")
		}
	}
}
