package molecule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// keyedLoopFixture is loopFixture with the Formula's intake.source set to the
// declared tracker plugin "jira". The plugin is never started: tests pass a
// fakeKeyed as LoopOptions.Tracker.
func keyedLoopFixture(t *testing.T, scopeAtom string) Options {
	t.Helper()
	o, _ := loopFixture(t, scopeAtom, defaultReviewAtom, "")
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const github = "intake: {source: github-issues, repo: acme/widgets}\n"
	if !strings.Contains(string(raw), github) {
		t.Fatalf("fixture Formula changed:\n%s", raw)
	}
	yaml := strings.Replace(string(raw), github, "intake: {source: jira, repo: acme/widgets}\nplugins:\n  - {name: jira, manifest: plugins/jira/plugin.json, sha256: "+strings.Repeat("a", 64)+"}\n", 1)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, yaml, "keyed-loop")
	o.Issue, o.ReadIssue = "", nil
	return o
}

func openTrackerItem(key string) contract.TrackerItem {
	return contract.TrackerItem{Key: key, Title: "Change code " + key, Labels: []string{}, UpdatedAt: "2026-11-01T10:00:00Z", State: "open"}
}

func onePage(items ...contract.TrackerItem) map[string]contract.TrackerPage {
	return map[string]contract.TrackerPage{"": {Items: items}}
}

func fakeIssues(keys ...string) map[string]contract.TrackerIssue {
	out := map[string]contract.TrackerIssue{}
	for _, key := range keys {
		out[key] = fakeTrackerIssue(key, "open")
	}
	return out
}

func keyedLoopOnce(t *testing.T, o Options, f *fakeKeyed, selector string) LoopReport {
	t.Helper()
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, AllowKeyed: true, Tracker: f, TrackerSelect: selector})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Passes) != 1 {
		t.Fatalf("a once run made %d passes", len(report.Passes))
	}
	return report
}

func keyedLoopItem(t *testing.T, pass LoopPass, key string) LoopItem {
	t.Helper()
	for _, item := range pass.Items {
		if item.Key == key {
			return item
		}
	}
	t.Fatalf("no report for %s: %+v", key, pass.Items)
	return LoopItem{}
}

func keyedRecords(t *testing.T, lab string) map[string][]Record {
	t.Helper()
	out := map[string][]Record{}
	entries, _ := os.ReadDir(filepath.Join(lab, "ledgers"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			r := loadRecord(t, lab, strings.TrimSuffix(e.Name(), ".json"))
			out[r.Issue.Key] = append(out[r.Issue.Key], r)
		}
	}
	return out
}

// keyedMolecule runs keyed intake only, leaving the Molecule at its scope gate.
func keyedMolecule(t *testing.T, o Options, f *fakeKeyed, key string) Record {
	t.Helper()
	o.ReadIssueKey = KeyedReader(f, nil)
	r, err := RunKeyed(context.Background(), o, key)
	if err != nil || r.Readiness == nil || statePhase(r) != "scope-waiting" {
		t.Fatalf("keyed intake did not reach the scope gate: %v %+v", err, r)
	}
	return r
}

