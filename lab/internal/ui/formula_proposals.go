package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
)

// The Formula view changes the Formula only through the engine's own proposal
// functions, the ones forgecell init uses, behind the drive write protection
// (cookie, tab header, exact Origin, Fetch Metadata and one strict JSON object of
// at most 4 KiB):
//
//	GET  /api/formula                                  → the active Formula, any pending proposal and what can be proposed
//	POST /api/formula/proposals        {"kind","digest","model","role","anyway"}
//	                                                   → onboarding.Prepare (init --rebind, --refresh-baseline, --enable-review, --model)
//	POST /api/formula/proposals/{id}/approve {"digest"} → onboarding.Review(approve) (init --approve)
//	POST /api/formula/proposals/{id}/dismiss {"digest"} → onboarding.Review(dismiss) (init --dismiss)
//
// digest binds what was shown: on create, the active Formula's SHA-256; on a
// decision, the pending proposal's YAML SHA-256. A mismatch is refused before the
// engine is called. One Formula write from this UI runs at a time. Engine error
// text, which can name local paths, is never served: each refusal is a fixed
// category, message and reason. Main-view fields carry no YAML, paths, assay ids
// or full digests; those are only under details.

const (
	// formulaTimeout bounds one proposal: repository discovery and the binding
	// probe run processes, inside the server's write timeout.
	formulaTimeout = 45 * time.Second
	// maxModelBytes bounds a model name before the engine validates it.
	maxModelBytes = 200
	// maxLabelBytes bounds a display value in the main view.
	maxLabelBytes = 120
)

// FormulaAction is the action name for the Formula proposal routes.
const FormulaAction = "Formula proposals"

// Proposal kinds the Formula view creates, as the CLI flags name them.
const (
	ProposeRebind   = "rebind"
	ProposeBaseline = "baseline"
	ProposeReview   = "enable-review"
	ProposeModel    = "model"
)

var assayID = regexp.MustCompile(`^assay-[a-f0-9]{64}$`)
var safeWord = regexp.MustCompile(`^[A-Za-z0-9._-]{1,80}$`)
var releaseVersion = regexp.MustCompile(`/releases/([0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?)/forgecell$`)

// formulaRoute reports a POST Formula path, and for a decision its proposal id.
func formulaRoute(path string) (id, action string, ok bool) {
	if path == "/api/formula/proposals" {
		return "", "create", true
	}
	rest, found := strings.CutPrefix(path, "/api/formula/proposals/")
	if !found {
		return "", "", false
	}
	for _, decision := range []string{"approve", "dismiss"} {
		if id, found = strings.CutSuffix(rest, "/"+decision); found && !strings.Contains(id, "/") {
			return id, decision, true
		}
	}
	return "", "", false
}

type proposalRequest struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Model  string `json:"model"`
	Role   string `json:"role"`
	Anyway bool   `json:"anyway"`
}

type decisionRequest struct {
	Digest string `json:"digest"`
}

// FormulaPage is GET /api/formula and every accepted Formula write.
type FormulaPage struct {
	Sample   bool               `json:"sample"`
	Writable bool               `json:"writable"`
	Reason   string             `json:"reason,omitempty"`
	Active   ActiveFormulaPage  `json:"active"`
	Pending  *ProposalView      `json:"pending,omitempty"`
	Create   []CreateOptionView `json:"create"`
	InFlight InFlightView       `json:"inFlight"`
	// Outcome names what the write just did: proposed, approved or dismissed.
	Outcome string `json:"outcome,omitempty"`
}

// ActiveFormulaPage is the active Formula in plain words.
type ActiveFormulaPage struct {
	State      string      `json:"state"`
	Name       string      `json:"name,omitempty"`
	ShortSHA   string      `json:"shortSha,omitempty"`
	Atoms      []string    `json:"atoms"`
	Coding     AgentView   `json:"coding"`
	Reviewer   *AgentView  `json:"reviewer,omitempty"`
	Checks     []CheckView `json:"checks"`
	ApprovedAt string      `json:"approvedAt,omitempty"`
	// ApprovedBy is never invented: the engine records no approver.
	ApprovedBy string             `json:"approvedBy"`
	Details    ActiveFormulaExtra `json:"details"`
}

