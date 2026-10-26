package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	testHost  = "127.0.0.1:4321"
	testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func testHandler(t *testing.T, o Options) http.Handler {
	t.Helper()
	h, err := NewHandler(o, testToken, testHost)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type request struct {
	method, path, origin, host, site string
	cookie                           *http.Cookie
	header                           map[string]string
	body                             string
}

// theSession stands for the handler's own session secret in a request cookie,
// and for its tab value in the X-Forgecell-Session header. An authed request
// sends both unless its header map sets X-Forgecell-Session itself ("" omits it).
const theSession = "the-session"

func authed(path string) request {
	return request{path: path, cookie: &http.Cookie{Name: "forgecell_ui_4321", Value: theSession}}
}

func do(t *testing.T, h http.Handler, r request) *httptest.ResponseRecorder {
	t.Helper()
	if r.method == "" {
		r.method = http.MethodGet
	}
	req := httptest.NewRequest(r.method, "http://"+testHost+r.path, strings.NewReader(r.body))
	req.Host = testHost
	if r.host != "" {
		req.Host = r.host
	}
	if r.origin != "" {
		req.Header.Set("Origin", r.origin)
	}
	if r.site != "" {
		req.Header.Set("Sec-Fetch-Site", r.site)
	}
	tab, explicit := r.header[sessionHeader]
	if !explicit && r.cookie != nil && r.cookie.Value == theSession {
		tab = theSession
	}
	if tab == theSession {
		tab = h.(*handler).tab
	}
	for k, v := range r.header {
		req.Header.Set(k, v)
	}
	req.Header.Del(sessionHeader)
	if tab != "" {
		req.Header.Set(sessionHeader, tab)
	}
	if r.cookie != nil {
		c := *r.cookie
		if c.Value == theSession {
			c.Value = h.(*handler).session
		}
		req.AddCookie(&c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestUITokenExchangeSetsStrictHttpOnlyCookie(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	if w := do(t, h, request{path: "/?t=" + strings.Repeat("f", 64)}); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Fatalf("wrong token: %d %v", w.Code, w.Result().Cookies())
	}
	// The launch page opened from a local file arrives as a cross-site navigation.
	w := do(t, h, request{path: "/?t=" + testToken, site: "cross-site", header: map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}})
	if w.Code != http.StatusOK || w.Body.String() != forwardPage(h.(*handler).tab) || w.Header().Get("Location") != "" {
		t.Fatalf("exchange: %d %v %s", w.Code, w.Header(), w.Body)
	}
	// The one page that carries the tab value is never cached and has no inline
	// script: only the external, same-origin session.js.
	page := w.Body.String()
	if w.Header().Get("Cache-Control") != "no-store" || regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]+</script>|<script>|\son[a-z]+=|\sstyle=`).MatchString(page) || !strings.Contains(page, `<script src="/session.js"></script>`) || !strings.Contains(page, `<meta name="forgecell-session" content="`+h.(*handler).tab+`">`) {
		t.Fatalf("exchange page %v %s", w.Header(), page)
	}
	select {
	case <-h.(*handler).exchanged:
	default:
		t.Fatal("the exchange was not signalled")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "forgecell_ui_4321" || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Path != "/" {
		t.Fatalf("cookie %+v", cookies)
	}
	// The cookie is a separate secret, never the launch token.
	if cookies[0].Value == testToken || len(cookies[0].Value) != 64 || cookies[0].Value != h.(*handler).session {
		t.Fatalf("cookie value %q", cookies[0].Value)
	}
	// The token is single-use: a second exchange is refused, also for the right
	// token, and a cookie equal to the token never authorizes.
	if w := do(t, h, request{path: "/?t=" + testToken}); w.Code != http.StatusForbidden || len(w.Result().Cookies()) != 0 {
		t.Fatalf("second exchange: %d %v", w.Code, w.Result().Cookies())
	}
	if w := do(t, h, request{path: "/api/lab", cookie: &http.Cookie{Name: "forgecell_ui_4321", Value: testToken}}); w.Code != http.StatusForbidden {
		t.Fatalf("the token as a cookie: %d", w.Code)
	}
	// Not even a browser holding the cookie gets the exchange page, and so the
	// tab value, a second time.
	if w := do(t, h, authed("/?t="+testToken)); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), h.(*handler).tab) {
		t.Fatalf("exchange with the session: %d", w.Code)
	}
	// Only a document navigation may arrive cross-site, and only to exchange.
	if w := do(t, testHandler(t, Options{LabDir: t.TempDir()}), request{path: "/?t=" + testToken, site: "cross-site", header: map[string]string{"Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"}}); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site fetch exchanged: %d", w.Code)
	}
	nav := authed("/api/lab")
	nav.site, nav.header = "cross-site", map[string]string{"Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
	if w := do(t, h, nav); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site navigation to the API: %d", w.Code)
	}
	if w := do(t, h, authed("/")); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<script src=\"/app.js\"></script>") {
		t.Fatalf("index with cookie: %d", w.Code)
	}
}

// A 127.0.0.1 cookie reaches every local port, so the API also needs the tab
// value, which only the exchange page hands out; static files need the cookie.
func TestUIAPIRequiresTheTabSessionHeader(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	for _, path := range []string{"/api/lab", "/api/molecules", "/api/molecules/m1"} {
		cookieOnly := authed(path)
		cookieOnly.header = map[string]string{sessionHeader: ""}
		wrong := authed(path)
		wrong.header = map[string]string{sessionHeader: h.(*handler).session}
		headerOnly := request{path: path, header: map[string]string{sessionHeader: theSession}}
		for name, r := range map[string]request{"cookie only": cookieOnly, "cookie secret as header": wrong, "header without cookie": headerOnly} {
			if w := do(t, h, r); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d", name, path, w.Code)
			}
		}
	}
	if w := do(t, h, authed("/api/lab")); w.Code != http.StatusOK {
		t.Fatalf("cookie and header: %d", w.Code)
	}
	if h.(*handler).tab == h.(*handler).session || h.(*handler).tab == testToken {
		t.Fatal("the tab value repeats another secret")
	}
	cookieOnly := authed("/")
	cookieOnly.header = map[string]string{sessionHeader: ""}
	if w := do(t, h, cookieOnly); w.Code != http.StatusOK {
		t.Fatalf("index with the cookie: %d", w.Code)
	}
	js := do(t, h, request{path: "/session.js"})
	if js.Code != http.StatusOK || strings.Contains(js.Body.String(), h.(*handler).tab) || !strings.Contains(js.Body.String(), "sessionStorage.setItem") || !strings.Contains(js.Body.String(), "location.replace('/')") {
		t.Fatalf("session.js: %d %s", js.Code, js.Body)
	}
}

func TestUIRefusesRequestsWithoutTheCookie(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	for _, path := range []string{"/", "/app.js", "/api/lab", "/api/molecules", "/api/molecules/m1"} {
		if w := do(t, h, request{path: path}); w.Code != http.StatusForbidden {
			t.Errorf("%s without cookie: %d", path, w.Code)
		}
		r := authed(path)
		r.cookie.Value = strings.Repeat("0", 64)
		if w := do(t, h, r); w.Code != http.StatusForbidden {
			t.Errorf("%s with a wrong cookie: %d", path, w.Code)
		}
		r = authed(path)
		r.cookie.Name = "forgecell_ui_9999"
		if w := do(t, h, r); w.Code != http.StatusForbidden {
			t.Errorf("%s with another port's cookie: %d", path, w.Code)
		}
	}
}

func TestUIRefusesForeignOriginAndHost(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	for name, mutate := range map[string]func(*request){
		"foreign origin":    func(r *request) { r.origin = "http://evil.example" },
		"other port origin": func(r *request) { r.origin = "http://127.0.0.1:9999" },
		"localhost origin":  func(r *request) { r.origin = "http://localhost:4321" },
		"null origin":       func(r *request) { r.origin = "null" },
		"rebinding host":    func(r *request) { r.host = "evil.example:4321" },
		"localhost host":    func(r *request) { r.host = "localhost:4321" },
		"cross-site fetch":  func(r *request) { r.site = "cross-site" },
		"same-site fetch":   func(r *request) { r.site = "same-site" },
	} {
		r := authed("/api/lab")
		mutate(&r)
		if w := do(t, h, r); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	r := authed("/api/lab")
	r.origin, r.site = "http://"+testHost, "same-origin"
	if w := do(t, h, r); w.Code != http.StatusOK {
		t.Fatalf("same origin: %d %s", w.Code, w.Body)
	}
}

// Every route except the three gate routes and the four drive routes serves GET
// only; those serve POST only. A same-origin POST elsewhere is still refused.
func TestUIServesGetOnlyWithSecurityHeaders(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	for _, path := range []string{"/", "/api/lab", "/api/loop", "/api/runs/1", "/api/molecules", "/api/molecules/m1", "/api/molecules/m1/job", "/api/molecules/m1/ledger", "/api/molecules/m1/delivery", "/api/molecules/m1/merge", "/api/molecules/m1/delivery/merge", "/api/molecules/m1/x/scope", "/api/molecules/m1/x/stop", "/api/loop/pause", "/api/runs/1/stop"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions} {
			r := authed(path)
			r.method, r.origin = method, "http://"+testHost
			if w := do(t, h, r); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodGet {
				t.Errorf("%s %s: %d", method, path, w.Code)
			}
		}
	}
	for _, path := range []string{"/api/molecules/m1/scope", "/api/molecules/m1/delivery/preview", "/api/molecules/m1/delivery/publish", "/api/runs", "/api/loop/start", "/api/loop/stop", "/api/molecules/m1/stop"} {
		for _, method := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead, http.MethodOptions} {
			r := authed(path)
			r.method = method
			if w := do(t, h, r); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
				t.Errorf("%s %s: %d", method, path, w.Code)
			}
		}
		if w := do(t, h, authed(path)); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/", "/api/lab", "/api/molecules", "/nope"} {
		w := do(t, h, authed(path))
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s CSP %q", path, csp)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s headers %v", path, w.Header())
		}
		for k := range w.Header() {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s sent CORS header %s", path, k)
			}
		}
	}
}

// labFixture writes a Lab with one complete retained Molecule whose untrusted
// text includes markup, plus paths that must never be served.
func labFixture(t *testing.T) (string, molecule.Record) {
	t.Helper()
	lab := t.TempDir()
	yaml := "schemaVersion: v0\nid: f\n"
	sum := sha256.Sum256([]byte(yaml))
	cost := 0.25
	input := int64(1200)
	r := molecule.Record{
		SchemaVersion: "v0", Kind: "molecule", ID: "mol-7-abc", Status: "waiting", FormulaID: "f", Mode: "ticket",
		StartedAt: "2026-10-24T10:00:00Z", LabDir: lab,
		Issue:           molecule.Issue{Repo: "o/r", Number: 7, Title: `<script>alert("x")</script> Fix <b>it</b>`, Body: "Body <img src=x onerror=alert(1)> from " + lab, URL: "https://github.com/o/r/issues/7", State: "OPEN", UpdatedAt: "2026-10-24T09:00:00Z"},
		Workspace:       molecule.Workspace{SourceRoot: "/Users/someone/src/r", Path: filepath.Join(lab, "workspaces", "issue-7"), Branch: "forgecell/issue-7", BaseCommit: "0123456789abcdef0123456789abcdef01234567", Repo: "o/r"},
		Readiness:       &readiness.State{SchemaVersion: "v1", Phase: "review-waiting", Plans: []readiness.Proposal{{Plan: readiness.Plan{Revision: 1}, Digest: "d1"}, {Plan: readiness.Plan{Revision: 2, ParentDigest: "d1"}, Digest: "d2"}}, Events: []readiness.Event{{Kind: "proposed", Digest: "d1", At: "2026-10-24T10:00:01Z", Detail: "plan at " + filepath.Join(lab, "workspaces", "issue-7")}, {Kind: "amended", Digest: "d2", At: "2026-10-24T10:05:00Z"}}},
		FormulaApproved: true,
		FormulaSnapshot: molecule.Snapshot{YAML: yaml, SHA256: hex.EncodeToString(sum[:])},
		Atoms: []molecule.Atom{
			{ID: "intake", Type: "intake", Status: "completed", Detail: "Read issue"},
			{ID: "code", Type: "harness", Status: "completed", Detail: "Coded in " + filepath.Join(lab, "workspaces", "issue-7"), Provenance: &molecule.AtomProvenance{DeclaredBinding: "claude-code", PlannedAction: "edit", Actions: []molecule.AtomAction{{AttemptID: "h1", PlannedAction: "edit", StartedAt: "2026-10-24T10:06:00Z", ExecutedBinding: "claude-code", Observed: &molecule.AtomObservation{Action: "edit", Result: "ok", FinishedAt: "2026-10-24T10:07:00Z", Evidence: []molecule.ProvenanceEvidence{
				{Path: filepath.Join(lab, "evidence", "h1.json"), SHA256: "aa"},
				{Path: "evidence/h2.json", SHA256: "bb"},
				{Path: "/etc/passwd", SHA256: "cc"},
				{Path: "../outside.json", SHA256: "dd"},
				{Pointer: "/harnessAttempts/0"},
			}}}}}},
			{ID: "review", Type: "review", Status: "waiting", Detail: "Waiting for review"},
			{ID: "deliver", Type: "gate", Status: "pending", Detail: "Not started"},
		},
		Notes:           []string{"note naming " + lab},
		HarnessAttempts: []molecule.HarnessAttempt{{ID: "h1", PlanDigest: "d2", Model: "claude-opus-5-5", Usage: &harness.Usage{TotalCostUSD: &cost, CostBasis: "list", Total: harness.TokenCategories{Input: &input}}}},
	}
	writeRecord(t, lab, r)
	return lab, r
}

func writeRecord(t *testing.T, lab string, r molecule.Record) {
	t.Helper()
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "ledgers", r.ID+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeReceipt(t *testing.T, lab, id, state, lifecycle string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(lab, "deliveries"), 0700); err != nil {
		t.Fatal(err)
	}
	life := ""
	if lifecycle != "" {
		life = fmt.Sprintf(`,"lifecycle":{"state":%q,"observedAt":"2026-10-24T11:00:00Z"}`, lifecycle)
	}
	raw := fmt.Sprintf(`{"state":%q,"title":"t","acceptanceReviewAcknowledged":false,"preview":{"moleculeId":%q,"diff":"secret diff"},"url":"https://github.com/o/r/pull/5"%s}`, state, id, life)
	if err := os.WriteFile(filepath.Join(lab, "deliveries", id+".json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
}

func TestUIRecordProjectionOfARetainedMolecule(t *testing.T) {
	lab, r := labFixture(t)
	writeReceipt(t, lab, r.ID, "committed", "")
	h := testHandler(t, Options{LabDir: lab})
	w := do(t, h, authed("/api/molecules/"+r.ID))
	raw := w.Body.String()
	var d Detail
	decode(t, w, &d)
	if d.ID != r.ID || d.Sample || d.Issue.Title != r.Issue.Title || d.Issue.Number != 7 || d.Issue.URL != "https://github.com/o/r/issues/7" {
		t.Fatalf("intake %+v", d.Issue)
	}
	if d.Workspace.Branch != "forgecell/issue-7" || d.Workspace.BaseCommit != r.Workspace.BaseCommit || d.Formula.SHA256 != r.FormulaSnapshot.SHA256 || d.Formula.YAML != "" || !d.Formula.Approved {
		t.Fatalf("workspace/formula %+v %+v", d.Workspace, d.Formula)
	}
	if d.Readiness == nil || len(d.Readiness.Plans) != 2 || d.Readiness.Plans[1].Revision != 2 || d.Readiness.Plans[1].Digest != "d2" || d.Readiness.Plans[1].ParentDigest != "d1" || len(d.Readiness.Events) != 2 {
		t.Fatalf("readiness %+v", d.Readiness)
	}
	if len(d.Atoms) != 4 || d.Atoms[1].Provenance == nil || d.CurrentAtom != "review" {
		t.Fatalf("atoms %+v current %q", d.Atoms, d.CurrentAtom)
	}
	ev := d.Atoms[1].Provenance.Actions[0].Observed.Evidence
	if len(ev) != 5 || ev[0].Path != "evidence/h1.json" || ev[0].SHA256 != "aa" || ev[1].Path != "evidence/h2.json" || ev[2].Path != "" || ev[2].Note == "" || ev[3].Path != "" || ev[4].Pointer != "/harnessAttempts/0" {
		t.Fatalf("evidence %+v", ev)
	}
	if len(d.Attempts) != 1 || d.Attempts[0].Model != "claude-opus-5-5" || d.Attempts[0].Usage == nil || *d.Attempts[0].Usage.TotalCostUSD != 0.25 || *d.Attempts[0].Usage.Total.Input != 1200 {
		t.Fatalf("attempts %+v", d.Attempts)
	}
	if d.Lane != LaneDecision || d.Delivery == nil || d.Delivery.Published || d.Delivery.State != "committed" || !strings.HasPrefix(d.Delivery.Label, "Delivery in progress") || d.Delivery.URL != "https://github.com/o/r/pull/5" {
		t.Fatalf("lane %q delivery %+v", d.Lane, d.Delivery)
	}
	// Nothing local or secret leaves the server, and markup is escaped JSON text.
	for _, leak := range []string{lab, "/Users/someone", r.Workspace.Path, "labDir", "sourceRoot", "secret diff", "/etc/passwd", "<script>", "<img", "notes"} {
		if strings.Contains(raw, leak) {
			t.Errorf("projection carries %q", leak)
		}
	}
	escaped := `\` + "u003cscript" + `\` + "u003e"
	if !strings.Contains(raw, escaped) || !strings.Contains(d.Issue.Body, "<lab>") || !strings.Contains(d.Atoms[1].Detail, "<workspace>") {
		t.Fatalf("untrusted text was not served as redacted, escaped JSON text: %s", raw)
	}
	var withYAML Detail
	decode(t, do(t, h, authed("/api/molecules/"+r.ID+"?yaml=1")), &withYAML)
	if withYAML.Formula.YAML != r.FormulaSnapshot.YAML || len(withYAML.Diagnostics) != 0 {
		t.Fatalf("yaml %+v", withYAML.Formula)
	}
	for path, code := range map[string]int{"/api/molecules/absent": 404, "/api/molecules/..%2fetc": 400, "/api/molecules/a.b": 400, "/api/molecules/": 400} {
		if w := do(t, h, authed(path)); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
}

// forgecell deliver publishes the draft without moving the ledger phase: a
// review-waiting Molecule with a published receipt is finished, counted as Draft
// published, and its detail shows the review gate resolved by draft publication
// and the merge-on-GitHub next action.
func TestUIPublishedReviewWaitingMoleculeIsFinishedWithTheGateResolved(t *testing.T) {
	lab, r := labFixture(t)
	r.FinishedAt = "2026-10-24T10:30:00Z"
	writeRecord(t, lab, r)
	writeReceipt(t, lab, r.ID, "published", "OPEN")
	h := testHandler(t, Options{LabDir: lab})
	var list MoleculeList
	decode(t, do(t, h, authed("/api/molecules")), &list)
	if len(list.Molecules) != 1 || list.Molecules[0].Lane != LaneFinished || list.Counts[LaneFinished] != 1 || list.Counts[LaneDecision] != 0 || list.Molecules[0].Delivery != "Draft published" {
		t.Fatalf("list %+v counts %v", list.Molecules, list.Counts)
	}
	var analytics AnalyticsView
	decode(t, do(t, h, authed("/api/analytics")), &analytics)
	if analytics.Counts[LaneFinished] != 1 || analytics.Counts[LaneDecision] != 0 {
		t.Fatalf("analytics counts %v", analytics.Counts)
	}
	w := do(t, h, authed("/api/molecules/"+r.ID))
	var d Detail
	decode(t, w, &d)
	if d.Lane != LaneFinished || d.Gate != (GateView{State: GatePublished, Receipt: "published", Lifecycle: "OPEN", URL: "https://github.com/o/r/pull/5"}) {
		t.Fatalf("lane %q gate %+v", d.Lane, d.Gate)
	}
	if d.Next.Title != "Draft published" || d.Next.Copy != "Draft published · merge on GitHub; merge and production are not verified by Forgecell." || strings.Contains(d.Next.Copy, "Gates and decisions") {
		t.Fatalf("next %+v", d.Next)
	}
	noLeak(t, w.Body.String(), lab)
	// A receipt from a PR on another repository is never linked.
	if err := os.WriteFile(filepath.Join(lab, "deliveries", r.ID+".json"), []byte(`{"state":"published","title":"t","acceptanceReviewAcknowledged":false,"preview":{"moleculeId":"`+r.ID+`"},"url":"https://github.com/x/y/pull/5"}`), 0600); err != nil {
		t.Fatal(err)
	}
	d = Detail{}
	decode(t, do(t, h, authed("/api/molecules/"+r.ID)), &d)
	if d.Gate.State != GatePublished || d.Gate.URL != "" {
		t.Fatalf("foreign PR gate %+v", d.Gate)
	}
	// Blocked with a receipt stays blocked; scope-waiting with one stays a decision.
	blocked := r
	blocked.Status = "blocked"
	writeRecord(t, lab, blocked)
	writeReceipt(t, lab, r.ID, "published", "OPEN")
	d = Detail{}
	decode(t, do(t, h, authed("/api/molecules/"+r.ID)), &d)
	if d.Lane != LaneBlocked || strings.Contains(d.Next.Copy, "Gates and decisions") {
		t.Fatalf("blocked lane %q next %+v", d.Lane, d.Next)
	}
	_, scope, digest := gateFixture(t)
	scope.LabDir = lab
	writeRecord(t, lab, scope)
	d = Detail{}
	decode(t, do(t, h, authed("/api/molecules/"+scope.ID)), &d)
	if d.Lane != LaneDecision || d.Gate != (GateView{State: GateScope, Digest: digest}) || !strings.Contains(d.Next.Copy, "decide under Gates and decisions") {
		t.Fatalf("scope-waiting lane %q gate %+v next %+v", d.Lane, d.Gate, d.Next)
	}
}

// NEXT ACTION and GATES AND DECISIONS come from one gate state: NEXT ACTION tells
// the user to decide under Gates and decisions exactly when a gate there is open.
func TestUINextActionAgreesWithTheGateState(t *testing.T) {
	lab, base := labFixture(t)
	h := testHandler(t, Options{LabDir: lab})
	digest := strings.Repeat("ab", 32)
	pending := &readiness.State{SchemaVersion: "v1", Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: readiness.Plan{Revision: 1}, Digest: digest}}}
	n := 0
	for _, status := range []string{"waiting", "recorded", "running", "blocked", "completed"} {
		for _, finished := range []string{"", "2026-10-24T10:30:00Z"} {
			for _, readinessState := range []*readiness.State{nil, readinessPhase("review-waiting"), readinessPhase("scope-waiting"), pending, readinessPhase("coding")} {
				for _, receipt := range []string{"", "committed", "published"} {
					n++
					r := base
					r.ID, r.Status, r.FinishedAt, r.Readiness = fmt.Sprintf("m%03d", n), status, finished, readinessState
					writeRecord(t, lab, r)
					if receipt != "" {
						writeReceipt(t, lab, r.ID, receipt, "OPEN")
					}
					var d Detail
					decode(t, do(t, h, authed("/api/molecules/"+r.ID)), &d)
					open := d.Gate.State == GateScope || d.Gate.State == GateDelivery
					if open && (d.Lane == LaneRunning || d.Lane == LaneBlocked || d.Lane == LaneFinished) {
						t.Errorf("%s: a %s Molecule has open gate %+v", r.ID, d.Lane, d.Gate)
					}
					if says := strings.Contains(d.Next.Copy, "Gates and decisions"); says != open {
						t.Errorf("%s %s finished %q receipt %q: gate %+v, next %+v", status, d.Phase, finished, receipt, d.Gate, d.Next)
					}
					if d.Next.Title == "" || d.Next.Copy == "" || (d.Gate.State == GateScope) != (d.Gate.Digest == digest) {
						t.Errorf("%s: gate %+v next %+v", r.ID, d.Gate, d.Next)
					}
					if receipt == "published" && d.Gate.State != GateScope && d.Gate.State != GatePublished {
						t.Errorf("%s: a published receipt left gate %+v", r.ID, d.Gate)
					}
				}
			}
		}
	}
}

func TestUIRejectsAnIssueURLForAnotherIssue(t *testing.T) {
	lab, r := labFixture(t)
	for _, url := range []string{"https://github.com/o/r/issues/8", "https://github.com/x/r/issues/7", "javascript:alert(1)", "http://github.com/o/r/issues/7"} {
		r.Issue.URL = url
		writeRecord(t, lab, r)
		var d Detail
		decode(t, do(t, testHandler(t, Options{LabDir: lab}), authed("/api/molecules/"+r.ID)), &d)
		if d.Issue.URL != "" || len(d.Diagnostics) == 0 {
			t.Errorf("%s served as %q", url, d.Issue.URL)
		}
	}
}

func TestUIMoleculeListCountsAndETag(t *testing.T) {
	lab, r := labFixture(t)
	ledger := filepath.Join(lab, "ledgers", r.ID+".json")
	age(t, ledger, time.Hour)
	h := testHandler(t, Options{LabDir: lab})
	w := do(t, h, authed("/api/molecules"))
	var list MoleculeList
	decode(t, w, &list)
	etag := w.Header().Get("ETag")
	if etag == "" || list.ETag != etag || list.Sample || !list.Complete || len(list.Molecules) != 1 {
		t.Fatalf("list %+v etag %q", list, etag)
	}
	m := list.Molecules[0]
	if m.ID != r.ID || m.Repository != "o/r" || m.Number != 7 || m.Title != r.Issue.Title || m.Status != "waiting" || m.Phase != "review-waiting" || m.CurrentAtom != "review" || m.StartedAt != r.StartedAt || m.Lane != LaneDecision {
		t.Fatalf("summary %+v", m)
	}
	if list.Counts["all"] != 1 || list.Counts[LaneDecision] != 1 {
		t.Fatalf("counts %v", list.Counts)
	}
	cached := authed("/api/molecules")
	cached.header = map[string]string{"If-None-Match": etag}
	if w := do(t, h, cached); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("unchanged poll: %d", w.Code)
	}
	r.Status = "blocked"
	r.Notes = append(r.Notes, "changed so the size differs")
	writeRecord(t, lab, r)
	age(t, ledger, 30*time.Minute)
	w = do(t, h, cached)
	if w.Code != http.StatusOK || w.Header().Get("ETag") == etag {
		t.Fatalf("changed ledger kept etag: %d", w.Code)
	}
	decode(t, w, &list)
	if list.Molecules[0].Lane != LaneBlocked {
		t.Fatalf("lane after change %+v", list.Molecules[0])
	}
	writeReceipt(t, lab, r.ID, "published", "OPEN")
	if w := do(t, h, cached); w.Header().Get("ETag") == etag {
		t.Fatal("a new receipt kept the etag")
	}
}

// A ledger rewritten within the racy window, even at the same size and
// modification second, is never answered with 304: its ETag changes per call.
func TestUIListETagChangesWhileAnEntryIsFresh(t *testing.T) {
	lab, r := labFixture(t)
	ledger := filepath.Join(lab, "ledgers", r.ID+".json")
	if ledgerETag(lab) == ledgerETag(lab) {
		t.Fatal("a fresh ledger kept its ETag")
	}
	age(t, ledger, time.Hour)
	first := ledgerETag(lab)
	if ledgerETag(lab) != first {
		t.Fatal("an aged ledger changed its ETag")
	}
	writeReceipt(t, lab, r.ID, "committed", "")
	if ledgerETag(lab) == ledgerETag(lab) {
		t.Fatal("a fresh receipt kept the ETag")
	}
	age(t, filepath.Join(lab, "deliveries", r.ID+".json"), time.Hour)
	if a := ledgerETag(lab); a != ledgerETag(lab) || a == first {
		t.Fatal("aged entries did not settle on a new ETag")
	}
}

func TestUIListReportsUnreadableLedgersWithoutLocalPaths(t *testing.T) {
	lab, _ := labFixture(t)
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "bad.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(lab, "ledgers", "link.json")); err != nil {
		t.Fatal(err)
	}
	w := do(t, testHandler(t, Options{LabDir: lab}), authed("/api/molecules"))
	var list MoleculeList
	decode(t, w, &list)
	if list.Complete || len(list.Diagnostics) != 2 || len(list.Molecules) != 1 || list.Counts["all"] != 1 {
		t.Fatalf("list %+v", list)
	}
	if strings.Contains(w.Body.String(), lab) {
		t.Fatalf("diagnostics carry the Lab path: %s", w.Body)
	}
}

func TestUILabIdentityWithoutAFormula(t *testing.T) {
	lab := t.TempDir()
	var v LabView
	decode(t, do(t, testHandler(t, Options{LabDir: lab, Version: "v9"}), authed("/api/lab")), &v)
	if v.Sample || v.ReadOnly || len(v.Gates) != 2 || v.Version != "v9" || v.Name != filepath.Base(lab) || v.Formula.State != "none" || len(v.Unavailable) == 0 {
		t.Fatalf("lab %+v", v)
	}
	if err := os.WriteFile(filepath.Join(lab, ".formula-write.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	decode(t, do(t, testHandler(t, Options{LabDir: lab}), authed("/api/lab")), &v)
	if v.Formula.State != "activation-locked" {
		t.Fatalf("locked lab %+v", v.Formula)
	}
}

// snapshot lists every path, size and modification time in a directory tree.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d %d %s\n", path, info.Size(), info.ModTime().UnixNano(), info.Mode())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Reads, writes to non-gate routes and unauthenticated or forged gate and drive
// writes never change the Lab and never reach the engine. Only gates_test.go
// and drive_test.go cover accepted writes, through engine seams.
func TestUINeverWritesTheLab(t *testing.T) {
	lab, r := labFixture(t)
	writeReceipt(t, lab, r.ID, "published", "OPEN")
	before := snapshot(t, lab)
	h := testHandler(t, Options{LabDir: lab, SourceRoot: "/src/checkout"})
	engine := refuseEngine(t, h)
	drive := installDrive(h, &fakeDrive{})
	for _, path := range []string{"/", "/app.js", "/style.css", "/api/lab", "/api/loop", "/api/runs/7", "/api/molecules", "/api/molecules/" + r.ID, "/api/molecules/" + r.ID + "?yaml=1", "/api/molecules/" + r.ID + "/job", "/api/runs", "/api/loop/start", "/api/loop/stop", "/api/molecules/" + r.ID + "/stop"} {
		do(t, h, authed(path))
		post := authed(path)
		post.method, post.origin = http.MethodPost, "http://"+testHost
		do(t, h, post)
	}
	body := `{"digest":"` + strings.Repeat("a", 64) + `","title":"t"}`
	for _, action := range []string{"scope", "delivery/preview", "delivery/publish"} {
		for name, forged := range forgedWrites("/api/molecules/"+r.ID+"/"+action, body) {
			if w := do(t, h, forged); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d", name, action, w.Code)
			}
		}
	}
	for path, body := range map[string]string{"/api/runs": `{"issue":7}`, "/api/loop/start": `{"select":"oldest","maxConcurrent":1}`, "/api/loop/stop": `{}`, "/api/molecules/" + r.ID + "/stop": `{}`} {
		for name, forged := range forgedWrites(path, body) {
			if w := do(t, h, forged); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d", name, path, w.Code)
			}
		}
	}
	if after := snapshot(t, lab); after != before {
		t.Fatalf("the UI changed the Lab:\n%s\n---\n%s", before, after)
	}
	if n := engine.calls() + drive.calls(); n != 0 {
		t.Fatalf("refused writes reached the engine %d times", n)
	}
}

// The served script never parses data as markup: no HTML sinks at all, and the
// page has no inline script, style or handler for data to reach.
func TestUIServedJSNeverAssignsDataToInnerHTML(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	js := do(t, h, authed("/app.js"))
	if js.Code != http.StatusOK || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("app.js: %d %v", js.Code, js.Header())
	}
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "DOMParser", "createContextualFragment", "eval(", "new Function", "srcdoc", "javascript:"} {
		if strings.Contains(js.Body.String(), sink) {
			t.Errorf("app.js uses %s", sink)
		}
	}
	if !strings.Contains(js.Body.String(), "document.createTextNode") || !strings.Contains(js.Body.String(), "textContent") {
		t.Fatal("app.js does not render text through text nodes")
	}
	page := do(t, h, authed("/")).Body.String()
	if regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]+</script>|<script>`).MatchString(page) || regexp.MustCompile(`\son[a-z]+=|\sstyle=`).MatchString(page) {
		t.Fatal("index.html has inline script, style or handlers")
	}
	for _, name := range []string{"app.js", "style.css", "index.html"} {
		data, err := web.ReadFile("web/" + name)
		if err != nil || len(data) == 0 {
			t.Fatalf("embedded %s: %v", name, err)
		}
	}
}

