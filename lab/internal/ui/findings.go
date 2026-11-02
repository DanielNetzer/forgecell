package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/fixround"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// Reviewer findings and fix rounds (plugins slice 5d-2). A published Molecule
// whose ledger-snapshot Formula declares reviewers serves its findings and its
// next fix round:
//
//	GET  /api/molecules/{id}/findings            → ReviewerFindingsView
//	POST /api/molecules/{id}/findings/collect    {}                      → fixround.Collect as job findings:<id>
//	POST /api/molecules/{id}/findings/propose    {"include"}             → fixround.Propose, at most 50 includes
//	POST /api/molecules/{id}/findings/dismiss    {"fingerprint","reason"} → delivery.DismissFinding
//	POST /api/molecules/{id}/findings/undismiss  {"fingerprint"}         → delivery.UndismissFinding
//	POST /api/molecules/{id}/fixround/push       {"approve","confirm"}   → fixround.Push as job fixround:<id>
//
// Every write needs the write protection of the gates and a Lab UI started
// from a source checkout. Findings are untrusted evidence: their text is
// served only redacted and bounded, and nothing here approves a plan. Only an
// explicit push POST with the current preview digest, and the confirmation
// that preview needs, pushes; no background pass or Inbox entry ever does.
// Engine, plugin and git error text is never served: each refusal is a fixed
// text.

const (
	// findingsMaxOpen bounds the open findings one view serves; more are
	// counted and marked truncated.
	findingsMaxOpen = 500
	// findingsMaxRuneText bounds a served finding title or error.
	findingsMaxRuneText = 300
	// findingsMaxPathBytes bounds a served finding path.
	findingsMaxPathBytes = 1024
	// findingsWriteTimeout bounds a synchronous findings write and the
	// fix-round preview a view or a push reads.
	findingsWriteTimeout = previewTimeout
)

// fixRoundObject is an exact git object name, as the engine records trees.
var fixRoundObject = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Pending fix-round states.
const (
	fixRoundWaitingApproval = "waiting-approval"
	fixRoundRunning         = "running"
	fixRoundBlocked         = "blocked"
	fixRoundVerified        = "verified"
	fixRoundReady           = "ready"
)

// Fixed texts of the findings API; none carries engine, plugin or git output.
const (
	fixRoundStaleText     = "the fix round changed; review it again"
	findingsReadOnlyText  = "this Lab UI was started without a source checkout, so it collects, triages and pushes nothing; nothing was written"
	findingsUnreadableTxt = "the findings record could not be read safely"
	fixRoundPreviewText   = "the fix round cannot be pushed as it stands; nothing was pushed"
)

// findingsReasons name the next step of each findings refusal category the
// gates' categoryReasons does not hold.
var findingsReasons = map[string]string{
	"read-only":       "Restart forgecell ui from a checkout of this Lab's repository to act on reviewer findings",
	"no-reviewers":    "This Molecule's Formula declares no reviewers, so it has no findings to collect",
	"not-published":   "Findings and fix rounds follow a published draft; publish the delivery first",
	"not-collected":   "Collect findings for the change request's current head first, then propose again",
	"unknown-finding": "Reload the findings and choose a finding they list now",
	"dismissed":       "The finding is already dismissed; reload the findings",
	"not-dismissed":   "The finding is not dismissed; reload the findings",
	"full":            "Undismiss a finding first; the findings record holds at most 500 dismissals",
	"no-fix-round":    "Propose a fix round from the open findings first, then approve and verify it",
	"not-verified":    "Wait for the fix round's verification to pass, then review its push",
	"confirm":         "Review the fix round, tick the confirmation, then push again",
	"fix-rounds-stop": "This change request has reached its fix-round bound; continue it by hand",
	"too-many":        "Raise the reviewers' autoFix level in the Formula or include fewer findings",
	"no-formula":      "This Lab has no approved active Formula; approve one with forgecell init first",
}