// ActiveFormulaExtra is shown only behind details.
type ActiveFormulaExtra struct {
	FormulaID string `json:"formulaId,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// AgentView is a binding in plain words.
type AgentView struct {
	Agent   string `json:"agent"`
	Model   string `json:"model,omitempty"`
	Source  string `json:"source,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// CheckView is a required check by its command, with its id behind details.
type CheckView struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

// CreateOptionView is one proposal kind the view offers. Available is false with
// Reason when it cannot be proposed now; NeedsAnyway asks for an explicit Rebind
// Anyway, with Reason, before an older or unordered build is proposed.
type CreateOptionView struct {
	Kind        string `json:"kind"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason,omitempty"`
	NeedsAnyway bool   `json:"needsAnyway,omitempty"`
	Build       string `json:"build,omitempty"`
}

// InFlightView counts Molecules the ledgers show running or allocating, and those
// waiting for plan approval. Incomplete reports a ledger inventory that could not
// be read whole, so the counts may be low.
type InFlightView struct {
	Running    int  `json:"running"`
	Waiting    int  `json:"waiting"`
	Incomplete bool `json:"incomplete,omitempty"`
}

// ProposalView is the pending proposal for review.
type ProposalView struct {
	Kind        string             `json:"kind"`
	Title       string             `json:"title"`
	CreatedAt   string             `json:"createdAt,omitempty"`
	Changes     []ChangeView       `json:"changes"`
	Truncated   bool               `json:"truncated,omitempty"`
	Notes       []string           `json:"notes"`
	Warning     string             `json:"warning,omitempty"`
	Interrupted bool               `json:"interrupted,omitempty"`
	Rebase      bool               `json:"rebase,omitempty"`
	Details     ProposalDetailView `json:"details"`
}

// ChangeView is one changed key in plain words: old → new.
type ChangeView struct {
	Label  string `json:"label"`
	Change string `json:"change"`
	Old    string `json:"old"`
	New    string `json:"new"`
}

// ProposalDetailView is shown only behind details: the exact id and digest the
// decision binds, the raw changed keys and the exact YAML line diff.
type ProposalDetailView struct {
	ID        string                 `json:"id"`
	Digest    string                 `json:"digest"`
	Keys      []onboarding.KeyChange `json:"keys"`
	Lines     []onboarding.DiffLine  `json:"lines"`
	Truncated bool                   `json:"truncated,omitempty"`
}

// formulaRefusal is one fixed refusal for engine error text it recognises.
type formulaRefusal struct {
	parts                     []string
	code                      int
	category, message, reason string
}

var proposalRefusals = []formulaRefusal{
	{[]string{"a proposal is already pending"}, http.StatusConflict, "pending", "a proposal is already pending; nothing was saved", "Approve or dismiss the pending proposal first"},
	{[]string{"another Formula write is active"}, http.StatusConflict, "locked", "another Formula write is active in this Lab; nothing was saved", "Wait for it to finish; if nothing is running, inspect the Lab's Formula write lock with forgecell init"},
	{[]string{"already has a review Atom", "already has an Atom with id review"}, http.StatusConflict, "has-review", "the Formula already has a review Atom; nothing was saved", "In-loop review is already part of this Formula"},
	{[]string{"no usable binding", "configure harnesses.review, or a meta binding"}, http.StatusConflict, "no-reviewer", "no reviewer binding: the review Atom would have no agent to run; nothing was saved", "Declare harnesses.review or a meta binding through a reviewed Formula change, then enable review"},
	{[]string{"ticket readiness"}, http.StatusConflict, "not-ready", "the Formula is not in the shape review needs; nothing was saved", "Run forgecell init --enable-review to see the engine's own reason"},
	{[]string{"nothing to rebind"}, http.StatusConflict, "nothing-to-rebind", "the coding agent is already bound to this build; nothing to rebind", "Nothing is needed"},
	{[]string{"cannot rebind to", "launcher and executable are required"}, http.StatusConflict, "rebind-unavailable", "the coding agent is not ready on this machine, or this build's launcher is unknown; nothing was saved", "Run forgecell doctor to see what is missing, then try again"},
	{[]string{"already pinned to", "nothing to remove"}, http.StatusConflict, "unchanged", "every targeted agent already uses this model; nothing was saved", "Choose another model, or leave the pin as it is"},
	{[]string{"cannot pin a model"}, http.StatusConflict, "model-unsupported", "a targeted agent cannot carry a pinned model; nothing was saved", "Pin a model on a Claude Code agent with a role, or run forgecell init --model to see which agents refuse it"},
	{[]string{"is not declared, so there is no"}, http.StatusConflict, "role-undeclared", "this Formula declares no agent for that role; nothing was saved", "Leave the role empty to pin the default agent, or declare the role through a reviewed Formula change"},
	{[]string{"yield no recognized baseline check"}, http.StatusConflict, "baseline-empty", "your pull-request workflows yield no check Forgecell recognises, so the baseline would drop every protected check; nothing was saved", "Keep the current checks, or add a recognised check to a pull-request workflow"},
	{[]string{"cannot be edited in place", "cannot be located in place"}, http.StatusConflict, "not-editable", "the Formula's text cannot be changed in place for this kind; nothing was saved", "Run the same change with forgecell init to see the engine's own reason"},
	{[]string{"belongs to another repository"}, http.StatusConflict, "other-repo", "this checkout is not the repository this Lab's Formula targets; nothing was saved", "Restart forgecell ui from a checkout of that repository"},
	{[]string{"current exact approval", "needs an approved Formula"}, http.StatusConflict, "unapproved", "the active Formula has no current exact approval; nothing was saved", "Approve the active Formula with forgecell init first"},
	{[]string{"repository discovery failed", "origin must identify", "GitHub repository identity mismatch"}, http.StatusConflict, "checkout", "the source checkout or its GitHub repository could not be read; nothing was saved", "Check that gh is signed in and the checkout's origin is the Lab's repository, then try again"},
}

var decisionRefusals = []formulaRefusal{
	{[]string{"another Formula write is active"}, http.StatusConflict, "locked", "another Formula write is active in this Lab; nothing was decided", "Wait for it to finish; if nothing is running, inspect the Lab's Formula write lock with forgecell init"},
	{[]string{"stale approval refused", "original active Formula changed", "proposal changed since review"}, http.StatusConflict, "stale", "the Formula or the proposal changed since it was shown; nothing was approved", "Dismiss this proposal and create a new one from the current Formula"},
	{[]string{"approval started; resume approve"}, http.StatusConflict, "interrupted", "an approval of this proposal was interrupted; it cannot be dismissed", "Approve it to resume the exact saved approval"},
	{[]string{"proposal already reviewed"}, http.StatusConflict, "reviewed", "this proposal was already decided; nothing was written", "Reload the Formula view"},
}

// refuseFormula serves the fixed refusal matching err, or a generic one.
func (h *handler) refuseFormula(w http.ResponseWriter, err error, refusals []formulaRefusal) {
	if errors.Is(err, context.DeadlineExceeded) {
		h.refuseWith(w, http.StatusGatewayTimeout, "timeout", "the proposal took too long and was stopped; nothing was saved", "Try again; if it keeps timing out, run the same change with forgecell init")
		return
	}
	text := err.Error()
	for _, r := range refusals {
		if slices.ContainsFunc(r.parts, func(p string) bool { return strings.Contains(text, p) }) {
			h.refuseWith(w, r.code, r.category, r.message, r.reason)
			return
		}
	}
	h.refuseWith(w, http.StatusUnprocessableEntity, "refused", "the engine refused the change; nothing else was attempted", categoryReasons["refused"])
}

// formulaPost serves one POST Formula route after the write protection passed.
func (h *handler) formulaPost(w http.ResponseWriter, r *http.Request, id, action string) {
	if h.opts.Demo {
		h.refuseWith(w, http.StatusNotFound, "sample", "sample data has no Formula to change; nothing was written", categoryReasons["sample"])
		return
	}
	if action != "create" && !assayID.MatchString(id) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "invalid proposal id")
		return
	}
	var create proposalRequest
	var decide decisionRequest
	var body any = &decide
	if action == "create" {
		body = &create
	}
	if decodeGate(w, r, body) != nil {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "send one small JSON object with exactly the action's fields")
		return
	}
	if !h.formulaMu.TryLock() {
		h.refuseWith(w, http.StatusConflict, "busy", "another Formula change from this Lab UI is in progress; nothing was written", categoryReasons["busy"])
		return
	}
	defer h.formulaMu.Unlock()
	if action == "create" {
		h.createProposal(w, r.Context(), create)
		return
	}
	h.decideProposal(w, id, action, decide)
}

