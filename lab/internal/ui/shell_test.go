package ui

import (
	"bytes"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

var shellAssets = map[string]string{
	"/assets/glasshouse-fern.webp":      "image/webp",
	"/assets/orchid-maidenhair.webp":    "image/webp",
	"/assets/lab-companions.webp":       "image/webp",
	"/assets/instrument-sans-400.woff2": "font/woff2",
	"/assets/instrument-sans-500.woff2": "font/woff2",
}

// The committed art and fonts are served from the embedded web/ folder with
// their content types, a long cache header and every existing security header.
// They need the cookie and GET; any other /assets/ path is not found, and
// nothing else gets the long cache.
func TestUIAssetsServedWithTypesCacheAndSecurityHeaders(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	index := do(t, h, authed("/"))
	for path, contentType := range shellAssets {
		w := do(t, h, authed(path))
		want, err := web.ReadFile("web" + path)
		if err != nil || len(want) == 0 {
			t.Fatalf("embedded %s: %v", path, err)
		}
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != contentType || !bytes.Equal(w.Body.Bytes(), want) {
			t.Errorf("%s: %d %q %d bytes", path, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
		if got := w.Header().Get("Cache-Control"); got != assetCache || !strings.Contains(got, "max-age=31536000") {
			t.Errorf("%s Cache-Control %q", path, got)
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
	}
	for _, path := range []string{"/assets/", "/assets", "/assets/README.md", "/assets/nope.webp", "/assets/../app.js", "/assets/%2e%2e/app.js", "/assets/glasshouse-fern.webp/", "/web/assets/glasshouse-fern.webp", "/assets/GLASSHOUSE-FERN.webp"} {
		if w := do(t, h, authed(path)); w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %q", path, w.Code, w.Header().Get("Cache-Control"))
		}
	}
	for _, path := range []string{"/", "/app.js", "/style.css", "/api/lab"} {
		if got := do(t, h, authed(path)).Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control %q", path, got)
		}
	}
}

// Every asset the served page, script and stylesheet reference carries the
// binary's version as ?v=, so the year-long immutable cache never serves stale
// art or fonts after an upgrade; the versioned URL is served, and the version is
// URL-safe and bounded.
func TestUIAssetsReferencedWithVersionQuery(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir(), Version: "v1.4.0-rc.1"})
	ref := regexp.MustCompile(`/assets/[A-Za-z0-9._-]+\.(?:webp|woff2)[^"'\s)]*`)
	seen := map[string]bool{}
	for _, file := range []string{"/", "/app.js", "/style.css"} {
		body := do(t, h, authed(file)).Body.String()
		if strings.Contains(body, assetVersionMark) {
			t.Errorf("%s leaks the version placeholder", file)
		}
		for _, url := range ref.FindAllString(body, -1) {
			path, query, _ := strings.Cut(url, "?")
			if query != "v=v1.4.0-rc.1" {
				t.Errorf("%s references %s without the version query", file, url)
			}
			seen[path] = true
			if w := do(t, h, authed(url)); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != assetCache {
				t.Errorf("%s: %d %q", url, w.Code, w.Header().Get("Cache-Control"))
			}
		}
	}
	for path := range shellAssets {
		if !seen[path] {
			t.Errorf("no served file references %s", path)
		}
	}
	for version, want := range map[string]string{"": "dev", "dev": "dev", "v1.2.3": "v1.2.3", `v1"><script>/../x?y&z`: "v1script..xyz", "ünï": "n", strings.Repeat("a", 100): strings.Repeat("a", maxAssetVersionBytes)} {
		if got := assetVersion(version); got != want {
			t.Errorf("assetVersion(%q) = %q, want %q", version, got, want)
		}
	}
}

