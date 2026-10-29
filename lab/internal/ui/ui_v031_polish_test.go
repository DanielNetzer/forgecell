package ui

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// panelStates reads the statuses and phases actionsState accepts from the
// served JS; its body must be nothing but those comparisons.
func panelStates(t *testing.T, js string) (statuses, phases []string) {
	t.Helper()
	body := strings.TrimSpace(moduleFunction(t, js, "actionsState"))
	if !regexp.MustCompile(`^return d\.(status|phase) === '[a-z-]+'( \|\| d\.(status|phase) === '[a-z-]+')*; \}$`).MatchString(body) {
		t.Fatalf("actionsState is not a plain list of states: %q", body)
	}
	for _, m := range regexp.MustCompile(`d\.(status|phase) === '([a-z-]+)'`).FindAllStringSubmatch(body, -1) {
		if m[1] == "status" {
			statuses = append(statuses, m[2])
		} else {
			phases = append(phases, m[2])
		}
	}
	return statuses, phases
}

// panelShown mirrors renderActions's visibility rule, which the test pins
// verbatim, for one served detail and its served actions.
func panelShown(statuses, phases []string, status, phase string, kinds []string, notOffered int, blocking string) bool {
	return panelShownHolding(statuses, phases, status, phase, kinds, notOffered, blocking, false)
}

// panelShownHolding is panelShown with a held action result or message: it
// counts only while the Molecule is stuck.
func panelShownHolding(statuses, phases []string, status, phase string, kinds []string, notOffered int, blocking string, held bool) bool {
	stuck := slices.Contains(statuses, status) || slices.Contains(phases, phase) || notOffered > 0
	for _, k := range kinds {
		if k != "retire" {
			stuck = true
		}
	}
	return stuck && (len(kinds) > 0 || notOffered > 0 || blocking != "" || held)
}

// panelOutcome mirrors forget: the held outcome survives only while the
// Molecule's actionsKey (id, status, phase, lane, latest digest) is the one
// it was recorded for.
func panelOutcome(seen, key []string, held bool) bool {
	return held && slices.Equal(seen, key)
}

