package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"io"
	"mime"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// The UI's gate writes are three human gates, each a POST through the engine
// (drive.go adds starting and stopping work, behind the same protection):
//
//	POST /api/molecules/{id}/scope              {"digest"}  → molecule.Run(Approve) as a job
//	POST /api/molecules/{id}/delivery/preview   {}          → delivery.Preview
//	POST /api/molecules/{id}/delivery/publish   {"digest","title","acceptUnverified"}
//	                                                        → delivery.Publish (draft PR) as a job
//	GET  /api/molecules/{id}/job                            → the job and its canonical read-back
//
// Request changes (amend.go) is the same kind of POST and never approves:
//
//	POST /api/molecules/{id}/amend  {"digest","addAcceptance","addScope"}
//	                                → molecule.AmendFromLatest (no --run); the new revision is approved separately
//
// The engine validates every digest, lock and receipt; the UI only refuses
// early what the engine would refuse anyway. There is no merge, deployment or
// permission route. A job's own outcome never shows a decision as committed:
// the job view reads the ledger or the receipt back on every request.

const (
	// maxGateBody bounds a gate request body; every gate body is a small object.
	maxGateBody = 4 << 10
	// maxGateJobs bounds the job registry; finished jobs are evicted oldest first.
	maxGateJobs = 64
	// maxTitleBytes bounds the delivery commit and PR title.
	maxTitleBytes = 256
	// maxPreviewDiff bounds the diff text one preview response carries.
	maxPreviewDiff = 1 << 20
	// previewTimeout keeps a preview inside the server's write timeout.
	previewTimeout = 45 * time.Second
	// jobStopGrace bounds how long a stopping server waits for cancelled jobs.
	jobStopGrace = 30 * time.Second
)

// Gates lists the human gates this UI performs through the engine.
var Gates = []string{"scope approval", "draft publication"}

// gateRoute reports a POST gate path and its Molecule id and action.
func gateRoute(path string) (id, action string, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/molecules/")
	if !found {
		return "", "", false
	}
	id, action, _ = strings.Cut(rest, "/")
	switch action {
	case "scope", "amend", "delivery/preview", "delivery/publish":
		return id, action, true
	}
	return "", "", false
}

// writeAllowed is the write protection every gate request needs on top of the
// cookie and the tab header: the exact server Origin (never absent) and, when
// the browser sends Fetch Metadata, a same-origin request.
func (h *handler) writeAllowed(r *http.Request) bool {
	if r.Header.Get("Origin") != h.origin {
		return false
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

var errGateBody = errors.New("invalid gate request body")

// decodeGate reads one bounded JSON object with no unknown fields and nothing
// after it.
func decodeGate(w http.ResponseWriter, r *http.Request, v any) error {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		return errGateBody
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxGateBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errGateBody
	}
	if _, err := dec.Token(); err != io.EOF {
		return errGateBody
	}
	return nil
}

type scopeRequest struct {
	Digest string `json:"digest"`
}

type previewRequest struct{}

type publishRequest struct {
	Digest           string `json:"digest"`
	Title            string `json:"title"`
	AcceptUnverified bool   `json:"acceptUnverified"`
}

// validTitle is the one-line title the engine accepts, bounded here.
func validTitle(s string) bool {
	if s == "" || len(s) > maxTitleBytes || strings.TrimSpace(s) != s || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// gateRefusal is a refused gate request: a category the UI keys on, a fixed
// message and an actionable reason naming the next step. Engine error text, which
// can name local paths, is never served: the reason is a fixed phrase.
type gateRefusal struct {
	Error    string `json:"error"`
	Category string `json:"category"`
	Reason   string `json:"reason,omitempty"`
}

func (h *handler) refuseGate(w http.ResponseWriter, code int, category, message string) {
	h.refuseWith(w, code, category, message, refusalReason(category, ""))
}

func (h *handler) refuseWith(w http.ResponseWriter, code int, category, message, reason string) {
	h.json(w, code, gateRefusal{Error: message, Category: category, Reason: reason})
}

// gate serves one POST gate after the write protection has passed.
func (h *handler) gate(w http.ResponseWriter, r *http.Request, id, action string) {
	if !moleculeID.MatchString(id) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "invalid Molecule id")
		return
	}
	if h.opts.Demo {
		h.refuseGate(w, http.StatusNotFound, "sample", "sample data has no gates; nothing was written")
		return
	}
	var scope scopeRequest
	var amend amendRequest
	var publish publishRequest
	var body any = &previewRequest{}
	switch action {
	case "scope":
		body = &scope
	case "amend":
		body = &amend
	case "delivery/publish":
		body = &publish
	}
	if decodeGate(w, r, body) != nil {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "send one small JSON object with exactly the gate's fields")
		return
	}
	record, err := h.records(h.opts.LabDir, id)
	switch {
	case err == nil:
	case errors.Is(err, os.ErrNotExist):
		h.refuseGate(w, http.StatusNotFound, "missing", "no such Molecule")
		return
	default:
		h.refuseGate(w, http.StatusUnprocessableEntity, "refused", "the ledger was refused: it is invalid, unsafe, oversized or not this Lab's")
		return
	}
	switch action {
	case "scope":
		h.approveScope(w, record, scope)
	case "amend":
		h.amendPlan(w, record, amend)
	case "delivery/preview":
		h.previewDelivery(w, record)
	default:
		h.publishDelivery(w, record, publish)
	}
}