// The served page is the app shell: a skip link, one main, a header with the
// loop pill and the avatar, real ?view= links for the five views in the nav and
// the bottom tab bar, the needs-you badge on Inbox, coming-next placeholders,
// decorative art and the resident's slot. The Work, Insights and Formula views
// keep their existing containers.
func TestUIShellServedPageHasViewsNavAndTabBar(t *testing.T) {
	page := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/")).Body.String()
	if n := strings.Count(page, "<main"); n != 1 || !strings.Contains(page, `<main id="main" tabindex="-1">`) || !strings.Contains(page, `<a class="skip-link" href="#main">Skip to content</a>`) {
		t.Errorf("page has %d main elements or no skip link", n)
	}
	nav := between(t, page, `<nav class="app-nav" aria-label="Lab">`, "</nav>")
	tabs := between(t, page, `<nav class="tab-bar" aria-label="Lab sections">`, "</nav>")
	for _, view := range []string{"inbox", "issues", "work", "insights", "formula"} {
		link := `href="?view=` + view + `" data-view="` + view + `"`
		if !strings.Contains(nav, `<a class="nav-link" `+link) {
			t.Errorf("the nav has no real link to %s", view)
		}
		if !strings.Contains(tabs, link) {
			t.Errorf("the tab bar has no real link to %s", view)
		}
		if n := strings.Count(page, `data-view-panel="`+view+`"`); n != 1 {
			t.Errorf("%d panels for view %s", n, view)
		}
	}
	if !regexp.MustCompile(`data-view="inbox">[^\n]*Inbox<span id="needs-you-badge" class="nav-badge" hidden></span></a>`).MatchString(nav) || !strings.Contains(tabs, `id="needs-you-tab-badge"`) {
		t.Error("the Inbox link lacks the needs-you badge")
	}
	if !strings.Contains(tabs, `<details id="tab-more" class="tab-more">`) || strings.Count(tabs, `class="tab"`) != 4 {
		t.Error("the tab bar is not Inbox, Issues, Work and More")
	}
	// Only Inbox, the default view, is shown before the script runs.
	for _, panel := range regexp.MustCompile(`<[a-z]+ [^>]*data-view-panel="([a-z]+)"[^>]*>`).FindAllStringSubmatch(page, -1) {
		if hidden := strings.Contains(panel[0], " hidden"); hidden == (panel[1] == "inbox") {
			t.Errorf("panel %s hidden=%t", panel[1], hidden)
		}
	}
	for _, id := range []string{"view-inbox", "view-issues"} {
		if !strings.Contains(between(t, page, `id="`+id+`"`, "</section>"), "Coming next") {
			t.Errorf("%s is not a coming-next placeholder", id)
		}
	}
	work := between(t, page, `id="view-work"`, `id="aggregate"`)
	for _, want := range []string{`id="run-filters"`, `id="cubes"`, `id="molecule-search"`, `id="selected"`, `id="motion-toggle"`, "Inside your Lab."} {
		if !strings.Contains(work, want) {
			t.Errorf("the Work view lacks %s", want)
		}
	}
	if insights := between(t, page, `id="aggregate"`, "</section>"); !strings.Contains(insights, `data-view-panel="insights"`) || !strings.Contains(insights, `<article id="analytics" class="card">`) {
		t.Error("the Insights view is not the analytics overview")
	}
	if formula := between(t, page, `id="view-formula"`, "</section>"); !strings.Contains(formula, `<div id="active-formula" class="active-formula"></div>`) {
		t.Error("the Formula view lacks the active-Formula card")
	}
	header := between(t, page, `<header class="app-header">`, "</header>")
	for _, want := range []string{`<span class="wordmark">Forgecell</span>`, `id="lab-repo"`, `id="loop-pill"`, `id="loop-pill-text">Loop is idle<`, `<a id="loop-start-link" class="btn btn-ghost loop-pill-action" href="#drive-loop">Start loop</a>`, `<details id="loop-stop" class="loop-stop" hidden>`, `<button type="button" id="loop-stop-confirm" class="btn btn-danger">Stop loop</button>`, `id="operator-avatar" class="avatar" role="img" aria-label="Operator"`} {
		if !strings.Contains(header, want) {
			t.Errorf("the header lacks %s", want)
		}
	}
	// The danger action sits only inside the Stop disclosure.
	if strings.Count(page, "btn-danger") != 1 || !strings.Contains(between(t, page, `<details id="loop-stop"`, "</details>"), "btn-danger") {
		t.Error("the Stop loop danger button is not behind its disclosure")
	}
	for _, art := range []string{`<img class="art art-fern" src="/assets/glasshouse-fern.webp?v=dev" alt="" aria-hidden="true"`, `<img class="art art-orchid" src="/assets/orchid-maidenhair.webp?v=dev" alt="" aria-hidden="true"`} {
		if !strings.Contains(page, art) {
			t.Errorf("the page lacks decorative art %s", art)
		}
	}
	side := between(t, page, `<aside class="side-panel" aria-label="Lab status">`, "</aside>")
	if !strings.Contains(side, `<div id="resident" class="glass resident" data-pose="reading">`) || !strings.Contains(side, `<div class="resident-figure" aria-hidden="true">`) || !strings.Contains(side, `<div id="drive-slot" class="drive-slot"></div>`) {
		t.Error("the side panel lacks the resident or the Start Work slot")
	}
}

