package ui

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// keyedLoopHandler is a tracker-plugin Lab that can drive, with the loop
// engine replaced by d and the tracker by fake.
func keyedLoopHandler(t *testing.T, fake *issuesListFake, d *fakeDrive) (http.Handler, string, molecule.Record) {
	t.Helper()
	lab, base := labFixture(t)
	h, _ := driveHandler(t, lab, guardWith(64<<30), d)
	installKeyedListing(h, fake, &keyedRuns{})
	return h, lab, base
}

func TestUIKeyedLoopStartsWithADescribedSelector(t *testing.T) {
	report := molecule.LoopReport{Repository: "o/r", TrackerSelect: "label:bug", Passes: []molecule.LoopPass{{Items: []molecule.LoopItem{
		{Repository: "o/r", Key: "ENG-5", MoleculeID: "mol-eng-5-a", State: molecule.LoopWaiting, Started: true},
		{Repository: "o/r", Key: "bad\nkey", State: molecule.LoopWaiting},
	}}}}
	// The first loop holds until the accepted view is checked, so the view is
	// still running however fast the machine is.
	release := make(chan struct{})
	var first sync.Once
	d := &fakeDrive{loop: func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		first.Do(func() { <-release })
		return report, nil
	}}
	fake := newIssuesListFake(nil)
	fake.selectors = []string{"oldest", "label:", "filter:", "bad\x00", ":"}
	h, _, _ := keyedLoopHandler(t, fake, d)

	v := viewOf[LoopView](t, do(t, h, gatePost("/api/loop/start", `{"select":"label:bug","maxConcurrent":2}`)), http.StatusAccepted)
	if v.State != "running" || v.Select != "label:bug" || !v.Keyed {
		close(release)
		t.Fatalf("accepted keyed loop %+v", v)
	}
	close(release)
	waitLoop(t, h)
	if d.calls() != 1 {
		t.Fatalf("loop engine calls %d", d.calls())
	}
	o := d.loops[0]
	if !o.AllowKeyed || o.TrackerSelect != "label:bug" || o.MaxConcurrent != 2 || o.Select != (intake.Selector{}) || o.Tracker != nil || o.ListIssues != nil || o.Template.LabDir == "" || o.Template.SourceRoot != "/src/checkout" || o.Marker == nil {
		t.Fatalf("keyed loop options %+v", o)
	}
	// Keyed loop items carry their key; a key that is not valid is never served.
	v = viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK)
	if v.Report == nil || len(v.Report.Items) != 2 || v.Report.Items[0].Key != "ENG-5" || v.Report.Items[0].Issue != 0 || v.Report.Items[0].MoleculeID != "mol-eng-5-a" || v.Report.Items[1].Key != "" {
		t.Fatalf("keyed loop report %+v", v.Report)
	}
	// The served selectors are the described ones, cleaned; the default is oldest.
	if !v.Keyed || strings.Join(v.Selectors, ",") != "oldest,label:,filter:" || v.DefaultSelect != "oldest" || v.DefaultSelectNote != keyedOldestNote {
		t.Fatalf("keyed loop view %+v", v)
	}
	// An empty select is oldest.
	viewOf[LoopView](t, do(t, h, gatePost("/api/loop/start", `{"select":"","maxConcurrent":1}`)), http.StatusAccepted)
	waitLoop(t, h)
	if d.calls() != 2 || d.loops[1].TrackerSelect != "oldest" || !d.loops[1].AllowKeyed {
		t.Fatalf("default keyed loop %+v", d.loops[1])
	}
	// The tracker was described once, for the first start.
	if _, described := fake.count(); described != 1 {
		t.Fatalf("described %d times", described)
	}
}

func TestUIKeyedLoopRefusesSelectorsTheTrackerDidNotDescribe(t *testing.T) {
	d := &fakeDrive{loop: func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		return molecule.LoopReport{}, nil
	}}
	fake := newIssuesListFake(nil)
	fake.selectors = []string{"oldest", "label:"}
	h, lab, _ := keyedLoopHandler(t, fake, d)
	before := snapshot(t, lab)
	for _, body := range []string{
		`{"select":"milestone:v1","maxConcurrent":1}`,
		`{"select":"label:","maxConcurrent":1}`,
		`{"select":"labelx","maxConcurrent":1}`,
		`{"select":"label:a\u0000b","maxConcurrent":1}`,
		`{"select":"label:a‮b","maxConcurrent":1}`,
		`{"select":"label:` + strings.Repeat("a", 300) + `","maxConcurrent":1}`,
		`{"select":"oldest","maxConcurrent":0}`,
		`{"select":"oldest","maxConcurrent":9}`,
	} {
		refusalOf(t, do(t, h, gatePost("/api/loop/start", body)), http.StatusBadRequest, "invalid")
	}
	if d.calls() != 0 {
		t.Fatalf("a refused keyed loop reached the engine %d times", d.calls())
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("a refused keyed loop changed the Lab:\n%s\n---\n%s", before, after)
	}

	// A tracker that cannot describe refuses the start; a later start retries.
	fake.lmu.Lock()
	fake.descErr = errors.New("plugin said /Users/someone/secret")
	fake.lmu.Unlock()
	h2, _, _ := keyedLoopHandler(t, fake, d)
	w := do(t, h2, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`))
	refusalOf(t, w, http.StatusConflict, "tracker")
	if strings.Contains(w.Body.String(), "secret") || d.calls() != 0 {
		t.Fatalf("describe failure %s, engine calls %d", w.Body, d.calls())
	}
	fake.lmu.Lock()
	fake.descErr = nil
	fake.lmu.Unlock()
	viewOf[LoopView](t, do(t, h2, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`)), http.StatusAccepted)
	waitLoop(t, h2)
	if d.calls() != 1 {
		t.Fatalf("retried keyed loop calls %d", d.calls())
	}
}

