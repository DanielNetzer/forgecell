package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// inboxTypes is the Atom type order of a ticket Formula with the review Atom:
// intake, scope gate, harness, check, review, review gate, ship, document.
var inboxTypes = []string{"intake", "gate", "harness", "check", "review", "gate", "ship", "document"}

// inboxAtoms gives the ticket Formula's Atoms the statuses in order; the rest
// are pending.
func inboxAtoms(statuses ...string) []molecule.Atom {
	out := make([]molecule.Atom, len(inboxTypes))
	for i, typ := range inboxTypes {
		out[i] = molecule.Atom{ID: typ + "-" + string(rune('a'+i)), Type: typ, Status: "pending"}
		if i < len(statuses) {
			out[i].Status = statuses[i]
		}
	}
	return out
}

func inboxRecord(base molecule.Record, id string, number int64, status, phase string, atoms []molecule.Atom) molecule.Record {
	r := base
	r.ID, r.Status, r.Issue.Number, r.FinishedAt = id, status, number, ""
	r.Readiness = &readiness.State{SchemaVersion: "v1", Phase: phase}
	r.Atoms, r.HarnessAttempts = atoms, nil
	return r
}

var digest64 = strings.Repeat("ab", 32)

// inboxLab is a fixture Lab with one Molecule in each Inbox state, plus ones the
// Inbox does not list.
func inboxLab(t *testing.T) string {
	t.Helper()
	lab, base := labFixture(t)
	os.Remove(filepath.Join(lab, "ledgers", base.ID+".json"))

	scope := inboxRecord(base, "mol-1-scope", 1, "waiting", "scope-waiting", inboxAtoms("done", "waiting"))
	scope.Readiness.Plans = []readiness.Proposal{{Digest: digest64, Plan: readiness.Plan{Revision: 1, Analysis: readiness.Analysis{
		Summary: "Adds the Inbox.\nSee mol-250-e510dc0bd246216e3411bc83 at " + filepath.Join(lab, "workspaces") + " plan " + digest64 + " on 2026-10-26T18:18:29Z, ghp_" + strings.Repeat("x", 36) + " " + strings.Repeat("long ", 80),
		Scope:   []readiness.ScopedPath{{Path: "a"}, {Path: "b"}, {Path: "c"}},
		Checks:  []readiness.Check{{ID: "1"}, {ID: "2"}, {ID: "3"}, {ID: "4"}, {ID: "5"}},
	}}}}
	writeRecord(t, lab, scope)

	publish := inboxRecord(base, "mol-2-publish", 2, "waiting", "review-waiting", inboxAtoms("done", "done", "done", "done", "done", "waiting", "skipped", "skipped"))
	publish.FinishedAt = "2026-10-26T11:00:00Z"
	publish.Verification = &verification.Result{Checks: []verification.Observation{{ID: "a", Result: process.Result{OK: true}}, {ID: "b", Result: process.Result{OK: true}}, {ID: "c"}}}
	publish.Capture = &verification.CaptureResult{Changes: []string{"a", "b", "c", "d"}}
	writeRecord(t, lab, publish)

	blocked := inboxRecord(base, "mol-3-blocked", 3, "blocked", "blocked", inboxAtoms("done", "done", "done", "failed"))
	writeRecord(t, lab, blocked)

	running := inboxRecord(base, "mol-4-running", 4, "running", "coding", inboxAtoms("done", "done", "active"))
	running.Atoms[2].StartedAt = "2026-10-26T10:00:00.123+02:00"
	writeRecord(t, lab, running)

	queued := inboxRecord(base, "mol-5-queued", 5, "running", "check-pending", inboxAtoms("done", "done", "done", "active"))
	queued.Readiness.Events = []readiness.Event{{Kind: readiness.VerificationQueued, At: "2026-10-26T10:30:00Z"}}
	writeRecord(t, lab, queued)

	for id, life := range map[string]string{"mol-6-draft": "OPEN", "mol-7-merged": "MERGED"} {
		r := inboxRecord(base, id, int64(len(id)), "waiting", "review-waiting", inboxAtoms("done", "done", "done", "done", "done", "done", "done"))
		writeRecord(t, lab, r)
		writeReceipt(t, lab, id, "published", life)
	}

	writeRecord(t, lab, inboxRecord(base, "mol-8-retired", 8, molecule.StatusRetired, "scope-waiting", inboxAtoms("done", "waiting")))
	writeRecord(t, lab, inboxRecord(base, "mol-9-other", 9, "recorded", "", nil))
	return lab
}

