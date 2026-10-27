package ui

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// servedModules are the entry and every script module it loads, in the fixed
// order servedJS concatenates them.
var servedModules = []string{
	"/app.js",
	"/js/core.js",
	"/js/core-gates.js",
	"/js/inbox.js",
	"/js/molecule.js",
	"/js/molecule-plan.js",
	"/js/molecule-decision.js",
	"/js/molecule-timeline.js",
	"/js/work.js",
	"/js/issues.js",
	"/js/insights.js",
	"/js/formula.js",
	"/js/loop.js",
}

// servedStylesheets are the stylesheets index.html links, in link order:
// tokens, base, then one per view.
var servedStylesheets = []string{
	"/css/tokens.css",
	"/css/base.css",
	"/css/inbox.css",
	"/css/molecule.css",
	"/css/work.css",
	"/css/issues.css",
	"/css/insights.css",
	"/css/formula.css",
	"/css/loop.css",
}

// viewModules are the view modules; each exports mount(root, ctx) and
// update(state), and imports only the core and its own sub-modules.
var viewModules = []string{"inbox", "molecule", "work", "issues", "insights", "formula", "loop"}

// maxModuleLines bounds every script module and stylesheet.
const maxModuleLines = 600

func servedAll(t *testing.T, paths []string) string {
	t.Helper()
	h := testHandler(t, Options{LabDir: t.TempDir()})
	var b strings.Builder
	for _, path := range paths {
		w := do(t, h, authed(path))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, w.Code)
		}
		b.WriteString(w.Body.String())
		b.WriteString("\n")
	}
	return b.String()
}

// servedJS is every served script module, concatenated in servedModules order.
func servedJS(t *testing.T) string {
	t.Helper()
	return servedAll(t, servedModules)
}

// servedCSS is every linked stylesheet, concatenated in link order.
func servedCSS(t *testing.T) string {
	t.Helper()
	return servedAll(t, servedStylesheets)
}

// moduleFunction returns the body of the top-level function name in module
// code, which sits at column 0: up to its closing brace at column 0, or the
// rest of its line for a one-line function.
func moduleFunction(t *testing.T, js, name string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^(?:export )?(?:async )?function ` + regexp.QuoteMeta(name) + `\([^)]*\) \{(.*)$`).FindStringSubmatchIndex(js)
	if m == nil {
		t.Fatalf("the served JS has no top-level function %s", name)
	}
	if rest := js[m[2]:m[3]]; rest != "" {
		return rest
	}
	end := strings.Index(js[m[1]:], "\n}\n")
	if end < 0 {
		t.Fatalf("function %s is not closed", name)
	}
	return js[m[1] : m[1]+end]
}

// moduleTable returns the body of the top-level table "const name = …" in
// module code, up to its closing bracket at column 0.
func moduleTable(t *testing.T, js, name, closing string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\nconst ` + regexp.QuoteMeta(name) + ` = [\[{](.*?)\n` + regexp.QuoteMeta(closing) + `;`).FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("the served JS has no %s table", name)
	}
	return m[1]
}

// mediaBlocks joins the bodies of every top-level at-rule that starts with
// head, across all stylesheets: each view keeps its own part of a media query.
func mediaBlocks(t *testing.T, css, head string) string {
	t.Helper()
	var parts []string
	for rest := css; ; {
		i := strings.Index(rest, head+" {")
		if i < 0 {
			break
		}
		rest = rest[i+len(head)+2:]
		j := strings.Index(rest, "\n}\n")
		if j < 0 {
			t.Fatalf("%s block is not closed", head)
		}
		parts = append(parts, rest[:j])
		rest = rest[j:]
	}
	if len(parts) == 0 {
		t.Fatalf("the served CSS has no %s block", head)
	}
	return strings.Join(parts, "\n")
}

// importRe matches a module's static imports.
var importRe = regexp.MustCompile(`(?m)^import (?:\{[^}]*\}|\* as [A-Za-z]+) from '([^']+)';$`)

