package ui

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Findings tab module (plugins slice 5d-3) is a served Molecule-view
// sub-module, listed here so served_test.go stays as it is.
func init() {
	servedModules = append(servedModules, "/js/molecule-findings.js")
}

// moleculeFindingsModule is the served Findings module.
func moleculeFindingsModule(t *testing.T) string {
	t.Helper()
	w := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/js/molecule-findings.js"))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatalf("/js/molecule-findings.js: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	return w.Body.String()
}

// The module is served from the allowlist, listed in servedModules, imported
// by molecule.js and within the line bound, as molecule.js stays.
func TestMoleculeFindingsModuleIsServed(t *testing.T) {
	js := moleculeFindingsModule(t)
	if staticFiles["/js/molecule-findings.js"] != "js/molecule-findings.js" {
		t.Errorf("staticFiles serves /js/molecule-findings.js from %q", staticFiles["/js/molecule-findings.js"])
	}
	listed := 0
	for _, path := range servedModules {
		if path == "/js/molecule-findings.js" {
			listed++
		}
	}
	if listed != 1 {
		t.Errorf("servedModules lists /js/molecule-findings.js %d times", listed)
	}
	molecule := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/js/molecule.js")).Body.String()
	if !regexp.MustCompile(`(?m)^import \{[^}]*\bfindingsSelect\b[^}]*\} from './molecule-findings\.js';$`).MatchString(molecule) {
		t.Error("molecule.js does not import ./molecule-findings.js")
	}
	for name, body := range map[string]string{"molecule-findings.js": js, "molecule.js": molecule} {
		if n := strings.Count(body, "\n"); n > maxModuleLines {
			t.Errorf("%s has %d lines, more than %d", name, n, maxModuleLines)
		}
	}
	// It imports only the core and the Molecule view's own sub-modules.
	for _, m := range importRe.FindAllStringSubmatch(js, -1) {
		if !strings.HasPrefix(m[1], "./core") && !strings.HasPrefix(m[1], "./molecule-") {
			t.Errorf("molecule-findings.js imports %s", m[1])
		}
	}
}

// The module renders text only, follows a 202 with the bounded visible-only
// poll (2 s, at most 5 minutes) and reads the view on its tick at most once
// per 30 s while the tab is visible.
func TestMoleculeFindingsModuleRendersTextAndPollsBounded(t *testing.T) {
	js := moleculeFindingsModule(t)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "setInterval(", "type: 'submit'", ".append(", "stderr", "stdout"} {
		if strings.Contains(js, bad) {
			t.Errorf("molecule-findings.js has %q", bad)
		}
	}
	for _, want := range []string{
		"const FINDINGS_POLL_MS = 2000;",
		"const FINDINGS_FOLLOW_MS = 5 * 60 * 1000;",
		"const FINDINGS_TICK_MS = 30000;",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("molecule-findings.js lacks %q", want)
		}
	}
	for name, wants := range map[string][]string{
		"findingsFollowTick": {
			"if (Date.now() - f.started >= FINDINGS_FOLLOW_MS) {",
			"still running; refresh to check.",
			"!document.hidden && !$('#mv-findings').hidden ? await findingsLoad() : false",
			"if (read && fx.view && !running) {",
			"f.timer = setTimeout(() => findingsFollowTick(f), FINDINGS_POLL_MS);",
		},
		"findingsFollow":   {"f.timer = setTimeout(() => findingsFollowTick(f), FINDINGS_POLL_MS);"},
		"findingsTick":     {"!findingsVisible()", "if (Date.now() - fx.fetchedAt < FINDINGS_TICK_MS) return;"},
		"findingsVisible":  {"!document.hidden", "!$('#mv-findings').hidden"},
		"findingsCollect":  {"if (await findingsPost('findings/collect', {})) findingsFollow('collect');"},
		"findingsPush":     {"findingsPost('fixround/push', { approve: digest, confirm: true })", "findingsFollow('push');"},
		"findingsPost":     {"if (res.status === 409 && data && data.category === 'stale') {", "await findingsLoad();", "refusalText(data)"},
		"findingsDismiss":  {"findingsPost('findings/dismiss', { fingerprint: fp, reason: reason })", "[...reason].length > FINDINGS_MAX_REASON"},
		"findingsPropose":  {"findingsPost('findings/propose', { include: include })", "include.length > FINDINGS_MAX_INCLUDES"},
		"findingsPullURL":  {"PULL_URL.test(dec.pullUrl)"},
		"findingsGateText": {"'Forgecell’s review passed on this exact change.'", "'This push needs your confirmation because '"},
	} {
		body := moduleFunction(t, js, name)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	// Every follow re-arms through the one bounded tick; nothing else loops.
	if n := strings.Count(js, "setTimeout("); n != 2 {
		t.Errorf("molecule-findings.js arms %d timers, want the follow's 2", n)
	}
	// Links are https only, through the PULL_URL check; no other href is set,
	// and no digest is rendered as text.
	if strings.Contains(js, "href") || !strings.Contains(js, "externalLink(pull, PULL_URL, 'Open the change request ↗')") {
		t.Error("molecule-findings.js sets a link outside the PULL_URL check")
	}
	if m := regexp.MustCompile(`(setText|el)\([^;]*\.digest\b`).FindAllString(js, -1); len(m) != 0 {
		t.Errorf("molecule-findings.js renders a digest: %q", m)
	}
}

