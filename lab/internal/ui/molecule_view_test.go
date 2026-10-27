package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// viewFormula is a Formula snapshot with a coding override and a review Atom, so
// the roles and the coding budget come from it.
const viewFormula = "kind: formula\nid: f\nharness:\n  binding: default\n  command: [/bin/default]\n  timeoutMs: 1800000\nharnesses:\n  coding: {binding: codex, command: [/bin/c]}\n  review: {binding: claude-code, command: [/bin/r]}\natoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n  - {id: review, type: review}\n"

// viewPlan is a plan with two scope paths, one in the base tree, a candidate
// check and the five Formula baseline checks.
func viewPlan(base string) readiness.Plan {
	return readiness.Plan{Revision: 1, Inputs: readiness.Inputs{Repository: "o/r", TargetBranch: "main", BaseCommit: base},
		FormulaBaseline: []string{"base-test", "base-vet", "base-fmt", "base-hygiene", "base-hygiene-test"},
		Evidence:        []readiness.Evidence{{ID: "file-1", Path: "lab/go.mod", SHA256: strings.Repeat("1", 64)}},
		Analysis: readiness.Analysis{
			Summary:    "Add the Molecule view.",
			Scope:      []readiness.ScopedPath{{Path: "lab/internal/ui/web/app.js", Reason: "Rebuild the view"}, {Path: "lab/internal/ui/estimate.go", Reason: "New estimates; this reason calls it an edit"}},
			Acceptance: []readiness.Criterion{{Description: "One primary per card"}},
			Questions:  []readiness.Question{{Question: "Which window?"}},
			Checks: []readiness.Check{
				{ID: "targeted", Category: "candidate", Argv: []string{"go", "test", "./internal/ui/...", "./internal/molecule/..."}, Dir: "lab"},
				{ID: "base-test", Category: "regression", Argv: []string{"go", "test", "-timeout", "20m", "./..."}, Dir: "lab"},
				{ID: "base-vet", Category: "regression", Argv: []string{"go", "vet", "./..."}, Dir: "lab"},
				{ID: "base-fmt", Category: "regression", Argv: []string{"sh", "-c", `test -z "$(gofmt -l internal cmd)"`}, Dir: "lab"},
				{ID: "base-hygiene", Category: "regression", Argv: []string{"sh", "scripts/check-repo-hygiene.sh"}, Dir: "."},
				{ID: "base-hygiene-test", Category: "regression", Argv: []string{"sh", "scripts/check-repo-hygiene.test.sh"}, Dir: "."},
			},
		}}
}

// viewFixture is labFixture waiting for scope approval of viewPlan, with a
// frozen base tree that holds app.js but not estimate.go.
func viewFixture(t *testing.T) (string, molecule.Record, string) {
	t.Helper()
	lab, r := labFixture(t)
	digest := strings.Repeat("cd", 32)
	sum := sha256.Sum256([]byte(viewFormula))
	r.FormulaSnapshot = molecule.Snapshot{YAML: viewFormula, SHA256: hex.EncodeToString(sum[:])}
	r.RepositoryEvidence = &readiness.RepositorySnapshot{Commit: r.Workspace.BaseCommit, Files: []readiness.TreeFile{{Path: "lab/internal/ui/web/app.js"}, {Path: "lab/go.mod"}}}
	r.Readiness = &readiness.State{SchemaVersion: "v1", Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: viewPlan(r.Workspace.BaseCommit), Digest: digest}},
		Events: []readiness.Event{{Kind: "proposed", Digest: digest, At: "2026-10-26T18:46:00Z"}}}
	cost := 1.82
	r.AnalysisAttempts = []molecule.AnalysisAttempt{{Ordinal: 1, Status: "completed", Usage: &harness.Usage{TotalCostUSD: &cost}}, {Ordinal: 2, Status: "completed"}}
	writeRecord(t, lab, r)
	return lab, r, digest
}

func detailOf(t *testing.T, h http.Handler, id string) Detail {
	t.Helper()
	w := do(t, h, authed("/api/molecules/"+id))
	if w.Code != http.StatusOK {
		t.Fatalf("detail %s: %d %s", id, w.Code, w.Body)
	}
	var d Detail
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.View == nil {
		t.Fatalf("detail %s has no Molecule view", id)
	}
	return d
}