// pendingDigest is the latest plan digest while the Molecule waits for scope
// approval.
func pendingDigest(r molecule.Record) (string, bool) {
	s := r.Readiness
	if s == nil || s.Phase != "scope-waiting" || len(s.Plans) == 0 {
		return "", false
	}
	return s.Plans[len(s.Plans)-1].Digest, true
}

// scopeOptions is the engine's human approval call for the recorded Issue, the
// form forgecell amend --run uses.
func scopeOptions(lab string, r molecule.Record, digest string) molecule.Options {
	p := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	return molecule.Options{LabDir: lab, Issue: fmt.Sprintf("%s#%d", r.Issue.Repo, r.Issue.Number), FormulaID: r.FormulaID, Base: p.Inputs.BaseCommit, TargetBranch: p.Inputs.TargetBranch, Approve: digest}
}

func (h *handler) approveScope(w http.ResponseWriter, r molecule.Record, req scopeRequest) {
	if !sha256Hex.MatchString(req.Digest) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give the exact 64-character plan digest shown")
		return
	}
	digest, pending := pendingDigest(r)
	if !pending || req.Digest != digest {
		h.refuseGate(w, http.StatusConflict, "stale", "this plan digest is not the one waiting for approval; reload the Molecule and review the current plan")
		return
	}
	opts := scopeOptions(h.opts.LabDir, r, digest)
	run := h.runScope
	h.startJob(w, r.ID, "scope", digest, func(ctx context.Context) error {
		_, err := run(ctx, opts)
		return err
	})
}

// deliveryOptions binds delivery to the approved plan: its target branch as the
// PR base and its approved scope as the files.
func deliveryOptions(lab string, r molecule.Record) (delivery.Options, bool) {
	if r.Readiness == nil || len(r.Readiness.Plans) == 0 {
		return delivery.Options{}, false
	}
	p := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan
	o := delivery.Options{LabDir: lab, MoleculeID: r.ID, BaseBranch: p.Inputs.TargetBranch}
	for _, s := range p.Analysis.Scope {
		o.Files = append(o.Files, s.Path)
	}
	return o, o.BaseBranch != "" && len(o.Files) > 0
}

// PreviewView is POST …/delivery/preview: delivery.Preview with its diff
// bounded and redacted. Digest is the value publication must send back.
type PreviewView struct {
	Digest               string   `json:"digest"`
	MoleculeID           string   `json:"moleculeId"`
	Repo                 string   `json:"repo"`
	BaseBranch           string   `json:"baseBranch"`
	BaseCommit           string   `json:"baseCommit"`
	Branch               string   `json:"branch"`
	Tree                 string   `json:"tree"`
	Files                []string `json:"files"`
	AcceptanceUnverified bool     `json:"acceptanceUnverified"`
	Diff                 string   `json:"diff"`
	DiffBytes            int      `json:"diffBytes"`
	DiffTruncated        bool     `json:"diffTruncated"`
}