// findingsRefuse answers a findings refusal with its fixed message and the
// category's next step.
func (h *handler) findingsRefuse(w http.ResponseWriter, code int, category, message string) {
	if reason, ok := findingsReasons[category]; ok {
		h.refuseWith(w, code, category, message, redactor{}.reason(reason))
		return
	}
	h.refuseGate(w, code, category, message)
}

// findingsSeams sets the engine calls behind the findings routes; tests
// replace them, so no plugin, git or forge process starts.
func findingsSeams(h *handler) {
	h.findingsCollect = func(ctx context.Context, lab, id string, f formula.Formula) (delivery.FindingsRecord, error) {
		resolved, err := filepath.EvalSymlinks(lab)
		if err != nil {
			return delivery.FindingsRecord{}, fmt.Errorf("the Lab directory cannot be resolved")
		}
		return fixround.Collect(ctx, resolved, id, f)
	}
	h.fixRoundPropose = func(ctx context.Context, lab, id string, includes []string) (fixround.Proposal, error) {
		resolved, err := filepath.EvalSymlinks(lab)
		if err != nil {
			return fixround.Proposal{}, fmt.Errorf("the Lab directory cannot be resolved")
		}
		return fixround.Propose(ctx, resolved, id, includes)
	}
	h.fixRoundPreview = func(ctx context.Context, lab, id string) (delivery.FixRoundPreview, error) {
		return fixround.Preview(ctx, lab, id, "")
	}
	h.fixRoundPush = func(ctx context.Context, lab, id, approve string, confirm bool) error {
		resolved, err := filepath.EvalSymlinks(lab)
		if err != nil {
			return fmt.Errorf("the Lab directory cannot be resolved")
		}
		_, err = fixround.Push(ctx, resolved, id, approve, "", confirm)
		return err
	}
	h.findingsDismiss = func(lab, id, fingerprint, reason string) error {
		return delivery.DismissFinding(lab, id, fingerprint, reason, h.findingsNow)
	}
	h.findingsUndismiss = delivery.UndismissFinding
	h.findingsNow = time.Now
	h.findingsPoll = findingsSyncPoll
	h.findingsSync = &findingsSyncState{entries: map[string]*findingsSyncEntry{}}
}

// findingsJobKey and fixRoundJobKey are the job registry keys of a collection
// and a push, so neither shares the gate job slot of the Molecule.
func findingsJobKey(id string) string { return "findings:" + id }
func fixRoundJobKey(id string) string { return "fixround:" + id }

// findingsBusy reports a gate, collection or push job, or a background
// collection, running for the Molecule.
func (h *handler) findingsBusy(id string) bool {
	if h.jobs.busy(id) || h.jobs.busy(findingsJobKey(id)) || h.jobs.busy(fixRoundJobKey(id)) {
		return true
	}
	return h.findingsSync.collectingID() == id
}

// findingsForget drops the Molecule's cached Summary after a UI findings
// write. The findings stamp in the cache key (cache.go) already refreshes it,
// including after CLI writes; this only frees the stale entry at once.
func (h *handler) findingsForget(id string) {
	c := h.cache
	c.mu.Lock()
	delete(c.entries, id)
	c.mu.Unlock()
}

// findingsSnapshot is the Molecule's ledger-snapshot Formula when it matches
// its recorded SHA-256, parses and declares reviewers.
func findingsSnapshot(r molecule.Record) (formula.Formula, bool) {
	s := r.FormulaSnapshot
	if s.YAML == "" {
		return formula.Formula{}, false
	}
	if sum := sha256.Sum256([]byte(s.YAML)); hex.EncodeToString(sum[:]) != s.SHA256 {
		return formula.Formula{}, false
	}
	f, err := formula.Parse([]byte(s.YAML))
	if err != nil || len(f.Reviewers()) == 0 {
		return formula.Formula{}, false
	}
	return f, true
}

// findingsAutoFix maps each reviewer identity to its autoFix level.
func findingsAutoFix(f formula.Formula) map[string]string {
	out := map[string]string{}
	for i, r := range f.Reviewers() {
		out[delivery.ReviewerKey(i, r.Plugin)] = r.AutoFix
	}
	return out
}