// dynamicImportRe matches a dynamic import( call, and reExportRe an
// "export … from" re-export, in any form or spacing.
var (
	dynamicImportRe = regexp.MustCompile(`\bimport\s*\(`)
	reExportRe      = regexp.MustCompile(`\bexport\s*(?:\*|\{[^}]*\})[^;'"]*?\bfrom\s*['"]`)
)

// Every module and stylesheet is named in the allowlist and served with its
// type, no-store, the security headers and the version mark replaced; each
// needs the cookie and GET. index.html links exactly the stylesheets in order
// and loads the entry as a module, and every import resolves to a served file.
func TestUIServesEveryModuleAndStylesheet(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir(), Version: "v1.4.0-rc.1"})
	index := do(t, h, authed("/"))
	want := map[string]bool{}
	for _, path := range append(append([]string{}, servedModules...), servedStylesheets...) {
		want[path] = true
		contentType := "text/javascript; charset=utf-8"
		if strings.HasSuffix(path, ".css") {
			contentType = "text/css; charset=utf-8"
		}
		w := do(t, h, authed(path))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != contentType || w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() == 0 {
			t.Errorf("%s: %d %q %q %d bytes", path, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"), w.Body.Len())
		}
		if strings.Contains(w.Body.String(), assetVersionMark) {
			t.Errorf("%s leaks the version placeholder", path)
		}
		for _, name := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy", "X-Frame-Options", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy"} {
			if got := w.Header().Get(name); got == "" || got != index.Header().Get(name) {
				t.Errorf("%s %s %q, the index sends %q", path, name, got, index.Header().Get(name))
			}
		}
		if w := do(t, h, request{path: path}); w.Code != http.StatusForbidden || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s without the cookie: %d %q", path, w.Code, w.Header().Get("Cache-Control"))
		}
		post := authed(path)
		post.method, post.origin = http.MethodPost, "http://"+testHost
		if w := do(t, h, post); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
			t.Errorf("POST %s: %d", path, w.Code)
		}
		// Each file is listed explicitly, by its own path under web/.
		if staticFiles[path] != strings.TrimPrefix(path, "/") {
			t.Errorf("%s is served from %q", path, staticFiles[path])
		}
	}
	// The allowlist holds no other script or stylesheet, and no /style.css.
	for path := range staticFiles {
		if (strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".css")) && path != "/session.js" && !want[path] {
			t.Errorf("staticFiles serves the unlisted %s", path)
		}
	}

	page := index.Body.String()
	var links []string
	for _, m := range regexp.MustCompile(`<link rel="stylesheet" href="([^"]+)">`).FindAllStringSubmatch(page, -1) {
		links = append(links, m[1])
	}
	if strings.Join(links, " ") != strings.Join(servedStylesheets, " ") || strings.Count(page, "<link") != len(servedStylesheets) {
		t.Errorf("index.html links %q, want %q", links, servedStylesheets)
	}
	if strings.Count(page, "<script") != 1 || !strings.Contains(page, `<script type="module" src="/app.js"></script>`) {
		t.Error("index.html does not load the entry as its one module script")
	}

	// Every import resolves, from the importing file's directory, to a served
	// module, and every served module is imported.
	imported := map[string]bool{"/app.js": true}
	for _, path := range servedModules {
		body := do(t, h, authed(path)).Body.String()
		dir := path[:strings.LastIndex(path, "/")+1]
		for _, m := range importRe.FindAllStringSubmatch(body, -1) {
			target := dir + strings.TrimPrefix(m[1], "./")
			if !strings.HasPrefix(m[1], "./") || !want[target] {
				t.Errorf("%s imports %q, which is not a served module", path, m[1])
			}
			imported[target] = true
		}
		if n := strings.Count("\n"+body, "\nimport "); n != len(importRe.FindAllString(body, -1)) {
			t.Errorf("%s has %d import statements the check does not read", path, n)
		}
	}
	for _, path := range servedModules {
		if !imported[path] {
			t.Errorf("no module imports %s", path)
		}
	}
}