// activeFormula is the approved active Formula, or false when there is none.
func (h *handler) activeFormula() (formula.Loaded, bool) {
	active, err := formula.Load(h.opts.LabDir, "")
	return active, err == nil && active.Approved && active.Formula.ID != ""
}

func nativeLauncher(command []string) string {
	if len(command) == 4 && command[1] == "__adapter" {
		return command[0]
	}
	return ""
}

// launcherUnknownReason is the plain next step when this build's launcher was
// not accepted by ValidateLauncher.
const launcherUnknownReason = "Start forgecell ui from a Forgecell build you installed under your Forgecell home to rebind to it"

// needsAnyway reports a launcher order a rebind proposes only with an explicit
// Rebind Anyway: an older build, two dev builds whose commit times do not order
// them, or a build unrelated to the Formula's launcher.
func needsAnyway(order string) bool {
	return order == formula.LauncherOlder || order == formula.LauncherUnordered || order == formula.LauncherUnrelated
}

// managedTrees are the trees under the managed Forgecell home a rebind may name.
var managedTrees = []string{"releases", "development"}

// ValidateLauncher returns candidate, the launcher forgecell init --rebind would
// save, only when it resolves through symlinks (and a managed launcher through
// its active release) to a regular file this user owns under home's releases or
// development tree. Otherwise it returns "", and the Formula view says the
// launcher is unknown and offers no rebind.
func ValidateLauncher(home, candidate string) string {
	if home == "" || candidate == "" || !filepath.IsAbs(home) || !filepath.IsAbs(candidate) || len(candidate) > 4096 {
		return ""
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return ""
	}
	exe := harness.ResolveLauncher(candidate).Executable
	if exe == "" {
		return ""
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil || underManagedTree(realHome, real) == "" {
		return ""
	}
	st, err := os.Lstat(real)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return ""
	}
	return candidate
}