func inboxList(t *testing.T, h http.Handler) (MoleculeList, map[string]*InboxView) {
	t.Helper()
	var list MoleculeList
	decode(t, do(t, h, authed("/api/molecules")), &list)
	byID := map[string]*InboxView{}
	for _, m := range list.Molecules {
		byID[m.ID] = nil
		if v := m.Inbox; v.Kind != "" {
			byID[m.ID] = &v
		}
	}
	return list, byID
}

// Each Molecule state maps to exactly one Inbox entry with its title, counts
// and one primary action; Molecules the Inbox does not list have none.
func TestUIInboxMapsEachStateToOneEntry(t *testing.T) {
	lab := inboxLab(t)
	list, byID := inboxList(t, testHandler(t, Options{LabDir: lab}))
	if len(list.Molecules) != 9 || !list.Complete {
		t.Fatalf("list %d complete=%t %v", len(list.Molecules), list.Complete, list.Diagnostics)
	}

	scope := byID["mol-1-scope"]
	if scope == nil || scope.Kind != InboxScope || scope.Title != "Plan ready for your approval" || scope.Action != "Review plan" || !scope.Planned || scope.Files != 3 || scope.Checks != 5 || scope.Verified || scope.Captured || scope.Step != StepApproval || scope.StepLabel != "Your approval" {
		t.Fatalf("scope card %+v", scope)
	}
	if !strings.HasPrefix(scope.Summary, "Adds the Inbox. See a Molecule at <lab>/workspaces plan abababa… on 2026-10-26, [redacted]") || !strings.HasSuffix(scope.Summary, "…") || len([]rune(scope.Summary)) != maxInboxSummaryRunes {
		t.Errorf("scope summary %q", scope.Summary)
	}

	publish := byID["mol-2-publish"]
	if publish == nil || publish.Kind != InboxPublish || publish.Title != "Ready to publish" || publish.Action != "Review & publish" || !publish.Verified || publish.Passed != 2 || publish.Ran != 3 || !publish.Captured || publish.Changed != 4 || publish.Step != StepReview || publish.Planned {
		t.Fatalf("publish card %+v", publish)
	}

	blocked := byID["mol-3-blocked"]
	if blocked == nil || blocked.Kind != InboxBlocked || blocked.Title != "Blocked" || blocked.Action != "Open" || blocked.Summary != "Required checks did not pass." || blocked.Step != StepChecks {
		t.Fatalf("blocked card %+v", blocked)
	}

	running := byID["mol-4-running"]
	if running == nil || running.Kind != InboxProgress || running.Title != "Coding" || running.Action != "Watch" || running.Step != StepCoding || running.StepLabel != "Coding" || running.StepSince != "2026-10-26T08:00:00Z" || running.Queued {
		t.Fatalf("running row %+v", running)
	}
	queued := byID["mol-5-queued"]
	if queued == nil || queued.Kind != InboxProgress || !queued.Queued || queued.StepLabel != "Queued for checks" || queued.Step != StepChecks || queued.Action != "Watch" {
		t.Fatalf("queued row %+v", queued)
	}

	draft, merged := byID["mol-6-draft"], byID["mol-7-merged"]
	if draft == nil || draft.Kind != InboxRecorded || draft.Title != "Draft published" || draft.Merged || draft.Step != StepMerged {
		t.Fatalf("draft %+v", draft)
	}
	if merged == nil || merged.Kind != InboxRecorded || merged.Title != "Merged" || !merged.Merged {
		t.Fatalf("merged %+v", merged)
	}
	for _, id := range []string{"mol-8-retired", "mol-9-other"} {
		if v, ok := byID[id]; !ok || v != nil {
			t.Errorf("%s has an Inbox entry %+v", id, v)
		}
	}
}