// TestKeyedLoopStartsSkipsAndApprovesByPolicy: an eligible key is started
// under its start lock and approved by policy; a key with an active Molecule
// is skipped, a key whose start lock is held is reported locked, and a closed
// item is not listed.
func TestKeyedLoopStartsSkipsAndApprovesByPolicy(t *testing.T) {
	t.Parallel()
	o := keyedLoopFixture(t, policyScopeAtom)
	closed := openTrackerItem("ENG-3")
	closed.State = "closed"
	f := &fakeKeyed{pattern: "^ENG-[0-9]+$", issues: fakeIssues("ENG-1", "ENG-2", "ENG-3", "ENG-4"),
		pages: onePage(openTrackerItem("ENG-1"), openTrackerItem("ENG-2"), closed, openTrackerItem("ENG-4"))}
	active := keyedMolecule(t, o, f, "ENG-2")
	active.Status = "running"
	if err := save(active); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockIssueKeyAs(o.LabDir, "acme/widgets", "ENG-4", issueOwnerRun)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// The first Get is intake's: the loop holds ENG-1's start lock meanwhile.
	var lockedDuringIntake []bool
	var mu sync.Mutex
	probe := &probingKeyed{fakeKeyed: f, onGet: func(key string) {
		if key != "ENG-1" {
			return
		}
		release, err := lockIssueKeyAs(o.LabDir, "acme/widgets", key, "probe")
		if err == nil {
			release()
		}
		mu.Lock()
		lockedDuringIntake = append(lockedDuringIntake, errors.Is(err, errLocked))
		mu.Unlock()
	}}
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, AllowKeyed: true, Tracker: probe})
	if err != nil {
		t.Fatal(err)
	}
	pass := report.Passes[0]
	if report.Repository != "acme/widgets" || report.TrackerSelect != "oldest" || len(pass.Items) != 3 {
		t.Fatalf("report %+v", report)
	}
	started := keyedLoopItem(t, pass, "ENG-1")
	if !started.Started || !started.Approved || started.Failed || started.Issue != 0 || started.MoleculeID == "" || !strings.HasPrefix(started.MoleculeID, "mol-eng-1-") {
		t.Fatalf("ENG-1: %+v", started)
	}
	if len(lockedDuringIntake) != 2 || !lockedDuringIntake[0] || lockedDuringIntake[1] {
		t.Fatalf("start lock held during intake, then released before approval: %v", lockedDuringIntake)
	}
	r := loadRecord(t, o.LabDir, started.MoleculeID)
	if r.Issue.Key != "ENG-1" || r.Issue.Number != 0 || len(r.PolicyDecisions) != 1 || len(r.HarnessAttempts) != 1 {
		t.Fatalf("ENG-1 record: issue %+v decisions %d attempts %d notes %q", r.Issue, len(r.PolicyDecisions), len(r.HarnessAttempts), r.Notes)
	}
	skipped := keyedLoopItem(t, pass, "ENG-2")
	if skipped.State != LoopActive || skipped.Started || skipped.Approved || skipped.MoleculeID != active.ID {
		t.Fatalf("ENG-2: %+v", skipped)
	}
	locked := keyedLoopItem(t, pass, "ENG-4")
	if locked.State != LoopLocked || locked.Started || !strings.Contains(locked.Reason, "a forgecell run") {
		t.Fatalf("ENG-4: %+v", locked)
	}
	records := keyedRecords(t, o.LabDir)
	if len(records["ENG-1"]) != 1 || len(records["ENG-2"]) != 1 || len(records["ENG-3"]) != 0 || len(records["ENG-4"]) != 0 {
		t.Fatalf("records: %v", records)
	}
	if got := f.listCalls(); len(got) != 1 || got[0] != "oldest|" {
		t.Fatalf("list calls = %q", got)
	}
	// A second pass starts nothing new for the started key.
	again := keyedLoopOnce(t, o, f, "")
	if item := keyedLoopItem(t, again.Passes[0], "ENG-1"); item.Started || item.MoleculeID != started.MoleculeID {
		t.Fatalf("second pass: %+v", item)
	}
	if n := len(keyedRecords(t, o.LabDir)["ENG-1"]); n != 1 {
		t.Fatalf("ENG-1 Molecules after a second pass: %d", n)
	}
}

// TestKeyedLoopIsOptIn: without AllowKeyed a tracker-plugin Formula is refused
// as an unsupported intake source, as before, and the Tracker is never used;
// with it the same Lab runs keyed.
func TestKeyedLoopIsOptIn(t *testing.T) {
	t.Parallel()
	o := keyedLoopFixture(t, defaultScopeAtom)
	f := &fakeKeyed{pattern: "^ENG-[0-9]+$", issues: fakeIssues("ENG-1"), pages: onePage(openTrackerItem("ENG-1"))}
	for name, lo := range map[string]LoopOptions{
		"no tracker": {Template: o, MaxConcurrent: 1, Once: true},
		"tracker":    {Template: o, MaxConcurrent: 1, Once: true, Tracker: f, TrackerSelect: "filter:123"},
	} {
		report, err := RunLoop(context.Background(), lo)
		if err == nil || err.Error() != "unsupported intake source" || len(report.Passes) != 0 || report.TrackerSelect != "" {
			t.Fatalf("%s: report %+v err %v", name, report, err)
		}
	}
	if got := f.listCalls(); len(got) != 0 || len(keyedRecords(t, o.LabDir)) != 0 {
		t.Fatalf("a refused loop listed %q or wrote a ledger", got)
	}
	report := keyedLoopOnce(t, o, f, "")
	if item := keyedLoopItem(t, report.Passes[0], "ENG-1"); !item.Started {
		t.Fatalf("with AllowKeyed: %+v", item)
	}
}