// between returns the text of s after the first start up to the next end.
func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("no %q", start)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("no %q after %q", end, start)
	}
	return rest[:j]
}

// The served script routes ?view= for the five views through real links and
// history.pushState, restores the view on Back, badges Inbox with the
// needs-you count, maps Lab state to the resident's pose, renders the loop
// pill through the existing stop call and the avatar as text.
func TestUIShellServedJSRoutesViewsBadgePillAvatarAndPose(t *testing.T) {
	js := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/app.js")).Body.String()
	for _, want := range []string{
		"const VIEWS = ['inbox', 'issues', 'work', 'insights', 'formula'];",
		"const view = new URLSearchParams(location.search).get('view');",
		"return VIEWS.includes(view) ? view : 'inbox';",
		"if (view !== viewFromURL()) history.pushState({ view: view }, '', '?view=' + view);",
		"window.addEventListener('popstate', () => showView(viewFromURL()));",
		"const link = e.target.closest('a[data-view]');",
		"if (!link || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;",
		"panel.hidden = panel.dataset.viewPanel !== view;",
		"link.setAttribute('aria-current', 'page')",
		"showView(viewFromURL());",
		// A Molecule opens under Work from any view.
		"if (elsewhere) navigate('work', false);",
		// The badge and the resident follow the list counts, patched in place.
		"const waiting = counts.decision || 0;",
		"for (const badge of [$('#needs-you-badge'), $('#needs-you-tab-badge')]) {",
		"badge.hidden = waiting === 0;",
		"setText(badge, String(waiting));",
		"new Intl.PluralRules('en-US')",
		"function setText(node, text) { if (node.textContent !== text) node.textContent = text; }",
		"if (resident.dataset.pose !== pose) resident.dataset.pose = pose;",
		"renderShell();",
		// The loop pill reuses the existing stop call; the form gets its id.
		"$('#loop-stop-confirm').addEventListener('click', stopLoop);",
		"phase === 'idle' ? 'Loop is idle' : (phase === 'draining' ? 'Loop stopping · ' : 'Loop running · ') + inFlight + ' in flight'",
		"'Stop the loop' + (loop && loop.select ? ' on “' + loop.select + '”' : '') + '?'",
		"id: 'drive-loop', tabindex: '-1', role: 'group', 'aria-labelledby': 'drive-loop-title'",
		"fill($('#drive-slot'), drive.panel);",
		"renderLoopPill();",
		// The avatar is text and attributes only.
		"renderOperator(lab.operator);",
		"avatar.setAttribute('aria-label', 'Operator: ' + name);",
		"/^[\\p{L}\\p{N}]{1,2}$/u.test(o.initials)",
		"setText($('#operator-initials'), initials);",
		"toggle('resident-offscreen', !entry.isIntersecting)).observe($('#resident'));",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	pose := jsFunction(t, js, "residentPose")
	waving := strings.Index(pose, "if ((counts.decision || 0) > 0) return 'waving';")
	studying := strings.Index(pose, "if ((counts.running || 0) > 0) return 'studying';")
	reading := strings.Index(pose, "return 'reading';")
	if waving < 0 || studying < waving || reading < studying {
		t.Errorf("residentPose does not map needs-you, then running, then reading:\n%s", pose)
	}
	// The overview no longer hides itself when a Molecule is selected.
	if strings.Contains(js, "$('#aggregate').hidden") {
		t.Error("app.js still hides the Insights view on selection")
	}
}

// The served CSS loads Instrument Sans from the embedded assets, hides the art
// under 900px and in forced colors, floats only the resident's working pose and
// pauses it, swaps the nav for the tab bar on phones and gives coarse pointers
// 44px targets.
func TestUIShellServedCSSFontArtPoseAndTabBar(t *testing.T) {
	css := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/style.css")).Body.String()
	rules := cssRules(css)
	wantDecls(t, rules, "@font-face", `font-family: "Instrument Sans";`, "font-weight: 400;", "font-weight: 500 600;", "font-display: swap;",
		`src: url("/assets/instrument-sans-400.woff2?v=dev") format("woff2");`, `src: url("/assets/instrument-sans-500.woff2?v=dev") format("woff2");`)
	wantDecls(t, rules, "body", `font: 15px/1.5 "Instrument Sans", system-ui`)
	wantDecls(t, rules, "a, button, summary, input, label", "touch-action: manipulation;")
	wantDecls(t, rules, ".art", "position: fixed;", "pointer-events: none;")
	wantDecls(t, rules, ".resident-sprite", `background: url("/assets/lab-companions.webp?v=dev")`, "/ 300% 100%")
	// The base sprite rule (before the reduced-motion one) never animates.
	if base := strings.SplitN(rules[".resident-sprite"], "animation: none", 2)[0]; strings.Contains(base, "animation") {
		t.Errorf("the resident floats outside its working pose: %s", base)
	}
	wantDecls(t, rules, ".resident[data-pose=studying] .resident-sprite", "background-position: left center;", "animation: resident-float")
	wantDecls(t, rules, ".resident[data-pose=reading] .resident-sprite", "background-position: center center;")
	wantDecls(t, rules, ".resident[data-pose=waving] .resident-sprite", "background-position: right center;")
	for _, still := range []string{".resident[data-pose=reading] .resident-sprite", ".resident[data-pose=waving] .resident-sprite"} {
		if strings.Contains(rules[still], "animation") {
			t.Errorf("%s animates: %s", still, rules[still])
		}
	}
	wantDecls(t, rules, ".motion-paused .resident-sprite, .page-hidden .resident-sprite, .resident-offscreen .resident-sprite", "animation-play-state: paused !important;")
	wantDecls(t, cssRules(mediaBlock(t, css, "@media (prefers-reduced-motion: reduce)")), ".resident-sprite", "animation: none !important;")
	wantDecls(t, cssRules(mediaBlock(t, css, "@media (max-width: 900px)")), ".art", "display: none;")
	forced := cssRules(mediaBlock(t, css, "@media (forced-colors: active)"))
	wantDecls(t, forced, ".art", "display: none;")
	wantDecls(t, forced, ".nav-link[aria-current=page], .tab[aria-current=page]", "outline: 2px solid Highlight;")
	mobile := cssRules(mediaBlock(t, css, "@media (max-width: 720px)"))
	wantDecls(t, mobile, ".app-nav", "display: none;")
	wantDecls(t, mobile, ".tab-bar", "position: fixed;", "display: flex;")
	wantDecls(t, mobile, ".shell", "grid-template-columns: minmax(0, 1fr);")
	wantDecls(t, rules, ".tab-bar", "display: none;")
	wantDecls(t, cssRules(mediaBlock(t, css, "@media (pointer: coarse)")), ".nav-link, .tab, .btn, .loop-pill-action, .lane-segment, .text-button, summary, .motion-option", "min-height: 44px;")
	wantDecls(t, rules, ".skip-link:focus", "top: 12px;")
	wantDecls(t, rules, ".btn-danger", "color: var(--danger-ink);", "border-color: var(--danger-edge);")
	wantDecls(t, rules, ".avatar", "border-radius: 50%;", "background: #1e4d3b;")
	wantDecls(t, rules, ".nav-badge", "font-variant-numeric: tabular-nums;")
}
