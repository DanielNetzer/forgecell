package ui

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// Each owner of a running Lab loop is named in plain words with its selector,
// and only this UI's own loop says nothing about stopping it elsewhere. A
// marker that is not running names no owner.
func TestUILoopOwnerTextNamesEachOwner(t *testing.T) {
	started := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	status := func(starter, token string) molecule.LoopMarkerState {
		return molecule.LoopMarkerState{Running: true, State: molecule.LoopMarkerRunning, Held: true, Status: &molecule.LoopStatus{Token: token, Starter: starter, StartedAt: started, Selector: "milestone:v1", MaxConcurrent: 2, InFlight: []string{"mol-7-a"}, Intake: []molecule.LoopIntake{{Issue: 243, Phase: molecule.LoopIntakePlanning}}}}
	}
	for name, c := range map[string]struct {
		lab     molecule.LoopMarkerState
		own     string
		text    string
		canStop bool
	}{
		"this UI":     {status(molecule.LoopStarterUI, "tok"), "tok", "Started from this Lab UI with milestone:v1", true},
		"another UI":  {status(molecule.LoopStarterUI, "other"), "tok", "Started from another Lab UI with milestone:v1 · stop it there", false},
		"terminal":    {status(molecule.LoopStarterCLI, "cli"), "", "Started from the command line with milestone:v1 · stop it there", false},
		"not running": {molecule.LoopMarkerState{State: molecule.LoopMarkerNotRunning}, "", "", false},
	} {
		v := labLoopView(c.lab, c.own, true)
		if v.OwnerText != c.text || v.CanStop != c.canStop {
			t.Errorf("%s: owner text %q can stop %t", name, v.OwnerText, v.CanStop)
		}
		if c.lab.Running && (v.StartedAt != "2026-10-27T09:00:00Z" || v.MaxConcurrent != 2 || len(v.InFlight) != 1 || len(v.Intake) != 1 || v.Intake[0].Issue != 243 || v.Intake[0].Phase != "planning") {
			t.Errorf("%s: %+v", name, v)
		}
	}
	// A loop already stopping cannot be stopped again.
	if v := labLoopView(status(molecule.LoopStarterUI, "tok"), "tok", false); v.CanStop {
		t.Error("a draining loop is stoppable")
	}
}

// GET /api/loop names a terminal loop's owner and where to stop it, carries
// the Start loop form's default selector and its explanation, and a start
// beside it is refused with the engine's plain reason.
func TestUILoopStatusNamesATerminalLoopAndTheDefaultSelector(t *testing.T) {
	lab, _ := labFixture(t)
	h, d := driveHandler(t, lab, nil, &fakeDrive{})
	idle := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK)
	// No Issue state was read, so the active milestone is unknown: no selector
	// is guessed, oldest least of all.
	if idle.DefaultSelect != "" || idle.DefaultSelectNote != unknownNote || !strings.Contains(idle.DefaultSelectNote, "oldest open Issue, any milestone") || idle.LabLoop.OwnerText != "" {
		t.Fatalf("idle %+v", idle)
	}
	selector, err := intake.ParseSelector("milestone:v1")
	if err != nil {
		t.Fatal(err)
	}
	cli := molecule.NewLoopMarker(molecule.LoopStarterCLI, 0)
	if err = cli.Acquire(lab, selector, 2); err != nil {
		t.Fatal(err)
	}
	defer cli.Release()
	cli.TrackIntake(243)
	v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK)
	if v.LabLoop.OwnerText != "Started from the command line with milestone:v1 · stop it there" || v.LabLoop.CanStop || len(v.LabLoop.Intake) != 1 || v.LabLoop.Intake[0].Issue != 243 {
		t.Fatalf("terminal loop %+v", v.LabLoop)
	}
	w := do(t, h, gatePost("/api/loop/start", `{"select":"oldest","maxConcurrent":1}`))
	refusalOf(t, w, http.StatusConflict, "busy")
	if !strings.Contains(w.Body.String(), "started in a terminal") || d.calls() != 0 {
		t.Fatalf("refusal %s after %d engine calls", w.Body, d.calls())
	}
}