// Each gate state projects exactly one primary action, and the scope primary is
// bound to the exact digest the plan shows; states with nothing to decide have
// none. Request changes is offered at the scope gate only.
func TestMoleculeViewOnePrimaryPerGateBoundToTheShownDigest(t *testing.T) {
	lab, r, digest := viewFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	d := detailOf(t, h, r.ID)
	p := d.View.Decision.Primary
	if d.View.Decision.Kind != DecisionScope || p == nil || p.Action != PrimaryApprove || p.Label != "Approve plan & start coding" || p.Digest != digest {
		t.Fatalf("scope decision %+v", d.View.Decision)
	}
	if p.Digest != d.Gate.Digest || p.Digest != d.View.Plan.Digest || !d.View.Decision.RequestChanges || d.View.Decision.Title != "Approve this plan?" {
		t.Fatalf("scope primary not bound to the shown plan: gate %q plan %q", d.Gate.Digest, d.View.Plan.Digest)
	}
	if want := "Forgecell will code 2 files, then run 6 checks on its own. Nothing is published until you approve the draft PR."; d.View.Decision.Copy != want {
		t.Errorf("copy %q", d.View.Decision.Copy)
	}

	// A plan whose digest is not a full digest offers no approval at all.
	bad := r
	bad.Readiness = &readiness.State{SchemaVersion: "v1", Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: viewPlan(r.Workspace.BaseCommit), Digest: "short"}}}
	writeRecord(t, lab, bad)
	if d := detailOf(t, h, r.ID); d.View.Decision.Primary != nil || d.View.Decision.RequestChanges {
		t.Fatalf("an unbindable plan offers %+v", d.View.Decision)
	}

	// The review gate: Preview delivery is the one primary, bound to nothing yet.
	review := r
	review.Status, review.FinishedAt = "recorded", "2026-10-26T19:40:00Z"
	review.Readiness = &readiness.State{SchemaVersion: "v1", Phase: "review-waiting", Plans: []readiness.Proposal{{Plan: viewPlan(r.Workspace.BaseCommit), Digest: digest}}}
	writeRecord(t, lab, review)
	d = detailOf(t, h, r.ID)
	if d.View.Decision.Kind != DecisionDelivery || d.View.Decision.Primary == nil || d.View.Decision.Primary.Action != PrimaryPreview || d.View.Decision.Primary.Digest != "" || d.View.Decision.RequestChanges {
		t.Fatalf("review decision %+v", d.View.Decision)
	}
	if d.View.State.Label != "Ready to publish" || d.View.State.Tone != ToneNeeds {
		t.Errorf("review state %+v", d.View.State)
	}

	// Published and merged: nothing to do, the pull request, no primary.
	for lifecycle, label := range map[string]string{"OPEN": "Draft published", "MERGED": "Merged"} {
		writeReceipt(t, lab, r.ID, "published", lifecycle)
		d = detailOf(t, h, r.ID)
		if d.View.Decision.Kind != DecisionPublished || d.View.Decision.Primary != nil || d.View.Decision.Title != "Nothing to do" || d.View.Decision.PullURL != "https://github.com/o/r/pull/5" || d.View.State.Label != label {
			t.Errorf("%s: decision %+v state %+v", lifecycle, d.View.Decision, d.View.State)
		}
	}

	// Running, blocked and retired Molecules have no primary.
	for _, c := range []struct{ status, phase string }{{"running", "coding"}, {"blocked", "failed"}, {molecule.StatusRetired, "scope-waiting"}} {
		other := r
		other.Status = c.status
		other.Readiness = &readiness.State{SchemaVersion: "v1", Phase: c.phase, Plans: []readiness.Proposal{{Plan: viewPlan(r.Workspace.BaseCommit), Digest: digest}}}
		if c.status == molecule.StatusRetired {
			other.Retired = &molecule.Retirement{At: "2026-10-26T20:00:00Z", Reason: "stale", PriorStatus: "waiting"}
		}
		lab2, _ := labFixture(t)
		other.LabDir = lab2
		writeRecord(t, lab2, other)
		d := detailOf(t, testHandler(t, Options{LabDir: lab2}), other.ID)
		if d.View.Decision.Primary != nil || d.View.Decision.RequestChanges || d.View.Decision.Kind != DecisionNone {
			t.Errorf("%s: decision %+v", c.status, d.View.Decision)
		}
	}
}