// probingKeyed calls onGet before each Get.
type probingKeyed struct {
	*fakeKeyed
	onGet func(string)
}

func (p *probingKeyed) Get(ctx context.Context, key string) (contract.TrackerIssue, error) {
	p.onGet(key)
	return p.fakeKeyed.Get(ctx, key)
}

// TestKeyedLoopPassesTheSelectorRawAndWaitsAtAHumanGate: TrackerSelect reaches
// List verbatim and the loop status; a human scope gate is never approved.
func TestKeyedLoopPassesTheSelectorRawAndWaitsAtAHumanGate(t *testing.T) {
	t.Parallel()
	o := keyedLoopFixture(t, defaultScopeAtom)
	f := &fakeKeyed{pattern: "^ENG-[0-9]+$", issues: fakeIssues("ENG-1"), pages: onePage(openTrackerItem("ENG-1"), openTrackerItem("ENG-1"))}
	marker := NewLoopMarker(LoopStarterCLI, 0)
	defer marker.Release()
	var status []LoopIntake
	var selector string
	probe := &probingKeyed{fakeKeyed: f, onGet: func(string) {
		if s := ReadLoopStatus(o.LabDir); s.Running && status == nil {
			status, selector = s.Status.Intake, s.Status.Selector
		}
	}}
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, AllowKeyed: true, Tracker: probe, TrackerSelect: "filter:123", Marker: marker})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.listCalls(); len(got) != 1 || got[0] != "filter:123|" {
		t.Fatalf("list calls = %q", got)
	}
	if report.TrackerSelect != "filter:123" || len(report.Passes[0].Items) != 1 {
		t.Fatalf("report %+v", report)
	}
	item := keyedLoopItem(t, report.Passes[0], "ENG-1")
	if !item.Started || item.Approved || item.State != LoopWaiting || item.Failed {
		t.Fatalf("%+v", item)
	}
	if selector != "filter:123" || len(status) != 1 || status[0] != (LoopIntake{Key: "ENG-1", Phase: LoopIntakePlanning}) {
		t.Fatalf("loop status during intake: selector %q intake %+v", selector, status)
	}
	if s := ReadLoopStatus(o.LabDir); !s.Running || len(s.Status.Intake) != 0 {
		t.Fatalf("loop status after the pass: %+v", s)
	}
	raw, err := json.Marshal(item)
	if err != nil || !strings.Contains(string(raw), `"Issue":0,"Key":"ENG-1","MoleculeID":`) {
		t.Fatalf("LoopItem JSON: %s %v", raw, err)
	}
}

// TestKeyedLoopTruncatedListingUnderASelectorStartsNothing mirrors the
// numeric rule: five full pages with more left are a truncated listing, which a
// non-oldest selector refuses before anything starts; under oldest it is only
// a warning.
func TestKeyedLoopTruncatedListingUnderASelectorStartsNothing(t *testing.T) {
	t.Parallel()
	o := keyedLoopFixture(t, defaultScopeAtom)
	pages := map[string]contract.TrackerPage{}
	for i := range 6 {
		cursor := ""
		if i > 0 {
			cursor = fmt.Sprintf("c%d", i)
		}
		next := fmt.Sprintf("c%d", i+1)
		item := openTrackerItem(fmt.Sprintf("ENG-%d", i+1))
		item.State = "closed"
		pages[cursor] = contract.TrackerPage{Items: []contract.TrackerItem{item}, NextCursor: &next}
	}
	f := &fakeKeyed{pattern: "^ENG-[0-9]+$", pages: pages}
	_, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, AllowKeyed: true, Tracker: f, TrackerSelect: "filter:123"})
	if err == nil || IsTransientLoopError(err) || !strings.Contains(err.Error(), "truncated after 0 Issues, so filter:123 cannot be applied") || !strings.Contains(err.Error(), "nothing was started or advanced") {
		t.Fatalf("err = %v", err)
	}
	want := []string{"filter:123|", "filter:123|c1", "filter:123|c2", "filter:123|c3", "filter:123|c4"}
	if got := f.listCalls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("list calls = %q", got)
	}
	if len(keyedRecords(t, o.LabDir)) != 0 {
		t.Fatal("a truncated listing under a selector wrote a ledger")
	}
	report := keyedLoopOnce(t, o, &fakeKeyed{pattern: "^ENG-[0-9]+$", pages: pages}, "")
	if notes := report.Passes[0].Notes; len(notes) != 1 || notes[0] != "open Issue listing truncated after 0 Issues; later Issues were not considered" {
		t.Fatalf("notes = %q", notes)
	}
}