// underManagedTree names the managed tree under home that holds p, or "".
func underManagedTree(home, p string) string {
	for _, tree := range managedTrees {
		rel, err := filepath.Rel(filepath.Join(home, tree), p)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel) {
			return tree
		}
	}
	return ""
}

// buildWord names a Forgecell launcher in plain words: a managed release, a dev
// build, or a build outside the managed tree, by version only, never by path.
func buildWord(home, launcher string) string {
	version := ""
	if m := releaseVersion.FindStringSubmatch(launcher); m != nil {
		version = m[1]
	}
	tree := ""
	if home != "" && filepath.IsAbs(launcher) && filepath.Clean(launcher) == launcher {
		roots := []string{home}
		if real, err := filepath.EvalSymlinks(home); err == nil && real != home {
			roots = append(roots, real)
		}
		for _, root := range roots {
			if tree = underManagedTree(root, launcher); tree != "" {
				break
			}
		}
	}
	dev := strings.Contains(version, "-dev.")
	switch {
	case tree == "releases" && version != "" && dev:
		return "Forgecell dev build " + version
	case tree == "releases" && version != "":
		return "Forgecell release " + version
	case tree == "development":
		return "a Forgecell dev build"
	case tree != "":
		return "a managed Forgecell build"
	case version != "":
		return "Forgecell " + version + ", outside the managed tree"
	}
	return "a Forgecell build outside the managed tree"
}

// rebindOrder orders this build against the Formula's launcher; current is ""
// when the default harness is not a native command.
func (h *handler) rebindOrder(f formula.Formula) (current, order, reason string) {
	current = nativeLauncher(f.Harness.Command)
	if current == "" || h.opts.Launcher == "" {
		return current, "", ""
	}
	order, reason = formula.CompareLaunchers(current, h.opts.Launcher)
	return current, order, reason
}

// initOptions are the InitOptions forgecell init builds for kind.
func (h *handler) initOptions(f formula.Formula, req proposalRequest) onboarding.InitOptions {
	o := onboarding.InitOptions{Cwd: h.opts.SourceRoot, LabDir: h.opts.LabDir, Launcher: h.opts.Launcher, Runner: h.formulaRunner, Candidates: h.formulaCandidates}
	switch req.Kind {
	case ProposeRebind:
		o.Rebind = f.Harness.Binding
	case ProposeBaseline:
		o.RefreshBaseline = true
	case ProposeReview:
		o.EnableReview = true
	case ProposeModel:
		o.Model, o.ModelSet, o.Role = req.Model, true, req.Role
	}
	return o
}