// The panel's held result and message are dropped whenever actionsKey
// changes, and never show the panel on their own: an accept-failures result,
// then the Molecule moves on to waiting for approval or to published, hides
// the panel.
func TestUIActionsPanelDropsStaleOutcome(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	js := do(t, h, authed("/js/molecule-actions.js")).Body.String()
	// The panel is its own sub-module: it imports only the core, and the
	// decision card imports it.
	imports := importRe.FindAllStringSubmatch(js, -1)
	if len(imports) == 0 {
		t.Fatal("molecule-actions.js imports nothing")
	}
	for _, m := range imports {
		if m[1] != "./core.js" && !strings.HasPrefix(m[1], "./core-") {
			t.Errorf("molecule-actions.js imports %s, not the core", m[1])
		}
	}
	if decision := do(t, h, authed("/js/molecule-decision.js")).Body.String(); !strings.Contains(decision, "from './molecule-actions.js';") || strings.Contains(decision, "function renderActions(") {
		t.Error("molecule-decision.js does not take the panel from molecule-actions.js")
	}
	for name, wants := range map[string][]string{
		"actionsKey":    {"return [d.id, d.status, d.phase, d.lane, latestDigest(d)].join(':');"},
		"forget":        {"const seen = actionsKey(d);", "if (acts.seen !== seen) { acts.seen = seen; acts.result = null; acts.message = ''; }"},
		"loadActions":   {"forget(d);"},
		"renderActions": {"forget(d);", "const shown = Boolean(stuck && (list.length || notOffered.length || blocking || acts.result || acts.message));", "node.hidden = !shown;"},
	} {
		body := moduleFunction(t, js, name)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	// forget runs before anything in renderActions reads the held outcome.
	render := moduleFunction(t, js, "renderActions")
	if strings.Index(render, "forget(d);") > strings.Index(render, "acts.result") {
		t.Error("renderActions reads acts.result before forget(d)")
	}
	statuses, phases := panelStates(t, js)
	digest := strings.Repeat("a", 64)
	blocked := []string{"mol-1", "blocked", "check-pending", "ready", digest}
	// Right after accept-failures: the same state, offered actions, the result
	// held. The panel shows it.
	held := panelOutcome(blocked, blocked, true)
	if !panelShownHolding(statuses, phases, "blocked", "check-pending", []string{"accept-failures", "retire"}, 0, "", held) {
		t.Error("the accept-failures result is not shown on the blocked Molecule")
	}
	for _, next := range []struct {
		name, status, phase, lane string
	}{
		{"waiting for approval of the new plan", "waiting", "scope-waiting", "decision"},
		{"published", "completed", "published", "done"},
	} {
		key := []string{"mol-1", next.status, next.phase, next.lane, strings.Repeat("b", 64)}
		held := panelOutcome(blocked, key, true)
		if held {
			t.Errorf("%s: the result survives an actionsKey change", next.name)
		}
		if panelShownHolding(statuses, phases, next.status, next.phase, []string{"retire"}, 0, "", held) {
			t.Errorf("%s: the panel is shown", next.name)
		}
		// Even a result that somehow survived never shows the panel there.
		if panelShownHolding(statuses, phases, next.status, next.phase, []string{"retire"}, 0, "", true) {
			t.Errorf("%s: a held result shows the panel on its own", next.name)
		}
		if panelShownHolding(statuses, phases, next.status, next.phase, nil, 0, "", true) {
			t.Errorf("%s: a held result shows an empty panel", next.name)
		}
	}
}

// Item 2: "Why it's blocked · What you can do" shows only for a blocked,
// failed, interrupted or scope-change Molecule, or one offered more than
// Retire. A Molecule waiting for plan approval or ready to publish, offered
// only Retire, keeps it hidden.
func TestUIActionsPanelOnlyForStuckMolecules(t *testing.T) {
	js := servedJS(t)
	render := moduleFunction(t, js, "renderActions")
	for _, want := range []string{
		"const stuck = actionsState(d) || list.some((a) => a.kind !== 'retire') || notOffered.length > 0;",
		"const shown = Boolean(stuck && (list.length || notOffered.length || blocking || acts.result || acts.message));",
		"node.hidden = !shown;",
	} {
		if !strings.Contains(render, want) {
			t.Fatalf("renderActions lacks %q", want)
		}
	}
	statuses, phases := panelStates(t, js)
	for _, want := range []string{"blocked", "failed"} {
		if !slices.Contains(statuses, want) {
			t.Errorf("actionsState misses status %q", want)
		}
	}
	for _, want := range []string{"interrupted", "scope-change"} {
		if !slices.Contains(phases, want) {
			t.Errorf("actionsState misses phase %q", want)
		}
	}
	for _, gate := range []string{"waiting", "completed", "running", "scope-waiting", "review-waiting", "check-pending"} {
		if slices.Contains(statuses, gate) || slices.Contains(phases, gate) {
			t.Errorf("actionsState shows the panel for %q", gate)
		}
	}

	// Handler fixtures: each state as the server serves its detail and actions.
	lab, r, _ := gateFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	type servedState struct {
		status, phase string
		kinds         []string
		notOffered    int
		blocking      string
	}
	read := func(name string) servedState {
		t.Helper()
		d := detailOf(t, h, r.ID)
		var v ActionsView
		decode(t, do(t, h, authed("/api/molecules/"+r.ID+"/actions")), &v)
		s := servedState{status: d.Status, phase: d.Phase, notOffered: len(v.NotOffered), blocking: v.Blocking}
		for _, a := range v.Actions {
			s.kinds = append(s.kinds, a.Kind)
		}
		t.Logf("%s: %+v", name, s)
		return s
	}
	set := func(status, phase string) {
		t.Helper()
		next := r
		next.Status = status
		state := *r.Readiness
		state.Phase = phase
		next.Readiness = &state
		writeRecord(t, lab, next)
	}
	for _, c := range []struct {
		name, status, phase string
		shown               bool
	}{
		{"waiting for plan approval", "waiting", "scope-waiting", false},
		{"ready to publish", "completed", "review-waiting", false},
		{"blocked", "blocked", "check-pending", true},
		{"failed", "failed", "failed", true},
		{"interrupted", "blocked", "interrupted", true},
		{"scope-change", "blocked", "scope-change", true},
	} {
		set(c.status, c.phase)
		s := read(c.name)
		if s.status != c.status || s.phase != c.phase {
			t.Fatalf("%s: served status %q phase %q", c.name, s.status, s.phase)
		}
		if !c.shown && !slices.Equal(s.kinds, []string{"retire"}) {
			t.Fatalf("%s: the fixture must offer only Retire, got %v", c.name, s.kinds)
		}
		// A stuck state shows the panel whatever the engine offers, Retire alone
		// included.
		kinds := s.kinds
		if c.shown && len(kinds) == 0 && s.blocking == "" {
			kinds = []string{"retire"}
		}
		if got := panelShown(statuses, phases, s.status, s.phase, kinds, s.notOffered, s.blocking); got != c.shown {
			t.Errorf("%s: panel shown %v, want %v (%+v)", c.name, got, c.shown, s)
		}
	}
	// A served recover action shows the panel even on a ledger left running.
	if !panelShown(statuses, phases, "running", "coding", []string{"recover"}, 0, "") {
		t.Error("a recover action does not show the panel")
	}
	if panelShown(statuses, phases, "failed", "failed", nil, 0, "") {
		t.Error("an empty panel is shown")
	}
}

var servedPlaceholder = regexp.MustCompile(`placeholder(?:: '|=")([^'"]*)['"]`)

// Item 3: no placeholder or inline hint the page serves names Forgecell's own
// repository; neutral examples replace them.
func TestUIPlaceholdersAreNeutral(t *testing.T) {
	js := servedJS(t)
	page := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/")).Body.String()
	var all []string
	for _, text := range []string{js, page} {
		for _, m := range servedPlaceholder.FindAllStringSubmatch(text, -1) {
			all = append(all, m[1])
		}
	}
	if len(all) < 5 {
		t.Fatalf("only %d placeholders found: %v", len(all), all)
	}
	leaks := []string{"lab/", "internal/", "app.js", ".go", "web/", "Molecule view", "Lab UI", "Wire the new button", "forgecell", "Forgecell"}
	for _, p := range all {
		for _, leak := range leaks {
			if strings.Contains(p, leak) {
				t.Errorf("placeholder %q names %q", p, leak)
			}
		}
	}
	for _, gone := range []string{"lab/internal/ui/web/app.js", "Add the Molecule view to the Lab UI", "Wire the new button"} {
		if strings.Contains(js, gone) {
			t.Errorf("the served JS still holds %q", gone)
		}
	}
	for _, want := range []string{"placeholder: 'path/to/file…'", "placeholder: 'Describe the change in a few words…'", "such as path/to/file.'"} {
		if !strings.Contains(js, want) {
			t.Errorf("the served JS lacks %q", want)
		}
	}
}

var (
	inputID    = regexp.MustCompile(`el\('input', \{[^}]*?\bid: ([^,}]+?)[,}]`)
	inputAny   = regexp.MustCompile(`el\('input', \{`)
	labelFor   = regexp.MustCompile(`el\('label', \{[^}]*?\bfor: ([^,}]+?)\s*[,}]`)
	fieldCall  = regexp.MustCompile(`\bfield\(([^,]+),`)
	labelOpen  = regexp.MustCompile(`el\('label', \{`)
	idProducer = regexp.MustCompile(`^(ids\.[a-z]+|id|fieldId\('[a-z-]+'( \+ [a-z.]+)?\))$`)
)

// Item 4: every input the Molecule view and Set up your Lab build has an id a
// label binds with for, from a useId-style uid(); the publish checkbox is named
// by its label, not "on".
func TestUIEveryInputHasABoundLabel(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	core := do(t, h, authed("/js/core.js")).Body.String()
	if !strings.Contains(core, "export function uid(prefix) { uidCount += 1;") {
		t.Fatal("core.js exports no uid() helper")
	}
	for _, path := range []string{"/js/molecule-decision.js", "/js/molecule-actions.js", "/js/setup.js"} {
		js := do(t, h, authed(path)).Body.String()
		if !strings.Contains(js, "uid") {
			t.Errorf("%s does not take ids from uid()", path)
		}
		inputs := inputID.FindAllStringSubmatch(js, -1)
		if n := len(inputAny.FindAllString(js, -1)); n != len(inputs) || n == 0 {
			t.Fatalf("%s: %d inputs, %d with an id", path, n, len(inputs))
		}
		bound := map[string]bool{}
		for _, m := range labelFor.FindAllStringSubmatch(js, -1) {
			bound[m[1]] = true
		}
		if len(labelOpen.FindAllString(js, -1)) != len(labelFor.FindAllString(js, -1)) {
			t.Errorf("%s: a label has no for", path)
		}
		for _, m := range fieldCall.FindAllStringSubmatch(js, -1) {
			bound[m[1]] = true
		}
		for _, m := range inputs {
			id := strings.TrimSpace(m[1])
			if !bound[id] {
				t.Errorf("%s: input id %s has no label bound to it", path, id)
			}
			// The retire reason keeps its fixed, unique id; every other one is
			// a uid() id.
			if id != "'mv-retire-reason'" && !idProducer.MatchString(id) {
				t.Errorf("%s: input id %s is not from uid()", path, id)
			}
		}
	}
	js := servedJS(t)
	for _, want := range []string{
		"const ids = { acceptance: uid('mv-amend-acceptance'), path: uid('mv-amend-path'), reason: uid('mv-amend-reason'), title: uid('mv-publish-title'), accept: uid('mv-publish-accept') };",
		"el('label', { class: 'accept-label', for: ids.accept }, c.accept, el('span', null, 'I checked the acceptance criteria myself'))",
		"field(ids.title, 'Draft PR title', c.title)",
		"field(ids.acceptance, 'Add an acceptance note', c.acceptance)",
		"acts.ids.set(name, uid('mv-action'));",
		"fieldIds.set(key, uid('setup-field'));",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the served JS lacks %q", want)
		}
	}
	// Ids stay bounded and stable: no uid() call inside a render function.
	for _, name := range []string{"renderDecision", "renderActions", "actionItem", "chooseCard", "agentCard", "reviewCard"} {
		if strings.Contains(moduleFunction(t, js, name), "uid(") {
			t.Errorf("%s calls uid() on every render", name)
		}
	}
}

// Item 5: Set up your Lab is one column at every width, so its cards never
// fall into the navigation column. The operator checks the look at ~800px.
func TestUISetupShellIsOneColumn(t *testing.T) {
	css := servedCSS(t)
	if !strings.Contains(css, "body.setup-mode .shell { grid-template-columns: minmax(0, 1fr); }") {
		t.Error("the setup view keeps the navigation column")
	}
	if !strings.Contains(css, ".setup { display: grid; gap: 16px; width: 100%; max-width: 760px; min-width: 0; }") {
		t.Error("the setup view does not take the available width")
	}
}
