package ui

import (
	"regexp"
	"strings"
	"testing"
)

// served returns one served file's body.
func served(t *testing.T, path string) string {
	t.Helper()
	w := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed(path))
	if w.Code != 200 {
		t.Fatalf("%s: %d", path, w.Code)
	}
	return w.Body.String()
}

func wantAll(t *testing.T, where, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("%s lacks %q", where, want)
		}
	}
}

func wantNone(t *testing.T, where, body string, gone ...string) {
	t.Helper()
	for _, g := range gone {
		if strings.Contains(body, g) {
			t.Errorf("%s still has %q", where, g)
		}
	}
}

// TestWorkLaneBarNeverClips is the CSS contract of the lane bar (#261): it starts
// at its left edge, its snap points keep its padding in view, segments never
// shrink, it wraps on narrow screens, and a rebuild restores the scroll and then
// shows the pressed segment whole.
func TestWorkLaneBarNeverClips(t *testing.T) {
	css := servedCSS(t)
	rules := cssRules(css)
	wantDecls(t, rules, ".lane-bar", "justify-content: flex-start;", "scroll-padding-inline: 4px;", "overflow-x: auto;", "min-width: 0;", "max-width: 100%;")
	wantDecls(t, rules, ".lane-segment", "flex: 0 0 auto;", "white-space: nowrap;")
	for _, bad := range []string{"justify-content: center", "justify-content: flex-end", "justify-content: end", "justify-content: space-", "direction: rtl", "flex-direction: row-reverse"} {
		if strings.Contains(rules[".lane-bar"], bad) {
			t.Errorf(".lane-bar has %q, which can push its first segment out of view", bad)
		}
	}
	if strings.Contains(rules[".lane-segment"], "flex-shrink: 1") || strings.Contains(rules[".lane-segment"], "flex: 1") {
		t.Errorf(".lane-segment may shrink: %s", rules[".lane-segment"])
	}
	wantDecls(t, cssRules(mediaBlocks(t, css, "@media (max-width: 720px)")), ".lane-bar", "flex: 1 1 100%;", "flex-wrap: wrap;")

	js := served(t, "/js/work.js")
	filters := moduleFunction(t, js, "renderFilters")
	wantAll(t, "renderFilters", filters, "const left = bar.scrollLeft;", "keepLaneStart(bar, left);")
	if strings.Index(filters, "const left = bar.scrollLeft;") > strings.Index(filters, "fill($('#run-filters')") || strings.Index(filters, "keepLaneStart(bar, left);") < strings.Index(filters, "fill($('#run-filters')") {
		t.Error("renderFilters does not read the scroll before the rebuild and restore it after")
	}
	wantAll(t, "keepLaneStart", moduleFunction(t, js, "keepLaneStart"),
		"bar.scrollLeft = left;", "bar.querySelector('[aria-pressed=\"true\"]')", "if (seg.left < box.left + pad) bar.scrollLeft -= box.left + pad - seg.left;", "else if (seg.right > box.right - pad)")
}

// TestWorkGridSizesToItsContent: the grid has as many rows as it shows cells and
// no fixed height, an empty filter shows a short line instead of the grid, and the
// axis labels span exactly the grid's width.
func TestWorkGridSizesToItsContent(t *testing.T) {
	css := servedCSS(t)
	rules := cssRules(css)
	wantDecls(t, rules, ".cubes", "grid-template-rows: repeat(var(--rows, 7), 26px);", "width: max-content;")
	wantDecls(t, cssRules(mediaBlocks(t, css, "@media (max-width: 720px)")), ".cubes", "grid-template-rows: repeat(var(--rows, 7), 24px);")
	if strings.Contains(rules[".cubes"], "min-height") || strings.Contains(rules[".cubes"], "height:") {
		t.Errorf(".cubes keeps a fixed height: %s", rules[".cubes"])
	}
	wantDecls(t, rules, ".cubes:has(+ .filter-empty:not([hidden]))", "display: none;")
	wantDecls(t, rules, ".culture-scroll", "display: grid;", "grid-template-columns: max-content;", "justify-content: start;", "overflow-x: auto;")
	wantDecls(t, rules, ".grid-axis", "width: 0;", "min-width: 100%;", "justify-content: space-between;")

	apply := moduleFunction(t, served(t, "/js/work.js"), "applyFilter")
	wantAll(t, "applyFilter", apply,
		"$('#cubes').style.setProperty('--rows', String(Math.max(1, Math.min(7, shown.length))));",
		"$('.grid-axis').hidden = Math.ceil(shown.length / 7) < AXIS_COLUMNS;",
		"empty.hidden = shown.length > 0;",
		"'No Molecules in this lane.'")
	page := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/")).Body.String()
	scroll := between(t, page, `<div class="culture-scroll"`, "</div>\n  <div class=\"shelf-caption\">")
	if a, c := strings.Index(scroll, `class="grid-axis"`), strings.Index(scroll, `id="cubes"`); a < 0 || c < 0 || a > c {
		t.Error("the axis labels are not directly above the grid in the scroll area")
	}
}