func (h *handler) createProposal(w http.ResponseWriter, ctx context.Context, req proposalRequest) {
	switch req.Kind {
	case ProposeRebind, ProposeBaseline, ProposeReview, ProposeModel:
	default:
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give kind as rebind, baseline, enable-review or model")
		return
	}
	if !sha256Hex.MatchString(req.Digest) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give the digest of the active Formula shown")
		return
	}
	if req.Kind == ProposeModel {
		if len(req.Model) > maxModelBytes || req.Model == "" || formula.ValidateModel(req.Model) != nil {
			h.refuseWith(w, http.StatusBadRequest, "invalid", "give a model name such as claude-opus-4-8; nothing was saved", categoryReasons["invalid"])
			return
		}
		if req.Role != "" && !slices.Contains([]string{"analysis", "coding", "meta", "review"}, req.Role) {
			h.refuseWith(w, http.StatusBadRequest, "invalid", "give the role as analysis, coding, meta or review, or leave it empty; nothing was saved", categoryReasons["invalid"])
			return
		}
	} else if req.Model != "" || req.Role != "" {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "model and role apply only to a model pin")
		return
	}
	if req.Anyway && req.Kind != ProposeRebind {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "anyway applies only to a rebind")
		return
	}
	if h.sourceMissing(w) {
		return
	}
	active, ok := h.activeFormula()
	if !ok {
		h.refuseWith(w, http.StatusConflict, "unapproved", "this Lab has no approved active Formula; nothing was saved", "Approve a Formula with forgecell init first")
		return
	}
	if active.Formula.SHA256 != req.Digest {
		h.refuseWith(w, http.StatusConflict, "stale", "the active Formula changed since it was shown; nothing was saved", "Reload the Formula view and decide again")
		return
	}
	if pending, err := onboarding.PendingProposal(h.opts.LabDir); err != nil {
		h.refuseWith(w, http.StatusUnprocessableEntity, "refused", "a saved proposal could not be read safely; nothing was saved", "Inspect the Lab's proposals with forgecell init")
		return
	} else if pending != nil {
		h.refuseWith(w, proposalRefusals[0].code, proposalRefusals[0].category, proposalRefusals[0].message, proposalRefusals[0].reason)
		return
	}
	if req.Kind == ProposeRebind {
		current, order, reason := h.rebindOrder(active.Formula)
		switch {
		case h.opts.Launcher == "":
			h.refuseWith(w, http.StatusConflict, "rebind-unavailable", "this build's launcher is unknown; nothing was saved", launcherUnknownReason)
			return
		case current == "":
			h.refuseWith(w, http.StatusConflict, "rebind-unavailable", "the coding agent runs a custom command, which a rebind does not repoint; nothing was saved", "Change it through a reviewed Formula change")
			return
		case order == formula.LauncherSame:
			h.refuseWith(w, http.StatusConflict, "nothing-to-rebind", "the coding agent is already bound to this build; nothing to rebind", "Nothing is needed")
			return
		case order == formula.LauncherOlder && !req.Anyway:
			h.refuseWith(w, http.StatusConflict, "older-build", reason+" Nothing was saved.", "Choose Rebind Anyway if you mean to bind this build")
			return
		case needsAnyway(order) && !req.Anyway:
			h.refuseWith(w, http.StatusConflict, "unordered-build", reason+" Nothing was saved.", "Choose Rebind Anyway if you mean to bind this build")
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, formulaTimeout)
	defer cancel()
	out, err := h.prepare(ctx, h.initOptions(active.Formula, req))
	if err != nil {
		h.refuseFormula(w, err, proposalRefusals)
		return
	}
	if out.Proposal == nil {
		if req.Kind == ProposeBaseline && out.Status == "unchanged" {
			h.refuseWith(w, http.StatusConflict, "unchanged", "the CI baseline already matches the approved required checks; nothing was saved", "Nothing is needed")
			return
		}
		h.refuseWith(w, http.StatusUnprocessableEntity, "refused", "the engine saved no proposal", categoryReasons["refused"])
		return
	}
	// The proposal stays bound to the Formula shown: a Formula approved elsewhere
	// (forgecell init --approve) between the check above and the engine's own read
	// would make it a change of another Formula than the one the request showed.
	if out.Proposal.ActiveHashBefore != req.Digest {
		if _, err = onboarding.Review(h.opts.LabDir, out.Proposal.ID, "dismiss"); err != nil {
			h.refuseWith(w, http.StatusConflict, "stale", "the active Formula changed while the proposal was made, and the new proposal could not be dismissed; it stays pending and nothing was approved", "Review it in the Formula view, or dismiss it with forgecell init --dismiss")
			return
		}
		h.refuseWith(w, http.StatusConflict, "stale", "the active Formula changed while the proposal was made; the new proposal was dismissed and nothing was approved", "Reload the Formula view and decide again")
		return
	}
	v := h.formulaView()
	v.Outcome = "proposed"
	h.json(w, http.StatusOK, v)
}

func (h *handler) decideProposal(w http.ResponseWriter, id, decision string, req decisionRequest) {
	if !sha256Hex.MatchString(req.Digest) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "give the digest of the proposal shown")
		return
	}
	pending, err := onboarding.PendingProposal(h.opts.LabDir)
	if err != nil {
		h.refuseWith(w, http.StatusUnprocessableEntity, "refused", "a saved proposal could not be read safely; nothing was decided", "Inspect the Lab's proposals with forgecell init")
		return
	}
	if pending == nil || pending.ID != id {
		// A proposal decided elsewhere (forgecell init --approve or --dismiss) after
		// it was shown is reported as already decided, never as this view's decision.
		if p, err := onboarding.InspectProposal(h.opts.LabDir, id); err == nil && p.ID == id && p.Status != "pending" {
			h.refuseWith(w, decisionRefusals[3].code, decisionRefusals[3].category, "this proposal was already decided outside this view; nothing was written", decisionRefusals[3].reason)
			return
		}
		h.refuseWith(w, http.StatusConflict, "stale", "this is not the pending proposal; nothing was decided", "Reload the Formula view and decide again")
		return
	}
	p, err := onboarding.InspectProposal(h.opts.LabDir, id)
	if err != nil || p.YAMLHash != req.Digest || p.ID != id {
		h.refuseWith(w, http.StatusConflict, "stale", "the proposal changed since it was shown; nothing was decided", "Reload the Formula view and decide again")
		return
	}
	if _, err = onboarding.Review(h.opts.LabDir, id, decision); err != nil {
		h.refuseFormula(w, err, decisionRefusals)
		return
	}
	v := h.formulaView()
	v.Outcome = map[string]string{"approve": "approved", "dismiss": "dismissed"}[decision]
	h.json(w, http.StatusOK, v)
}

func (h *handler) formulaGet(w http.ResponseWriter) {
	if h.opts.Demo {
		h.json(w, http.StatusOK, demoFormulaPage())
		return
	}
	h.json(w, http.StatusOK, h.formulaView())
}

