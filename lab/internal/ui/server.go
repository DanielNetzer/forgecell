// Package ui serves the local Lab UI on the loopback interface. It reads the Lab
// only through engine Go functions, never a shell. Every read is a GET; the only
// writes are the human gates in gates.go (scope approval and draft publication),
// Request changes on a waiting plan (amend.go, which records a revision and never
// approves it), the drive actions in drive.go (start intake, start or stop the
// loop, stop a Molecule this UI runs), retiring one Molecule (retire.go), the
// next actions of a blocked Molecule (actions.go: accept by-design failures,
// approve a requested scope change, re-run checks and recover, each recording a
// revision or an interruption and never approving) and retiring the attempts
// for closed Issues (bulk_retire.go), each a POST that needs the session cookie,
// the exact Origin and the per-tab session header, and each performed by the
// engine, which validates digests, locks and receipts. The reads that run a
// process are a Molecule's diff size (diffsize.go), git diff --numstat through
// the engine, bounded and cached by tree, and its next actions, whose
// accept-failures preview reads the retained tree through git. It never
// merges, deploys or changes permissions, and recovers only on an explicit
// confirmation.
package ui

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web
var web embed.FS

// Options configure one UI launch.
type Options struct {
	// LabDir is the Lab to read; it is ignored in demo mode.
	LabDir  string
	Version string
	// Demo serves the prototype's synthetic data, labeled Sample data, instead of
	// the Lab. Real mode never serves synthetic values.
	Demo bool
	// Port is the loopback port; zero chooses a free one.
	Port int
	// SourceRoot is the source checkout work started from the UI runs in, as the
	// CLI's working directory is for forgecell run and loop; without it the UI
	// starts no work.
	SourceRoot string
	// Resources is the Lab's resource guard (#189) for work started from the UI.
	Resources *resources.Guard
	// Operator is the source checkout's git config user.name, as the CLI read it.
	// It is untrusted: GET /api/lab serves it only through operatorView.
	Operator string
}

// Server is a UI listening on 127.0.0.1.
type Server struct {
	// URL carries the single-use launch token; the first visit exchanges it for a
	// session cookie and every later visit to it is refused.
	URL      string
	listener net.Listener
	server   *http.Server
	handler  *handler
}

// Exchanged is closed by the first successful token exchange, after which the
// launch URL is spent and any launch page that carried it can be deleted.
func (s *Server) Exchanged() <-chan struct{} { return s.handler.exchanged }

// Listen binds 127.0.0.1 and prepares the handler with a fresh random token.
func Listen(o Options) (*Server, error) {
	if o.Port < 0 || o.Port > 65535 {
		return nil, fmt.Errorf("invalid port %d", o.Port)
	}
	if !o.Demo {
		abs, err := filepath.Abs(o.LabDir)
		if err != nil {
			return nil, err
		}
		o.LabDir = abs
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(o.Port)))
	if err != nil {
		return nil, err
	}
	host := ln.Addr().String()
	h, err := NewHandler(o, token, host)
	if err != nil {
		ln.Close()
		return nil, err
	}
	return &Server{
		URL:      "http://" + host + "/?t=" + token,
		listener: ln,
		handler:  h.(*handler),
		server:   &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10},
	}, nil
}

