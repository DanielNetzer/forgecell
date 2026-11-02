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

// moleculeFindingsDocText is a document as one lower-case line: quote markers
// and line breaks do not hide a phrase.
func moleculeFindingsDocText(raw string) string {
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimLeft(strings.TrimSpace(line), "> ")
	}
	return strings.ToLower(strings.Join(strings.Fields(strings.Join(lines, " ")), " "))
}

var moleculeFindingsGoneDocPhrases = []string{"outside reviewers are not supported yet", "outside reviewers are post-v1", "gitlab and jira are not supported yet", "(post-v1, #259)"}

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
	if !strings.Contains(moleculeFindingsDocText(install), "gitlab and jira are available in the source on `main` and not yet in a release") {
		t.Error("docs/install.md does not say GitLab and Jira are on main and not yet released")
	}
	for _, want := range []string{"### Outside reviewers and fix rounds", "reviewers:", "--collect", "--propose", "--dismiss", "--push", "Findings", "at most 3 fix rounds", "GitHub Labs can't serve"} {
		if !strings.Contains(install, want) {
			t.Errorf("docs/install.md lacks %q", want)
		}
	}
	if !strings.Contains(docs["lab/plugins/forgecomments/README.md"], "docs/install.md#outside-reviewers-and-fix-rounds") {
		t.Error("the forgecomments README does not link the install subsection")
	}
	if !strings.Contains(moleculeFindingsDocText(docs["README.md"]), "outside reviewers are available in the source on `main` and not yet in a release") {
		t.Error("README.md does not say outside reviewers are on main and not yet released")
	}
	if !strings.Contains(read("docs/design/2026-10-30-plugins-design.md"), "\nSlices 1-5 implemented on main (2026-11-02); slice 6 (contributor kit) pending.\n") {
		t.Error("the plugins design doc lacks the slice status line")
	}
}