// demoFormulaPage is the sample Formula view: it offers nothing.
func demoFormulaPage() FormulaPage {
	v := FormulaPage{Sample: true, Reason: "Sample data never writes; run forgecell ui in a Lab of your own repository to change its Formula", Active: ActiveFormulaPage{State: "sample", Name: "repository-care", Atoms: atomWords(nil), Coding: AgentView{Agent: "Sample agent"}, Checks: []CheckView{}, ApprovedBy: "Not recorded"}, Create: []CreateOptionView{}}
	for _, kind := range []string{ProposeRebind, ProposeBaseline, ProposeReview, ProposeModel} {
		v.Create = append(v.Create, CreateOptionView{Kind: kind, Reason: "Sample data never writes"})
	}
	return v
}

// formulaView reads the active Formula, the pending proposal and the ledgers.
func (h *handler) formulaView() FormulaPage {
	v := FormulaPage{Create: []CreateOptionView{}, Active: ActiveFormulaPage{Atoms: []string{}, Checks: []CheckView{}, ApprovedBy: "Not recorded"}}
	list := h.list(ledgerETag(h.opts.LabDir))
	v.InFlight.Incomplete = !list.Complete || list.Truncated
	for _, m := range list.Molecules {
		if active(m.Status) {
			v.InFlight.Running++
		} else if m.Phase == "scope-waiting" && m.Lane != LaneRetired {
			v.InFlight.Waiting++
		}
	}
	id := formula.ActiveIdentity(h.opts.LabDir)
	v.Active.State = id.State
	loaded, approved := h.activeFormula()
	if approved {
		v.Active = activeView(h.opts.LabDir, loaded)
	} else if id.FormulaID != "" && safeWord.MatchString(id.FormulaID) {
		v.Active.Name = id.FormulaID
	}
	pending, err := onboarding.PendingProposal(h.opts.LabDir)
	pendingReason := ""
	switch {
	case err != nil:
		pendingReason = "A saved proposal could not be read safely; inspect the Lab's proposals with forgecell init"
	case pending != nil:
		v.Pending = proposalView(h.opts.LabDir, h.opts.Home, *pending)
		pendingReason = "A proposal is already pending; approve or dismiss it first"
	}
	v.Writable = h.opts.SourceRoot != "" && approved && pendingReason == ""
	switch {
	case h.opts.SourceRoot == "":
		v.Reason = "This Lab UI was started without a source checkout, so it proposes nothing; you can still approve or dismiss a pending proposal"
	case !approved:
		v.Reason = "This Lab has no approved active Formula; approve one with forgecell init first"
	default:
		v.Reason = pendingReason
	}
	offer := func(kind string, available bool, reason string) CreateOptionView {
		o := CreateOptionView{Kind: kind, Available: v.Writable && available, Reason: v.Reason}
		if v.Writable && !available {
			o.Reason = reason
		}
		return o
	}
	if approved {
		f := loaded.Formula
		current, order, reason := h.rebindOrder(f)
		switch {
		case current == "":
			// A custom coding command is not repointed by a rebind.
		case h.opts.Launcher == "":
			v.Create = append(v.Create, offer(ProposeRebind, false, "This build's launcher is unknown, so no rebind is offered. "+launcherUnknownReason))
		case current != h.opts.Launcher && order != formula.LauncherSame:
			o := offer(ProposeRebind, true, "")
			if m := releaseVersion.FindStringSubmatch(h.opts.Launcher); m != nil {
				o.Build = m[1]
			}
			if o.Available && needsAnyway(order) {
				o.NeedsAnyway, o.Reason = true, reason
			}
			v.Create = append(v.Create, o)
		}
		v.Create = append(v.Create, offer(ProposeBaseline, true, ""))
		if _, present, err := f.ReviewAtom(); err == nil && !present {
			v.Create = append(v.Create, offer(ProposeReview, true, ""))
		}
		v.Create = append(v.Create, offer(ProposeModel, true, ""))
	}
	return v
}

// atomWords are the Formula's Atoms in plain words, in Formula order.
func atomWords(atoms []formula.Atom) []string {
	words := []string{}
	for _, a := range atoms {
		switch {
		case a.Type == "gate" && a.Purpose == "scope":
			words = append(words, "Plan approval")
		case a.Type == "gate":
			words = append(words, "Your review")
		default:
			if w, ok := map[string]string{"intake": "Intake", "harness": "Coding", "check": "Checks", "review": "Review", "ship": "Ship", "document": "Document", "learn": "Learn"}[a.Type]; ok {
				words = append(words, w)
			} else {
				words = append(words, "Other Atom")
			}
		}
	}
	return words
}

// agentName is a binding id in plain words.
func agentName(binding string) string {
	if name, ok := map[string]string{"claude-code": "Claude Code", "codex": "Codex", "cursor": "Cursor"}[binding]; ok {
		return name
	}
	if safeWord.MatchString(binding) {
		return binding
	}
	return "Custom agent"
}

var reviewerSources = map[string]string{"review-role": "its own review binding", "meta-override": "the meta binding", "learn-atom": "the learn Atom's binding"}