func previewView(lab string, r molecule.Record, p delivery.PreviewResult) PreviewView {
	x := newRedactor(lab, r)
	text := x.text(p.Diff)
	diff := cutRedacted(text, maxPreviewDiff)
	files := make([]string, 0, len(p.Files))
	for _, f := range p.Files {
		files = append(files, x.text(f))
	}
	return PreviewView{Digest: p.Digest, MoleculeID: p.MoleculeID, Repo: p.Repo, BaseBranch: p.BaseBranch, BaseCommit: p.BaseCommit, Branch: p.Branch, Tree: p.Tree, Files: files, AcceptanceUnverified: p.AcceptanceUnverified, Diff: diff, DiffBytes: len(p.Diff), DiffTruncated: len(text) > maxPreviewDiff}
}

func (h *handler) previewDelivery(w http.ResponseWriter, r molecule.Record) {
	opts, ok := deliveryOptions(h.opts.LabDir, r)
	if !ok {
		h.refuseGate(w, http.StatusConflict, "not-ready", "this Molecule has no approved plan with a target branch and file scope to deliver")
		return
	}
	if h.jobs.busy(r.ID) {
		h.refuseGate(w, http.StatusConflict, "busy", "a gate job is still running for this Molecule; wait for it and read its result")
		return
	}
	ctx, cancel := context.WithTimeout(h.jobs.ctx, previewTimeout)
	defer cancel()
	p, err := h.preview(ctx, opts)
	if err != nil {
		state, category, message, reason := classify(err)
		code := http.StatusConflict
		if state == "interrupted" {
			code = http.StatusServiceUnavailable
		} else if category == "refused" || category == "demo" {
			code = http.StatusUnprocessableEntity
		}
		h.refuseWith(w, code, category, message, reason)
		return
	}
	h.json(w, http.StatusOK, previewView(h.opts.LabDir, r, p))
}

func (h *handler) publishDelivery(w http.ResponseWriter, r molecule.Record, req publishRequest) {
	if !sha256Hex.MatchString(req.Digest) || !validTitle(req.Title) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give the exact 64-character preview digest and a one-line title of at most 256 bytes")
		return
	}
	opts, ok := deliveryOptions(h.opts.LabDir, r)
	if !ok {
		h.refuseGate(w, http.StatusConflict, "not-ready", "this Molecule has no approved plan with a target branch and file scope to deliver")
		return
	}
	publish := delivery.PublishOptions{Options: opts, Approve: req.Digest, Title: req.Title, AcceptUnverified: req.AcceptUnverified}
	run := h.publish
	h.startJob(w, r.ID, "publish", req.Digest, func(ctx context.Context) error {
		_, err := run(ctx, publish)
		return err
	})
}

func (h *handler) startJob(w http.ResponseWriter, id, kind, digest string, run func(context.Context) error) {
	job, err := h.jobs.start(id, kind, digest, run)
	if h.refuseJobStart(w, err, "a gate job is already running for this Molecule; this submission was not started") {
		return
	}
	h.json(w, http.StatusAccepted, h.jobView(id, job.snapshot(&h.jobs.mu)))
}

// refuseJobStart answers a job registry refusal and reports whether there was
// one; busy is the message for a job already running under the same key.
func (h *handler) refuseJobStart(w http.ResponseWriter, err error, busy string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errGateBusy):
		h.refuseGate(w, http.StatusConflict, "busy", busy)
	case errors.Is(err, errGateFull):
		h.refuseGate(w, http.StatusServiceUnavailable, "busy", "too many gate jobs are running; nothing was started")
	default:
		h.refuseGate(w, http.StatusServiceUnavailable, "stopped", "the Lab UI is stopping; nothing was started")
	}
	return true
}