// DOM replaceChildren and append render a null child as the text "null", so the
// served script renders only through el, add and fill, which skip null,
// undefined and false: the overview, analytics included, has no literal null.
func TestUIServedJSNeverRendersNullChildren(t *testing.T) {
	js := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/app.js")).Body.String()
	if !strings.Contains(js, "if (child === undefined || child === null || child === false) continue;") || !strings.Contains(js, "return add(node, ...children);") {
		t.Fatal("el and fill do not skip null children through add")
	}
	if m := regexp.MustCompile(`replaceChildren\(\s*[^)\s]`).FindAllString(js, -1); len(m) != 0 {
		t.Errorf("app.js passes children to replaceChildren directly: %q", m)
	}
	if n := strings.Count(js, ".append("); n != 1 || !strings.Contains(js, "node.append(child instanceof Node ? child : document.createTextNode(String(child)));") {
		t.Errorf("app.js appends outside add: %d calls", n)
	}
	for _, name := range []string{"renderOverview", "renderAnalytics"} {
		body := regexp.MustCompile(`(?s)function ` + name + `\(\) \{(.*?)\n  \}\n`).FindStringSubmatch(js)
		if body == nil {
			t.Fatalf("app.js has no %s", name)
		}
		if strings.Contains(body[1], "replaceChildren") || !strings.Contains(body[1], "fill(") || regexp.MustCompile(`['"]null['"]`).MatchString(body[1]) {
			t.Errorf("%s may render a literal null:\n%s", name, body[1])
		}
	}
}