// Any path not named in the allowlist is not found and never cached: the
// removed /style.css, unlisted or directory paths under /js/ and /css/,
// traversal, a trailing slash and the wrong case.
func TestUIUnlistedStaticPathsAreNotFound(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	for _, path := range []string{
		"/style.css", "/js/nope.js", "/css/nope.css", "/js/", "/js", "/css/", "/css",
		"/css/../app.js", "/js/../app.js", "/js/%2e%2e/app.js", "/css/%2e%2e/css/base.css", "/js/core", "/js/core.js/", "/css/base.css/",
		"/js/CORE.js", "/CSS/base.css", "/web/js/core.js", "/web/css/base.css", "/js/style.css", "/css/core.js", "/js/core.js.map",
	} {
		if w := do(t, h, authed(path)); w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Cache-Control"))
		}
	}
}

// Each view module exports mount(root, ctx) and update(state) and imports only
// the core and its own sub-modules, never another view; only the entry imports
// the views. No module or stylesheet exceeds the line bound.
func TestUIViewModulesImportOnlyTheCore(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	views := map[string]bool{}
	for _, v := range viewModules {
		views[v] = true
	}
	for _, path := range servedModules {
		body := do(t, h, authed(path)).Body.String()
		name := strings.TrimSuffix(path[strings.LastIndex(path, "/")+1:], ".js")
		owner, _, _ := strings.Cut(name, "-")
		// Only the static imports read below load code: no dynamic import(
		// and no "export … from" re-export can reach around the check.
		if dynamicImportRe.MatchString(body) {
			t.Errorf("%s uses a dynamic import(", path)
		}
		if reExportRe.MatchString(body) {
			t.Errorf("%s re-exports with export … from", path)
		}
		for _, m := range importRe.FindAllStringSubmatch(body, -1) {
			target := strings.TrimSuffix(strings.TrimPrefix(m[1], "./"), ".js")
			if path == "/app.js" {
				continue
			}
			targetOwner, _, _ := strings.Cut(target, "-")
			if target != "core" && !strings.HasPrefix(target, "core-") && targetOwner != owner {
				t.Errorf("%s imports %s: a view imports only the core and its own sub-modules", path, m[1])
			}
			if views[target] && target != name && owner != target {
				t.Errorf("%s imports the view %s", path, target)
			}
		}
		if views[name] {
			if !strings.Contains(body, "\nexport function mount(root, ctx) {") || !strings.Contains(body, "\nexport function update(state) {") {
				t.Errorf("%s does not export mount(root, ctx) and update(state)", path)
			}
		}
	}
	// Sub-modules of a view are imported only by that view.
	entry := do(t, h, authed("/app.js")).Body.String()
	for _, v := range viewModules {
		if !strings.Contains(entry, "import * as "+v+" from './js/"+v+".js';") {
			t.Errorf("the entry does not import the view %s", v)
		}
	}
	if strings.Contains(entry, "molecule-") {
		t.Error("the entry imports a view's sub-module")
	}
	// The two patterns catch every form they guard against, and no plain code.
	for _, bad := range []string{"import('./inbox.js')", "await import (\n'./js/work.js')", "x = import(`./${v}.js`);"} {
		if !dynamicImportRe.MatchString(bad) {
			t.Errorf("dynamicImportRe misses %q", bad)
		}
	}
	for _, bad := range []string{"export * from './inbox.js';", "export * as inbox from './inbox.js';", "export { renderInbox } from './inbox.js';", "export {\n  a,\n  b,\n} from \"./work.js\";", "export{a}from'./loop.js'"} {
		if !reExportRe.MatchString(bad) {
			t.Errorf("reExportRe misses %q", bad)
		}
	}
	for _, good := range []string{"import { el } from './core.js';", "export function mount(root, ctx) {", "export const mv = {};", "const from = 'x'; // imported"} {
		if dynamicImportRe.MatchString(good) || reExportRe.MatchString(good) {
			t.Errorf("the import-form check rejects plain code %q", good)
		}
	}
	for _, path := range append(append([]string{}, servedModules...), servedStylesheets...) {
		data, err := web.ReadFile("web" + path)
		if err != nil {
			t.Fatalf("embedded %s: %v", path, err)
		}
		if n := strings.Count(string(data), "\n"); n > maxModuleLines {
			t.Errorf("%s has %d lines, more than %d", path, n, maxModuleLines)
		}
	}
}