// TestKeyedLoopRefusesAHostileListing: a cursor that does not advance fails the
// pass as transient; invalid keys are left out with a note.
func TestKeyedLoopRefusesAHostileListing(t *testing.T) {
	t.Parallel()
	o := keyedLoopFixture(t, defaultScopeAtom)
	same := "a"
	loop := &fakeKeyed{pattern: "^ENG-[0-9]+$", pages: map[string]contract.TrackerPage{
		"":  {Items: []contract.TrackerItem{openTrackerItem("ENG-1")}, NextCursor: &same},
		"a": {Items: []contract.TrackerItem{openTrackerItem("ENG-1")}, NextCursor: &same},
	}}
	if _, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, AllowKeyed: true, Tracker: loop}); !IsTransientLoopError(err) || !strings.Contains(err.Error(), "does not advance") {
		t.Fatalf("err = %v", err)
	}
	if len(keyedRecords(t, o.LabDir)) != 0 {
		t.Fatal("a refused listing wrote a ledger")
	}
	invalid := &fakeKeyed{pattern: "^ENG-[0-9]+$", pages: onePage(openTrackerItem("ENG-\x1b[2J"), openTrackerItem("---"), openTrackerItem(""))}
	report := keyedLoopOnce(t, o, invalid, "")
	if pass := report.Passes[0]; len(pass.Items) != 0 || len(pass.Notes) != 1 || !strings.Contains(pass.Notes[0], "listed 3 open Issue(s) whose key is not a valid Issue key") {
		t.Fatalf("pass = %+v", pass)
	}
	// A failing listing is transient and starts nothing.
	failing := &fakeKeyed{pattern: "^ENG-[0-9]+$"}
	failing.pages = nil
	if _, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, AllowKeyed: true, Tracker: &erroringList{failing}}); !IsTransientLoopError(err) {
		t.Fatalf("err = %v", err)
	}
}

type erroringList struct{ *fakeKeyed }

func (e *erroringList) List(context.Context, string, string, int) (contract.TrackerPage, error) {
	return contract.TrackerPage{}, errors.New("plugin jira tracker_list: failed")
}

// TestKeyedLoopReportsACrashedKeyedIntakeByItsKeyLock: a keyed ledger that never
// reached a plan is stalled only while no loop holds the key's start lock.
func TestKeyedLoopReportsACrashedKeyedIntakeByItsKeyLock(t *testing.T) {
	t.Parallel()
	o := keyedLoopFixture(t, defaultScopeAtom)
	f := &fakeKeyed{pattern: "^ENG-[0-9]+$", issues: fakeIssues("ENG-1"), pages: onePage(openTrackerItem("ENG-1"))}
	r := keyedMolecule(t, o, f, "ENG-1")
	r.Status, r.Readiness, r.Analysis = "running", nil, nil
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(filepath.Join(o.LabDir, "ledgers", r.ID+".json"), old, old); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockIssueKeyAs(o.LabDir, "acme/widgets", "ENG-1", issueOwnerLoop)
	if err != nil {
		t.Fatal(err)
	}
	if item := keyedLoopItem(t, keyedLoopOnce(t, o, f, "").Passes[0], "ENG-1"); item.State != LoopActive || item.MoleculeID != r.ID {
		t.Fatalf("with the key lock held: %+v", item)
	}
	unlock()
	item := keyedLoopItem(t, keyedLoopOnce(t, o, f, "").Passes[0], "ENG-1")
	if item.State != LoopStalled || item.Started || !strings.Contains(item.Reason, "never reached the scope gate") {
		t.Fatalf("without the key lock: %+v", item)
	}
}