// classify maps an engine error to a job state, a category, a fixed message and
// an actionable reason. The error text itself is never served or logged: it only
// selects one of the fixed reasons, which names the next step.
func classify(err error) (state, category, message, reason string) {
	if err == nil {
		return "finished", "", "", ""
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "interrupted", "interrupted", "the request was interrupted before the engine finished; the canonical state was read back", refusalReason("interrupted", "")
	}
	if errors.Is(err, delivery.ErrDemoLab) {
		return "refused", "demo", "this is a forgecell demo Lab; it never contacts GitHub", refusalReason("demo", "")
	}
	text := err.Error()
	has := func(parts ...string) bool {
		for _, p := range parts {
			if strings.Contains(text, p) {
				return true
			}
		}
		return false
	}
	state, category, message = "refused", "refused", "the engine refused the request; nothing else was attempted"
	switch {
	case has("PR reconciliation required"):
		state, category, message = "reconciliation-required", "reconciliation", "the pull request outcome is uncertain; the receipt was read back and nothing is retried"
	case has("is locked", "lock could not be taken", "delivery lock exists"):
		category, message = "locked", "the Molecule or its delivery is locked by another command or a loop; nothing is retried"
	case has("resume must match the recorded review content"):
		category, message = "receipt-mismatch", "the recorded delivery receipt was reviewed with a different digest, title or file scope; nothing was published"
	case has("independent acceptance is unverified", "lacks explicit acceptance review"):
		category, message = "acceptance-unverified", "independent acceptance is unverified; explicit human acceptance review is required before publication"
	case has("approval is stale", "approval requires the exact pending plan digest", "no Molecule matches this approval digest", "readiness inputs changed", "prepared content changed", "issue or Formula changed", "source base changed", "pending workspace changed", "review required", "differs from reviewed", "differs from verified"):
		category, message = "stale", "the reviewed content or plan changed"
	}
	return state, category, message, refusalReason(category, text)
}

// categoryReasons names the next step for each refusal category the UI serves.
var categoryReasons = map[string]string{
	"invalid":               "Correct the value and submit again; nothing was sent to the engine",
	"sample":                "Sample data never writes; run forgecell ui in a Lab of your own repository to act on real Molecules",
	"missing":               "No ledger in this Lab has this id; reload the Lab and choose a Molecule from the grid",
	"none":                  "Nothing was started for it from this Lab UI",
	"stale":                 "Reload this Molecule, review the current plan or preview, and decide again",
	"busy":                  "Wait for the running work to finish and read its result here, then try again",
	"not-ready":             "Delivery needs an approved plan with a target branch and a file scope; approve the plan first, or refresh intake with forgecell run",
	"unavailable":           "Restart forgecell ui from a checkout of this Lab's repository so the engine can run there",
	"resources":             "Free memory or disk space on the Lab's volumes until they are above the resource floors, then try again",
	"not-running":           "Nothing is running to stop; this page shows the current ledger state",
	"not-owned":             "Stop it where it was started: stop the loop here, or interrupt the forgecell command that runs it",
	"stopped":               "Restart forgecell ui and open the new link it prints",
	"refused":               "Run the same action with the forgecell CLI to see the engine's own reason; this page shows only reasons it can state safely",
	"locked":                "Wait for the loop or the other forgecell command to finish, then try again",
	"receipt-mismatch":      "Publish with the digest, title and files the receipt records, or inspect the receipt with the forgecell CLI first",
	"acceptance-unverified": "Review acceptance yourself, tick the acceptance confirmation, then preview and publish again",
	"reconciliation":        "Check the pull request on GitHub, then reconcile the receipt with forgecell deliver; this page never retries a publication",
	"demo":                  "This demo Lab never contacts GitHub; run forgecell init in a Lab of your own repository to publish",
	"interrupted":           "Read the state back here; if the Molecule stays locked, inspect it with forgecell ledger and recover it with forgecell recover",
	"duplicate":             "Add a path or an acceptance note the plan does not list yet, or approve the plan as it is",
	"scope-refused":         "Choose an exact path inside a component the plan's evidence covers, or run forgecell amend to see the engine's own reason",
	"checkout":              "Refresh intake with forgecell run so the Molecule has a coding checkout, then review the new plan",
}

