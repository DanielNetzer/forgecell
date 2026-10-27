package ui

import (
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var (
	cssComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	cssRuleRe  = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	outlineOff = regexp.MustCompile(`outline:\s*(none|0)\s*(;|$)`)
)

// cssRules maps each plain rule's selector list to its declarations, joined
// across repeated selectors (the media queries included).
func cssRules(css string) map[string]string {
	rules := map[string]string{}
	for _, m := range cssRuleRe.FindAllStringSubmatch(cssComment.ReplaceAllString(css, ""), -1) {
		selector := strings.TrimSpace(m[1])
		rules[selector] += strings.TrimSpace(m[2]) + " "
	}
	return rules
}

// outlineCovers lists each selector allowed to reset its outline, with the rule
// that draws its replacement focus ring.
var outlineCovers = map[string]string{".grid-search input": ".grid-search:focus-within"}

// uncoveredOutlineResets returns every rule that removes an outline without a
// focus-visible or focus-within replacement drawing a real ring.
func uncoveredOutlineResets(css string) []string {
	rules := cssRules(css)
	var bad []string
	for selector, decls := range rules {
		if !outlineOff.MatchString(decls) {
			continue
		}
		cover, ok := outlineCovers[selector]
		if !ok || !strings.Contains(cover, ":focus") || !strings.Contains(rules[cover], "outline: 2px solid") {
			bad = append(bad, selector)
		}
	}
	return bad
}

// mediaBlock returns the body of the top-level at-rule that starts with head.
func mediaBlock(t *testing.T, css, head string) string {
	t.Helper()
	i := strings.Index(css, head+" {")
	if i < 0 {
		t.Fatalf("style.css has no %s block", head)
	}
	rest := css[i+len(head)+2:]
	j := strings.Index(rest, "\n}\n")
	if j < 0 {
		t.Fatalf("%s block is not closed", head)
	}
	return rest[:j]
}

func wantDecls(t *testing.T, rules map[string]string, selector string, decls ...string) {
	t.Helper()
	got, ok := rules[selector]
	if !ok {
		t.Errorf("style.css has no %q rule", selector)
		return
	}
	for _, d := range decls {
		if !strings.Contains(got, d) {
			t.Errorf("%s lacks %q: %s", selector, d, got)
		}
	}
}

// The served CSS defines the .btn system, the lane bar, the sharper glass and
// the radius scale, styles the Start Work inputs, and never removes an outline
// without a replacement focus ring.
func TestUIServedCSSPolish(t *testing.T) {
	res := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/css/base.css"))
	if res.Code != http.StatusOK || !strings.HasPrefix(res.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("base.css: %d %v", res.Code, res.Header())
	}
	css := servedCSS(t)
	rules := cssRules(css)

	wantDecls(t, rules, ":root", "--forest: #183e32;", "--radius-panel: 20px;", "--radius-card: 14px;", "--radius-control: 12px;",
		"--glass: #fbfcf6e6;", "--glass-solid: #fbfcf6;", "--shadow-glass: inset 0 1px 0 #ffffffcc, 0 1px 2px #1a3a2a14, 0 12px 32px #1a3a2a12;")

	// Buttons.
	wantDecls(t, rules, ".btn", "height: 40px;", "border-radius: var(--radius-control);", "border: 1px solid")
	wantDecls(t, rules, ".btn-primary", "background: var(--forest);", "color: var(--paper);", "border-color: var(--forest);", "box-shadow: inset 0 1px 0")
	wantDecls(t, rules, ".btn-primary:active:not(:disabled)", "transform: translateY(1px);")
	wantDecls(t, rules, ".btn-secondary", "background: var(--paper);", "color: var(--forest);", "border-color: var(--forest);")
	wantDecls(t, rules, ".btn:disabled, .btn[aria-disabled=true]", "cursor: not-allowed;")
	if got := strings.TrimSpace(rules[".text-button"]); got != "border: 0; background: transparent; color: inherit; padding: 8px; font-size: 12px; text-decoration: underline; text-underline-offset: 4px;" {
		t.Errorf(".text-button was restyled globally: %q", got)
	}

	// Start Work inputs and stepper.
	wantDecls(t, rules, ".drive-field input, .stepper input", "height: 40px;", "border-radius: var(--radius-control);", "border: 1px solid var(--edge);", "box-shadow: inset 0 1px")
	wantDecls(t, rules, ".drive-field input:hover, .stepper input:hover", "border-color: var(--edge-strong);")
	wantDecls(t, rules, ".drive-field input:focus-visible, .stepper input:focus-visible", "outline: 2px solid var(--focus);", "outline-offset: 2px;")
	wantDecls(t, rules, ".drive-field input::placeholder", "color: var(--muted);")
	wantDecls(t, rules, ".stepper input", "font-variant-numeric: tabular-nums;")
	wantDecls(t, rules, ".drive-help", "max-width: 70ch;", "color: var(--muted);")
	wantDecls(t, rules, ".drive-groups", "display: grid;")

	// Lane bar.
	wantDecls(t, rules, ".lane-bar", "border-radius: 999px;", "padding: 4px;", "border: 1px solid var(--edge);", "overflow-x: auto;", "scroll-snap-type: x", "min-width: 0;", "max-width: 100%;")
	wantDecls(t, rules, ".lane-segment", "scroll-snap-align: start;", "border-radius: 999px;")
	wantDecls(t, rules, ".lane-segment:hover", "background:")
	wantDecls(t, rules, ".lane-segment[aria-pressed=true]", "background: var(--paper);", "color: var(--forest);", "box-shadow:")
	wantDecls(t, rules, ".lane-count", "font-variant-numeric: tabular-nums;", "border-radius: 999px;")
	wantDecls(t, rules, ".lane-dot", "border-radius: 50%;")
	wantDecls(t, rules, ".lane-divider", "width: 1px;")
	for _, lane := range []string{"running", "decision", "blocked", "finished"} {
		if _, ok := rules[".lane-dot-"+lane]; !ok {
			t.Errorf("style.css has no dot for lane %s", lane)
		}
	}
	wantDecls(t, rules, ".lane-dot-other, .lane-dot-retired", "background:")
	if strings.Contains(css, "#235b43") || strings.Contains(css, ".filter-chip") {
		t.Error("style.css keeps the old solid-fill filter chips")
	}
	mobile := cssRules(mediaBlocks(t, css, "@media (max-width: 720px)"))
	wantDecls(t, mobile, ".lane-segment", "min-height: 44px;")
	wantDecls(t, mobile, ".grid-search", "width: 100%;")

	// Glass and inner cards.
	wantDecls(t, rules, ".glass", "background: var(--glass-solid);", "border: 1px solid", "border-radius: var(--radius-panel);", "box-shadow: var(--shadow-glass);")
	if !strings.Contains(css, "@supports (backdrop-filter: blur(20px)) { .glass { background: var(--glass); backdrop-filter: blur(20px) saturate(160%); } }") {
		t.Error("style.css lacks the translucent glass behind @supports, over an opaque fallback")
	}
	for _, card := range []string{".card", ".active-formula", ".drive-panel"} {
		wantDecls(t, rules, card, "background: var(--paper);", "border: 1px solid var(--card-edge);", "border-radius: var(--radius-card);")
	}
	if !strings.Contains(rules[".review-row"], "background: var(--paper); border: 1px solid var(--card-edge); border-radius: var(--radius-card);") {
		t.Errorf(".review-row is not a crisp paper card: %s", rules[".review-row"])
	}

	// Focus, motion and forced colors.
	if bad := uncoveredOutlineResets(css); len(bad) != 0 {
		t.Errorf("style.css removes outlines without a focus replacement: %q", bad)
	}
	if !strings.Contains(rules["button:focus-visible, a:focus-visible, summary:focus-visible, [tabindex]:focus-visible, .motion-option input:focus-visible"], "outline: 2px solid var(--focus);") {
		t.Error("style.css lost the global focus-visible ring")
	}
	if !strings.Contains(mediaBlocks(t, css, "@media (prefers-reduced-motion: reduce)"), "transition: none !important;") {
		t.Error("reduced motion no longer disables transitions")
	}
	// Each sheet keeps the forced-colors edge of its own controls, after the
	// rules it overrides.
	forced := cssRules(mediaBlocks(t, css, "@media (forced-colors: active)"))
	for _, selector := range []string{".btn, .drive-field input, .stepper input", ".stepper-button", ".lane-segment, .grid-search, .lane-bar"} {
		wantDecls(t, forced, selector, "border: 1px solid ButtonText;")
	}
	wantDecls(t, forced, ".lane-segment[aria-pressed=true]", "background: Highlight;", "color: HighlightText;")
}

// The outline check fails on a reset with no replacement ring, and on a known
// selector whose replacement ring was removed.
func TestUIOutlineResetCheckFailsWithoutReplacement(t *testing.T) {
	for name, css := range map[string]string{
		"no cover":      ".x { outline: none; }\n.x:focus { color: red; }",
		"zero":          ".y { border: 0; outline: 0 }",
		"cover lost":    ".grid-search input { outline: 0; }",
		"cover no ring": ".grid-search input { outline: 0; }\n.grid-search:focus-within { color: red; }",
	} {
		if bad := uncoveredOutlineResets(css); len(bad) != 1 {
			t.Errorf("%s: got %q, want one uncovered reset", name, bad)
		}
	}
	if bad := uncoveredOutlineResets(".grid-search:focus-within { outline: 2px solid #000; }\n.grid-search input { outline: 0; }"); len(bad) != 0 {
		t.Errorf("a covered reset was reported: %q", bad)
	}
}

func jsFunction(t *testing.T, js, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)function ` + name + `\([^)]*\) \{(.*?)\n  \}\n`).FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("app.js has no %s", name)
	}
	return m[1]
}

// The served script renders each lane filter as a <button> with aria-pressed
// and the same ids, with no Paused chip, and composes Start Work from labelled
// inputs, a datalist on the selector and a labelled stepper bounded 1..max,
// keeping the drive validation and pending semantics.
func TestUIServedJSStartWorkAndLaneBar(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	js := servedJS(t)

	filters := moduleTable(t, js, "FILTERS", "]")
	var ids []string
	for _, m := range regexp.MustCompile(`\['([a-z]+)', '`).FindAllStringSubmatch(filters, -1) {
		ids = append(ids, m[1])
	}
	if want := []string{"all", "running", "decision", "blocked", "finished", "other", "retired"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("filter ids %q, want %q", ids, want)
	}
	bar := moduleFunction(t, js, "renderFilters")
	for _, want := range []string{
		"fill($('#run-filters'), FILTERS.flatMap(",
		"el('button', {",
		"type: 'button', class: 'lane-segment', dataset: { filter: id }, 'aria-pressed': String(state.filter === id),",
		`onclick: () => { hideTip(); state.filter = id; renderFilters(); applyFilter(); $('[data-filter="' + id + '"]').focus({ preventScroll: true }); },`,
		"id === RETIRED ? el('span', { class: 'lane-divider', 'aria-hidden': 'true' }) : null,",
		"el('span', { class: 'lane-dot lane-dot-' + id, 'aria-hidden': 'true' })",
		"el('span', { class: 'lane-count' }, String(id === 'all' ? counts.all || 0 : counts[id] || 0))",
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("renderFilters lacks %q", want)
		}
	}
	for _, gone := range []string{"paused", "Unavailable", "disabled"} {
		if strings.Contains(bar, gone) {
			t.Errorf("renderFilters still has %q", gone)
		}
	}

	// Run the loop and Start intake are real forms (#250): labelled inputs with a
	// name, autocomplete off and an example placeholder, a segmented scope
	// control, the labelled stepper bounded 1..max and submit buttons, so Enter
	// submits. Stop loop is no longer beside the primary; it is behind the header
	// pill's disclosure and confirmation.
	panel := moduleFunction(t, js, "ensureDrive")
	for _, want := range []string{
		"el('label', { class: 'field-label', for: 'drive-issue' }, 'Issue number'), issue)",
		"type: 'text', id: 'drive-issue', name: 'issue', inputmode: 'numeric', maxlength: '10', autocomplete: 'off', placeholder: '245…', 'aria-describedby': 'drive-run-error'",
		"type: 'text', id: 'drive-value', name: 'value', maxlength: '200', autocomplete: 'off', spellcheck: 'false', placeholder: 'v1…', 'aria-describedby': 'drive-loop-error'",
		"el('label', { class: 'field-label', for: 'drive-max' }, 'At once')",
		"type: 'number', id: 'drive-max', name: 'maxConcurrent', min: '1', max: '8', step: '1', value: '1', inputmode: 'numeric', autocomplete: 'off', 'aria-describedby': 'drive-loop-error', dataset: { drive: 'max' }",
		"el('div', { class: 'segmented', role: 'group', 'aria-label': 'Which Issues' }, modes)",
		"el('button', { type: 'button', class: 'seg', 'aria-pressed': String(mode === drive.mode), dataset: { drive: 'mode-' + mode }, onclick: () => chooseMode(mode) }, m.label)",
		"'aria-label': 'Fewer at once', 'aria-controls': 'drive-max', dataset: { drive: 'fewer' }, onclick: () => step(-1)",
		"'aria-label': 'More at once', 'aria-controls': 'drive-max', dataset: { drive: 'more' }, onclick: () => step(1)",
		"max.value = String(Math.max(1, Math.min(Number.isInteger(limit) ? limit : 8, next)));",
		"el('button', { type: 'submit', class: 'btn btn-primary drive-submit', dataset: { drive: 'start' } }, 'Start loop')",
		"el('button', { type: 'submit', class: 'btn btn-secondary', dataset: { drive: 'run' } }, 'Start intake')",
		"el('form', { class: 'drive-panel drive-form', id: 'drive-run', 'aria-labelledby': 'drive-run-title', novalidate: true, onsubmit: submitRun }",
		"el('p', { class: 'drive-error', id: 'drive-loop-error', tabindex: '-1', hidden: true })",
		"el('p', { class: 'drive-error', id: 'drive-run-error', tabindex: '-1', hidden: true })",
		"'Run the loop'",
		"el('p', { class: 'drive-help' },",
		"fill($('#drive-slot'), drive.panel);",
	} {
		if !strings.Contains(panel, want) {
			t.Errorf("ensureDrive lacks %q", want)
		}
	}
	for _, gone := range []string{"text-button", "stopLoop", "btn-danger", "datalist"} {
		if strings.Contains(panel, gone) {
			t.Errorf("the side panel forms still have %q", gone)
		}
	}

	render := moduleFunction(t, js, "renderDrive")
	for _, want := range []string{
		"c.run.disabled = drive.pending || Boolean(drive.run && drive.run.state === 'running');",
		"c.start.disabled = drive.pending || looping;",
		"b.setAttribute('aria-disabled', String(b.disabled))",
		"setText(c.run, drive.pending && drive.where === 'run' ? 'Starting…' : 'Start intake');",
		"setText(c.start, drive.pending && drive.where === 'loop' ? 'Starting…' : looping ? 'Loop is running' : 'Start loop');",
		"if (loop && Number.isInteger(loop.maxConcurrentLimit)) c.max.max = String(loop.maxConcurrentLimit);",
		"if (text && drive.announce) { drive.announce = false; error.focus(); }",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderDrive lacks %q", want)
		}
	}
	for _, want := range []string{
		"if (!/^[1-9][0-9]{0,9}$/.test(value)) { driveMessage('Enter an Issue number, such as 245.'); return; }",
		"if (!Number.isInteger(n) || n < 1) { driveMessage('Enter how many Molecules to advance at once.'); return; }",
		"if (drive.pending) return null;",
		"const FOCUS_KEYS = ['id', 'filter', 'atom', 'origin', 'review', 'evidence', 'gate', 'drive'];",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}

	page := do(t, h, authed("/")).Body.String()
	if !strings.Contains(page, `<div id="run-filters" class="run-filters lane-bar" role="group" aria-label="Filter Molecules"></div>`) {
		t.Error("index.html lacks the lane bar container")
	}
}