func reviewerView(f formula.Formula) *AgentView {
	res, configured, err := onboarding.ResolveReviewBinding(f)
	if !configured {
		return nil
	}
	if err != nil {
		return &AgentView{Agent: "No reviewer binding", Warning: "The review Atom has no agent to run; declare harnesses.review or a meta binding"}
	}
	a := &AgentView{Agent: agentName(res.Binding.Binding), Model: modelWord(res.Binding.Model), Source: reviewerSources[res.Source]}
	if res.SameCommandAsCoding {
		a.Warning = "Warning: " + onboarding.SameReviewerWarning
	}
	return a
}

func modelWord(model string) string {
	if model != "" && len(model) <= maxModelBytes && formula.ValidateModel(model) == nil {
		return model
	}
	return ""
}

// checkName is a required check's command, or "" when it names an absolute path.
func checkName(c formula.RequiredCheck) string {
	for _, arg := range c.Argv {
		if strings.HasPrefix(arg, "/") || strings.ContainsAny(arg, "\\\n") {
			return ""
		}
	}
	name := strings.Join(c.Argv, " ")
	if len(name) > maxLabelBytes {
		name, _ = clip(name)
	}
	return name
}

func clip(s string) (string, bool) {
	if len(s) <= maxLabelBytes {
		return s, false
	}
	cut := maxLabelBytes
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…", true
}

func activeView(lab string, loaded formula.Loaded) ActiveFormulaPage {
	f := loaded.Formula
	v := ActiveFormulaPage{State: formula.IdentityApproved, Atoms: atomWords(f.Atoms), Checks: []CheckView{}, ApprovedBy: "Not recorded", Details: ActiveFormulaExtra{SHA256: f.SHA256}}
	if safeWord.MatchString(f.ID) {
		v.Name, v.Details.FormulaID = f.ID, f.ID
	}
	if len(f.SHA256) >= 12 {
		v.ShortSHA = f.SHA256[:12]
	}
	coding, _ := f.RoleBinding(formula.CodingHarness)
	v.Coding = AgentView{Agent: agentName(coding.Binding), Model: modelWord(coding.Model)}
	v.Reviewer = reviewerView(f)
	for _, c := range f.RequiredChecks {
		id := c.ID
		if !safeWord.MatchString(id) {
			id = ""
		}
		name := checkName(c)
		if name == "" {
			name = notRecordedWord(id)
		}
		v.Checks = append(v.Checks, CheckView{Name: name, ID: id})
	}
	if loaded.Approval.SourceKind == "bootstrap-assay" && assayID.MatchString(loaded.Approval.SourceID) {
		if p, err := onboarding.InspectProposal(lab, loaded.Approval.SourceID); err == nil {
			if _, err := time.Parse(time.RFC3339Nano, p.ReviewedAt); err == nil {
				v.ApprovedAt = p.ReviewedAt
			}
		}
	}
	return v
}

func notRecordedWord(s string) string {
	if s == "" {
		return "Not recorded"
	}
	return s
}

var kindTitles = map[string]string{
	onboarding.KindRebind:       "Rebind the coding agent",
	onboarding.KindBaseline:     "Refresh the required checks from CI",
	onboarding.KindEnableReview: "Enable in-loop review",
	onboarding.KindModelPin:     "Change the pinned model",
	onboarding.KindBootstrap:    "Approve a new Formula",
	onboarding.KindChange:       "Change the Formula",
}

var kindNotes = map[string]string{
	onboarding.KindRebind:       "Only the coding agent's binding and launcher change. Atoms, checks, instructions and timeouts are kept.",
	onboarding.KindBaseline:     "Only the required checks change, re-derived from your repository's pull-request workflows. Every later plan must include them.",
	onboarding.KindEnableReview: "Adds one review Atom after Checks and before your review. Runs still stop for your review.",
	onboarding.KindModelPin:     "Only model pins change. Plans in flight for an agent whose pin changes need a fresh intake.",
	onboarding.KindBootstrap:    "This Lab has no approved Formula before it. Review the exact YAML under details before you approve.",
	onboarding.KindChange:       "This change is not one of the kinds this view proposes. Review the exact YAML under details before you approve.",
}