// findingsText is untrusted text made one bounded line: secrets and local
// paths replaced, controls stripped.
func findingsText(x redactor, s string, limit int) string {
	return harness.ReviewDisplayText(x.text(redactSecrets(s)), limit)
}

// findingsPath is a finding's recorded path when it serves unchanged, or "".
func findingsPath(x redactor, p string) string {
	if p == "" || len(p) > findingsMaxPathBytes || findingsText(x, p, findingsMaxPathBytes) != p {
		return ""
	}
	return p
}

// findingsGetRoute reports GET /api/molecules/{id}/findings and its id.
func findingsGetRoute(path string) (string, bool) {
	rest, found := strings.CutPrefix(path, "/api/molecules/")
	if !found {
		return "", false
	}
	id, view, _ := strings.Cut(rest, "/")
	return id, view == "findings"
}

// findingsGetPath reports GET /api/molecules/{id}/findings.
func findingsGetPath(path string) bool {
	_, ok := findingsGetRoute(path)
	return ok
}

// findingsRoute reports a findings POST path and its Molecule id and action.
func findingsRoute(path string) (id, action string, ok bool) {
	rest, found := strings.CutPrefix(path, "/api/molecules/")
	if !found {
		return "", "", false
	}
	id, action, _ = strings.Cut(rest, "/")
	switch action {
	case "findings/collect", "findings/propose", "findings/dismiss", "findings/undismiss", "fixround/push":
		return id, action, true
	}
	return "", "", false
}

// findingsGet serves GET /api/molecules/{id}/findings.
func (h *handler) findingsGet(w http.ResponseWriter, r *http.Request, id string) {
	if !moleculeID.MatchString(id) {
		h.refuse(w, http.StatusBadRequest, "invalid Molecule id")
		return
	}
	if h.opts.Demo {
		h.refuse(w, http.StatusNotFound, "sample data has no findings")
		return
	}
	record, ok := h.findingsRecord(w, id)
	if !ok {
		return
	}
	h.json(w, http.StatusOK, h.findingsView(r.Context(), record))
}

// findingsRecord reads the Molecule's ledger, answering a refusal by category.
func (h *handler) findingsRecord(w http.ResponseWriter, id string) (molecule.Record, bool) {
	record, err := h.records(h.opts.LabDir, id)
	switch {
	case err == nil:
		return record, true
	case errors.Is(err, os.ErrNotExist):
		h.refuseGate(w, http.StatusNotFound, "missing", "no such Molecule")
	default:
		h.refuseGate(w, http.StatusUnprocessableEntity, "refused", "the ledger was refused: it is invalid, unsafe, oversized or not this Lab's")
	}
	return molecule.Record{}, false
}

type findingsCollectRequest struct{}

type findingsProposeRequest struct {
	Include []string `json:"include"`
}

type findingsDismissRequest struct {
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason"`
}

type findingsUndismissRequest struct {
	Fingerprint string `json:"fingerprint"`
}

type fixRoundPushRequest struct {
	Approve string `json:"approve"`
	Confirm bool   `json:"confirm"`
}

