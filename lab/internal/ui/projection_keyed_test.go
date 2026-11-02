package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

const githubDiagnostic = "recorded Issue URL does not name this Issue on GitHub; no link is shown"

// keyedFixture is labFixture's Molecule as a keyed one, beside the numeric one.
func keyedFixture(t *testing.T) (string, molecule.Record, molecule.Record) {
	t.Helper()
	lab, numeric := labFixture(t)
	keyed := numeric
	keyed.ID = "mol-eng-7-abc"
	keyed.Issue.Key, keyed.Issue.Number, keyed.Issue.URL = "ENG-7", 0, "https://tracker.example/browse/ENG-7"
	writeRecord(t, lab, keyed)
	return lab, numeric, keyed
}

// issueObject is the raw issue object of a served Detail.
func issueObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var raw struct {
		Issue map[string]any `json:"issue"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	return raw.Issue
}

func hasDiagnostic(d Detail, part string) bool {
	for _, s := range d.Diagnostics {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

func TestUIKeyedProjectionShowsTheKeyAndTheTrackerURL(t *testing.T) {
	lab, numeric, keyed := keyedFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	w := do(t, h, authed("/api/molecules/"+keyed.ID))
	d := viewOf[Detail](t, w, http.StatusOK)
	if d.Issue.Key != "ENG-7" || d.Issue.Number != 0 || d.Issue.URL != "https://tracker.example/browse/ENG-7" || d.Issue.Repository != "o/r" {
		t.Fatalf("keyed issue %+v", d.Issue)
	}
	if hasDiagnostic(d, "on GitHub") || hasDiagnostic(d, "tracker") {
		t.Fatalf("keyed diagnostics %q", d.Diagnostics)
	}
	// A keyed URL that is not a plain https link is not linked; the diagnostic
	// names the tracker, never GitHub.
	for _, bad := range []string{"http://tracker.example/browse/ENG-7", "https://user@tracker.example/browse/ENG-7", "https://tracker.example/browse/ENG 7", "javascript:alert(1)", "https://", "https://tracker.example/" + strings.Repeat("a", maxKeyedURLBytes)} {
		r := keyed
		r.Issue.URL = bad
		writeRecord(t, lab, r)
		d := viewOf[Detail](t, do(t, h, authed("/api/molecules/"+keyed.ID)), http.StatusOK)
		if d.Issue.URL != "" || d.Issue.Key != "ENG-7" || !hasDiagnostic(d, "not an https link to the tracker") || hasDiagnostic(d, "on GitHub") {
			t.Errorf("%q: issue %+v, diagnostics %q", bad, d.Issue, d.Diagnostics)
		}
	}
	// The list keeps the key as display text.
	list := viewOf[MoleculeList](t, do(t, h, authed("/api/molecules")), http.StatusOK)
	found := false
	for _, m := range list.Molecules {
		if m.ID == keyed.ID {
			found = m.Key == "ENG-7" && m.Number == 0
		}
	}
	if !found {
		t.Fatalf("the list does not show the keyed Molecule by its key: %+v", list.Molecules)
	}

	// A numeric record is unchanged: no key, and the GitHub diagnostic for a URL
	// that names another Issue.
	w = do(t, h, authed("/api/molecules/"+numeric.ID))
	if d := viewOf[Detail](t, w, http.StatusOK); d.Issue.Number != 7 || d.Issue.URL != "https://github.com/o/r/issues/7" || len(d.Diagnostics) != 0 {
		t.Fatalf("numeric issue %+v %q", d.Issue, d.Diagnostics)
	}
	if _, has := issueObject(t, w.Body.Bytes())["key"]; has {
		t.Fatal("a numeric Issue serves a key")
	}
	numeric.Issue.URL = "https://github.com/o/r/issues/8"
	writeRecord(t, lab, numeric)
	if d := viewOf[Detail](t, do(t, h, authed("/api/molecules/"+numeric.ID)), http.StatusOK); d.Issue.URL != "" || !hasDiagnostic(d, githubDiagnostic) {
		t.Fatalf("numeric diagnostics %+v %q", d.Issue, d.Diagnostics)
	}
}

func TestUIKeyedFreshIntakeActionPostsTheKey(t *testing.T) {
	lab, numeric, keyed := keyedFixture(t)
	h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	hh := h.(*handler)
	hh.deriveActions = func(context.Context, string, molecule.Record, bool, time.Duration) []molecule.NextAction {
		return []molecule.NextAction{{Kind: molecule.NextFreshIntake, Questions: []string{"Which endpoint?"}}}
	}
	fresh := func(id string) (NextActionView, []byte, ActionsView) {
		t.Helper()
		w := do(t, h, authed("/api/molecules/"+id+"/actions"))
		v := viewOf[ActionsView](t, w, http.StatusOK)
		for _, a := range v.Actions {
			if a.Kind == ActionFreshIntake {
				return a, w.Body.Bytes(), v
			}
		}
		return NextActionView{}, w.Body.Bytes(), v
	}
	a, _, _ := fresh(keyed.ID)
	if a.IssueKey != "ENG-7" || a.Issue != 0 || a.IssueURL != "https://tracker.example/browse/ENG-7" || len(a.Questions) != 1 {
		t.Fatalf("keyed fresh intake %+v", a)
	}
	// The numeric action serializes as before: no issueKey.
	n, body, _ := fresh(numeric.ID)
	if n.IssueKey != "" || n.Issue != 7 || n.IssueURL != "https://github.com/o/r/issues/7" || strings.Contains(string(body), "issueKey") {
		t.Fatalf("numeric fresh intake %+v: %s", n, body)
	}
	// A key that redaction would change cannot be posted back exactly, so the
	// action is not offered.
	hostile := keyed
	hostile.ID, hostile.Issue.Key = "mol-see-users-someone-x-abc", "see /Users/someone/x"
	writeRecord(t, lab, hostile)
	if got, _, v := fresh(hostile.ID); got.Kind != "" || len(v.NotOffered) != 1 || v.NotOffered[0].Kind != ActionFreshIntake || v.NotOffered[0].Reason != freshIntakeKeyNotShown {
		t.Fatalf("hostile key action %+v not offered %+v", got, v.NotOffered)
	}

	// Posting the action's key starts the keyed run for exactly that key.
	fake := &keyedFake{pattern: `^ENG-[0-9]+$`}
	runs := &keyedRuns{run: func(context.Context, molecule.Options, string) error { return nil }}
	installKeyed(h, fake, runs)
	body, err := json.Marshal(map[string]string{"key": a.IssueKey})
	if err != nil {
		t.Fatal(err)
	}
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", string(body))), http.StatusAccepted)
	waitJob(t, h, keyedRunJob("ENG-7"))
	if runs.calls() != 1 || runs.keys[0] != "ENG-7" || d.calls() != 0 {
		t.Fatalf("fresh intake ran %d keyed calls (%q) and %d numeric", runs.calls(), runs.keys, d.calls())
	}
}

func TestUIKeyedLabViewServesTheIntakeHintFromACachedDescribe(t *testing.T) {
	lab, _ := labFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	// A GitHub Lab's view is unchanged: no intake.
	if w := do(t, h, authed("/api/lab")); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"intake"`) {
		t.Fatalf("GitHub Lab view %d %s", w.Code, w.Body)
	}
	// A Lab that cannot drive never starts the plugin for its view: the hint says
	// keyed, with no pattern.
	idle := &keyedFake{pattern: `ENG-[0-9]+`}
	installKeyed(h, idle, &keyedRuns{})
	for i := 0; i < 3; i++ {
		v := viewOf[LabView](t, do(t, h, authed("/api/lab")), http.StatusOK)
		if v.Intake == nil || !v.Intake.Keyed || v.Intake.KeyPattern != "" {
			t.Fatalf("idle keyed intake %+v", v.Intake)
		}
	}
	if describes, _, prepared := idle.counts(); describes != 0 || prepared != 0 {
		t.Fatalf("the Lab view of a Lab that cannot drive started the plugin: %d describes, %d prepared", describes, prepared)
	}
	// A Lab that can drive describes once in the background; the view serves the
	// cached pattern once it is known, and never describes again.
	d, _ := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	fake := &keyedFake{pattern: `ENG-[0-9]+`}
	installKeyed(d, fake, &keyedRuns{})
	if v := viewOf[LabView](t, do(t, d, authed("/api/lab")), http.StatusOK); v.Intake == nil || !v.Intake.Keyed || v.Intake.KeyPattern != "" {
		t.Fatalf("first keyed intake %+v", v.Intake)
	}
	keyedBackgroundDone(t, d)
	for i := 0; i < 3; i++ {
		v := viewOf[LabView](t, do(t, d, authed("/api/lab")), http.StatusOK)
		if v.Intake == nil || !v.Intake.Keyed || v.Intake.KeyPattern != "^(?:ENG-[0-9]+)$" {
			t.Fatalf("keyed intake %+v", v.Intake)
		}
	}
	if describes, _, _ := fake.counts(); describes != 1 {
		t.Fatalf("the Lab view described %d times, want once", describes)
	}
	// A pattern the plugin serves that is not one line of text is never served,
	// and the failure is cached: a poll never describes again.
	other, _ := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	hostile := &keyedFake{pattern: "ENG-[0-9]+\n<b>"}
	installKeyed(other, hostile, &keyedRuns{})
	viewOf[LabView](t, do(t, other, authed("/api/lab")), http.StatusOK)
	keyedBackgroundDone(t, other)
	for i := 0; i < 3; i++ {
		if v := viewOf[LabView](t, do(t, other, authed("/api/lab")), http.StatusOK); v.Intake == nil || !v.Intake.Keyed || v.Intake.KeyPattern != "" {
			t.Fatalf("hostile pattern intake %+v", v.Intake)
		}
	}
	if describes, _, _ := hostile.counts(); describes != 1 {
		t.Fatalf("a hostile pattern was described %d times, want once", describes)
	}
}