// TestInsightsValuesAreReadable: money goes through core.js money() (Intl, two
// decimals, never $0 for a nonzero amount) with no six-decimal noise, durations
// are hours and minutes, and numbers use tabular figures.
func TestInsightsValuesAreReadable(t *testing.T) {
	core, js := served(t, "/js/core.js"), served(t, "/js/insights.js")
	wantAll(t, "core.js", core,
		"const dollars = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 2 });",
		"return value > 0 && value < 0.01 ? 'under $0.01' : dollars.format(value);",
		"export function hoursText(ms) {",
		"return ms === 0 ? '0 min' : minutesText(ms);")
	wantAll(t, "insights.js", js, "money, wholeNumber, plural, hoursText", "function usdText(value, notFinite) { return money(num(value)) ||", "function timeText(ms) { return hoursText(num(ms)) || NOT_REPORTED; }", "const NOT_REPORTED = 'not reported';")
	wantNone(t, "insights.js", js, "toFixed(6)", "'$' +", "durationLabel", ".toLocaleString(")
	if m := regexp.MustCompile(`toFixed\(\d\)`).FindAllString(js, -1); len(m) != 4 {
		// Only the four SVG coordinates of a bar are rounded, never a shown value.
		t.Errorf("insights.js rounds with toFixed %d times: %q", len(m), m)
	}
	rules := cssRules(servedCSS(t))
	for _, selector := range []string{".stat-value", ".model-table", ".insight-facts dd", ".outcome-row strong"} {
		wantDecls(t, rules, selector, "font-variant-numeric: tabular-nums;")
	}
}

// TestInsightsSortIsStableAndInTheURL: the sort keys are an allowlist read from
// ?sort=… and written with history.pushState; an invalid value falls back to the
// default, nulls sort last and ties fall back to role, model and server order.
func TestInsightsSortIsStableAndInTheURL(t *testing.T) {
	core, js := served(t, "/js/core.js"), served(t, "/js/insights.js")
	set := moduleFunction(t, core, "setQueryParam")
	wantAll(t, "setQueryParam", set, "new URLSearchParams(location.search)", "history.pushState({ view: state.view }, '', next);")
	wantAll(t, "queryParam", moduleFunction(t, core, "queryParam"), "return value !== null && allowed(value) ? value : null;")

	sorts := moduleTable(t, js, "SORTS", "}")
	var keys []string
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z]+): \{ label: '`).FindAllStringSubmatch(sorts, -1) {
		keys = append(keys, m[1])
	}
	if strings.Join(keys, " ") != "model role attempts molecules cost median in out cache time outcome" {
		t.Errorf("sort keys %q", keys)
	}
	wantAll(t, "insights.js", js,
		"const SORT_VALUE = /^([a-z]{2,9})(?:-(asc|desc))?$/;",
		"function validSort(value) { const m = SORT_VALUE.exec(value); return Boolean(m && Object.hasOwn(SORTS, m[1])); }",
		"const m = SORT_VALUE.exec(queryParam('sort', validSort) || DEFAULT_SORT);")
	wantAll(t, "chooseSort", moduleFunction(t, js, "chooseSort"), "setQueryParam('sort', key === DEFAULT_SORT && dir === 'asc' ? null : sortValue(key, dir));", "keepFocus(renderAnalytics);")
	wantAll(t, "sortRows", moduleFunction(t, js, "sortRows"),
		"if (a.v === null) return 1;", "if (b.v === null) return -1;",
		"cmp(ROLE_ORDER[a.r.role] ?? 9, ROLE_ORDER[b.r.role] ?? 9) || cmp(modelLabel(a.r), modelLabel(b.r)) || a.i - b.i;")
	wantAll(t, "mount", moduleFunction(t, js, "mount"), "window.addEventListener('popstate', () => keepFocus(renderAnalytics));")
	model := moduleFunction(t, js, "byModelSection")
	wantAll(t, "byModelSection", model, "'aria-sort': on ?", "el('button', { type: 'button', class: 'sort-button', id: 'sort-' + key, onclick: () => chooseSort(key) }", "el('th', { scope: 'row', class: 'model-name' }, modelLabel(r))", "roleLabel('meta')")
	// The table rebuilds only when the analytics or the sort change.
	wantAll(t, "renderAnalytics", moduleFunction(t, js, "renderAnalytics"), "if (key === shownKey && box.childElementCount) return;")
}