func TestUIDemoServesLabeledSampleDataOnly(t *testing.T) {
	demo := testHandler(t, Options{Demo: true, Version: "v"})
	var lab LabView
	decode(t, do(t, demo, authed("/api/lab")), &lab)
	var list MoleculeList
	decode(t, do(t, demo, authed("/api/molecules")), &list)
	if !lab.Sample || !list.Sample || len(list.Molecules) != demoCount {
		t.Fatalf("demo lab %+v list %d", lab, len(list.Molecules))
	}
	sum := 0
	for _, lane := range Lanes {
		sum += list.Counts[lane]
	}
	if sum != list.Counts["all"] || list.Counts[LaneRunning] == 0 || list.Counts[LaneDecision] == 0 || list.Counts[LaneBlocked] == 0 || list.Counts[LaneFinished] == 0 {
		t.Fatalf("demo counts %v", list.Counts)
	}
	var d Detail
	decode(t, do(t, demo, authed("/api/molecules/demo-101")), &d)
	if !d.Sample || len(d.Atoms) != 6 || d.Next.Title == "" || d.Gate.State != GateNone || strings.Contains(d.Next.Copy, "Gates and decisions") {
		t.Fatalf("demo detail %+v", d)
	}
	if w := do(t, demo, authed("/api/molecules/demo-9999")); w.Code != http.StatusNotFound {
		t.Fatalf("unknown sample: %d", w.Code)
	}
	// Real mode never serves synthetic values, even for a sample-shaped id.
	realMode := testHandler(t, Options{LabDir: t.TempDir()})
	w := do(t, realMode, authed("/api/molecules"))
	list = MoleculeList{}
	decode(t, w, &list)
	if list.Sample || len(list.Molecules) != 0 || strings.Contains(w.Body.String(), "demo-") {
		t.Fatalf("real list %s", w.Body)
	}
	if w := do(t, realMode, authed("/api/molecules/demo-101")); w.Code != http.StatusNotFound {
		t.Fatalf("real mode served a sample Molecule: %d", w.Code)
	}
}