// Serve answers requests until ctx is cancelled, then shuts down gracefully.
func (s *Server) Serve(ctx context.Context) error {
	done := make(chan error, 1)
	go func() { done <- s.server.Serve(s.listener) }()
	select {
	case <-ctx.Done():
		// Gate and run jobs and previews are cancelled first, and the loop drains, and
		// no new one starts; the engine records an interrupted run in the ledger.
		s.handler.stopWork(jobStopGrace)
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// A connection still open after the grace period (for example one a browser
		// opened speculatively) is closed rather than failing the stop.
		if err := s.server.Shutdown(shutdown); err != nil {
			s.server.Close()
		}
		<-done
		return nil
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type handler struct {
	opts  Options
	token string
	// used is set by the first valid exchange; the launch token is single-use.
	used      atomic.Bool
	exchanged chan struct{}
	// session is the cookie value: a separate random secret, so the launch URL
	// and the cookie never carry the same value.
	session string
	// tab is the X-Forgecell-Session value every API request must also carry. A
	// browser sends 127.0.0.1 cookies to every port, so the cookie alone can reach
	// another local server; tab is handed over only once, in the exchange page, and
	// kept in the tab's origin-scoped sessionStorage.
	tab    string
	host   string
	origin string
	cookie string
	static fs.FS
	// records and receipts read ledgers and delivery receipts; tests may replace
	// them.
	records  func(lab, id string) (molecule.Record, error)
	receipts receiptReader
	cache    *listCache
	// runScope, preview and publish are the engine calls behind the gates; tests
	// replace them so no git, gh or harness process starts.
	runScope func(context.Context, molecule.Options) (molecule.Record, error)
	preview  func(context.Context, delivery.Options) (delivery.PreviewResult, error)
	publish  func(context.Context, delivery.PublishOptions) (delivery.Receipt, error)
	jobs     *gateJobs
	// runIssue and runLoop are the engine calls behind the drive routes; tests
	// replace them. loop is this UI's single loop slot.
	runIssue func(context.Context, molecule.Options) (molecule.Record, error)
	runLoop  func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error)
	loop     *driveLoop
	// listIssues is the loop's open Issue listing, the engine's default; the UI
	// wraps it to learn which Molecules each loop pass works on. Tests replace it.
	listIssues func(context.Context, string, intake.Selector) (intake.OpenIssues, error)
	// retire is the engine call behind the retire route; tests may replace it.
	retire func(lab, id, reason string) (molecule.Record, error)
	// amend is the engine call behind Request changes, the path of forgecell amend
	// without --run; amendMu keeps one amendment from this UI at a time. Tests may
	// replace amend.
	amend   func(context.Context, string, string, molecule.AmendRequest) (molecule.Record, error)
	amendMu sync.Mutex
	// numstat measures a captured tree's diff size; diffs keeps each size by base
	// and tree. Tests may replace numstat.
	numstat func(ctx context.Context, lab, workspace, base, tree string) (molecule.DiffStat, error)
	diffs   *diffCache
	// forge is the read-only GitHub sync behind /api/issues and /api/sync; tests
	// replace its fetcher and clock.
	forge *issueSync
	// recover and deriveActions are the engine calls behind the next actions
	// (actions.go): forgecell recover and molecule.NextActions. Tests may replace
	// them.
	recover       func(ctx context.Context, lab, id, digest string, confirmedStopped bool) (molecule.Record, error)
	deriveActions func(ctx context.Context, lab string, r molecule.Record, preview bool, timeout time.Duration) []molecule.NextAction
	// closedDeadline, when a test sets it, is the bulk retire's overall deadline
	// in place of bulkRetireTimeout (bulk_retire.go).
	closedDeadline func() (context.Context, context.CancelFunc)
}

// staticFiles are the only embedded files served, with their content types.
// Every script module and stylesheet is named here: there is no wildcard or
// prefix match, so any other /js/ or /css/ path is not found.
var staticFiles = map[string]string{
	"/":       "index.html",
	"/app.js": "app.js",
	// The entry's modules: the shared core and one module per view, with the
	// Molecule view's sub-modules.
	"/js/core.js":              "js/core.js",
	"/js/core-gates.js":        "js/core-gates.js",
	"/js/inbox.js":             "js/inbox.js",
	"/js/molecule.js":          "js/molecule.js",
	"/js/molecule-plan.js":     "js/molecule-plan.js",
	"/js/molecule-decision.js": "js/molecule-decision.js",
	"/js/molecule-timeline.js": "js/molecule-timeline.js",
	"/js/work.js":              "js/work.js",
	"/js/issues.js":            "js/issues.js",
	"/js/insights.js":          "js/insights.js",
	"/js/formula.js":           "js/formula.js",
	"/js/loop.js":              "js/loop.js",
	// The stylesheets index.html links, in its order: tokens, base, then views.
	"/css/tokens.css":   "css/tokens.css",
	"/css/base.css":     "css/base.css",
	"/css/inbox.css":    "css/inbox.css",
	"/css/molecule.css": "css/molecule.css",
	"/css/work.css":     "css/work.css",
	"/css/issues.css":   "css/issues.css",
	"/css/insights.css": "css/insights.css",
	"/css/formula.css":  "css/formula.css",
	"/css/loop.css":     "css/loop.css",
	// session.js only copies the exchange page's value into sessionStorage; it
	// holds no secret, so it is served without the cookie.
	"/session.js": "session.js",
	// The committed art and fonts. Like every other file they need the cookie;
	// any other /assets/ path is not found.
	"/assets/glasshouse-fern.webp":      "assets/glasshouse-fern.webp",
	"/assets/orchid-maidenhair.webp":    "assets/orchid-maidenhair.webp",
	"/assets/lab-companions.webp":       "assets/lab-companions.webp",
	"/assets/instrument-sans-400.woff2": "assets/instrument-sans-400.woff2",
	"/assets/instrument-sans-500.woff2": "assets/instrument-sans-500.woff2",
}

// assetCache is the Cache-Control of the files under /assets/, which never
// change within a release; every other response stays no-store.
const assetCache = "private, max-age=31536000, immutable"

// assetVersionMark is the placeholder index.html, the scripts and the
// stylesheets (css/tokens.css and css/base.css today) put in every asset URL's
// ?v= query; file replaces it with assetVersion, so an
// upgrade changes each asset URL and the immutable cache never serves stale art
// or fonts.
const assetVersionMark = "__ASSET_VERSION__"

// maxAssetVersionBytes bounds the version in asset URLs.
const maxAssetVersionBytes = 64

// assetVersion is the binary's version reduced to URL-safe characters and
// bounded, or "dev" when nothing is left.
func assetVersion(version string) string {
	var b strings.Builder
	for _, r := range version {
		if b.Len() == maxAssetVersionBytes {
			break
		}
		if r < 0x80 && (r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "dev"
	}
	return b.String()
}

const sessionHeader = "X-Forgecell-Session"

var contentTypes = map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".webp": "image/webp", ".woff2": "font/woff2"}

var moleculeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

// unavailable lists capabilities the UI shows as Unavailable in this slice.
var unavailable = []string{"cost per merged PR", "suggestions", "evaluation", "pause"}

// NewHandler serves the UI for host (127.0.0.1:PORT) with the given launch token.
func NewHandler(o Options, token, host string) (http.Handler, error) {
	static, err := fs.Sub(web, "web")
	if err != nil {
		return nil, err
	}
	if len(token) < 32 {
		return nil, fmt.Errorf("launch token is too short")
	}
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		return nil, err
	}
	session, err := newToken()
	if err != nil {
		return nil, err
	}
	tab, err := newToken()
	if err != nil {
		return nil, err
	}
	return &handler{opts: o, token: token, exchanged: make(chan struct{}), session: session, tab: tab, host: host, origin: "http://" + host, cookie: "forgecell_ui_" + port, static: static, records: molecule.ReadRecord, receipts: delivery.ReadReceiptStatus, cache: newListCache(), runScope: molecule.Run, preview: delivery.Preview, publish: delivery.Publish, jobs: newGateJobs(), runIssue: molecule.RunIntake, runLoop: molecule.RunLoop, loop: &driveLoop{}, retire: molecule.Retire, listIssues: intake.ReadSelectedOpenIssues, amend: molecule.AmendFromLatest, numstat: molecule.DiffNumstat, diffs: newDiffCache(), forge: newIssueSync(nil, nil), recover: molecule.Recover, deriveActions: molecule.NextActions}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Cross-Origin-Resource-Policy", "same-origin")
	header.Set("Cache-Control", "no-store")
	// The Host check refuses DNS rebinding; the Origin and Fetch Metadata checks
	// refuse other sites and other local pages. There are no CORS headers.
	if r.Host != h.host {
		h.refuse(w, http.StatusForbidden, "unexpected Host")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != h.origin {
		h.refuse(w, http.StatusForbidden, "cross-origin request refused")
		return
	}
	launch := r.Method == http.MethodGet && r.URL.Path == "/" && r.URL.Query().Has("t")
	// The launch page opened from a local file navigates here cross-site, so the
	// token exchange alone accepts a top-level document navigation from anywhere;
	// it needs the single-use token and serves no Lab data.
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" && !(launch && navigation(r)) {
		h.refuse(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	// GET serves every read; POST is served only on the gate and drive routes.
	gateID, gateAction, gate := gateRoute(r.URL.Path)
	driveID, driveAction, drive := driveRoute(r.URL.Path)
	actionID, actionKind, action := actionRoute(r.URL.Path)
	if r.Method != http.MethodGet && (r.Method != http.MethodPost || !(gate || drive || action)) {
		if gate || drive || action {
			header.Set("Allow", http.MethodPost)
		} else {
			header.Set("Allow", http.MethodGet)
		}
		h.refuse(w, http.StatusMethodNotAllowed, "only GET is served here; the only writes are the gate and drive POST routes")
		return
	}
	if launch {
		h.exchange(w, r)
		return
	}
	if r.URL.Path == "/session.js" {
		h.file(w, staticFiles[r.URL.Path])
		return
	}
	if !h.authorized(r) {
		h.refuse(w, http.StatusForbidden, "open the URL printed by forgecell ui")
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/") && subtle.ConstantTimeCompare([]byte(r.Header.Get(sessionHeader)), []byte(h.tab)) != 1 {
		h.refuse(w, http.StatusForbidden, "this tab has no Lab UI session; open a new launch link from forgecell ui")
		return
	}
	if r.Method == http.MethodPost {
		if !h.writeAllowed(r) {
			h.refuse(w, http.StatusForbidden, "a write needs this server's exact Origin from the same site; nothing was written")
			return
		}
		if drive {
			h.drive(w, r, driveID, driveAction)
			return
		}
		if action {
			if !moleculeID.MatchString(actionID) {
				h.refuseGate(w, http.StatusBadRequest, "invalid", "invalid Molecule id")
				return
			}
			if h.opts.Demo {
				h.refuseGate(w, http.StatusNotFound, "sample", "sample data has no actions; nothing was written")
				return
			}
			h.moleculeAction(w, r, actionID, actionKind)
			return
		}
		if h.refuseRetiredScope(w, gateID, gateAction) {
			return
		}
		h.gate(w, r, gateID, gateAction)
		return
	}
	switch path := r.URL.Path; {
	case path == "/api/lab":
		h.lab(w)
	case path == "/api/loop":
		h.loopStatus(w)
	case strings.HasPrefix(path, "/api/runs/"):
		h.runStatus(w, strings.TrimPrefix(path, "/api/runs/"))
	case path == "/api/molecules":
		h.molecules(w, r)
	case path == "/api/analytics":
		h.analytics(w, r)
	case path == "/api/issues":
		h.issues(w, r)
	case path == "/api/sync":
		h.syncStates(w, r)
	case path == "/api/retire-closed":
		h.retireClosedStatus(w, r)
	case strings.HasPrefix(path, "/api/molecules/"):
		h.molecule(w, r, strings.TrimPrefix(path, "/api/molecules/"))
	case staticFiles[path] != "":
		h.file(w, staticFiles[path])
	default:
		h.refuse(w, http.StatusNotFound, "not found")
	}
}

func navigation(r *http.Request) bool {
	mode, dest := r.Header.Get("Sec-Fetch-Mode"), r.Header.Get("Sec-Fetch-Dest")
	return (mode == "" || mode == "navigate") && (dest == "" || dest == "document")
}

// forwardPage is served once, on the successful exchange (never cached, as every
// response is no-store). It carries the tab value in a meta element; the
// external session.js copies it into sessionStorage and replaces the page with
// "/", which drops the token from the address bar and starts a same-origin
// navigation, so the SameSite=Strict cookie is sent even when the launch came
// from a local file. The value is hex, so it needs no escaping.
func forwardPage(tab string) string {
	return `<!doctype html><meta charset="utf-8"><meta name="forgecell-session" content="` + tab + `"><title>Forgecell Lab UI</title><script src="/session.js"></script><p>Opening the Forgecell Lab UI…</p>`
}

// exchange trades the launch token, once, for an HttpOnly, SameSite=Strict
// session cookie and the tab value. Every later exchange is refused, also from a
// browser holding the cookie, so the tab value is never handed out again.
func (h *handler) exchange(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(h.token)) != 1 {
		h.refuse(w, http.StatusForbidden, "invalid launch token")
		return
	}
	if !h.used.CompareAndSwap(false, true) {
		h.refuse(w, http.StatusForbidden, "this launch URL was already used; restart forgecell ui for a new one")
		return
	}
	close(h.exchanged)
	http.SetCookie(w, &http.Cookie{Name: h.cookie, Value: h.session, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Content-Type", contentTypes[".html"])
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, forwardPage(h.tab))
}

func (h *handler) authorized(r *http.Request) bool {
	c, err := r.Cookie(h.cookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(h.session)) == 1
}

func (h *handler) refuse(w http.ResponseWriter, code int, message string) {
	h.json(w, code, map[string]string{"error": message})
}

func (h *handler) json(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	// The default encoder escapes <, > and &, so no response is parsed as markup.
	_ = json.NewEncoder(w).Encode(value)
}

func (h *handler) file(w http.ResponseWriter, name string) {
	data, err := fs.ReadFile(h.static, name)
	if err != nil {
		h.refuse(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", contentTypes[filepath.Ext(name)])
	if strings.HasPrefix(name, "assets/") {
		w.Header().Set("Cache-Control", assetCache)
	} else {
		data = bytes.ReplaceAll(data, []byte(assetVersionMark), []byte(assetVersion(h.opts.Version)))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// LabView is GET /api/lab. ReadOnly is true only for sample data; a real Lab
// lists the human gates it performs through the engine in Gates, and the work it
// can start and stop through the engine in Actions.
type LabView struct {
	Sample      bool             `json:"sample"`
	ReadOnly    bool             `json:"readOnly"`
	Gates       []string         `json:"gates"`
	Actions     []string         `json:"actions"`
	Name        string           `json:"name"`
	Version     string           `json:"version"`
	Repository  string           `json:"repository"`
	Binding     string           `json:"binding"`
	Model       string           `json:"model"`
	Formula     formula.Identity `json:"formula"`
	Unavailable []string         `json:"unavailable"`
	// Operator is read-only display text for the avatar; it is never an email.
	Operator OperatorView `json:"operator"`
}

func (h *handler) lab(w http.ResponseWriter) {
	if h.opts.Demo {
		h.json(w, http.StatusOK, demoLab(h.opts.Version))
		return
	}
	id := formula.ActiveIdentity(h.opts.LabDir)
	// Retiring, the next actions and Request changes need no source checkout;
	// starting work does.
	actions := []string{RetireAction, UnblockAction, BulkRetireAction, AmendAction}
	if h.opts.SourceRoot != "" {
		actions = append(append([]string{}, DriveActions...), UnblockAction, BulkRetireAction, AmendAction)
	}
	h.json(w, http.StatusOK, LabView{Gates: Gates, Actions: actions, Name: filepath.Base(h.opts.LabDir), Version: h.opts.Version, Repository: id.Repository, Binding: id.Binding, Model: id.Model, Formula: id, Unavailable: unavailable, Operator: operatorView(h.opts.Operator)})
}

func (h *handler) molecules(w http.ResponseWriter, r *http.Request) {
	if h.opts.Demo {
		h.json(w, http.StatusOK, demoList())
		return
	}
	// The tag is taken before reading, so a change during the read shows next poll.
	etag := ledgerETag(h.opts.LabDir)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.json(w, http.StatusOK, h.list(etag))
}

// counts tallies lanes from the same summaries the response carries. All, and
// the Lanes summing to it, exclude retired Molecules; they are counted under
// their own key, so none is silently dropped.
func counts(ms []Summary) map[string]int {
	c := map[string]int{"all": 0}
	for _, lane := range FilterLanes {
		c[lane] = 0
	}
	for _, m := range ms {
		c[m.Lane]++
		if m.Lane != LaneRetired {
			c["all"]++
		}
	}
	return c
}

// molecule serves GET /api/molecules/{id}, its read-only file views,
// /evidence?path=… and /ledger, its gate job status, /job, and its diff size
// and estimate, /stats.
func (h *handler) molecule(w http.ResponseWriter, r *http.Request, rest string) {
	id, view, _ := strings.Cut(rest, "/")
	withYAML := r.URL.Query().Get("yaml") == "1"
	if !moleculeID.MatchString(id) {
		h.refuse(w, http.StatusBadRequest, "invalid Molecule id")
		return
	}
	if view != "" && view != "evidence" && view != "ledger" && view != "job" && view != "stats" && view != "actions" {
		h.refuse(w, http.StatusNotFound, "not found")
		return
	}
	if h.opts.Demo && view != "" {
		h.refuse(w, http.StatusNotFound, "sample data has no ledger files")
		return
	}
	if view == "job" {
		h.job(w, id)
		return
	}
	if h.opts.Demo {
		d, ok := demoDetail(id, withYAML)
		if !ok {
			h.refuse(w, http.StatusNotFound, "no such sample Molecule")
			return
		}
		h.json(w, http.StatusOK, d)
		return
	}
	record, err := h.records(h.opts.LabDir, id)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		h.refuse(w, http.StatusNotFound, "no such Molecule")
		return
	default:
		// Error text can name local paths, so only the category is returned.
		h.refuse(w, http.StatusUnprocessableEntity, "the ledger was refused: it is invalid, unsafe, oversized or not this Lab's")
		return
	}
	switch view {
	case "evidence":
		h.evidence(w, r, record)
	case "ledger":
		text, err := readLedgerText(h.opts.LabDir, record.ID, newRedactor(h.opts.LabDir, record))
		if err != nil {
			h.fileRefusal(w, err)
			return
		}
		h.json(w, http.StatusOK, text)
	case "stats":
		h.json(w, http.StatusOK, h.stats(record))
	case "actions":
		ctx, cancel := context.WithTimeout(r.Context(), previewTimeout)
		defer cancel()
		h.json(w, http.StatusOK, h.actionsView(ctx, record))
	default:
		d := project(h.opts.LabDir, record, h.receipts, withYAML)
		if d.View != nil {
			d.View.Control = h.control(record, d.View.Current)
		}
		h.json(w, http.StatusOK, d)
	}
}

// evidence serves one file only when the ledger lists its exact Lab-relative
// path with a SHA-256; nothing is read from disk for any other path.
func (h *handler) evidence(w http.ResponseWriter, r *http.Request, record molecule.Record) {
	paths := r.URL.Query()["path"]
	if len(paths) != 1 || !validEvidencePath(paths[0]) {
		h.refuse(w, http.StatusBadRequest, "invalid evidence path: give one clean Lab-relative path")
		return
	}
	sums := evidenceAllowlist(h.opts.LabDir, record)[paths[0]]
	if len(sums) == 0 {
		h.refuse(w, http.StatusNotFound, "this path is not listed as evidence with a SHA-256 in the ledger")
		return
	}
	text, err := readEvidence(h.opts.LabDir, paths[0], sums, newRedactor(h.opts.LabDir, record))
	if err != nil {
		h.fileRefusal(w, err)
		return
	}
	h.json(w, http.StatusOK, text)
}

// fileRefusal reports a refused file read by category only; error text could
// name local paths.
func (h *handler) fileRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		h.refuse(w, http.StatusNotFound, "the file is not in the Lab")
	case errors.Is(err, errSHAMismatch):
		h.refuse(w, http.StatusConflict, "the file does not match its recorded SHA-256; nothing is served")
	case errors.Is(err, errEvidenceLarge):
		h.refuse(w, http.StatusRequestEntityTooLarge, "the file is larger than the UI hashes; nothing is served")
	default:
		h.refuse(w, http.StatusUnprocessableEntity, "the file was refused: it is a symlink, not a regular file, not yours, or changed while it was read")
	}
}