// TestInsightsChartHasATextFallback: the weekly chart is inline SVG built with
// createElementNS and hidden from assistive technology, beside a captioned table
// that carries the same values; no chart library is loaded.
func TestInsightsChartHasATextFallback(t *testing.T) {
	js := served(t, "/js/insights.js")
	wantAll(t, "svg", moduleFunction(t, js, "svg"), "document.createElementNS(SVG_NS, tag)", "return add(node, ...children);")
	chart := moduleFunction(t, js, "stackChart")
	wantAll(t, "stackChart", chart, "svg('svg', { viewBox: '0 0 ' + W + ' ' + H, class: 'chart-svg', 'aria-hidden': 'true', focusable: 'false' }", "nothing reported in these weeks")
	weekly := moduleFunction(t, js, "weeklySection")
	wantAll(t, "weeklySection", weekly, "el('table', { class: 'sr-only' },", "el('caption', null, 'Reported cost and Molecules finished per week, by model')", "el('th', { scope: 'row' }, weekText(wk.start))", "usdText(wk.costTotal, wk.costNotFinite)", "countText(wk.finishedTotal)")
	page := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/")).Body.String()
	for _, body := range []string{js, page} {
		wantNone(t, "Insights", body, "chart.js", "d3.", "echarts", "innerHTML")
	}
}

// TestInsightsLayoutAndFooter: the analytics article carries the sections
// without card chrome, the definition lists are a two-column grid that reflows at
// 400 px, controls are 44 px on coarse pointers, forced colors are honoured, and
// Cost per merged PR and Formula suggestions sit in a small Not yet available footer.
func TestInsightsLayoutAndFooter(t *testing.T) {
	css := servedCSS(t)
	rules := cssRules(css)
	wantDecls(t, rules, ".insight-facts", "display: grid;", "grid-template-columns: minmax(0, max-content) minmax(0, 1fr);")
	wantDecls(t, rules, ".insight-facts dd", "margin: 0;", "padding: 0;")
	wantDecls(t, rules, ".table-scroll", "overflow-x: auto;", "max-width: 100%;")
	wantDecls(t, rules, ".aggregate > #analytics", "background: transparent;", "border: 0;", "min-width: 0;")
	narrow := cssRules(mediaBlocks(t, css, "@media (max-width: 400px)"))
	wantDecls(t, narrow, ".insight-facts", "grid-template-columns: minmax(0, 1fr);")
	wantDecls(t, narrow, ".stat-tiles", "grid-template-columns: minmax(0, 1fr);")
	wantDecls(t, cssRules(mediaBlocks(t, css, "@media (pointer: coarse)")), ".sort-button", "min-height: 44px;")
	wantDecls(t, rules, ".sort-button", "touch-action: manipulation;")
	wantDecls(t, rules, ".sort-button:focus-visible", "outline: 2px solid var(--focus);")
	wantDecls(t, cssRules(mediaBlocks(t, css, "@media (forced-colors: active)")), ".insight-section, .stat-tile", "background: Canvas;", "border: 1px solid CanvasText;")

	page := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/")).Body.String()
	insights := between(t, page, `id="aggregate"`, "</section>")
	footer := between(t, insights, `<div class="not-yet">`, "</dl>")
	wantAll(t, "the Not yet available footer", footer, "Not yet available", "COST PER MERGED PR", "FORMULA SUGGESTIONS", "Needs verified merge attribution.", "Suggestion review is not connected in this slice.")
	wantNone(t, "the Insights view", insights, `class="metrics"`, `<article class="card"><span class="label">COST PER MERGED PR`)
	if strings.Index(insights, `<article id="analytics" class="card">`) > strings.Index(insights, `<div class="not-yet">`) {
		t.Error("the footer is not below the analytics")
	}
	js := served(t, "/js/insights.js")
	for _, name := range []string{"summarySection", "byModelSection", "weeklySection", "timeSection"} {
		body := moduleFunction(t, js, name)
		if !strings.Contains(body, "el('h2', { id: 'insight-") || !strings.Contains(body, "'aria-labelledby': 'insight-") {
			t.Errorf("%s is not a labelled section with an h2", name)
		}
	}
	wantAll(t, "tile", moduleFunction(t, js, "tile"), "el('h3', { class: 'stat-label' }, label)")
}