// The Inbox never carries a raw phase code, Molecule or assay id, full digest,
// ISO timestamp, local path or secret in its text; StepSince is data only.
func TestUIInboxTextHasNoRawCodes(t *testing.T) {
	lab := inboxLab(t)
	w := do(t, testHandler(t, Options{LabDir: lab}), authed("/api/molecules"))
	var list struct {
		Molecules []struct {
			Inbox map[string]any `json:"inbox"`
		} `json:"molecules"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	raw := regexp.MustCompile(`scope-waiting|review-waiting|check-pending|verification-queued|\bmol-|\bassay-|[0-9a-f]{64}|\d{4}-\d{2}-\d{2}T\d{2}|/Users/|/home/|/tmp/|/private/|ghp_`)
	seen := 0
	for _, m := range list.Molecules {
		for key, value := range m.Inbox {
			text, ok := value.(string)
			if !ok || key == "stepSince" {
				continue
			}
			seen++
			if raw.MatchString(text) || strings.ContainsAny(text, "\n\r\t") {
				t.Errorf("inbox %s carries raw text: %q", key, text)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no Inbox text was checked")
	}
	// The browser renders only those plain fields: the Inbox code never turns a
	// phase, status, Atom, Formula or digest into text, and ids reach only
	// links' href and dataset.
	js := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/app.js")).Body.String()
	inbox := between(t, js, "// ---- Inbox ----", "showView(viewFromURL());\n  restoreSelection();")
	for _, forbidden := range []string{"m.phase", "m.status", "m.currentAtom", "m.formulaId", "m.lane", ".digest", "short("} {
		if strings.Contains(inbox, forbidden) {
			t.Errorf("the Inbox renders %s", forbidden)
		}
	}
	for _, line := range regexp.MustCompile(`setText\([^;]*;`).FindAllString(inbox, -1) {
		if strings.Contains(line, "m.id") {
			t.Errorf("the Inbox shows a Molecule id: %s", line)
		}
	}
}

// The progress step comes from the Atoms: the first active or waiting Atom,
// then a failed one, then the readiness phase; queued and published Molecules
// have fixed steps.
func TestUIInboxProgressStepsFromAtoms(t *testing.T) {
	published := &DeliveryState{State: "published", Lifecycle: "OPEN"}
	mergedReceipt := &DeliveryState{State: "published", Lifecycle: "MERGED"}
	at := func(statuses ...string) []molecule.Atom { return inboxAtoms(statuses...) }
	since := at("done", "done", "active")
	since[2].StartedAt = "2026-10-26T10:00:00Z"
	bad := at("done", "done", "active")
	bad[2].StartedAt = "<script>yesterday"
	demo := []molecule.Atom{{Type: "intake", Status: "done"}, {Type: "harness", Status: "done"}, {Type: "check", Status: "done"}, {Type: "gate", Status: "waiting"}}
	for name, c := range map[string]struct {
		atoms    []molecule.Atom
		phase    string
		activity string
		d        *DeliveryState
		step     int
		label    string
		since    string
	}{
		"no atoms, scope phase": {nil, "scope-waiting", "", nil, StepApproval, "Your approval", ""},
		"no atoms, no phase":    {nil, "", "", nil, StepPlan, "Plan", ""},
		"planning":              {at("active"), "", "", nil, StepPlan, "Plan", ""},
		"scope gate":            {at("done", "waiting"), "scope-waiting", "", nil, StepApproval, "Your approval", ""},
		"coding":                {since, "coding", "", nil, StepCoding, "Coding", "2026-10-26T10:00:00Z"},
		"coding, bad time":      {bad, "coding", "", nil, StepCoding, "Coding", ""},
		"checks":                {at("done", "done", "done", "running"), "check-pending", "", nil, StepChecks, "Checks", ""},
		"queued":                {at("done", "done", "done", "active"), "check-pending", ActivityQueued, nil, StepChecks, "Queued for checks", ""},
		"adversarial review":    {at("done", "done", "done", "done", "active"), "check-pending", "", nil, StepReview, "Review", ""},
		"review gate":           {at("done", "done", "done", "done", "done", "waiting"), "review-waiting", "", nil, StepReview, "Review", ""},
		"draft":                 {at("done", "done", "done", "done", "done", "done", "active"), "", "", nil, StepDraft, "Draft PR", ""},
		"documenting":           {at("done", "done", "done", "done", "done", "done", "done", "active"), "", "", nil, StepDraft, "Draft PR", ""},
		"failed checks":         {at("done", "done", "done", "failed", "skipped"), "blocked", "", nil, StepChecks, "Checks", ""},
		"gate after harness":    {demo, "", "", nil, StepReview, "Review", ""},
		"draft published":       {at("done", "done", "done", "done", "done", "done", "done"), "review-waiting", "", published, StepMerged, "Draft published", ""},
		"merged":                {nil, "review-waiting", "", mergedReceipt, StepMerged, "Merged", ""},
	} {
		r := molecule.Record{Atoms: c.atoms, Readiness: &readiness.State{Phase: c.phase}}
		step, label, got := progress(r, "", c.activity, c.d)
		if step != c.step || label != c.label || got != c.since {
			t.Errorf("%s: step %d %q since %q, want %d %q %q", name, step, label, got, c.step, c.label, c.since)
		}
		if step < 0 || step >= len(StepLabels) {
			t.Errorf("%s: step %d out of range", name, step)
		}
	}
	if len(StepLabels) != 7 || StepLabels[StepApproval] != "Your approval" || StepLabels[StepMerged] != "Merged" {
		t.Errorf("step labels %q", StepLabels)
	}
}

// A Molecule still in the running lane whose receipt is already published is
// recorded, never an In-progress row titled Draft published with a full bar; a
// running Molecule without a published receipt stays In progress.
func TestUIInboxRunningButPublishedIsRecorded(t *testing.T) {
	atoms := inboxAtoms("done", "done", "done", "done", "done", "done", "done", "active")
	r := molecule.Record{Status: "running", Atoms: atoms, Readiness: &readiness.State{Phase: "review-waiting"}}
	for name, c := range map[string]struct {
		d     *DeliveryState
		title string
		merge bool
	}{
		"draft":  {&DeliveryState{State: "published", Lifecycle: "OPEN"}, "Draft published", false},
		"merged": {&DeliveryState{State: "published", Lifecycle: "MERGED"}, "Merged", true},
	} {
		v := inboxView(redactor{}, r, LaneRunning, "", c.d)
		if v.Kind != InboxRecorded || v.Title != c.title || v.Merged != c.merge || v.Action != "Open" || v.Queued {
			t.Errorf("%s: running but published is %+v", name, v)
		}
	}
	for _, d := range []*DeliveryState{nil, {State: "previewed"}} {
		v := inboxView(redactor{}, r, LaneRunning, "", d)
		if v.Kind != InboxProgress || v.Title != "Draft PR" || v.Step != StepDraft || v.Action != "Watch" {
			t.Errorf("running without a published receipt (%+v) is %+v", d, v)
		}
	}
}

// The id scrubber replaces only real Molecule and assay ids; ordinary words
// that start the same way are kept.
func TestUIInboxSummaryScrubsOnlyRealIDs(t *testing.T) {
	for in, want := range map[string]string{
		"Fix mol-250-e510dc0bd246216e3411bc83 now":                                     "Fix a Molecule now",
		"From assay-b12a1e069c3c74ac82d7cbf20231144b7ffc3c68b7a45e93794d022f31a716e4.": "From an assay.",
		"Use assay-based checks for the mol-file loader and mol-250 notes.":            "Use assay-based checks for the mol-file loader and mol-250 notes.",
		"An assay-abcdef12 and mol-abcdef1 are ids":                                    "An an assay and a Molecule are ids",
		"Keep assay-cafebabes and mol-12-ab intact":                                    "Keep assay-cafebabes and mol-12-ab intact",
	} {
		if got := inboxSummary(redactor{}, in); got != want {
			t.Errorf("inboxSummary(%q) = %q, want %q", in, got, want)
		}
	}
}

// "Needs you" has one definition: the Molecules under Needs you (scope gate,
// ready to publish, blocked). The headline, the live region, the nav badge and
// the resident all count it, so a Lab with only a blocked Molecule never reads
// Nothing needs you or All quiet. The fixture's blocked Molecule is one of them.
func TestUIInboxOneNeedsYouDefinition(t *testing.T) {
	js := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/app.js")).Body.String()
	if !strings.Contains(js, "const NEEDS_KINDS = ['scope', 'publish', 'blocked'];") {
		t.Error("Needs you does not cover scope, publish and blocked")
	}
	if needs := jsFunction(t, js, "needsYou"); !strings.Contains(needs, "NEEDS_KINDS.includes(v.kind)") || !strings.Contains(needs, "live(m) && inboxOf(m)") {
		t.Errorf("needsYou does not list the Needs-you cards: %s", needs)
	}
	shell, inbox := jsFunction(t, js, "renderShell"), jsFunction(t, js, "renderInbox")
	for body, wants := range map[string][]string{
		shell: {"const waiting = needsYou().length;", "badge.hidden = waiting === 0;", "residentPose(waiting, counts)", "RESIDENT_TITLES[pose](waiting, counts)"},
		inbox: {"const needs = needsYou();", "const waiting = needs.length,", "waiting ? needsYouText(waiting) : 'Nothing needs you'", "setText($('#inbox-live'), waiting ? needsYouText(waiting) + '.' : 'Nothing needs you right now.');"},
	} {
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("app.js lacks %q in:\n%s", want, body)
			}
		}
	}
	for name, body := range map[string]string{"renderShell": shell, "renderInbox": inbox, "residentPose": jsFunction(t, js, "residentPose")} {
		if strings.Contains(body, "counts.decision") {
			t.Errorf("%s counts the decision lane instead of Needs you", name)
		}
	}
	if !strings.Contains(js, "waving: (w) => w + (plural.select(w) === 'one'") {
		t.Error("the resident title does not count Needs you")
	}

	// A Lab whose only Molecule is blocked has one Needs-you card and no
	// decision-lane Molecule: the lane count alone would have read All quiet.
	lab, base := labFixture(t)
	os.Remove(filepath.Join(lab, "ledgers", base.ID+".json"))
	writeRecord(t, lab, inboxRecord(base, "mol-3-blocked", 3, "blocked", "blocked", inboxAtoms("done", "done", "done", "failed")))
	list, byID := inboxList(t, testHandler(t, Options{LabDir: lab}))
	if v := byID["mol-3-blocked"]; v == nil || v.Kind != InboxBlocked || list.Counts[LaneDecision] != 0 {
		t.Fatalf("blocked-only Lab: %+v counts %v", v, list.Counts)
	}
}

// Blocked reasons are fixed plain words, never ledger text.
func TestUIInboxBlockedReasonsArePlainWords(t *testing.T) {
	for want, r := range map[string]molecule.Record{
		"Planning stopped before a plan was ready.":       {Status: "blocked", Atoms: inboxAtoms("failed")},
		"Coding stopped before the change was finished.":  {Status: "blocked", Atoms: inboxAtoms("done", "done", "failed")},
		"Review found a problem that blocks this change.": {Status: "blocked", Atoms: inboxAtoms("done", "done", "done", "done", "failed")},
		"This Molecule failed before it finished.":        {Status: "failed", Atoms: []molecule.Atom{{Type: "learn", Status: "failed", Detail: "/Users/x ghp_secret"}}},
		"This Molecule stopped before it finished.":       {Status: "blocked"},
		"The draft PR could not be published.":            {Status: "blocked", Atoms: inboxAtoms("done", "done", "done", "done", "done", "done", "blocked")},
		"The plan approval did not go through.":           {Status: "blocked", Atoms: inboxAtoms("done", "failed")},
		"Required checks did not pass.":                   {Status: "blocked", Atoms: inboxAtoms("done", "done", "done", "failed", "failed")},
	} {
		if got := blockedReason(r); got != want {
			t.Errorf("blockedReason = %q, want %q", got, want)
		}
	}
}

// With nothing waiting the Inbox reads "Nothing needs you right now" and the
// resident reads, titled All quiet; an incomplete inventory still lists the
// readable Molecules and says the Inbox may be missing some.
func TestUIInboxEmptyStateAndIncompleteInventory(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	_, byID := inboxList(t, h)
	if len(byID) != 0 {
		t.Fatalf("empty Lab lists %v", byID)
	}
	page := do(t, h, authed("/")).Body.String()
	inbox := between(t, page, `id="view-inbox"`, `id="view-issues"`)
	for _, want := range []string{
		`<p id="inbox-needs-empty" class="inbox-empty" hidden>Nothing needs you right now.</p>`,
		`<div id="inbox-needs" class="inbox-list"></div>`, `<div id="inbox-progress" class="inbox-list"></div>`, `<div id="inbox-recent" class="inbox-recent"></div>`,
		`<h2 id="needs-you-title" class="inbox-heading">Needs you</h2>`, `<h2 id="in-progress-title" class="inbox-heading">In progress</h2>`, `<h2 id="recent-title" class="inbox-heading">Recently recorded</h2>`,
		`<a class="btn btn-secondary" href="?view=issues" data-view="issues">`, "Pick an Issue</a>",
		`<p id="inbox-live" class="sr-only" role="status" aria-live="polite"></p>`,
	} {
		if !strings.Contains(inbox, want) {
			t.Errorf("the Inbox lacks %s", want)
		}
	}
	if n := strings.Count(inbox, "aria-live"); n != 1 {
		t.Errorf("the Inbox has %d live regions", n)
	}
	if !strings.Contains(page, `<p id="resident-title" class="resident-title">All quiet</p>`) {
		t.Error("the resident is not titled All quiet")
	}
	js := do(t, h, authed("/app.js")).Body.String()
	for _, want := range []string{
		"$('#inbox-needs-empty').hidden = needs.length > 0;",
		"setText($('#inbox-live'), waiting ? needsYouText(waiting) + '.' : 'Nothing needs you right now.');",
		"reading: () => 'All quiet'",
		"' thing is waiting for you' : ' things are waiting for you'",
		"'Good ' + partOfDay(new Date().getHours()) + (name ? ', ' + name : '')",
		"drafts ? counted(drafts, 'draft published', 'drafts published') : 'no draft published yet'",
		"blocked ? blocked + ' blocked' : 'nothing blocked'",
		"const complete = !state.list || state.list.complete;",
		"needs.slice(0, MAX_ENTRIES)",
		"kind('recorded').sort(newestFirst).slice(0, MAX_RECENT)",
		"const MAX_ENTRIES = 20, MAX_RECENT = 5;",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}

	lab := inboxLab(t)
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "mol-0-bad.json"), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	list, byID := inboxList(t, testHandler(t, Options{LabDir: lab}))
	if list.Complete || byID["mol-1-scope"] == nil || byID["mol-1-scope"].Kind != InboxScope {
		t.Fatalf("incomplete inventory: complete=%t scope=%+v", list.Complete, byID["mol-1-scope"])
	}
	noLeak(t, strings.Join(list.Diagnostics, "\n"))
}

// loopBody is the body the Run-the-loop form posts for a scope mode and value:
// loopSelector and startLoop in app.js, which the served-JS checks below pin.
func loopBody(mode, value string, n int) string {
	selector := "oldest"
	if mode != "oldest" {
		selector = mode + ":" + strings.TrimSpace(value)
	}
	raw, _ := json.Marshal(map[string]any{"select": selector, "maxConcurrent": n})
	return string(raw)
}

// The Run-the-loop form is a real form whose submit handler (Enter included)
// posts the same {select, maxConcurrent} to /api/loop/start as before, and the
// engine receives exactly that selector and bound; Start intake posts {issue}.
// A selector the server refuses never reaches the engine.
func TestUIInboxRunTheLoopFormSendsTheSameEngineRequest(t *testing.T) {
	js := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/app.js")).Body.String()
	for _, want := range []string{
		"function loopSelector(mode, value) { return mode === 'oldest' ? 'oldest' : mode + ':' + String(value).trim(); }",
		"const loop = await submitDrive('/api/loop/start', JSON.stringify({ select: String(select).trim(), maxConcurrent: n }));",
		"const job = await submitDrive('/api/runs', JSON.stringify({ issue: drive.runIssue }));",
		"oldest: { label: 'Oldest', field: '', placeholder: '' },",
		"milestone: { label: 'Milestone', field: 'Milestone title', placeholder: 'v1…' },",
		"label: { label: 'Label', field: 'Label name', placeholder: 'ready…' },",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	submit := jsFunction(t, js, "submitLoop")
	for _, want := range []string{"e.preventDefault();", "startLoop(loopSelector(drive.mode, c.value.value), c.max.value);", "driveMessage('Enter a ' + LOOP_MODES[drive.mode].field.toLowerCase() + ', or choose Oldest.');"} {
		if !strings.Contains(submit, want) {
			t.Errorf("submitLoop lacks %q", want)
		}
	}
	if run := jsFunction(t, js, "submitRun"); !strings.Contains(run, "e.preventDefault();") || !strings.Contains(run, "startRun(drive.controls.issue.value);") {
		t.Errorf("submitRun does not post the typed Issue: %s", run)
	}
	// The forms submit on Enter through a submit button; the CSP's form-action
	// 'none' never sees a navigation because the handlers prevent it.
	if !strings.Contains(js, "onsubmit: submitLoop") || !strings.Contains(js, "onsubmit: submitRun") || strings.Count(js, "type: 'submit'") != 2 {
		t.Error("the side panel forms do not submit on Enter")
	}

	for _, c := range []struct {
		mode, value, want string
		n                 int
	}{{"oldest", "", "oldest", 1}, {"milestone", " v1 ", "milestone:v1", 3}, {"label", "ready", "label:ready", maxLoopConcurrent}} {
		lab, _ := labFixture(t)
		h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{loop: func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
			return molecule.LoopReport{}, nil
		}})
		v := viewOf[LoopView](t, do(t, h, gatePost("/api/loop/start", loopBody(c.mode, c.value, c.n))), http.StatusAccepted)
		if v.Select != c.want || v.MaxConcurrent != c.n {
			t.Errorf("%s: accepted loop %+v", c.mode, v)
		}
		waitLoop(t, h)
		d.mu.Lock()
		loops := append([]molecule.LoopOptions(nil), d.loops...)
		d.mu.Unlock()
		if len(loops) != 1 || loops[0].Select.String() != c.want || loops[0].MaxConcurrent != c.n {
			t.Errorf("%s: engine loop options %+v", c.mode, loops)
		}
	}

	lab, _ := labFixture(t)
	h, d := driveHandler(t, lab, guardWith(64<<30), &fakeDrive{run: func(context.Context, molecule.Options) error { return nil }})
	for _, body := range []string{loopBody("milestone", "  ", 1), loopBody("label", "", 2), loopBody("oldest", "", maxLoopConcurrent+1), loopBody("oldest", "", 0)} {
		refusalOf(t, do(t, h, gatePost("/api/loop/start", body)), http.StatusBadRequest, "invalid")
	}
	if n := d.calls(); n != 0 {
		t.Fatalf("refused loop requests reached the engine %d times", n)
	}
	viewOf[RunJobView](t, do(t, h, gatePost("/api/runs", `{"issue":245}`)), http.StatusAccepted)
	waitJob(t, h, runKey(245))
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.runs) != 1 || d.runs[0].Issue != "245" || d.runs[0].Approve != "" {
		t.Fatalf("intake options %+v", d.runs)
	}
}

// Polling patches the Inbox in place keyed by Molecule id: a node shown for an
// id is kept and only its changed text and attributes are set; the lists are
// never refilled, and links are real ?view=work&m= links opened through
// history.pushState.
func TestUIInboxPollingPatchesCardsInPlace(t *testing.T) {
	js := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/app.js")).Body.String()
	patch := jsFunction(t, js, "patchList")
	for _, want := range []string{
		"const existing = new Map([...container.children].map((node) => [node.dataset.inboxId, node]));",
		"let node = existing.get(m.id);",
		"if (!node) { node = create(); node.dataset.inboxId = m.id; }",
		"update(node, m, inboxOf(m));",
		"if (node === at) at = at.nextElementSibling;",
		"else container.insertBefore(node, at);",
		"node.remove();",
	} {
		if !strings.Contains(patch, want) {
			t.Errorf("patchList lacks %q", want)
		}
	}
	for _, gone := range []string{"fill(", "replaceChildren", "create();\n      update"} {
		if strings.Contains(patch, gone) {
			t.Errorf("patchList rebuilds with %q", gone)
		}
	}
	render := jsFunction(t, js, "renderInbox")
	for _, want := range []string{
		"patchList($('#inbox-needs'), needs.slice(0, MAX_ENTRIES), createCard, updateCard);",
		"patchList($('#inbox-progress'), running.slice(0, MAX_ENTRIES), createRow, updateRow);",
		"patchList($('#inbox-recent'), recent, createRecent, updateRecent);",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderInbox lacks %q", want)
		}
	}
	if strings.Contains(render, "fill($('#inbox-needs')") || strings.Contains(render, "fill($('#inbox-progress')") || strings.Contains(render, "fill($('#inbox-recent')") {
		t.Error("renderInbox refills a list")
	}
	for name, wants := range map[string][]string{
		"updateCard": {"setText(slot(node, 'badge'), plainText(v.title, 'Needs you'));", "setLink(slot(node, 'action'), m, plainText(v.action, 'Open'));", "setText(detail, text);"},
		"updateRow":  {"setText(slot(node, 'step'), stepText(v));", "if (i === at) setAttr(li, 'aria-current', 'step');", "setLink(slot(node, 'action'), m, plainText(v.action, 'Watch'));"},
		"setLink":    {"setAttr(a, 'href', moleculeHref(m.id));", "if (a.dataset.molecule !== m.id) a.dataset.molecule = m.id;"},
		"setAttr":    {"if (node.getAttribute(name) !== value) node.setAttribute(name, value);"},
	} {
		body := jsFunction(t, js, name)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
		if strings.Contains(body, "fill(") || strings.Contains(body, "el(") {
			t.Errorf("%s rebuilds nodes", name)
		}
	}
	for _, want := range []string{
		"keepFocus(renderList)",
		"renderInbox();\n  }",
		"function moleculeHref(id) { return '?view=work&m=' + encodeURIComponent(id); }",
		"if (location.search !== moleculeHref(id)) history.pushState({ view: 'work', m: id }, '', moleculeHref(id));",
		"const link = e.target.closest('a[data-molecule]');",
		"if (moleculeFromURL() !== id) history.replaceState({ view: 'work', m: id }, '', moleculeHref(id));",
		"const MOLECULE_ID = /^[A-Za-z0-9_-]{1,200}$/;",
		"setInterval(() => { if (!document.hidden && state.list) renderInbox(); }, 30000);",
		"el('ol', { class: 'inbox-steps', 'aria-label': 'Progress' }, STEPS.map(",
		"const STEPS = ['Plan', 'Your approval', 'Coding', 'Checks', 'Review', 'Draft PR', 'Merged'];",
		"el('a', { class: 'btn btn-primary inbox-action', dataset: { slot: 'action' } })",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	// One primary action per card: createCard builds exactly one .btn.
	if card := jsFunction(t, js, "createCard"); strings.Count(card, "btn") != 2 || strings.Count(card, "btn-primary") != 1 || strings.Contains(card, "btn-danger") {
		t.Errorf("a card does not have exactly one primary action: %s", card)
	}
}

// The Inbox CSS stacks cards with full-width primary buttons on phones, gives
// coarse pointers 44px targets and honours forced colors.
func TestUIInboxServedCSS(t *testing.T) {
	css := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/style.css")).Body.String()
	rules := cssRules(css)
	wantDecls(t, rules, ".inbox-card", "display: flex;", "gap: 20px;")
	wantDecls(t, rules, ".inbox-steps", "grid-template-columns: repeat(7, minmax(0, 1fr));")
	wantDecls(t, rules, ".inbox-steps li.is-current .step-bar", "background: linear-gradient(")
	wantDecls(t, rules, ".seg[aria-pressed=true]", "background: var(--paper);")
	wantDecls(t, rules, ".inbox-counts", "font-variant-numeric: tabular-nums;")
	mobile := cssRules(mediaBlock(t, css, "@media (max-width: 720px)"))
	wantDecls(t, mobile, ".inbox-card", "flex-direction: column;")
	wantDecls(t, mobile, ".inbox-action", "width: 100%;", "min-height: 48px;")
	wantDecls(t, mobile, ".inbox-greeting > .btn", "width: 100%;")
	wantDecls(t, cssRules(mediaBlock(t, css, "@media (pointer: coarse)")), ".seg, .stepper-button, .inbox-watch, .inbox-recent-row", "min-height: 44px;")
	forced := cssRules(mediaBlock(t, css, "@media (forced-colors: active)"))
	wantDecls(t, forced, ".seg[aria-pressed=true]", "background: Highlight;")
	wantDecls(t, forced, ".inbox-steps li.is-done .step-bar, .inbox-steps li.is-current .step-bar", "background: Highlight;")
}

// Sample data's Inbox entries are labelled sample fixtures without counts or
// times, so nothing is invented.
func TestUIInboxDemoEntriesCarryNoInventedCounts(t *testing.T) {
	_, byID := inboxList(t, testHandler(t, Options{Demo: true}))
	kinds := map[string]int{}
	for id, v := range byID {
		if v == nil {
			t.Fatalf("sample %s has no Inbox entry", id)
		}
		kinds[v.Kind]++
		if v.Planned || v.Verified || v.Captured || v.Files != 0 || v.Checks != 0 || v.Passed != 0 || v.Ran != 0 || v.Changed != 0 || v.StepSince != "" {
			t.Errorf("sample %s invents counts or times: %+v", id, v)
		}
	}
	for _, k := range []string{InboxPublish, InboxBlocked, InboxProgress, InboxRecorded} {
		if kinds[k] == 0 {
			t.Errorf("sample data has no %s entry", k)
		}
	}
}