// The concatenations follow the fixed orders, so a contract test sees every
// module and every sheet.
func TestUIServedJSAndCSSConcatenateEveryFile(t *testing.T) {
	js, css := servedJS(t), servedCSS(t)
	for _, want := range []string{"export function el(", "export async function submitGate(", "function renderInbox(", "export function ensureMoleculeView(", "export function renderPlan(", "export function renderTimeline(", "function renderGrid(", "function renderOverview(", "function renderFormula(", "function renderLoopPill(", "function renderShell("} {
		if !strings.Contains(js, want) {
			t.Errorf("servedJS lacks %q", want)
		}
	}
	if strings.Index(js, "async function loadLab(") > strings.Index(js, "export const $ =") {
		t.Error("servedJS does not start with the entry")
	}
	for _, want := range []string{":root {", "* { box-sizing: border-box; }", ".inbox-card {", ".mv-layout {", ".lane-bar {", ".placeholder {", ".review-row {", ".active-formula {", ".drive-form {"} {
		if !strings.Contains(css, want) {
			t.Errorf("servedCSS lacks %q", want)
		}
	}
	if !(strings.Index(css, ":root {") < strings.Index(css, "* { box-sizing") && strings.Index(css, "* { box-sizing") < strings.Index(css, ".inbox {")) {
		t.Error("servedCSS is not in link order")
	}
	// Each view keeps its own part of a shared media query; mediaBlocks finds
	// them all.
	forced := cssRules(mediaBlocks(t, css, "@media (forced-colors: active)"))
	for _, selector := range []string{".glass, .card", ".inbox-row, .inbox-recent, .inbox-icon, .inbox-badge", ".atom-tab, .atom-detail", ".cube-button, .micro-cube", ".review-row", ".active-formula", ".drive-panel"} {
		wantDecls(t, forced, selector, "background: Canvas;", "border: 1px solid CanvasText;")
	}
}

// In forced-colors mode the shelf's gradient gives way to Canvas: base.css's
// ".glass, .card" Canvas rule now loads before work.css, so the shelf needs
// its own forced-colors rule after its gradient, or the gradient wins.
func TestUIServedCSSShelfIsCanvasInForcedColors(t *testing.T) {
	css := servedCSS(t)
	gradient := strings.Index(css, "\n.shelf {")
	if gradient < 0 {
		t.Fatal("the served CSS has no top-level .shelf rule")
	}
	if rule, _, _ := strings.Cut(css[gradient+1:], "\n"); !strings.Contains(rule, "gradient(") {
		t.Fatalf("the top-level .shelf rule has no gradient: %s", rule)
	}
	head := "@media (forced-colors: active) {"
	found := false
	for at := 0; ; {
		i := strings.Index(css[at:], head)
		if i < 0 {
			break
		}
		start := at + i + len(head)
		end := strings.Index(css[start:], "\n}\n")
		if end < 0 {
			t.Fatal("a forced-colors block is not closed")
		}
		block := css[start : start+end]
		if decls, ok := cssRules(block)[".shelf"]; ok {
			if start < gradient {
				t.Error("a forced-colors .shelf rule loads before the .shelf gradient, which overrides it")
			} else if strings.Contains(decls, "background: Canvas;") {
				found = true
			}
		}
		at = start + end
	}
	if !found {
		t.Error("no forced-colors block after the .shelf gradient sets .shelf { background: Canvas; }")
	}
}