// A failed read (a 5xx, a non-JSON body or a thrown error) keeps the last view
// and says so; only an explicit reviewers false, a 404 or a 422 drops it. The
// tab hides only on that drop, so a typed reason or an open confirmation
// survives, and the tick keeps its own guard.
func TestMoleculeFindingsFailedReadKeepsTheView(t *testing.T) {
	js := moleculeFindingsModule(t)
	if !strings.Contains(js, "const FINDINGS_READ_FAILED = 'could not read the findings; showing the last read';") {
		t.Error("molecule-findings.js lacks the fixed failed-read note")
	}
	load := moduleFunction(t, js, "findingsLoad")
	// The view is replaced in two places only: the explicit drop and a good read.
	if n := strings.Count(load, "fx.view = "); n != 2 || !strings.Contains(load, "fx.view = null;") || !strings.Contains(load, "fx.view = view;") {
		t.Errorf("findingsLoad sets fx.view %d times: %s", n, load)
	}
	drop := strings.Index(load, "fx.view = null;")
	guard := strings.Index(load, "if (res.status === 404 || res.status === 422 || (view && view.reviewers === false)) {")
	if guard < 0 || drop < guard {
		t.Error("findingsLoad drops the view outside the 404, 422 or reviewers false branch")
	}
	if !strings.Contains(load, "else { fx.fetchedAt = Date.now(); findingsReadFailed(); }") {
		t.Error("findingsLoad does not keep the view after a thrown read")
	}
	for name, wants := range map[string][]string{
		"findingsReadFailed": {"fx.readFailed = true;", "findingsRender();"},
		"findingsRender":     {"c.stale.hidden = !fx.readFailed;"},
		"findingsEnsure":     {"c.stale = el('p', { class: 'drive-help', hidden: true }, FINDINGS_READ_FAILED + '.');"},
	} {
		body := moduleFunction(t, js, name)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	for _, bad := range []string{"findingsShowTab(false)", "fx.view = ", "fx.confirming = ''", "reason.value"} {
		if strings.Contains(moduleFunction(t, js, "findingsReadFailed"), bad) {
			t.Errorf("findingsReadFailed has %q", bad)
		}
	}
}

// The panel's announcements reach assistive tech: the error is an alert, the
// note a focusable status, the dismiss error sits by its field and stays, and
// each announcement scrolls only as far as needed. The collect follow and the
// Collect button read the collect job alone, and a rebuild of the open rows
// gives the reason field its focus back.
func TestMoleculeFindingsAnnouncesAndFollowsTheCollectJob(t *testing.T) {
	js := moleculeFindingsModule(t)
	for name, wants := range map[string][]string{
		"findingsEnsure": {
			"c.message = el('p', { class: 'drive-error', role: 'alert', tabindex: '-1', hidden: true });",
			"c.note = el('p', { class: 'scope-note', role: 'status', tabindex: '-1' });",
			"c.reasonError = el('p', { class: 'drive-error', id: uid('mv-dismiss-error'), role: 'alert', hidden: true });",
			"'aria-describedby': c.reasonError.id",
			"field(reasonId, 'Why dismiss it?', c.reason), c.reasonError,",
		},
		"findingsAnnounce": {"node.focus({ preventScroll: true });", "node.scrollIntoView({ block: 'nearest' });"},
		"findingsRender":   {"findingsAnnounce(fx.message ? fx.c.message : fx.c.note)", "c.reasonError.hidden = !fx.reasonError;"},
		"findingsDismiss":  {"fx.reasonError = 'Enter a one-line reason", "fx.c.reasonError.scrollIntoView({ block: 'nearest' });"},
		"findingsFollowTick": {
			"const running = Boolean(job && job.state === 'running');",
		},
		"findingsRenderCollect": {
			"const collecting = Boolean(v.collect && v.collect.state === 'running') || Boolean(fx.follow && fx.follow.kind === 'collect');",
			"setText(c.collect, collecting ? 'Collecting…' : 'Collect now');",
		},
		"findingsRenderOpen": {
			"const typing = document.activeElement === c.reason;",
			"if (typing && c.reason.isConnected && document.activeElement !== c.reason) c.reason.focus({ preventScroll: true });",
		},
	} {
		body := moduleFunction(t, js, name)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	for _, name := range []string{"findingsFollowTick", "findingsRenderCollect"} {
		if strings.Contains(moduleFunction(t, js, name), "collecting === true") {
			t.Errorf("%s reads the broader collecting flag", name)
		}
	}
	// The dismiss error is not the panel-wide message.
	if strings.Contains(moduleFunction(t, js, "findingsDismiss"), "findingsSay(") {
		t.Error("findingsDismiss reports its validation error away from the field")
	}
}

// molecule.js shows the third tab only through the findings view, moves
// between the shown tabs, opens an Inbox fix-round or findings entry on the
// Findings tab, and keeps the Plan default.
func TestMoleculeFindingsTabWiring(t *testing.T) {
	js := servedJS(t)
	for name, wants := range map[string][]string{
		"chooseTab":        {"const pick = shownTabs().includes(tab) ? tab : 'plan';", "if (pick === 'findings') findingsOpened();"},
		"nextTab":          {"if (key === 'Home') return shown[0];", "if (key === 'End') return shown[shown.length - 1];"},
		"select":           {"findingsSelect(id, kind === 'fix-round' || kind === 'findings');"},
		"showMoleculeView": {"chooseTab('plan', false); findingsSelect(null, false);"},
		"findingsShowTab":  {"if (tab.hidden !== !on) tab.hidden = !on;", "fx.chooseTab('plan', false)"},
		"findingsLoad": {
			"if (res.status === 404 || res.status === 422 || (view && view.reviewers === false)) {",
			"findingsShowTab(false);",
			"if (!view || view.reviewers !== true) { findingsReadFailed(); return false; }",
			"findingsShowTab(true);",
		},
	} {
		body := moduleFunction(t, js, name)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	if !strings.Contains(js, "const TABS = [['plan', '#mv-tab-plan', '#mv-plan'], ['timeline', '#mv-tab-timeline', '#mv-timeline'], ['findings', '#mv-tab-findings', '#mv-findings']];") {
		t.Error("molecule.js does not list the three tabs in order")
	}
}

// index.html keeps the Plan and Timeline tab buttons byte for byte and adds
// the Findings tab and panel, both hidden by default.
func TestMoleculeFindingsTabMarkup(t *testing.T) {
	page := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/")).Body.String()
	tabs := between(t, page, `<div id="mv-tabs" class="mv-tabs" role="tablist" aria-label="Molecule sections">`, "</div>")
	want := "\n" +
		`    <button type="button" role="tab" id="mv-tab-plan" class="mv-tab" aria-controls="mv-plan" aria-selected="true" data-tab="plan">Plan</button>` + "\n" +
		`    <button type="button" role="tab" id="mv-tab-timeline" class="mv-tab" aria-controls="mv-timeline" aria-selected="false" tabindex="-1" data-tab="timeline">Timeline</button>` + "\n" +
		`    <button type="button" role="tab" id="mv-tab-findings" class="mv-tab" aria-controls="mv-findings" aria-selected="false" tabindex="-1" data-tab="findings" hidden>Findings</button>` + "\n  "
	if tabs != want {
		t.Errorf("the tab list is\n%s\nwant\n%s", tabs, want)
	}
	if !strings.Contains(page, `<div id="mv-findings" class="mv-findings" role="tabpanel" aria-labelledby="mv-tab-findings" hidden></div>`) {
		t.Error("the page lacks the hidden, empty Findings panel")
	}
}

// moleculeFindingsWants checks that each named function of js holds its wants.
func moleculeFindingsWants(t *testing.T, js string, wants map[string][]string) {
	t.Helper()
	for name, list := range wants {
		body := moduleFunction(t, js, name)
		for _, want := range list {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
}

// Item 2 of #386: a failed reviewer's round shows the failure and its reason
// only, never "round complete" next to it. Item 1: every reviewer is shown by
// its plugin name, never by its key ("0:forgecomments").
func TestMoleculeFindingsFailedRoundShowsFailureOnly(t *testing.T) {
	js := moleculeFindingsModule(t)
	round := moduleFunction(t, js, "findingsRoundItem")
	failure := "const line = status === 'failed' ? 'Failed: ' + (error || 'no reason was recorded') + '.' : "
	at := strings.Index(round, failure)
	if at < 0 {
		t.Fatalf("findingsRoundItem lacks the failure-only line: %s", round)
	}
	// The round's completeness is only on the branch of a reviewer that did not fail.
	for _, text := range []string{"'round complete'", "'round incomplete'"} {
		if i := strings.Index(round, text); i < at+len(failure) {
			t.Errorf("findingsRoundItem shows %s outside the not-failed branch", text)
		}
	}
	if !strings.Contains(round, "status !== 'failed' && error ? el('p', { class: 'file-reason' }, error) : null") {
		t.Error("findingsRoundItem repeats a failed reviewer's error")
	}
	moleculeFindingsWants(t, js, map[string][]string{
		"findingsName":            {"plainText(r.plugin, '')", ".replace(/^\\d+:/, '')"},
		"findingsRoundItem":       {"el('code', null, findingsName(r))"},
		"findingsOpenItem":        {"el('p', { class: 'file-reason' }, findingsName(f))"},
		"findingsRenderDismissed": {"[findingsName(d), when ? 'dismissed ' + when : '']"},
	})
	for _, bad := range []string{"plainText(r.reviewer, 'Reviewer'))", "plainText(f.reviewer, '')", "plainText(d.reviewer, 'Reviewer')"} {
		if strings.Contains(js, bad) {
			t.Errorf("molecule-findings.js still shows a reviewer key: %q", bad)
		}
	}
}

// Item 4 of #386: once the change request is merged or closed, on its receipt
// or as the GitHub sync observed it, Propose is disabled with its reason.
// Item 10: without open findings Propose is hidden, not only disabled.
func TestMoleculeFindingsProposeDisabledAfterMerge(t *testing.T) {
	js := moleculeFindingsModule(t)
	moleculeFindingsWants(t, js, map[string][]string{
		"findingsEnsure": {
			"c.proposeHelp = el('p', { class: 'drive-help', id: uid('mv-propose-help'), hidden: true });",
			"'aria-describedby': c.proposeHelp.id, dataset: { gate: 'findings-propose' }",
			"c.propose, c.proposeHelp, c.proposal",
		},
		"findingsRenderOpen": {
			"const s = syncOf(fx.id), pr = s && s.pr, ended = v.lifecycle === 'merged' || (pr && pr.state === 'MERGED') ? 'merged' : v.lifecycle === 'closed' || (pr && pr.state !== 'OPEN') ? 'closed' : '';",
			"c.propose.hidden = open.length === 0;",
			"c.propose.disabled = busy || v.published !== true || open.length === 0 || Boolean(ended);",
			"setText(c.proposeHelp, ended ? 'The change request is ' + ended + ', so there is nothing left to propose a fix round for.' : '');",
			"c.proposeHelp.hidden = !ended || open.length === 0;",
		},
	})
	if !regexp.MustCompile(`(?m)^import \{[^}]*\bsyncOf\b[^}]*\} from './core\.js';$`).MatchString(js) {
		t.Error("molecule-findings.js does not read the sync state from core.js")
	}
}

// Item 5 of #386: a refused proposal shows the served category, message and
// reason through refusalText, never a bare "Refused (refused)".
func TestMoleculeFindingsShowsRefusalReason(t *testing.T) {
	js := moleculeFindingsModule(t)
	moleculeFindingsWants(t, js, map[string][]string{
		"findingsPost":    {"if (!res.ok) { findingsSay(refusalText(data)); return null; }"},
		"findingsSay":     {"fx.message = text;", "fx.announce = true;"},
		"findingsPropose": {"const proposal = await findingsPost('findings/propose', { include: include });"},
	})
	gates := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/js/core-gates.js")).Body.String()
	moleculeFindingsWants(t, gates, map[string][]string{
		"refusalText": {"body.category", "notRecorded(body && body.error)", "reasonText(body)"},
		"reasonText":  {"body.reason"},
	})
	// The server side: the engine's no-reviewers refusal has its own category
	// and a next step that names the Formula mismatch.
	for _, category := range []string{"no-active-reviewers", "not-live"} {
		if reason := findingsReasons[category]; !boundedReason(reason) {
			t.Errorf("category %s has reason %q", category, reason)
		}
	}
	if !strings.Contains(findingsReasons["no-active-reviewers"], "Formula snapshot declares reviewers, but the Lab's active approved Formula does not") {
		t.Errorf("no-active-reviewers reason %q", findingsReasons["no-active-reviewers"])
	}
}

// Item 6 of #386: a refused or lost proposal keeps every ticked include box;
// only a proposal, or another Molecule, clears them.
func TestMoleculeFindingsKeepsSelectionAfterRefusal(t *testing.T) {
	js := moleculeFindingsModule(t)
	propose := moduleFunction(t, js, "findingsPropose")
	for _, want := range []string{
		"const id = fx.id, kept = new Set(fx.checked);",
		"if (!proposal) { fx.checked = kept; findingsRender(); return; }",
	} {
		if !strings.Contains(propose, want) {
			t.Errorf("findingsPropose lacks %q", want)
		}
	}
	refused, cleared := strings.Index(propose, "if (!proposal) {"), strings.Index(propose, "fx.checked = new Set();")
	if refused < 0 || cleared < refused || strings.Count(propose, "fx.checked = new Set();") != 1 {
		t.Errorf("findingsPropose clears the ticks before a proposal: %s", propose)
	}
	// Every render restores the boxes from fx.checked; only the success path,
	// a dismissal of that finding and a new selection change it.
	moleculeFindingsWants(t, js, map[string][]string{
		"findingsRenderOpen": {"for (const box of c.open.querySelectorAll('input[type=checkbox]')) box.checked = fx.checked.has(box.dataset.fingerprint);"},
		"findingsSelect":     {"fx.checked = new Set();"},
	})
	for _, name := range []string{"findingsPost", "findingsSay", "findingsLoad", "findingsRender"} {
		if strings.Contains(moduleFunction(t, js, name), "fx.checked") {
			t.Errorf("%s changes the ticked findings", name)
		}
	}
}

// Item 3 of #386: a finding's body is shown as text under its title, collapsed
// when long or multi-line. Item 7: the roles row names the declared reviewers.
func TestMoleculeFindingsShowsBodiesAndDeclaredReviewers(t *testing.T) {
	js := moleculeFindingsModule(t)
	moleculeFindingsWants(t, js, map[string][]string{
		"findingsBodyBlock": {
			"el('p', { class: 'mv-finding-body' }, body + (f.bodyTruncated === true ? ' …' : ''))",
			"body.length <= FINDINGS_BODY_SHORT && !body.includes('\\n')",
			"el('details', { class: 'mv-details' }, el('summary', null, 'The reviewer’s explanation')",
		},
		"findingsOpenItem":  {"el('p', { class: 'file-reason' }, plainText(f.title, '(untitled)')),\n      findingsBodyBlock(f),"},
		"findingsReviewers": {"fx.id === id", "fx.view.plugins"},
		"findingsShowTab":   {"if (fx.onView) fx.onView();"},
	})
	h := testHandler(t, Options{LabDir: t.TempDir()})
	moleculeFindingsWants(t, do(t, h, authed("/js/molecule.js")).Body.String(), map[string][]string{
		"renderDetail": {"renderRoles(d, findingsReviewers(d.id));"},
		"mount":        {"mountFindings(chooseTab, () => { const d = state.detail; if (d && d.view) renderRoles(d, findingsReviewers(d.id)); });"},
	})
	moleculeFindingsWants(t, do(t, h, authed("/js/molecule-decision.js")).Body.String(), map[string][]string{
		"renderRoles": {"Array.isArray(reviewers) ? reviewers.filter(", "outside.join(', ')", "|| 'No review step'"},
	})
	css := servedCSS(t)
	for _, want := range []string{".confirm-form .drive-field { flex: 0 0 auto; align-content: start; }", ".mv-finding-body { white-space: pre-wrap;"} {
		if !strings.Contains(css, want) {
			t.Errorf("molecule.css lacks %q", want)
		}
	}
}

// moleculeFindingsDocText is a document as one lower-case line: quote markers
// and line breaks do not hide a phrase.
func moleculeFindingsDocText(raw string) string {
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimLeft(strings.TrimSpace(line), "> ")
	}
	return strings.ToLower(strings.Join(strings.Fields(strings.Join(lines, " ")), " "))
}

var moleculeFindingsGoneDocPhrases = []string{"outside reviewers are not supported yet", "outside reviewers are post-v1", "gitlab and jira are not supported yet", "(post-v1, #259)", "not yet in a release"}

// The docs no longer say outside reviewers are unsupported: install.md has
// the subsection, the plugin README links it, and the design doc records the
// slice status.
func TestMoleculeFindingsDocs(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	read := func(rel string) string {
		raw, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	// The normalization finds the phrase across a quoted line break, as the
	// old README note had it.
	if !strings.Contains(moleculeFindingsDocText("> tracker; GitLab, Jira and outside\n> reviewers are post-v1."), moleculeFindingsGoneDocPhrases[1]) {
		t.Fatal("the doc normalization misses a phrase split across quoted lines")
	}
	docs := map[string]string{}
	for _, rel := range []string{"README.md", "docs/install.md", "lab/plugins/forgecomments/README.md"} {
		docs[rel] = read(rel)
		text := moleculeFindingsDocText(docs[rel])
		for _, gone := range moleculeFindingsGoneDocPhrases {
			if strings.Contains(text, gone) {
				t.Errorf("%s still says %q", rel, gone)
			}
		}
	}
	install := docs["docs/install.md"]
	if !strings.Contains(moleculeFindingsDocText(install), "gitlab and jira ship in the v0.4.0 release") {
		t.Error("docs/install.md does not say GitLab and Jira ship in v0.4.0")
	}
	if !strings.Contains(moleculeFindingsDocText(install), "outside reviewers (review bots and other reviewer plugins) ship in the v0.4.0 release") {
		t.Error("docs/install.md does not say outside reviewers ship in v0.4.0")
	}
	for _, want := range []string{"### Outside reviewers and fix rounds", "reviewers:", "--collect", "--propose", "--dismiss", "--push", "Findings", "at most 3 fix rounds", "GitHub Labs can't serve"} {
		if !strings.Contains(install, want) {
			t.Errorf("docs/install.md lacks %q", want)
		}
	}
	if !strings.Contains(docs["lab/plugins/forgecomments/README.md"], "docs/install.md#outside-reviewers-and-fix-rounds") {
		t.Error("the forgecomments README does not link the install subsection")
	}
	if !strings.Contains(moleculeFindingsDocText(docs["README.md"]), "v0.4.0 also ships the plugin runtime with bundled gitlab (forge), jira (tracker) and `forgecomments` (reviewer) plugins") {
		t.Error("README.md does not say v0.4.0 ships the GitLab, Jira and forgecomments plugins")
	}
	if !strings.Contains(read("docs/design/2026-10-30-plugins-design.md"), "\nSlices 1-6 implemented on main (2026-11-03); the contributor guide is `lab/plugins/README.md`.\n") {
		t.Error("the plugins design doc lacks the slice status line")
	}
}