// findingsPost serves one findings POST after the write protection passed.
func (h *handler) findingsPost(w http.ResponseWriter, r *http.Request, id, action string) {
	if !moleculeID.MatchString(id) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "invalid Molecule id")
		return
	}
	if h.opts.Demo {
		h.refuseGate(w, http.StatusNotFound, "sample", "sample data has no findings; nothing was written")
		return
	}
	if h.opts.SourceRoot == "" {
		h.findingsRefuse(w, http.StatusConflict, "read-only", findingsReadOnlyText)
		return
	}
	var collect findingsCollectRequest
	var propose findingsProposeRequest
	var dismiss findingsDismissRequest
	var undismiss findingsUndismissRequest
	var push fixRoundPushRequest
	body := map[string]any{"findings/collect": &collect, "findings/propose": &propose, "findings/dismiss": &dismiss, "findings/undismiss": &undismiss, "fixround/push": &push}[action]
	if decodeGate(w, r, body) != nil {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "send one small JSON object with exactly the request's fields")
		return
	}
	// Every value is checked before the ledger is read.
	switch action {
	case "findings/propose":
		if len(propose.Include) > fixround.MaxIncludes {
			h.refuseGate(w, http.StatusBadRequest, "invalid", fmt.Sprintf("include at most %d findings", fixround.MaxIncludes))
			return
		}
		for _, fp := range propose.Include {
			if !fixround.Fingerprint.MatchString(fp) {
				h.refuseGate(w, http.StatusBadRequest, "invalid", "an included finding is a fingerprint: 64 lower-case hex characters")
				return
			}
		}
	case "findings/dismiss", "findings/undismiss":
		fp := dismiss.Fingerprint + undismiss.Fingerprint
		if !fixround.Fingerprint.MatchString(fp) {
			h.refuseGate(w, http.StatusBadRequest, "invalid", "a finding fingerprint is 64 lower-case hex characters")
			return
		}
		if action == "findings/dismiss" {
			if _, err := delivery.DismissalReason(dismiss.Reason); err != nil {
				h.refuseGate(w, http.StatusBadRequest, "invalid", fmt.Sprintf("a dismissal needs a reason: one line of plain text of at most %d characters", delivery.MaxDismissalReasonRunes))
				return
			}
		}
	case "fixround/push":
		if !sha256Hex.MatchString(push.Approve) {
			h.refuseGate(w, http.StatusBadRequest, "invalid", "give the exact 64-character fix round preview digest shown")
			return
		}
	}
	record, ok := h.findingsRecord(w, id)
	if !ok {
		return
	}
	f, ok := findingsSnapshot(record)
	if !ok {
		h.findingsRefuse(w, http.StatusConflict, "no-reviewers", "this Molecule's Formula declares no reviewers; nothing was written")
		return
	}
	receipt, err := delivery.ReadFixRoundReceipt(h.opts.LabDir, id)
	if err != nil {
		h.findingsRefuse(w, http.StatusConflict, "not-published", "this Molecule has no published delivery; nothing was written")
		return
	}
	switch action {
	case "findings/collect":
		h.findingsCollectPost(w, id, f)
	case "findings/propose":
		h.findingsProposePost(w, id, propose.Include)
	case "findings/dismiss":
		h.findingsDismissPost(w, id, dismiss.Fingerprint, func() error {
			return h.findingsDismiss(h.opts.LabDir, id, dismiss.Fingerprint, dismiss.Reason)
		})
	case "findings/undismiss":
		h.findingsDismissPost(w, id, undismiss.Fingerprint, func() error {
			return h.findingsUndismiss(h.opts.LabDir, id, undismiss.Fingerprint)
		})
	default:
		h.fixRoundPushPost(w, record, receipt, push)
	}
}

func (h *handler) findingsCollectPost(w http.ResponseWriter, id string, f formula.Formula) {
	if h.findingsBusy(id) {
		h.refuseGate(w, http.StatusConflict, "busy", "a collection, push or gate job is already running for this Molecule; nothing was started")
		return
	}
	collect, lab := h.findingsCollect, h.opts.LabDir
	job, err := h.jobs.start(findingsJobKey(id), "findings", "", func(ctx context.Context) error {
		defer h.findingsForget(id)
		_, err := collect(ctx, lab, id, f)
		return err
	})
	if h.refuseJobStart(w, err, "a collection is already running for this Molecule; nothing was started") {
		return
	}
	h.json(w, http.StatusAccepted, findingsJobOf(job.snapshot(&h.jobs.mu)))
}

// FindingsProposalView is POST …/findings/propose: the fix round proposed, or
// None when no finding was picked and nothing was amended.
type FindingsProposalView struct {
	Round   int                `json:"round"`
	None    bool               `json:"none"`
	Picked  []FindingsOpenView `json:"picked"`
	Skipped []FindingsSkipView `json:"skipped"`
}