// NEW and EDIT come from the frozen base tree only: a path absent from it is
// new, whatever its reason says; without a frozen tree there is no badge.
func TestMoleculeViewNewAndEditComeFromTheBaseTree(t *testing.T) {
	lab, r, _ := viewFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	files := detailOf(t, h, r.ID).View.Plan.Files
	if len(files) != 2 || files[0].Path != "lab/internal/ui/web/app.js" || files[0].Change != ChangeEdit || files[1].Path != "lab/internal/ui/estimate.go" || files[1].Change != ChangeNew {
		t.Fatalf("files %+v", files)
	}
	if files[0].Reason != "Rebuild the view" {
		t.Errorf("reason %q", files[0].Reason)
	}
	r.RepositoryEvidence = nil
	writeRecord(t, lab, r)
	for _, f := range detailOf(t, h, r.ID).View.Plan.Files {
		if f.Change != "" {
			t.Errorf("%s has a badge without a frozen tree: %q", f.Path, f.Change)
		}
	}
}

// Checks are grouped under human names, the plan's own first, then the Formula
// baseline as Full test suite, Go vet · gofmt and Repository hygiene, each with
// its exact command quoted as a shell reads it.
func TestMoleculeViewGroupsAndNamesChecks(t *testing.T) {
	lab, r, _ := viewFixture(t)
	plan := detailOf(t, testHandler(t, Options{LabDir: lab}), r.ID).View.Plan
	got := map[string][]string{}
	var order []string
	for _, g := range plan.CheckGroups {
		order = append(order, g.Name)
		for _, c := range g.Checks {
			got[g.Name] = append(got[g.Name], c.Command+" @"+c.Dir)
		}
		if g.Baseline == (g.Name == GroupCandidate) {
			t.Errorf("%s baseline %t", g.Name, g.Baseline)
		}
	}
	if want := []string{GroupCandidate, GroupFullSuite, GroupVet, GroupHygiene}; !reflect.DeepEqual(order, want) {
		t.Fatalf("groups %q, want %q", order, want)
	}
	want := map[string][]string{
		GroupCandidate: {"go test ./internal/ui/... ./internal/molecule/... @lab"},
		GroupFullSuite: {"go test -timeout 20m ./... @lab"},
		GroupVet:       {"go vet ./... @lab", `sh -c 'test -z "$(gofmt -l internal cmd)"' @lab`},
		GroupHygiene:   {"sh scripts/check-repo-hygiene.sh @", "sh scripts/check-repo-hygiene.test.sh @"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("checks %q, want %q", got, want)
	}
	if plan.ChecksTotal != 6 || plan.Questions != 1 || plan.BudgetMS != 1800000 || len(plan.Components) != 2 || plan.Components[0] != "lab/internal/ui" || plan.Components[1] != "lab/internal/ui/web" {
		t.Errorf("plan facts %+v", plan)
	}
	// A shell metacharacter in an argument is quoted, never run together.
	if got := shellJoin([]string{"sh", "-c", "a; b", "it's"}); got != `sh -c 'a; b' 'it'\''s'` {
		t.Errorf("shellJoin %q", got)
	}
}

var (
	rawDigest = regexp.MustCompile(`[0-9a-f]{40,}`)
	rawTime   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}`)
)

// The view's text carries no raw digest, ISO timestamp, Molecule id or phase
// code: only Plan.Digest (bound, shown short inside Plan details) and the
// times the browser formats are data.
func TestMoleculeViewHasNoRawDigestOrTimestampOutsideDetails(t *testing.T) {
	lab, r, digest := viewFixture(t)
	p := r.Readiness.Plans[0].Plan
	p.Analysis.Summary = "Builds on " + strings.Repeat("ef", 32) + " from 2026-10-25T08:00:00Z for mol-12-0123456789abcdef in " + lab
	p.Analysis.Scope[0].Reason = "Like commit 0123456789abcdef0123456789abcdef01234567 at 2026-10-24T10:00:00+02:00"
	p.Analysis.Acceptance[0].Description = "Matches assay-0123456789ab"
	p.OperatorNote = "Note " + strings.Repeat("a1", 32)
	r.Readiness.Plans[0].Plan = p
	writeRecord(t, lab, r)
	v := detailOf(t, testHandler(t, Options{LabDir: lab}), r.ID).View
	if v.Plan.Digest != digest || v.Plan.PlannedAt != "2026-10-26T18:46:00Z" {
		t.Fatalf("bound data %q %q", v.Plan.Digest, v.Plan.PlannedAt)
	}
	stripped := *v
	plan := *v.Plan
	plan.Digest, plan.PlannedAt = "", ""
	stripped.Plan = &plan
	stripped.Decision.Primary = nil
	raw, _ := json.Marshal(stripped)
	text := string(raw)
	for name, re := range map[string]*regexp.Regexp{"digest": rawDigest, "timestamp": rawTime, "Molecule id": rawID} {
		if m := re.FindString(text); m != "" {
			t.Errorf("the view carries a raw %s %q: %s", name, m, text)
		}
	}
	for _, code := range []string{"scope-waiting", "review-waiting", r.ID} {
		if strings.Contains(text, code) {
			t.Errorf("the view carries %q", code)
		}
	}
	noLeak(t, text)
	if !strings.Contains(v.Plan.Summary, "efefefe…") || !strings.Contains(v.Plan.Summary, "2026-10-25") || !strings.Contains(v.Plan.Summary, "<lab>") {
		t.Errorf("summary %q", v.Plan.Summary)
	}
}

// The stepper and the pill come from the Atoms, the phase and the receipt.
func TestMoleculeViewStepperAndStateInPlainWords(t *testing.T) {
	lab, r, _ := viewFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	states := func(v *MoleculeView) string {
		var s []string
		for _, st := range v.Steps {
			s = append(s, st.State)
		}
		return strings.Join(s, " ")
	}
	r.Atoms = inboxAtoms("done", "waiting", "pending", "pending", "pending", "pending")
	writeRecord(t, lab, r)
	v := detailOf(t, h, r.ID).View
	if v.Current != StepApproval || states(v) != "done current todo todo todo todo todo" || v.State.Label != "Waiting for your approval" || v.State.Tone != ToneNeeds {
		t.Fatalf("scope: current %d %q %+v", v.Current, states(v), v.State)
	}
	if v.Roles.Coding != "codex" || v.Roles.Model != "claude-opus-5-5" || v.Roles.Reviewer != "claude-code" {
		t.Errorf("roles %+v", v.Roles)
	}
	if c := v.PlanningCost; c == nil || c.USD == nil || *c.USD != 1.82 || c.Reported != 1 || c.Attempts != 2 || !c.Partial {
		t.Errorf("planning cost %+v", c)
	}

	r.Status = "running"
	r.Readiness.Phase = "coding"
	r.Atoms = inboxAtoms("done", "done", "active", "pending", "pending", "pending")
	// The pill counts from the current coding attempt, not the Atom's first start.
	r.Atoms[2].StartedAt = "2026-10-26T17:00:00Z"
	r.Atoms[2].Provenance = &molecule.AtomProvenance{Actions: []molecule.AtomAction{{AttemptID: "coding-1", StartedAt: "2026-10-26T19:00:00.5Z"}}}
	harnessAttempts := r.HarnessAttempts
	r.HarnessAttempts = []molecule.HarnessAttempt{{ID: "coding-1", PlanDigest: r.Readiness.Plans[0].Digest}}
	writeRecord(t, lab, r)
	r.HarnessAttempts = harnessAttempts
	v = detailOf(t, h, r.ID).View
	if v.Current != StepCoding || states(v) != "done done current todo todo todo todo" || v.State.Label != "Coding" || v.State.Since != "2026-10-26T19:00:00Z" || v.State.Tone != ToneRunning {
		t.Fatalf("coding: %q %+v", states(v), v.State)
	}

	r.Status = "blocked"
	r.Atoms = inboxAtoms("done", "done", "done", "failed", "pending", "pending")
	writeRecord(t, lab, r)
	v = detailOf(t, h, r.ID).View
	if v.Current != StepChecks || states(v) != "done done done blocked todo todo todo" || v.State.Label != "Blocked · Required checks did not pass" || v.State.Tone != ToneBlocked {
		t.Fatalf("blocked: %q %+v", states(v), v.State)
	}

	r.Status = "recorded"
	r.Readiness.Phase = "review-waiting"
	r.Atoms = inboxAtoms("done", "done", "done", "done", "done", "done")
	writeRecord(t, lab, r)
	writeReceipt(t, lab, r.ID, "published", "MERGED")
	v = detailOf(t, h, r.ID).View
	if v.Current != StepMerged || states(v) != "done done done done done done done" {
		t.Fatalf("merged: %d %q", v.Current, states(v))
	}
}

// The served page has the Molecule view inside Work: a breadcrumb, the pill,
// the steps, one live region, the Plan and Timeline tabs with the Timeline
// keeping the Atoms, evidence and logs, and the aside for the decision. The
// danger zone is built by the script, so the page keeps one danger button.
func TestMoleculeViewServedPage(t *testing.T) {
	page := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/")).Body.String()
	view := between(t, page, `<section id="selected" class="molecule-view"`, "</section>\n</div>")
	for _, want := range []string{
		`<nav class="breadcrumb" aria-label="Breadcrumb"><a id="mv-back" href="?view=work" data-view="work">Work</a>`,
		`<span id="mv-state" class="state-pill" data-tone="neutral">`,
		`<h1 id="mv-title" class="mv-title" tabindex="-1"></h1>`,
		`<ol id="mv-steps" class="mv-steps" aria-label="Progress"></ol>`,
		`<p id="mv-live" class="sr-only" role="status" aria-live="polite"></p>`,
		`role="tab" id="mv-tab-plan"`, `role="tab" id="mv-tab-timeline"`,
		`id="mv-timeline" class="glass run-panel mv-timeline" role="tabpanel"`,
		`<div id="atoms" class="atoms"></div>`, `<div id="readiness"></div>`, `<div id="attempts"></div>`, `id="yaml"`,
		"What Forgecell will do", "Files it may change", "Checks it must pass",
		`<details id="mv-details" class="mv-details">`, `<button type="button" id="mv-copy-digest"`,
		`<aside id="mv-aside" class="mv-aside" aria-label="Your decision">`, `<div id="mv-danger-slot"></div>`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the Molecule view lacks %s", want)
		}
	}
	if strings.Count(view, "aria-live") != 1 || strings.Contains(view, "btn-danger") || strings.Contains(view, "btn-primary") {
		t.Error("the Molecule view markup has more than one live region or a static action")
	}
	for _, gone := range []string{"GATES AND DECISIONS", "NEXT ACTION", `id="run-title"`} {
		if strings.Contains(page, gone) {
			t.Errorf("the page still has %s", gone)
		}
	}
}

// The served script renders the decision card's one primary bound to the shown
// digest, keeps Retire and Stop behind a disclosure and a confirmation naming
// the Molecule and apart from any primary, routes ?m= and Back through
// history, and never rewrites the Publish title while polling.
func TestMoleculeViewServedJS(t *testing.T) {
	js := servedJS(t)
	build := moduleFunction(t, js, "ensureMoleculeView")
	decision := between(t, build, "fill($('#mv-decision'),", ");")
	danger := between(t, build, "c.stopOpen =", "fill($('#mv-danger-slot'), c.runnerNote, c.danger);")
	if strings.Contains(decision, "stop") || strings.Contains(decision, "retire") || strings.Contains(decision, "danger") {
		t.Errorf("the decision card holds a destructive action: %s", decision)
	}
	if strings.Contains(danger, "btn-primary") || strings.Count(danger, "btn-danger") != 4 {
		t.Errorf("the danger zone has a primary or a stray danger button:\n%s", danger)
	}
	// Three primaries are built (approve, preview, publish); renderDecision shows
	// one at a time.
	if n := strings.Count(build, "btn-primary"); n != 3 {
		t.Errorf("%d primaries built", n)
	}
	for name, wants := range map[string][]string{
		"ensureMoleculeView": {
			"if (mv.controls) return mv.controls;",
			"c.approveForm = el('form', { class: 'decision-form', id: 'mv-approve-form', novalidate: true, hidden: true, onsubmit: submitApprove }, c.approve);",
			"'Request changes…'",
			"c.amendForm = el('form', { class: 'amend-form', id: 'mv-amend-form'",
			"name: 'acceptance', maxlength: '1000', autocomplete: 'off', placeholder: 'The help text names the new flag…', 'aria-describedby': 'mv-amend-error'",
			"name: 'title', maxlength: '256', autocomplete: 'off', placeholder: 'Add the Molecule view to the Lab UI…', 'aria-describedby': 'mv-publish-error', dataset: { gate: 'title' }, oninput: (e) => { state.gate.title = e.target.value; } }",
			"el('summary', null, 'Not needed anymore?')",
			"onclick: () => openConfirm('retire') }, 'Retire this Molecule…')",
			"onclick: () => openConfirm('stop') }, 'Stop this Molecule…')",
			"onsubmit: submitRetire", "onsubmit: submitStop",
		},
		"renderDecision": {
			"const digest = readOnly ? '' : boundDigest(d);",
			"c.approveForm.hidden = !scope;",
			"if (scope) c.approveForm.dataset.digest = digest;",
			"c.approve.disabled = busy;",
			"setAttr(c.preview, 'class', preview ? 'btn btn-secondary btn-decision-secondary' : 'btn btn-primary btn-decision');",
			"c.publishForm.hidden = !preview;",
			"setText(c.retireQuestion, 'Retire ' + moleculeName(d) + '?');",
			"setText(c.stopQuestion, 'Stop ' + moleculeName(d) + '?');",
			"hasAction('request plan changes')",
			"if (gate.message && gate.announce) { gate.announce = false; target.focus(); }",
			// Stop only when the server says this Lab UI runs the engine call;
			// otherwise the runner's plain text replaces it.
			"const stoppable = live && Boolean(control && control.stoppable === true);",
			"const canStop = !readOnly && (stoppable || Boolean(gate.stop)) && hasAction('stop Molecule');",
			"c.stopOpen.hidden = !stoppable || Boolean(gate.stop);",
			"setText(c.runnerNote, runner);",
		},
		"submitGate":    {"if (!res.ok && action === '/amend' && body && body.category === 'stale') {", "'A newer plan exists, so your changes were not recorded."},
		"withAttempt":   {"if (RERUN_NOTES.includes(note)) parts.push(note);"},
		"boundDigest":   {"digest === plan.digest && digest === g.digest && digest === latestDigest(d)"},
		"submitApprove": {"e.preventDefault();", "const digest = mv.controls.approveForm.dataset.digest;", "if (digest) approveScope(digest);"},
		"requestChanges": {
			"addAcceptance: acceptance ? [acceptance] : [], addScope: path ? [{ path: path, reason: reason }] : []",
			"submitGate('/amend', JSON.stringify(body))",
		},
		"submitRetire":      {"e.preventDefault();", "retireMolecule();"},
		"openConfirm":       {"c[kind + 'Form'].hidden = false;"},
		"viewFromURL":       {"if (!view && moleculeFromURL()) return 'work';"},
		"restoreSelection":  {"else if (!id && state.selectedId) overview();", "if (VIEWS.includes(from)) mv.from = from;"},
		"openMolecule":      {"rememberFrom(from);"},
		"navigate":          {"if (moleculeFromURL()) history.pushState({ view: view }, '', '?view=' + view);", "ctx.restoreSelection();"},
		"renderHeader":      {"whenText(plan.plannedAt)", "dateTitle(plan.plannedAt)"},
		"renderPlanDetails": {"el('code', null, digest.slice(0, 7))", "setText($('#mv-digest-short'), digest ? short(digest) : 'Not recorded');"},
		"renderTimeline":    {"if (panel.contains(document.activeElement) && mv.timelineId === d.id) { mv.timelineDirty = true; return; }"},
		"money":             {"value > 0 && value < 0.01 ? 'under $0.01' : dollars.format(value)"},
		"fillKeyed":         {"if (node.dataset.key === key) return;"},
		"showMoleculeView":  {"$('.shelf').hidden = open;"},
		"renderPlan":        {"if (key === mv.planKey) return;"},
	} {
		body := moduleFunction(t, js, name)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	// A poll never rewrites a typed value or rebuilds the card.
	render := moduleFunction(t, js, "renderDecision")
	for _, gone := range []string{"title.value", "reasonInput.value", "acceptance.value", "fill($('#mv-decision')", "short(", "innerHTML"} {
		if strings.Contains(render, gone) {
			t.Errorf("renderDecision has %q", gone)
		}
	}
	// No raw digest or timestamp text is set outside Plan details.
	for _, name := range []string{"renderHeader", "renderDecision", "renderPlan", "fileItem", "groupItem", "renderRoles"} {
		body := moduleFunction(t, js, name)
		if regexp.MustCompile(`(setText|el)\([^;]*(\.digest\b[^)]*\)|plannedAt\))`).MatchString(strings.ReplaceAll(strings.ReplaceAll(body, "whenText(plan.plannedAt)", ""), "dateTitle(plan.plannedAt)", "")) {
			t.Errorf("%s renders a digest or a raw time", name)
		}
	}
	if !strings.Contains(js, "'Committed: the ledger records your approval of this plan. Coding starts on its own.'") || strings.Contains(js, "approval of plan ' + short(") {
		t.Error("the approval read-back still shows a digest")
	}
	if !strings.Contains(js, "const GATE_FORMS = { '/amend': 'amend', '/delivery/publish': 'publish', '/stop': 'stop', '/retire': 'retire' };") {
		t.Error("gate outcomes are not shown in the form that sent them")
	}
}

// The served CSS gives the decision primary 48px, keeps the decision aside
// sticky, stacks the view on narrow screens and keeps 44px targets and forced
// colors for its controls.
func TestMoleculeViewServedCSS(t *testing.T) {
	css := servedCSS(t)
	rules := cssRules(css)
	wantDecls(t, rules, ".btn-decision", "height: 48px;", "border-radius: 14px;")
	wantDecls(t, rules, ".mv-aside", "position: sticky;")
	wantDecls(t, rules, ".mv-steps", "grid-template-columns: repeat(7, minmax(0, 1fr));")
	wantDecls(t, rules, ".file-badge.is-new", "background: #dcefe9;")
	wantDecls(t, rules, ".danger-zone", "border: 1px solid #eccfc6;")
	wantDecls(t, rules, ".state-pill[data-tone=needs]", "background: var(--needs-bg);")
	wantDecls(t, rules, ".shell.has-molecule .side-panel", "display: none;")
	wantDecls(t, cssRules(mediaBlocks(t, css, "@media (max-width: 1199px)")), ".mv-layout", "grid-template-columns: minmax(0, 1fr);")
	mobile := cssRules(mediaBlocks(t, css, "@media (max-width: 720px)"))
	wantDecls(t, mobile, ".check-grid", "grid-template-columns: minmax(0, 1fr);")
	wantDecls(t, mobile, ".file", "grid-template-columns: minmax(0, 1fr);")
	wantDecls(t, cssRules(mediaBlocks(t, css, "@media (pointer: coarse)")), ".mv-tab, .btn-small, .breadcrumb a, .accept-label, .preview-info .text-button", "min-height: 44px;")
	forced := cssRules(mediaBlocks(t, css, "@media (forced-colors: active)"))
	wantDecls(t, forced, ".mv-tab[aria-selected=true]", "background: Highlight;")
	wantDecls(t, forced, ".mv-step.is-done .step-bar, .mv-step.is-current .step-bar, .mv-step.is-blocked .step-bar", "background: Highlight;")
	if bad := uncoveredOutlineResets(css); len(bad) != 0 {
		t.Errorf("outline resets without a ring: %q", bad)
	}
}