func TestUIListenServesLoopbackAndStops(t *testing.T) {
	s, err := Listen(Options{LabDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+/\?t=[0-9a-f]{64}$`).MatchString(s.URL) {
		t.Fatalf("URL %q", s.URL)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	res, err := client.Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	exchangePage := string(body)
	if res.StatusCode != http.StatusOK || exchangePage != forwardPage(s.handler.tab) {
		t.Fatalf("exchange via token: %d", res.StatusCode)
	}
	select {
	case <-s.Exchanged():
	default:
		t.Fatal("Exchanged was not closed by the exchange")
	}
	base := strings.SplitN(s.URL, "/?", 2)[0]
	res, err = client.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), "Inside your Lab.") {
		t.Fatalf("index with the session cookie: %d", res.StatusCode)
	}
	res, err = (&http.Client{Timeout: 10 * time.Second}).Get(s.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a second browser reused the launch URL: %d", res.StatusCode)
	}
	get := func(header string) int {
		req, _ := http.NewRequest(http.MethodGet, base+"/api/molecules", nil)
		if header != "" {
			req.Header.Set(sessionHeader, header)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := get(""); code != http.StatusForbidden {
		t.Fatalf("api with the cookie only: %d", code)
	}
	if code := get(regexp.MustCompile(`name="forgecell-session" content="([0-9a-f]{64})"`).FindStringSubmatch(exchangePage)[1]); code != http.StatusOK {
		t.Fatalf("api with cookie and header: %d", code)
	}
	res, err = http.Get(base + "/api/molecules")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("api without cookie: %d", res.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop after cancellation")
	}
	if _, err := http.Get(base + "/"); err == nil {
		t.Fatal("server still answers after stopping")
	}
}

// The poll loop keeps one request in flight and drops stale responses, keeps
// focus and open details across re-renders, re-announces the inventory note only
// on change, refetches the Lab when the ETag changes, and maps every Atom
// status the engine writes.
func TestUIServedJSGuardsPollsFocusAndAtomStatuses(t *testing.T) {
	js := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/app.js")).Body.String()
	for _, want := range []string{
		"if (document.hidden || state.polling || state.sessionLost) return;",
		"token !== state.pollToken",
		"finally { state.polling = false; }",
		"if (previous && previous !== state.etag) loadLab()",
		"token !== state.labToken",
		"text === state.detailText",
		"keepFocus(renderList)",
		"keepFocus(renderDetail)",
		"if (key === state.noteKey) return;",
		"{ open: wasOpen }",
		"'X-Forgecell-Session': session",
		"sessionStorage.getItem('forgecell-session')",
		"if (!session || state.sessionLost) throw new SessionError",
		"loadLab().catch(failed); }, 30000);",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	marks := regexp.MustCompile(`(?s)const ATOM_MARKS = \{(.*?)\n  \};`).FindStringSubmatch(js)
	if marks == nil {
		t.Fatal("app.js has no ATOM_MARKS table")
	}
	for status, mark := range map[string]string{"done": "['✓', 'is-done']", "failed": "['✕', 'is-failed']", "blocked": "['!', 'is-failed']", "skipped": "['–', 'is-skipped']", "waiting": "['◇', 'is-waiting']", "active": "[null, 'is-running']"} {
		if !strings.Contains(marks[1], " "+status+": "+mark) {
			t.Errorf("Atom status %s is not mapped to %s", status, mark)
		}
	}
	if strings.Contains(marks[1], "pending:") {
		t.Error("pending must show its step number, not a mark")
	}
}