// FindingsSkipView is an open finding the proposal left out, and why.
type FindingsSkipView struct {
	Fingerprint string `json:"fingerprint"`
	Reason      string `json:"reason"`
}

func (h *handler) findingsProposePost(w http.ResponseWriter, id string, includes []string) {
	if h.findingsBusy(id) || !h.amendMu.TryLock() {
		h.refuseGate(w, http.StatusConflict, "busy", "a collection, push, gate job or amendment is running for this Molecule; nothing was proposed")
		return
	}
	defer h.amendMu.Unlock()
	ctx, cancel := context.WithTimeout(h.jobs.ctx, findingsWriteTimeout)
	defer cancel()
	p, err := h.fixRoundPropose(ctx, h.opts.LabDir, id, includes)
	h.findingsForget(id)
	if err != nil {
		h.findingsEngineRefusal(w, err)
		return
	}
	record, rerr := h.records(h.opts.LabDir, id)
	x := newRedactor(h.opts.LabDir, record)
	if rerr != nil {
		x = newRedactor(h.opts.LabDir, molecule.Record{})
	}
	out := FindingsProposalView{Round: p.Round, None: p.None, Picked: []FindingsOpenView{}, Skipped: []FindingsSkipView{}}
	for _, sf := range p.Picked {
		path := findingsPath(x, sf.Path)
		out.Picked = append(out.Picked, FindingsOpenView{Reviewer: findingsText(x, sf.Reviewer, findingsMaxRuneText), Plugin: findingsText(x, sf.Plugin, findingsMaxRuneText), Severity: sf.Severity, Path: path, Line: findingsLine(path, sf.Line), Title: findingsText(x, sf.Title, findingsMaxRuneText), Fingerprint: sf.Fingerprint, AutoFix: sf.AutoFix})
	}
	for _, s := range p.Skipped {
		reason := "skipped"
		switch s.Reason {
		case fixround.SkipRepeated:
			reason = "came back after a fix"
		case fixround.SkipDismissed:
			reason = "dismissed"
		}
		out.Skipped = append(out.Skipped, FindingsSkipView{Fingerprint: s.Fingerprint, Reason: reason})
	}
	h.json(w, http.StatusOK, out)
}

// FindingsDismissView answers a dismissal or its removal.
type FindingsDismissView struct {
	Fingerprint string `json:"fingerprint"`
	Dismissed   bool   `json:"dismissed"`
}

func (h *handler) findingsDismissPost(w http.ResponseWriter, id, fingerprint string, write func() error) {
	if h.findingsBusy(id) {
		h.refuseGate(w, http.StatusConflict, "busy", "a collection, push or gate job is running for this Molecule; nothing was written")
		return
	}
	err := write()
	h.findingsForget(id)
	if err != nil {
		h.findingsEngineRefusal(w, err)
		return
	}
	h.json(w, http.StatusOK, FindingsDismissView{Fingerprint: fingerprint, Dismissed: h.findingsIsDismissed(id, fingerprint)})
}

// findingsIsDismissed reads back whether the record dismisses fingerprint.
func (h *handler) findingsIsDismissed(id, fingerprint string) bool {
	record, err := delivery.ReadFindings(h.opts.LabDir, id)
	return err == nil && record.DismissedSet()[fingerprint]
}