// GET /api/lab never waits on a describe: with the background describe held by
// a hanging tracker, the view still answers, keyed with no pattern, and starts
// no second describe. The pattern appears once the describe returns.
func TestUIKeyedLabViewAnswersAtOnceWithAHangingTracker(t *testing.T) {
	lab, _ := labFixture(t)
	h, _ := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	hang := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(hang)
		}
	})
	fake := &keyedFake{pattern: `^ENG-[0-9]+$`, hang: hang, entered: make(chan struct{}, 1)}
	installKeyed(h, fake, &keyedRuns{})
	viewOf[LabView](t, do(t, h, authed("/api/lab")), http.StatusOK)
	<-fake.entered
	for i := 0; i < 3; i++ {
		v := viewOf[LabView](t, do(t, h, authed("/api/lab")), http.StatusOK)
		if v.Intake == nil || !v.Intake.Keyed || v.Intake.KeyPattern != "" {
			t.Fatalf("intake while the tracker hangs %+v", v.Intake)
		}
	}
	if describes, _, _ := fake.counts(); describes != 1 {
		t.Fatalf("describes %d while one hangs, want 1", describes)
	}
	close(hang)
	released = true
	keyedBackgroundDone(t, h)
	if v := viewOf[LabView](t, do(t, h, authed("/api/lab")), http.StatusOK); v.Intake == nil || v.Intake.KeyPattern != "^ENG-[0-9]+$" {
		t.Fatalf("intake after the describe %+v", v.Intake)
	}
	if describes, _, _ := fake.counts(); describes != 1 {
		t.Fatalf("describes %d, want 1", describes)
	}
}

// A pattern with a line separator in it is not one line of text: the
// background describe caches its failure and the view serves no pattern.
func TestUIKeyedLabViewNeverServesALineSeparatedPattern(t *testing.T) {
	lab, _ := labFixture(t)
	other, _ := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{})
	installKeyed(other, &keyedFake{pattern: "ENG-[0-9]+ <b>"}, &keyedRuns{})
	viewOf[LabView](t, do(t, other, authed("/api/lab")), http.StatusOK)
	keyedBackgroundDone(t, other)
	if v := viewOf[LabView](t, do(t, other, authed("/api/lab")), http.StatusOK); v.Intake == nil || !v.Intake.Keyed || v.Intake.KeyPattern != "" {
		t.Fatalf("line-separated pattern intake %+v", v.Intake)
	}
}