// engineReasons are exact next steps for engine errors the UI recognises,
// checked in order before the category's reason. Only the fixed reason is
// served, never the matched text.
var engineReasons = []struct {
	parts  []string
	reason string
}{
	{[]string{"authentication_error", "not logged in", "Please run /login", "Invalid API key", "invalid x-api-key", "OAuth token has expired"}, "Harness not authenticated: run its login command (claude auth login for Claude Code, codex login for Codex, agent login for Cursor), then start again"},
	{[]string{"issue repository does not match the approved Formula"}, "This Issue is not in the repository this Lab's Formula targets; give the number of an Issue in that repository"},
	{[]string{"Issue is closed"}, "Issue is closed or is not the requested Issue; reopen it on GitHub or choose an open Issue"},
	{[]string{"approve a Formula with init before running"}, "This Lab has no approved Formula; approve one with forgecell init, then start again"},
	{[]string{"Git origin does not match the issue repository", "repository origin changed"}, "The source checkout's Git origin is not this Lab's repository; restart forgecell ui from a checkout of that repository"},
	{[]string{"open Issue listing was truncated"}, "There are too many open Issues to apply the selector to; narrow it with label:NAME or milestone:TITLE"},
	{[]string{"approved Formula could not be reloaded"}, "The approved Formula could not be reloaded; check it with forgecell init, then restart the loop"},
	{[]string{"inspect an interrupted attempt before recovery"}, "An interrupted attempt left the Molecule locked; inspect it with forgecell ledger and, once no Lab process runs, recover it with forgecell recover"},
	{[]string{"delivery lock exists"}, "A delivery lock exists; make sure no forgecell deliver is running, then recover the lock with the forgecell CLI"},
	{[]string{"readiness inputs changed", "issue or Formula changed", "source base changed", "pending workspace changed"}, "The Issue, Formula, source base or workspace changed since the plan; refresh intake with forgecell run, then review the new plan"},
	{[]string{"carries Claude project configuration"}, "The base commit carries Claude project configuration; remove it from the repository, commit that and refresh intake"},
	{[]string{"required extension"}, "A required extension is unavailable; repair or re-pin it through a reviewed Formula change"},
	{[]string{"PR not found"}, "The recorded pull request was not found on GitHub; check the delivery branch, then reconcile the receipt with forgecell deliver"},
	{[]string{"multiple PRs found"}, "More than one pull request matches this delivery; close the extra ones on GitHub, then reconcile the receipt with forgecell deliver"},
	{[]string{"PR is CLOSED"}, "The pull request is closed on GitHub; reopen it, then reconcile the receipt with forgecell deliver"},
	{[]string{"PR is MERGED"}, "The pull request is already merged on GitHub; reconcile the receipt with forgecell deliver"},
	{[]string{"PR is not a confirmed draft"}, "The pull request is not a confirmed draft; check it on GitHub, then reconcile the receipt with forgecell deliver"},
}

// formulaRepoChanged captures the repository a loop's Formula now targets.
var formulaRepoChanged = regexp.MustCompile(`intake\.repo changed from \S+ to (\S+) during the run`)

// refusalReason is the bounded, redacted, actionable reason for a refusal of
// category whose engine error text is text ("" for a refusal the UI makes).
func refusalReason(category, text string) string {
	reason := categoryReasons[category]
	if m := formulaRepoChanged.FindStringSubmatch(text); m != nil && repository(m[1]) != "" {
		reason = "This Lab's Formula now targets " + m[1] + "; restart the loop"
	} else {
		for _, e := range engineReasons {
			if slices.ContainsFunc(e.parts, func(p string) bool { return strings.Contains(text, p) }) {
				reason = e.reason
				break
			}
		}
	}
	return redactor{}.reason(reason)
}