// proposalView builds the review of a pending proposal; home names builds in the
// readable diff. Only Details carries the exact id, digest, keys and YAML lines.
func proposalView(lab, home string, p onboarding.Proposal) *ProposalView {
	v := &ProposalView{Changes: []ChangeView{}, Notes: []string{}, Details: ProposalDetailView{ID: p.ID, Digest: p.YAMLHash, Keys: []onboarding.KeyChange{}, Lines: []onboarding.DiffLine{}}}
	if _, err := time.Parse(time.RFC3339Nano, p.CreatedAt); err == nil {
		v.CreatedAt = p.CreatedAt
	}
	before, hasBefore := onboarding.ProposalBefore(lab, p)
	var changes []onboarding.KeyChange
	if hasBefore {
		var err error
		changes, v.Truncated, err = onboarding.ProposalDiff(before, p.YAML)
		if err != nil {
			v.Truncated = true
		}
	}
	v.Kind = onboarding.ProposalKind(changes, hasBefore)
	v.Title = kindTitles[v.Kind]
	v.Notes = append(v.Notes, kindNotes[v.Kind])
	v.Details.Keys = append(v.Details.Keys, changes...)
	v.Details.Lines, v.Details.Truncated = onboarding.LineDiff(before, p.YAML)
	v.Changes = readableChanges(home, changes)
	if f, err := formula.Parse([]byte(p.YAML)); err == nil && v.Kind == onboarding.KindEnableReview {
		if r := reviewerView(f); r != nil {
			note := "Reviewer: " + r.Agent
			if r.Source != "" {
				note += " (from " + r.Source + ")"
			}
			v.Notes = append(v.Notes, note+".")
			v.Warning = r.Warning
		}
	}
	if incomplete, err := onboarding.ProposalActivationIncomplete(lab, p); err == nil && incomplete {
		v.Interrupted = true
		v.Notes = append(v.Notes, "An approval of this proposal was interrupted; Approve resumes the exact saved approval.")
	}
	v.Rebase = v.Kind == onboarding.KindRebind && hasBefore && formula.LauncherOnlyChange([]byte(before), []byte(p.YAML)) == nil
	return v
}

// keyLabel names a changed key in plain words.
func keyLabel(key string) string {
	role := func(r string) string {
		if w, ok := map[string]string{"analysis": "Planning agent", "coding": "Coding agent", "meta": "Learning agent", "review": "Review agent"}[r]; ok {
			return w
		}
		return "Agent"
	}
	switch {
	case key == "harness.binding":
		return "Coding agent"
	case key == "harness.command":
		return "Coding agent's build"
	case key == "harness.model":
		return "Coding agent's model"
	case strings.HasPrefix(key, "harnesses."):
		parts := strings.Split(key, ".")
		if len(parts) == 3 {
			switch parts[2] {
			case "binding":
				return role(parts[1])
			case "command":
				return role(parts[1]) + "'s build"
			case "model":
				return role(parts[1]) + "'s model"
			}
		}
	case strings.HasPrefix(key, "atoms["):
		id, field, _ := strings.Cut(strings.TrimPrefix(key, "atoms["), "].")
		atom := map[string]string{"harness": "Coding", "learn": "Learn", "review": "Review", "check": "Checks"}[id]
		if atom == "" {
			atom = "An"
		}
		if label, ok := map[string]string{"binding": atom + " Atom's agent", "command": atom + " Atom's build"}[field]; ok {
			return label
		}
		return atom + " Atom setting"
	case strings.HasPrefix(key, "requiredChecks"):
		id, field, _ := strings.Cut(strings.TrimPrefix(key, "requiredChecks["), "].")
		if !safeWord.MatchString(id) {
			return "Required checks"
		}
		return "Required check " + id + " · " + map[string]string{"argv": "command", "dir": "folder", "timeoutMs": "time limit", "workflow": "workflow", "reason": "reason", "select": "selection"}[field]
	}
	return "Another Formula key"
}

// displayValue is a changed value in plain words, with paths left to details.
func displayValue(home, key, raw string) string {
	if raw == "" {
		return "none"
	}
	var list []string
	if json.Unmarshal([]byte(raw), &list) == nil {
		if launcher := nativeLauncher(list); launcher != "" && strings.HasSuffix(key, ".command") {
			return buildWord(home, launcher) + " · " + agentName(list[2])
		}
		for _, item := range list {
			if strings.Contains(item, "/") || strings.Contains(item, "\\") {
				return "see details"
			}
		}
		s, _ := clip(strings.Join(list, " "))
		return s
	}
	var s string
	if json.Unmarshal([]byte(raw), &s) == nil {
		if strings.HasSuffix(key, ".binding") {
			return agentName(s)
		}
		if strings.ContainsAny(s, "/\\\n") {
			return "see details"
		}
		s, _ = clip(s)
		return s
	}
	if strings.ContainsAny(raw, "/\\") {
		return "see details"
	}
	s, _ = clip(raw)
	return s
}

// readableChanges are the changed keys in plain words; the review Atom's keys
// read as one added Atom.
func readableChanges(home string, changes []onboarding.KeyChange) []ChangeView {
	out := []ChangeView{}
	review := false
	for _, c := range changes {
		if strings.HasPrefix(c.Key, "atoms[review].") && c.Change == "added" {
			if !review {
				out = append(out, ChangeView{Label: "Review Atom", Change: "added", Old: "none", New: "after Checks, before your review"})
				review = true
			}
			continue
		}
		out = append(out, ChangeView{Label: keyLabel(c.Key), Change: c.Change, Old: displayValue(home, c.Key, c.Old), New: displayValue(home, c.Key, c.New)})
	}
	return out
}