func TestUIKeyedLoopIntakeEntriesShowTheirKeys(t *testing.T) {
	status := &molecule.LoopStatus{Starter: molecule.LoopStarterCLI, StartedAt: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), Selector: "label:bug", MaxConcurrent: 1, Intake: []molecule.LoopIntake{
		{Key: "ENG-6", Phase: molecule.LoopIntakePlanning},
		{Issue: 9, Phase: molecule.LoopIntakePlanning},
		{Key: "bad\nkey", Phase: molecule.LoopIntakePlanning},
		{Key: "ENG-7", Phase: "coding"},
	}}
	v := labLoopView(molecule.LoopMarkerState{Running: true, Held: true, State: "running", Status: status}, "", false)
	if len(v.Intake) != 2 || v.Intake[0].Key != "ENG-6" || v.Intake[0].Issue != 0 || v.Intake[1].Issue != 9 || v.Intake[1].Key != "" || v.Select != "label:bug" {
		t.Fatalf("keyed intake entries %+v", v)
	}
}

// A keyed loop owns the Molecules its Lab status names: one in flight, and one
// started since the loop began for a key whose intake runs. Earlier Molecules of
// those keys, and keys the loop does not name, are someone else's.
func TestUIKeyedLoopPassOwnsOnlyItsMolecules(t *testing.T) {
	ready, release := make(chan error, 1), make(chan struct{})
	var lab string
	var base molecule.Record
	d := &fakeDrive{loop: func(_ context.Context, o molecule.LoopOptions) (molecule.LoopReport, error) {
		o.Marker.Track("mol-eng-5-a")
		o.Marker.TrackIntakeKey("ENG-6")
		err := errors.Join(
			saveRecord(lab, keyedRecord(base, "mol-eng-5-a", "ENG-5", now(), "running", "")),
			saveRecord(lab, keyedRecord(base, "mol-eng-6-new", "ENG-6", now(), "running", "")))
		ready <- err
		<-release
		return molecule.LoopReport{}, nil
	}}
	fake := newIssuesListFake(nil)
	h, l, b := keyedLoopHandler(t, fake, d)
	lab, base = l, b
	writeRecord(t, lab, keyedRecord(base, "mol-eng-5-old", "ENG-5", "2000-01-01T00:00:00Z", "running", ""))
	writeRecord(t, lab, keyedRecord(base, "mol-eng-6-old", "ENG-6", "2000-01-01T00:00:00Z", "running", ""))
	writeRecord(t, lab, keyedRecord(base, "mol-eng-8-cli", "ENG-8", "2000-01-01T00:00:00Z", "running", ""))
	viewOf[LoopView](t, do(t, h, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`)), http.StatusAccepted)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"mol-eng-5-a": RunnerLoop, "mol-eng-6-new": RunnerLoop, "mol-eng-5-old": RunnerOther, "mol-eng-6-old": RunnerOther, "mol-eng-8-cli": RunnerOther} {
		dv := viewOf[Detail](t, do(t, h, authed("/api/molecules/"+id)), http.StatusOK)
		if dv.View == nil || dv.View.Control == nil || dv.View.Control.Runner != want {
			t.Errorf("%s: control %+v, want runner %s", id, dv.View, want)
		}
	}
	// A loop Molecule is never stopped on its own.
	refusalOf(t, do(t, h, gatePost("/api/molecules/mol-eng-5-a/stop", `{}`)), http.StatusConflict, "not-owned")
	// The Lab loop's keyed intake entry shows its key.
	v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK)
	found := false
	for _, in := range v.LabLoop.Intake {
		found = found || in.Key == "ENG-6"
	}
	if !found || v.LabLoop.Owner != LoopOwnerThisUI {
		t.Fatalf("lab loop %+v", v.LabLoop)
	}
	close(release)
	waitLoop(t, h)
	// Once the loop ends it owns nothing.
	if dv := viewOf[Detail](t, do(t, h, authed("/api/molecules/mol-eng-5-a")), http.StatusOK); dv.View.Control.Runner != RunnerOther {
		t.Fatalf("after the loop: %+v", dv.View.Control)
	}
}

// A GitHub Lab's loop start is unchanged: its selector refusal text, and no
// keyed fields in its view.
func TestUIKeyedLoopLeavesGitHubLabsUnchanged(t *testing.T) {
	lab, _ := labFixture(t)
	h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	w := do(t, h, gatePost("/api/loop/start", `{"select":"filter:1","maxConcurrent":1}`))
	refusalOf(t, w, http.StatusBadRequest, "invalid")
	if !strings.Contains(w.Body.String(), "give select as oldest, label:NAME or milestone:TITLE") || d.calls() != 0 {
		t.Fatalf("GitHub refusal %s", w.Body)
	}
	body := do(t, h, authed("/api/loop")).Body.String()
	for _, absent := range []string{`"keyed"`, `"selectors"`, `"key"`} {
		if strings.Contains(body, absent) {
			t.Errorf("GitHub loop view carries %s: %s", absent, body)
		}
	}
	issues := do(t, h, authed("/api/issues")).Body.String()
	for _, absent := range []string{`"keyed"`, `"loading"`, `"key"`} {
		if strings.Contains(issues, absent) {
			t.Errorf("GitHub Issues view carries %s: %s", absent, issues)
		}
	}
}