// The default selector is milestone:<active milestone>: the milestone with the
// most open Issues in the sync's open-Issue listing, ties to the one with the
// lowest open Issue number. It is oldest only when the listing names no
// milestone, and "" (unknown, nothing guessed) without a complete listing.
func TestUILoopDefaultSelectorIsTheActiveMilestone(t *testing.T) {
	listing := func(truncated bool, milestones map[int64]string) *forgesync.Snapshot {
		s := &forgesync.Snapshot{Listing: forgesync.Listing{Truncated: truncated}}
		for n := int64(1); n <= 12; n++ {
			i := openSyncIssue(n)
			i.Milestone = milestones[n]
			s.Listing.Issues = append(s.Listing.Issues, i)
		}
		return s
	}
	for name, c := range map[string]struct {
		snap         *forgesync.Snapshot
		select_, has string
	}{
		"unknown":        {nil, "", "The active milestone is unknown"},
		"truncated":      {listing(true, map[int64]string{1: "v1"}), "", "The active milestone is unknown"},
		"no milestone":   {listing(false, nil), "oldest", "no open milestone"},
		"most open":      {listing(false, map[int64]string{1: "v2", 4: "v1", 5: "v1", 6: "v1", 7: "v2"}), "milestone:v1", "Milestone v1: the open milestone with the most open Issues (3)."},
		"tie":            {listing(false, map[int64]string{2: "v1", 9: "v1", 3: "v2", 10: "v2"}), "milestone:v1", "(2)"},
		"tie other way":  {listing(false, map[int64]string{8: "v1", 9: "v1", 3: "v2", 10: "v2"}), "milestone:v2", "(2)"},
		"unusable title": {listing(false, map[int64]string{1: "bad\ntitle", 2: "bad\ntitle", 3: "v3"}), "milestone:v3", "(1)"},
	} {
		got, note := activeMilestoneSelect(c.snap)
		if got != c.select_ || !strings.Contains(note, c.has) || !strings.Contains(note, "Oldest: oldest open Issue, any milestone.") {
			t.Errorf("%s: %q %q", name, got, note)
		}
	}

	// Served: /api/loop reads only the sync the Issues view already made, while
	// it is current; it never reads GitHub itself.
	f := newSyncFetcher()
	for i := range f.listing.Issues {
		if f.listing.Issues[i].Number <= 3 {
			f.listing.Issues[i].Milestone = "v1"
		}
	}
	h, clk := syncHandler(t, Options{LabDir: syncLab(t)}, f)
	if v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK); v.DefaultSelect != "" || f.calls() != 0 {
		t.Fatalf("before any sync: %q after %d GitHub reads", v.DefaultSelect, f.calls())
	}
	issueRows(t, h, "/api/issues")
	calls := f.calls()
	if v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK); v.DefaultSelect != "milestone:v1" || !strings.Contains(v.DefaultSelectNote, "(3)") || f.calls() != calls {
		t.Fatalf("after the sync: %q %q", v.DefaultSelect, v.DefaultSelectNote)
	}
	clk.add(3 * forgesync.TTL)
	if v := viewOf[LoopView](t, do(t, h, authed("/api/loop")), http.StatusOK); v.DefaultSelect != "" || f.calls() != calls {
		t.Fatalf("stale sync: %q", v.DefaultSelect)
	}
}

// The served loop module reads any Lab loop: the pill counts its in-flight
// Molecules and planning intakes, offers Stop only when canStop and otherwise
// names the owner; the panel renders "issue #N · planning", the owner text,
// the default selector's explanation and the engine's refusal text.
func TestUIServedJSLoopStatus(t *testing.T) {
	js := servedJS(t)
	for _, want := range []string{
		"const lab = (loop && loop.labLoop) || {};",
		"const phase = own !== 'idle' ? own : lab.running === true ? 'running' : 'idle';",
		"const canStop = phase === 'running' && lab.canStop === true;",
		"} else if (canStop) stop.hidden = false;",
		"const owner = lab.running === true && typeof lab.ownerText === 'string' ? lab.ownerText : '';",
		"const elsewhere = phase === 'running' && !canStop && owner ? ' · ' + owner : '';",
		"+ inFlight + ' in flight') + elsewhere);",
		"'issue #' + notRecorded(i.issue) + ' · planning'",
		"el('strong', null, notRecorded(lab.ownerText)",
		"if (loop.state === 'idle') return labLoopText(loop.labLoop);",
		"'Oldest: oldest open Issue, any milestone.'",
		"setText(c.selectNote, loop.defaultSelectNote)",
		"reasonText(loop)",
		// The server's default selector fills the form until the operator chooses.
		"mode: 'milestone', modeChosen: false, defaultSelect: null,",
		"applyDefaultSelect(loop);",
		"if (!c || drive.modeChosen || !loop || typeof loop.defaultSelect !== 'string' || loop.defaultSelect === drive.defaultSelect) return;",
		"if (loop.defaultSelect === 'oldest') { showMode('oldest'); return; }",
		"showMode('milestone');\n  c.value.value = title;",
		"oninput: () => { drive.modeChosen = true; }",
		"' not attempted (time bound reached)'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("loop.js lacks %q", want)
		}
	}
	if choose := moduleFunction(t, js, "chooseMode"); !strings.Contains(choose, "drive.modeChosen = true;") {
		t.Error("choosing a mode does not keep the operator's choice")
	}
	pill := moduleFunction(t, js, "renderLoopPill")
	if strings.Contains(pill, "phase === 'running') stop.hidden = false") {
		t.Error("the pill offers Stop for any running loop, not only when canStop")
	}
}