// TestKeyedClarificationHintNamesTheKey: a keyed record's next-run hint is the
// shell-quoted key, never run 0; the numeric hint is unchanged.
func TestKeyedClarificationHintNamesTheKey(t *testing.T) {
	t.Parallel()
	questions := &readiness.Analysis{Questions: []readiness.Question{{Question: "Which parser?", Reason: "Two entrypoints", Evidence: []string{"issue"}}}}
	_, reason, stopped := classify(Record{ID: "mol-eng-7-a", Status: "blocked", Issue: Issue{Key: "ENG 7's", Repo: "acme/widgets"}, Analysis: questions})
	if stopped || reason != `clarification required: the analysis has 1 open question(s); update the Issue body, then run forgecell run 'ENG 7'\''s'` {
		t.Fatalf("keyed: %q %t", reason, stopped)
	}
	_, reason, _ = classify(Record{ID: "mol-12-a", Status: "blocked", Issue: Issue{Number: 12, Repo: "acme/widgets"}, Analysis: questions})
	if reason != "clarification required: the analysis has 1 open question(s); update the Issue body, then run forgecell run 12" {
		t.Fatalf("numeric: %q", reason)
	}
	_, reason, _ = classify(Record{ID: "mol-eng-7-b", Status: "blocked", Issue: Issue{Key: "ENG-7\x1b[2J", Repo: "acme/widgets"}, Analysis: questions})
	if strings.Contains(reason, "\x1b") || strings.Contains(reason, "run 0") || !strings.Contains(reason, "not a valid tracker key") {
		t.Fatalf("invalid key: %q", reason)
	}
}