// findingsEngineRefusal maps an engine refusal of a synchronous findings write
// to a fixed text; the error text only selects it.
func (h *handler) findingsEngineRefusal(w http.ResponseWriter, err error) {
	if errors.Is(err, delivery.ErrDemoLab) {
		h.refuseGate(w, http.StatusUnprocessableEntity, "demo", "this is a forgecell demo Lab; it never contacts a forge")
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		h.refuseGate(w, http.StatusServiceUnavailable, "interrupted", "the request was interrupted before the engine finished")
		return
	}
	text := err.Error()
	for _, m := range []struct {
		part, category, message string
	}{
		{"holds the lock", "locked", "another delivery operation holds the delivery lock; nothing was written"},
		{"is locked", "locked", "the Molecule is locked by another command or a loop; nothing was written"},
		{"collect findings for the current head first", "not-collected", "no complete findings round is recorded on the change request's current head; nothing was proposed"},
		{"is not an open finding", "unknown-finding", "an included finding is not an open finding of the latest complete round on the head; nothing was proposed"},
		{"is not in any recorded findings round", "unknown-finding", "the finding is not in any recorded findings round; nothing was written"},
		{"is already dismissed", "dismissed", "the finding is already dismissed; nothing was written"},
		{"is not dismissed", "not-dismissed", "the finding is not dismissed; nothing was written"},
		{"dismissals; undismiss one first", "full", "the findings record holds its most dismissals; nothing was written"},
		{"fix rounds stop", "fix-rounds-stop", "the change request has reached its fix-round bound; nothing was proposed"},
		{"more than one fix round answers", "too-many", "more findings were picked than one fix round answers; nothing was proposed"},
		{"no approved active Formula", "no-formula", "the Lab has no approved active Formula; nothing was proposed"},
		{"not published", "not-published", "this Molecule has no published delivery; nothing was written"},
		{"no delivery receipt", "not-published", "this Molecule has no published delivery; nothing was written"},
	} {
		if strings.Contains(text, m.part) {
			h.findingsRefuse(w, http.StatusConflict, m.category, m.message)
			return
		}
	}
	h.refuseGate(w, http.StatusUnprocessableEntity, "refused", "the engine refused the request; nothing else was attempted")
}

// fixRoundPushPost starts the push of exactly the reviewed preview: approve
// must be the current preview digest, and confirm must be set when the
// preview needs it.
func (h *handler) fixRoundPushPost(w http.ResponseWriter, r molecule.Record, receipt delivery.Receipt, req fixRoundPushRequest) {
	pending, ok := fixRoundPendingOf(r, receipt)
	if !ok {
		h.findingsRefuse(w, http.StatusConflict, "no-fix-round", "the latest plan is not the change request's next fix round; nothing was pushed")
		return
	}
	if !fixRoundLedgerVerified(r, pending.Digest) {
		h.findingsRefuse(w, http.StatusConflict, "not-verified", "the fix round has no passing verification of its approved plan; nothing was pushed")
		return
	}
	if h.findingsBusy(r.ID) {
		h.refuseGate(w, http.StatusConflict, "busy", "a collection, push or gate job is running for this Molecule; nothing was pushed")
		return
	}
	ctx, cancel := context.WithTimeout(h.jobs.ctx, findingsWriteTimeout)
	preview, err := h.fixRoundPreview(ctx, h.opts.LabDir, r.ID)
	cancel()
	if err != nil {
		h.refuseGate(w, http.StatusConflict, "refused", fixRoundPreviewText)
		return
	}
	if preview.Digest != req.Approve {
		h.refuseGate(w, http.StatusConflict, "stale", fixRoundStaleText)
		return
	}
	if preview.NeedsConfirm() && !req.Confirm {
		h.findingsRefuse(w, http.StatusConflict, "confirm", "this fix round waits for your explicit confirmation: its independent acceptance is unverified or its review is not green; nothing was pushed")
		return
	}
	push, lab, id, approve, confirm := h.fixRoundPush, h.opts.LabDir, r.ID, req.Approve, req.Confirm
	job, err := h.jobs.start(fixRoundJobKey(id), "fixround", approve, func(ctx context.Context) error {
		defer h.findingsForget(id)
		return push(ctx, lab, id, approve, confirm)
	})
	if h.refuseJobStart(w, err, "a push is already running for this Molecule; nothing was started") {
		return
	}
	h.json(w, http.StatusAccepted, findingsJobOf(job.snapshot(&h.jobs.mu)))
}