// GateJobView is GET /api/molecules/{id}/job and every accepted gate response.
// Committed comes only from the canonical read-back, never from State.
type GateJobView struct {
	Kind      string        `json:"kind"`
	Digest    string        `json:"digest"`
	State     string        `json:"state"`
	Category  string        `json:"category,omitempty"`
	Error     string        `json:"error,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	Committed bool          `json:"committed"`
	Canonical CanonicalView `json:"canonical"`
}

// CanonicalView is the ledger (scope) or receipt (publish) state as read back.
type CanonicalView struct {
	Source     string `json:"source"`
	Unreadable bool   `json:"unreadable,omitempty"`
	Phase      string `json:"phase,omitempty"`
	Approved   bool   `json:"approved,omitempty"`
	Receipt    string `json:"receipt,omitempty"`
	Lifecycle  string `json:"lifecycle,omitempty"`
	Published  bool   `json:"published,omitempty"`
	URL        string `json:"url,omitempty"`
}

// jobView reads the canonical state back. Scope is committed once the ledger
// records the approval of exactly the submitted digest; publication once a
// receipt is committed or published with a lifecycle observation.
func (h *handler) jobView(id string, j jobSnapshot) GateJobView {
	v := GateJobView{Kind: j.kind, Digest: j.digest, State: j.state, Category: j.category, Error: j.message, Reason: j.reason}
	record, err := h.records(h.opts.LabDir, id)
	if j.kind == "scope" {
		v.Canonical.Source = "ledger"
		if err != nil {
			v.Canonical.Unreadable = true
			return v
		}
		v.Canonical.Phase = phase(record)
		if record.Readiness != nil {
			for _, e := range record.Readiness.Events {
				if e.Kind == "approved" && e.Digest == j.digest {
					v.Canonical.Approved = true
				}
			}
		}
		v.Committed = v.Canonical.Approved
		return v
	}
	v.Canonical.Source = "receipt"
	s, found, rerr := h.receipts(h.opts.LabDir, id)
	if rerr != nil {
		v.Canonical.Unreadable = true
		return v
	}
	if !found {
		return v
	}
	v.Canonical.Receipt, v.Canonical.Lifecycle = s.State, s.Lifecycle
	v.Canonical.Published = s.State == "published"
	if err == nil && pullURL(s.URL, record.Issue.Repo) {
		v.Canonical.URL = s.URL
	}
	// A receipt left by an earlier attempt is not this decision's commit: a refused
	// or failed publish never shows as committed, whatever the receipt says.
	ownOutcome := j.state == "finished" || j.state == "reconciliation-required"
	v.Committed = ownOutcome && (s.State == "committed" || s.State == "published") && s.Lifecycle != ""
	return v
}

// NoJobView is GET /api/molecules/{id}/job for a Molecule with no gate job from
// this Lab UI: 200 with state none, so selecting a Molecule is not an error.
type NoJobView struct {
	State string `json:"state"`
}

// job serves the job of a Molecule in this Lab; an unknown id stays 404.
func (h *handler) job(w http.ResponseWriter, id string) {
	if j, ok := h.jobs.get(id); ok {
		h.json(w, http.StatusOK, h.jobView(id, j))
		return
	}
	switch _, err := h.records(h.opts.LabDir, id); {
	case err == nil:
		h.json(w, http.StatusOK, NoJobView{State: "none"})
	case errors.Is(err, os.ErrNotExist):
		h.refuseGate(w, http.StatusNotFound, "missing", "no such Molecule")
	default:
		h.refuseGate(w, http.StatusUnprocessableEntity, "refused", "the ledger was refused: it is invalid, unsafe, oversized or not this Lab's")
	}
}

var (
	errGateBusy    = errors.New("a gate job is running for this Molecule")
	errGateFull    = errors.New("the gate job registry is full")
	errGateStopped = errors.New("the Lab UI is stopping")
)

// gateJobs is the in-memory registry: at most one running job per Molecule and
// maxGateJobs jobs in all. Jobs run on a context detached from requests and
// cancelled when the server stops.
type gateJobs struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	jobs   map[string]*gateJob
	order  []string
}

type gateJob struct {
	kind, digest string
	// started is taken before the engine call starts; cancel cancels only this
	// job's context.
	started time.Time
	cancel  context.CancelFunc
	// done is closed when the engine call has returned and the state is set.
	done                             chan struct{}
	state, category, message, reason string
}

type jobSnapshot struct {
	kind, digest, state, category, message, reason string
	started                                        time.Time
}

func (j *gateJob) snapshot(mu *sync.Mutex) jobSnapshot {
	mu.Lock()
	defer mu.Unlock()
	return jobSnapshot{j.kind, j.digest, j.state, j.category, j.message, j.reason, j.started}
}

func newGateJobs() *gateJobs {
	ctx, cancel := context.WithCancel(context.Background())
	return &gateJobs{ctx: ctx, cancel: cancel, jobs: map[string]*gateJob{}}
}

func (g *gateJobs) start(id, kind, digest string, run func(context.Context) error) (*gateJob, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ctx.Err() != nil {
		return nil, errGateStopped
	}
	if j := g.jobs[id]; j != nil && j.state == "running" {
		return nil, errGateBusy
	}
	if g.jobs[id] == nil && len(g.jobs) >= maxGateJobs && !g.evict() {
		return nil, errGateFull
	}
	g.forget(id)
	ctx, cancel := context.WithCancel(g.ctx)
	j := &gateJob{kind: kind, digest: digest, started: time.Now(), cancel: cancel, done: make(chan struct{}), state: "running"}
	g.jobs[id] = j
	g.order = append(g.order, id)
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer close(j.done)
		defer cancel()
		state, category, message, reason := classify(guarded(ctx, run))
		g.mu.Lock()
		j.state, j.category, j.message, j.reason = state, category, message, reason
		g.mu.Unlock()
	}()
	return j, nil
}

var errGatePanic = errors.New("the engine call failed unexpectedly")

// guarded runs one engine call; a panic becomes a refusal instead of stopping
// the server.
func guarded(ctx context.Context, run func(context.Context) error) (err error) {
	defer func() {
		if recover() != nil {
			err = errGatePanic
		}
	}()
	return run(ctx)
}

// evict drops the oldest finished job; it reports false when every job runs.
func (g *gateJobs) evict() bool {
	for _, id := range g.order {
		if g.jobs[id].state != "running" {
			g.forget(id)
			return true
		}
	}
	return false
}

func (g *gateJobs) forget(id string) {
	delete(g.jobs, id)
	for i, v := range g.order {
		if v == id {
			g.order = append(g.order[:i], g.order[i+1:]...)
			break
		}
	}
}

func (g *gateJobs) get(id string) (jobSnapshot, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	j := g.jobs[id]
	if j == nil {
		return jobSnapshot{}, false
	}
	return jobSnapshot{j.kind, j.digest, j.state, j.category, j.message, j.reason, j.started}, true
}

// cancelRunning cancels the running job under id when its kind is one of kinds
// and owns(started) holds; it reports whether a job was cancelled.
func (g *gateJobs) cancelRunning(id string, kinds []string, owns func(time.Time) bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	j := g.jobs[id]
	if j == nil || j.state != "running" || !slices.Contains(kinds, j.kind) || !owns(j.started) {
		return false
	}
	j.cancel()
	return true
}

// ownsRunning is cancelRunning's check alone: it reports a running job under id
// whose kind is one of kinds and for which owns(started) holds, and cancels nothing.
func (g *gateJobs) ownsRunning(id string, kinds []string, owns func(time.Time) bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	j := g.jobs[id]
	return j != nil && j.state == "running" && slices.Contains(kinds, j.kind) && owns(j.started)
}

func (g *gateJobs) busy(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	j := g.jobs[id]
	return j != nil && j.state == "running"
}

// stop cancels every job and waits for them, at most grace.
func (g *gateJobs) stop(grace time.Duration) {
	g.mu.Lock()
	g.cancel()
	g.mu.Unlock()
	done := make(chan struct{})
	go func() { g.wg.Wait(); close(done) }()
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}