// TestLoopStatusListsAndValidatesKeyedIntake: a keyed intake is listed by Key,
// the validator admits exactly one of Issue or Key, and a numeric-only status
// keeps its bytes.
func TestLoopStatusListsAndValidatesKeyedIntake(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	m := NewLoopMarker(LoopStarterCLI, 0)
	if err := m.Acquire(lab, intake.Selector{}, 1); err != nil {
		t.Fatal(err)
	}
	defer m.Release()
	m.TrackIntake(12)
	raw, err := os.ReadFile(filepath.Join(lab, loopStatusFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"intake":[{"issue":12,"phase":"planning"}]`) || strings.Contains(string(raw), `"key"`) {
		t.Fatalf("numeric status: %s", raw)
	}
	m.TrackIntakeKey("ENG-1")
	m.TrackIntakeKey("bad\x00key")
	s := ReadLoopStatus(lab)
	if !s.Running || len(s.Status.Intake) != 2 || s.Status.Intake[0] != (LoopIntake{Issue: 12, Phase: LoopIntakePlanning}) || s.Status.Intake[1] != (LoopIntake{Key: "ENG-1", Phase: LoopIntakePlanning}) {
		t.Fatalf("status: %+v", s)
	}
	m.UntrackIntakeKey("ENG-1")
	m.UntrackIntake(12)
	if s := ReadLoopStatus(lab); len(s.Status.Intake) != 0 {
		t.Fatalf("after untrack: %+v", s.Status.Intake)
	}
	base := loopStatusRecord{SchemaVersion: loopStatusSchema, Token: strings.Repeat("a", 32), LoopStatus: LoopStatus{Starter: LoopStarterCLI, StartedAt: time.Now(), Selector: "oldest", MaxConcurrent: 1, InFlight: []string{}}}
	for name, c := range map[string]struct {
		in    LoopIntake
		valid bool
	}{
		"number":      {LoopIntake{Issue: 12, Phase: LoopIntakePlanning}, true},
		"key":         {LoopIntake{Key: "ENG-1", Phase: LoopIntakePlanning}, true},
		"both":        {LoopIntake{Issue: 12, Key: "ENG-1", Phase: LoopIntakePlanning}, false},
		"neither":     {LoopIntake{Phase: LoopIntakePlanning}, false},
		"invalid key": {LoopIntake{Key: "ENG\n1", Phase: LoopIntakePlanning}, false},
		"no slug":     {LoopIntake{Key: "---", Phase: LoopIntakePlanning}, false},
		"phase":       {LoopIntake{Key: "ENG-1", Phase: "coding"}, false},
	} {
		rec := base
		rec.Intake = []LoopIntake{c.in}
		if err := rec.validate(); (err == nil) != c.valid {
			t.Errorf("%s: validate = %v", name, err)
		}
	}
}

// escapingKey is a valid key of MaxIssueKeyRunes runes whose every character
// but its prefix JSON-escapes to six bytes.
func escapingKey(i int) string {
	prefix := fmt.Sprintf("E%d-", i)
	return prefix + strings.Repeat("<>&", intake.MaxIssueKeyRunes)[:intake.MaxIssueKeyRunes-len(prefix)]
}

// TestLoopStatusBoundsKeyedIntakeByItsJSON: keys that JSON-escape are bounded
// by their marshalled size, so the status file stays readable and under
// maxLoopStatusBytes, beside a full in-flight list too, and the overflow is
// marked truncated.
func TestLoopStatusBoundsKeyedIntakeByItsJSON(t *testing.T) {
	t.Parallel()
	for name, ids := range map[string]int{"keys only": 0, "full in-flight list": MaxLoopStatusInFlight} {
		lab := t.TempDir()
		m := NewLoopMarker(LoopStarterCLI, 0)
		if err := m.Acquire(lab, intake.Selector{}, 1); err != nil {
			t.Fatal(err)
		}
		for i := range ids {
			m.Track(fmt.Sprintf("mol-%02d-", i) + strings.Repeat("a", 192))
		}
		for i := range MaxLoopStatusInFlight {
			key := escapingKey(i)
			if !validLoopKey(key) || loopIntakeKeyBytes(key) < 6*(intake.MaxIssueKeyRunes-4) {
				t.Fatalf("fixture key %q is not a valid escaping key", key)
			}
			m.TrackIntakeKey(key)
		}
		info, err := os.Stat(filepath.Join(lab, loopStatusFile))
		if err != nil || info.Size() > maxLoopStatusBytes {
			t.Fatalf("%s: status file %v %v", name, info, err)
		}
		s := ReadLoopStatus(lab)
		if !s.Running || !s.Status.InFlightTruncated || len(s.Status.InFlight) != ids || len(s.Status.Intake) == 0 || len(s.Status.Intake) >= MaxLoopStatusInFlight {
			t.Fatalf("%s: status running %t truncated %t in flight %d intake %d", name, s.Running, s.Status != nil && s.Status.InFlightTruncated, len(s.Status.InFlight), len(s.Status.Intake))
		}
		if err := m.takeStatusWriteError(); err != nil {
			t.Fatalf("%s: a bounded status write was refused: %v", name, err)
		}
		m.Release()
	}
	// The validator bounds the same marshalled size, so an over-long hand-made
	// status of escaping keys is refused even under the entry count.
	rec := loopStatusRecord{SchemaVersion: loopStatusSchema, Token: strings.Repeat("a", 32), LoopStatus: LoopStatus{Starter: LoopStarterCLI, StartedAt: time.Now(), Selector: "oldest", MaxConcurrent: 1, InFlight: []string{}}}
	for i := range maxLoopStatusKeyBytes/loopIntakeKeyBytes(escapingKey(0)) + 2 {
		rec.Intake = append(rec.Intake, LoopIntake{Key: escapingKey(i), Phase: LoopIntakePlanning})
	}
	if len(rec.Intake) > MaxLoopStatusInFlight {
		t.Fatalf("fixture has %d entries", len(rec.Intake))
	}
	if err := rec.validate(); err == nil {
		t.Fatal("the validator admitted keyed entries over maxLoopStatusKeyBytes")
	}
}

// TestKeyedLoopReportsARefusedStatusWrite: a status write that fails is kept
// and reported as a pass note without the Lab's path, once.
func TestKeyedLoopReportsARefusedStatusWrite(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	lab := t.TempDir()
	m := NewLoopMarker(LoopStarterCLI, 0)
	if err := m.Acquire(lab, intake.Selector{}, 1); err != nil {
		t.Fatal(err)
	}
	defer m.Release()
	before, err := os.ReadFile(filepath.Join(lab, loopStatusFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lab, 0o500); err != nil {
		t.Fatal(err)
	}
	m.TrackIntakeKey("ENG-1")
	if err := os.Chmod(lab, 0o700); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(filepath.Join(lab, loopStatusFile)); err != nil || string(after) != string(before) {
		t.Fatalf("a failed write changed the status: %s %v", after, err)
	}
	l := &loopRun{marker: m}
	var pass LoopPass
	l.noteStatusWrite(&pass)
	if len(pass.Notes) != 1 || !strings.HasPrefix(pass.Notes[0], "the loop status could not be written, so forgecell status may show an earlier status: ") || strings.Contains(pass.Notes[0], lab) {
		t.Fatalf("notes = %q", pass.Notes)
	}
	// The next write succeeds, and the reported failure is not reported again.
	m.UntrackIntakeKey("ENG-1")
	pass = LoopPass{}
	l.noteStatusWrite(&pass)
	if len(pass.Notes) != 0 {
		t.Fatalf("notes after a good write = %q", pass.Notes)
	}
}
