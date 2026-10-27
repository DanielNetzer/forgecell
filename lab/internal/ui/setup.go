package ui

import (
	"context"
	"errors"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Set up your Lab (#263): forgecell ui started in a checkout with no ready Lab
// serves this view instead of failing. Each step is the engine path the CLI
// uses, never a file the UI writes itself, behind the same write protection as
// the gates (cookie, tab header, exact Origin, Fetch Metadata and one strict
// JSON object):
//
//	GET  /api/setup                                → the step, read from the Lab's own state
//	POST /api/setup/check   {}                     → onboarding.ReadRepository and Discover (read-only)
//	POST /api/setup/choose  {"lab"}                → one of the matching Labs, by index
//	POST /api/setup/create  {"harness"}            → onboarding.Prepare, as forgecell init --harness ID
//	POST /api/setup/approve {"proposal","digest"}  → onboarding.Review approve, as init --approve
//	POST /api/setup/review  {}                     → onboarding.Prepare EnableReview, a second proposal
//
// One setup action runs at a time; a second submit is refused and nothing is
// resubmitted. Approval binds the shown proposal: its id and YAML digest must
// still be the pending proposal's, or nothing is approved. Engine error text,
// which can name local paths, is never served. Until the Lab has an approved
// Formula every other API route is refused.

// SetupOptions put a UI launch in setup mode. The functions are the engine
// calls; forgecell ui passes the ones forgecell init uses and tests replace them.
type SetupOptions struct {
	// Labs are the existing Lab directories that match the checkout's repository
	// when there is more than one; the operator chooses one first. Otherwise
	// LabDir is the Lab to set up.
	Labs []string
	// Launcher and Env are what forgecell init passes to Prepare.
	Launcher string
	Env      map[string]string
	// ReadRepository and Discover are read-only: git and gh's own repository
	// read, and the coding agent probes forgecell doctor runs.
	ReadRepository func(ctx context.Context, cwd string) (onboarding.Repository, error)
	Discover       func(ctx context.Context, cwd string) []onboarding.Candidate
	// Prepare and Review are forgecell init's proposal and decision paths.
	Prepare func(context.Context, onboarding.InitOptions) (onboarding.Outcome, error)
	Review  func(dir, id, decision string) (onboarding.Proposal, error)
	// Resources opens the Lab's resource guard once the Lab is ready, so nothing
	// is created in a Lab directory before the engine has made it.
	Resources func(lab string) (*resources.Guard, error)
}

const (
	// setupTimeout bounds one setup engine call; it outlives a lost response,
	// and the page reads the state back instead of resubmitting.
	setupTimeout = 10 * time.Minute
	// maxSetupProposals bounds the proposals read from assays/; more fails closed.
	maxSetupProposals = 256
	// maxSetupItems bounds the Atoms, checks and gates one proposal view carries.
	maxSetupItems = 64
	// maxSetupLabs bounds the Lab chooser.
	maxSetupLabs = 32
)

// SetupHarnesses are the coding agents setup offers, in display order.
var SetupHarnesses = []string{"claude-code", "codex", "cursor"}

var setupHarnessNames = map[string]string{"claude-code": "Claude Code", "codex": "Codex", "cursor": "Cursor"}

var (
	assayID       = regexp.MustCompile(`^assay-[a-f0-9]{64}$`)
	setupDigest   = regexp.MustCompile(`^[a-f0-9]{64}$`)
	defaultBranch = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)
	readinessWord = regexp.MustCompile(`^[a-z-]{1,32}$`)
)

// errSetupLab is a Lab whose proposals or Formula cannot be read safely.
var errSetupLab = errors.New("the Lab's proposals cannot be read")

// setupState is one launch's setup progress. busy admits one action at a time;
// ready is set, once, when the Lab has an approved Formula and its resource
// guard is open, and from then on every route is served.
type setupState struct {
	o     SetupOptions
	busy  atomic.Bool
	ready atomic.Bool
	// mu guards the fields below.
	mu         sync.Mutex
	lab        string
	chosen     bool
	checked    bool
	repository SetupRepository
	candidates []onboarding.Candidate
	// agentCwd is where Check probed the coding agents; Create probes there again.
	agentCwd string
}

func newSetupState(o *SetupOptions, lab string) *setupState {
	if o == nil {
		return nil
	}
	s := &setupState{o: *o, lab: lab}
	s.o.Labs = slices.Clone(o.Labs)
	if len(s.o.Labs) > maxSetupLabs {
		s.o.Labs = s.o.Labs[:maxSetupLabs]
	}
	return s
}

// choosing reports a Lab chooser still waiting for the operator.
func (s *setupState) choosing() bool { return len(s.o.Labs) > 1 && !s.chosen }

// setupActive reports that this launch is still setting up its Lab.
func (h *handler) setupActive() bool { return h.setup != nil && !h.setup.ready.Load() }

// setupRoute reports a POST setup path and its action.
func setupRoute(path string) (string, bool) {
	action, ok := strings.CutPrefix(path, "/api/setup/")
	switch action {
	case "check", "choose", "create", "approve", "review":
		return action, ok
	}
	return "", false
}

// SetupView is GET /api/setup and every accepted setup response.
type SetupView struct {
	// Active is false when this launch has a ready Lab or serves sample data.
	Active bool `json:"active"`
	// Step is choose, repository, agent, review, done or problem.
	Step       string           `json:"step"`
	Busy       bool             `json:"busy"`
	Lab        string           `json:"lab,omitempty"`
	Labs       []SetupLab       `json:"labs,omitempty"`
	Checked    bool             `json:"checked"`
	Repository *SetupRepository `json:"repository,omitempty"`
	Agents     []SetupAgent     `json:"agents,omitempty"`
	Proposal   *SetupProposal   `json:"proposal,omitempty"`
	// ReviewAtom is present, available or unavailable once the Formula is approved.
	ReviewAtom string `json:"reviewAtom,omitempty"`
	Problem    string `json:"problem,omitempty"`
}

// SetupLab is one Lab in the chooser: its directory name, never its path.
type SetupLab struct {
	Index   int    `json:"index"`
	Name    string `json:"name"`
	Formula string `json:"formula,omitempty"`
	Binding string `json:"binding,omitempty"`
}

// SetupRepository is the repository step in plain words.
type SetupRepository struct {
	OK            bool   `json:"ok"`
	Repo          string `json:"repo,omitempty"`
	DefaultBranch string `json:"defaultBranch,omitempty"`
	// GitHub is signed-in, sign-in-needed or unknown.
	GitHub  string `json:"github"`
	Message string `json:"message"`
}

// SetupAgent is one coding agent as the doctor probes found it. The probes
// record no provider version or model, so none is shown; the model is pinned
// later, on the approved Formula, with forgecell init --model.
type SetupAgent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Ready     bool   `json:"ready"`
	Readiness string `json:"readiness"`
	Installed string `json:"installed"`
	SignIn    string `json:"signIn"`
	Detail    string `json:"detail"`
}

// SetupProposal is the waiting Formula proposal in plain words. ID and Digest
// are what an approval must send back.
type SetupProposal struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	// Kind is bootstrap, enable-review or change.
	Kind string `json:"kind"`
	// Resume marks an approval that started and must be finished.
	Resume    bool         `json:"resume"`
	Formula   string       `json:"formula"`
	Binding   string       `json:"binding"`
	Atoms     []SetupAtom  `json:"atoms"`
	Checks    []SetupCheck `json:"checks"`
	Gates     []SetupGate  `json:"gates"`
	Truncated bool         `json:"truncated,omitempty"`
}

type SetupAtom struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Label string `json:"label"`
}

type SetupCheck struct {
	ID       string `json:"id"`
	Command  string `json:"command"`
	Dir      string `json:"dir,omitempty"`
	Workflow string `json:"workflow,omitempty"`
}

type SetupGate struct {
	Purpose string `json:"purpose"`
	Mode    string `json:"mode"`
}

// setupLab is a Lab's setup state, read from its own files.
type setupLab struct {
	approved   bool
	formula    formula.Formula
	pending    *onboarding.Proposal
	incomplete bool
}

// readSetupLab finds the proposal waiting for a decision, an interrupted
// activation first, as forgecell init does, and whether the active Formula has
// a current approval. It never writes.
func readSetupLab(dir string) (setupLab, error) {
	var st setupLab
	names, err := proposalNames(filepath.Join(dir, "assays"))
	if err != nil {
		return st, err
	}
	var pending, recovery *onboarding.Proposal
	for _, name := range names {
		id := strings.TrimSuffix(name, ".json")
		if name == id || !assayID.MatchString(id) {
			continue
		}
		p, err := onboarding.InspectProposal(dir, id)
		if err != nil {
			return st, errSetupLab
		}
		incomplete, err := onboarding.ProposalActivationIncomplete(dir, p)
		if err != nil {
			return st, errSetupLab
		}
		if incomplete {
			saved := p
			recovery = &saved
		}
		if p.Status == "pending" && pending == nil {
			saved := p
			pending = &saved
		}
	}
	st.pending, st.incomplete = pending, recovery != nil
	if recovery != nil {
		st.pending = recovery
	}
	if loaded, err := formula.Load(dir, ""); err == nil && loaded.Approved {
		st.approved, st.formula = true, loaded.Formula
	}
	return st, nil
}

// NeedsSetup reports whether forgecell ui should serve Set up your Lab for dir:
// whenever it has no approved active Formula. That is no lab.json yet, a
// proposal or an interrupted activation waiting, a lab.json with nothing
// waiting (Create proposes the Formula to re-approve), or proposals that cannot
// be read safely, which the problem step explains. Only a Lab with an approved
// Formula opens as before. It never writes.
func NeedsSetup(dir string) bool {
	loaded, err := formula.Load(dir, "")
	return err != nil || !loaded.Approved
}

// proposalNames lists assays/ without following a link, sorted; a missing
// directory is none, and more than maxSetupProposals entries fails closed.
func proposalNames(dir string) ([]string, error) {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.IsDir() {
		return nil, errSetupLab
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, errSetupLab
	}
	defer f.Close()
	names, err := f.Readdirnames(maxSetupProposals + 1)
	if err != nil && err != io.EOF {
		return nil, errSetupLab
	}
	if len(names) > maxSetupProposals {
		return nil, errSetupLab
	}
	sort.Strings(names)
	return names, nil
}

// setupStatus serves GET /api/setup.
func (h *handler) setupStatus(w http.ResponseWriter) {
	if h.setup == nil || h.opts.Demo {
		h.json(w, http.StatusOK, SetupView{Step: "none"})
		return
	}
	h.json(w, http.StatusOK, h.setupView())
}

// setupView derives the step from the Lab's real state, so a reload resumes
// where the Lab is. Once the Formula is approved it opens the resource guard
// and marks the Lab ready.
func (h *handler) setupView() SetupView {
	s := h.setup
	s.mu.Lock()
	defer s.mu.Unlock()
	v := SetupView{Active: !s.ready.Load(), Busy: s.busy.Load(), Checked: s.checked}
	if s.choosing() {
		v.Step = "choose"
		for i, dir := range s.o.Labs {
			lab := SetupLab{Index: i, Name: setupText(filepath.Base(dir))}
			if active, err := formula.Inspect(dir, ""); err == nil {
				lab.Formula, lab.Binding = setupText(active.Formula.ID), setupText(active.Formula.Harness.Binding)
			}
			v.Labs = append(v.Labs, lab)
		}
		return v
	}
	v.Lab = setupText(filepath.Base(s.lab))
	if s.checked {
		repository := s.repository
		v.Repository = &repository
		v.Agents = agentViews(s.candidates)
	}
	st, err := readSetupLab(s.lab)
	switch {
	case err != nil:
		v.Step, v.Problem = "problem", "The Lab's proposals could not be read safely: one is invalid, linked, oversized or changed. Run forgecell init --review to inspect it; nothing was changed."
	case st.pending != nil:
		v.Step, v.Proposal = "review", proposalView(*st.pending, st.incomplete)
	case st.approved:
		v.Step = "done"
		v.ReviewAtom = "available"
		if _, ok, err := st.formula.ReviewAtom(); err != nil {
			v.ReviewAtom = "unavailable"
		} else if ok {
			v.ReviewAtom = "present"
		}
		if !s.ready.Load() && !h.finishSetup() {
			// Every read retries the guard, so a reload is enough once its cause is gone.
			v.Problem = "Your Lab is set up, but its resource guard could not start. Reload to try again, or restart forgecell ui to see why."
		}
		v.Active = !s.ready.Load()
	case !s.checked || !s.repository.OK:
		v.Step = "repository"
	default:
		v.Step = "agent"
	}
	return v
}

// finishSetup opens the resource guard and serves the Lab, with s.mu held.
// The Lab directory and guard are written before ready is set, and every other
// route reads them only after reading ready.
func (h *handler) finishSetup() bool {
	s := h.setup
	var guard *resources.Guard
	if s.o.Resources != nil {
		g, err := s.o.Resources(s.lab)
		if err != nil {
			return false
		}
		guard = g
	}
	h.opts.LabDir, h.opts.Resources = s.lab, guard
	s.ready.Store(true)
	return true
}

type chooseRequest struct {
	Lab int `json:"lab"`
}

type createRequest struct {
	Harness string `json:"harness"`
}

type approveRequest struct {
	Proposal string `json:"proposal"`
	Digest   string `json:"digest"`
}

// setupPost serves one POST setup route after the write protection has passed.
func (h *handler) setupPost(w http.ResponseWriter, r *http.Request, action string) {
	s := h.setup
	if s == nil || h.opts.Demo {
		h.refuseGate(w, http.StatusNotFound, "setup", "this Lab UI is not setting up a Lab; nothing was written")
		return
	}
	var choose chooseRequest
	var create createRequest
	var approve approveRequest
	var body any = &emptyRequest{}
	switch action {
	case "choose":
		body = &choose
	case "create":
		body = &create
	case "approve":
		body = &approve
	}
	if decodeGate(w, r, body) != nil {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "send one small JSON object with exactly the action's fields")
		return
	}
	if !s.busy.CompareAndSwap(false, true) {
		h.refuseWith(w, http.StatusConflict, "busy", "another setup step is still running; nothing was resubmitted", "Wait for it to finish; the page reads the result back")
		return
	}
	ok := func() bool {
		defer s.busy.Store(false)
		// The engine call outlives a lost response; the page reads the state back.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), setupTimeout)
		defer cancel()
		switch action {
		case "check":
			return h.setupCheck(ctx, w)
		case "choose":
			return h.setupChoose(w, choose)
		case "create":
			return h.setupCreate(ctx, w, create)
		case "approve":
			return h.setupApprove(w, approve)
		}
		return h.setupReview(ctx, w)
	}()
	if ok {
		h.json(w, http.StatusOK, h.setupView())
	}
}

// setupLabDir is the Lab setup acts on, or a refusal while the chooser waits.
func (h *handler) setupLabDir(w http.ResponseWriter) (string, bool) {
	s := h.setup
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.choosing() {
		h.refuseWith(w, http.StatusConflict, "choose", "choose a Lab first; nothing was written", "Choose one of the Labs for this repository")
		return "", false
	}
	return s.lab, true
}

func (h *handler) setupCheck(ctx context.Context, w http.ResponseWriter) bool {
	s := h.setup
	root := h.opts.SourceRoot
	if root == "" || s.o.ReadRepository == nil || s.o.Discover == nil {
		h.refuseWith(w, http.StatusConflict, "source", "this Lab UI has no source checkout; nothing was read", "Run forgecell ui inside the repository's checkout")
		return false
	}
	repo, err := s.o.ReadRepository(ctx, root)
	view := repositoryView(repo, err)
	cwd := root
	if err == nil && filepath.IsAbs(repo.Root) {
		cwd = repo.Root
	}
	candidates := s.o.Discover(ctx, cwd)
	s.mu.Lock()
	s.checked, s.repository, s.candidates, s.agentCwd = true, view, slices.Clone(candidates), cwd
	s.mu.Unlock()
	return true
}

func (h *handler) setupChoose(w http.ResponseWriter, req chooseRequest) bool {
	s := h.setup
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.choosing() {
		h.refuseWith(w, http.StatusConflict, "chosen", "a Lab is already chosen; nothing was changed", "Restart forgecell ui to choose another Lab")
		return false
	}
	if req.Lab < 0 || req.Lab >= len(s.o.Labs) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "choose one of the listed Labs")
		return false
	}
	// The Lab is read and written from here on, so it must still be a real
	// directory, never a link swapped in since the scan.
	dir := s.o.Labs[req.Lab]
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		h.refuseWith(w, http.StatusConflict, "lab", "that Lab is no longer a plain directory; nothing was chosen", "Restart forgecell ui to list the Labs again")
		return false
	}
	s.lab, s.chosen = dir, true
	return true
}

func (h *handler) setupCreate(ctx context.Context, w http.ResponseWriter, req createRequest) bool {
	s := h.setup
	if !slices.Contains(SetupHarnesses, req.Harness) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "choose claude-code, codex or cursor")
		return false
	}
	lab, ok := h.setupLabDir(w)
	if !ok {
		return false
	}
	st, err := readSetupLab(lab)
	switch {
	case err != nil:
		h.refuseWith(w, http.StatusUnprocessableEntity, "lab", "the Lab's proposals could not be read safely; nothing was written", "Run forgecell init --review to inspect them")
		return false
	case st.pending != nil:
		// The Lab is already created and its proposal waits: continue, never
		// propose a second one.
		return true
	case st.approved:
		h.refuseWith(w, http.StatusConflict, "done", "this Lab already has an approved Formula; nothing was written", "Go to Issues")
		return false
	}
	s.mu.Lock()
	checked, repository, cwd := s.checked, s.repository, s.agentCwd
	s.mu.Unlock()
	if !checked || !repository.OK {
		h.refuseWith(w, http.StatusConflict, "repository", "the repository has not been read; nothing was written", "Check Again on the repository step")
		return false
	}
	if s.o.Prepare == nil || s.o.Discover == nil || s.o.Launcher == "" {
		h.refuseGate(w, http.StatusConflict, "refused", "this Lab UI cannot create a Lab; nothing was written")
		return false
	}
	// The probes shown at Check may be stale: an agent can sign out since. As
	// forgecell init does, Create probes again and proposes only from this run,
	// and the page shows the fresh probes whatever the outcome.
	candidates := s.o.Discover(ctx, cwd)
	s.mu.Lock()
	s.candidates = slices.Clone(candidates)
	s.mu.Unlock()
	i := slices.IndexFunc(candidates, func(c onboarding.Candidate) bool { return c.ID == req.Harness })
	if i < 0 || candidates[i].Readiness != "ready" {
		h.refuseWith(w, http.StatusConflict, "agent", "that coding agent is not ready now; nothing was written", "Sign in with its own CLI, then Check Again")
		return false
	}
	env := map[string]string{}
	for k, v := range s.o.Env {
		env[k] = v
	}
	out, err := s.o.Prepare(ctx, onboarding.InitOptions{Cwd: h.opts.SourceRoot, LabDir: lab, Launcher: s.o.Launcher, Preferred: req.Harness, Env: env, Candidates: candidates})
	if err != nil {
		h.refuseWith(w, http.StatusUnprocessableEntity, "refused", "the engine did not create the Lab; nothing else was attempted", setupReason(err))
		return false
	}
	if out.Status != "pending" || out.Proposal == nil {
		h.refuseWith(w, http.StatusConflict, "agent", "the engine proposed no Formula for that coding agent; nothing else was attempted", "Check Again, then choose a ready coding agent")
		return false
	}
	return true
}

func (h *handler) setupApprove(w http.ResponseWriter, req approveRequest) bool {
	s := h.setup
	if !assayID.MatchString(req.Proposal) || !setupDigest.MatchString(req.Digest) {
		h.refuseGate(w, http.StatusBadRequest, "invalid", "send the shown proposal and its digest")
		return false
	}
	lab, ok := h.setupLabDir(w)
	if !ok {
		return false
	}
	st, err := readSetupLab(lab)
	if err != nil {
		h.refuseWith(w, http.StatusUnprocessableEntity, "lab", "the Lab's proposals could not be read safely; nothing was approved", "Run forgecell init --review to inspect them")
		return false
	}
	// The approval binds exactly what was shown: the same proposal with the same
	// YAML bytes, still waiting. Anything else approves nothing.
	if st.pending == nil || st.pending.ID != req.Proposal || st.pending.YAMLHash != req.Digest {
		h.refuseWith(w, http.StatusConflict, "stale", "the proposal changed since it was shown; nothing was approved", "Review the current proposal, then approve it")
		return false
	}
	if s.o.Review == nil {
		h.refuseGate(w, http.StatusConflict, "refused", "this Lab UI cannot approve a Formula; nothing was approved")
		return false
	}
	if _, err := s.o.Review(lab, req.Proposal, "approve"); err != nil {
		h.refuseWith(w, http.StatusUnprocessableEntity, "refused", "the engine did not approve the Formula; nothing else was attempted", setupReason(err))
		return false
	}
	return true
}

func (h *handler) setupReview(ctx context.Context, w http.ResponseWriter) bool {
	s := h.setup
	lab, ok := h.setupLabDir(w)
	if !ok {
		return false
	}
	st, err := readSetupLab(lab)
	switch {
	case err != nil:
		h.refuseWith(w, http.StatusUnprocessableEntity, "lab", "the Lab's proposals could not be read safely; nothing was written", "Run forgecell init --review to inspect them")
		return false
	case st.pending != nil:
		h.refuseWith(w, http.StatusConflict, "pending", "a proposal is already waiting; nothing was written", "Approve or review the waiting proposal first")
		return false
	case !st.approved:
		h.refuseWith(w, http.StatusConflict, "approve", "the review Atom is added to an approved Formula; nothing was written", "Approve the first Formula first")
		return false
	}
	if _, present, err := st.formula.ReviewAtom(); err != nil || present {
		h.refuseWith(w, http.StatusConflict, "present", "this Formula already has a review Atom or cannot take one; nothing was written", "Go to Issues")
		return false
	}
	if s.o.Prepare == nil || h.opts.SourceRoot == "" {
		h.refuseGate(w, http.StatusConflict, "refused", "this Lab UI cannot propose a Formula change; nothing was written")
		return false
	}
	if _, err := s.o.Prepare(ctx, onboarding.InitOptions{Cwd: h.opts.SourceRoot, LabDir: lab, EnableReview: true}); err != nil {
		h.refuseWith(w, http.StatusUnprocessableEntity, "refused", "the engine did not propose the review Atom; nothing else was attempted", setupReason(err))
		return false
	}
	return true
}

// setupReasons map engine error text to a fixed next step; the text itself is
// never served.
var setupReasons = []struct{ part, reason string }{
	{"another Formula write is active", "Another Formula change is in progress; wait for it, then reload"},
	{"stale approval refused", "The Lab changed since this proposal; Check Again to propose a new one"},
	{"belongs to another repository", "This Lab belongs to another repository; run forgecell ui with --lab-name NAME for a separate Lab"},
	{"repository discovery failed for gh", "Run gh auth login, then Check Again"},
	{"launcher are required", "Install Forgecell so its launcher is known, then Check Again"},
}

func setupReason(err error) string {
	text := err.Error()
	for _, r := range setupReasons {
		if strings.Contains(text, r.part) {
			return r.reason
		}
	}
	return "Run forgecell init in this checkout to see the full error"
}

// repositoryView puts the repository read in plain words. Error text is
// classified, never served.
func repositoryView(repo onboarding.Repository, err error) SetupRepository {
	if err == nil {
		name := repository(repo.Repo)
		if name == "" {
			return SetupRepository{GitHub: "unknown", Message: "GitHub returned a repository name Forgecell does not accept. Check the origin remote, then Check Again."}
		}
		branch := repo.DefaultBranch
		if !defaultBranch.MatchString(branch) {
			branch = ""
		}
		return SetupRepository{OK: true, Repo: name, DefaultBranch: branch, GitHub: "signed-in", Message: "Signed in: the GitHub CLI can read " + name + "."}
	}
	text := err.Error()
	switch {
	case strings.HasPrefix(text, "repository discovery failed for gh"):
		return SetupRepository{GitHub: "sign-in-needed", Message: "The GitHub CLI could not read this repository. Run gh auth login, then Check Again."}
	case strings.HasPrefix(text, "repository discovery failed for git"):
		return SetupRepository{GitHub: "unknown", Message: "This is not a git checkout with an origin remote. Run forgecell ui inside your repository's checkout, then Check Again."}
	case strings.Contains(text, "GitHub repository"):
		return SetupRepository{GitHub: "unknown", Message: "The origin remote is not a GitHub repository this GitHub CLI can read. Check the origin remote, then Check Again."}
	}
	return SetupRepository{GitHub: "unknown", Message: "The repository could not be read. Run forgecell init in this checkout to see why, then Check Again."}
}

// agentViews lists every offered coding agent, ready or not, so an unavailable
// one is explained rather than hidden. Detail is redacted and bounded.
func agentViews(candidates []onboarding.Candidate) []SetupAgent {
	var out []SetupAgent
	for _, id := range SetupHarnesses {
		a := SetupAgent{ID: id, Name: setupHarnessNames[id], Readiness: "missing", Installed: "missing", SignIn: "unknown", Detail: "Not found on this machine. Install it, sign in with its own CLI, then Check Again."}
		if i := slices.IndexFunc(candidates, func(c onboarding.Candidate) bool { return c.ID == id }); i >= 0 {
			c := candidates[i].CompleteUnknowns()
			a.Ready = c.Readiness == "ready"
			a.Readiness = setupWord(c.Readiness)
			a.Installed = setupWord(c.Installed.State)
			a.SignIn = setupWord(c.Authentication.State)
			a.Detail = redactor{}.reason(c.Detail)
			if a.Detail == "" {
				a.Detail = "No detail was recorded."
			}
		}
		out = append(out, a)
	}
	return out
}

func setupWord(s string) string {
	if readinessWord.MatchString(s) {
		return s
	}
	return "unknown"
}

// setupText is one bounded line with local paths and secrets removed.
func setupText(s string) string { return redactor{}.reason(s) }

// proposalView shows a proposal in plain words: its Atoms in order, the CI
// baseline checks and the gate policy, each bounded; Truncated says so.
func proposalView(p onboarding.Proposal, resume bool) *SetupProposal {
	v := &SetupProposal{ID: p.ID, Digest: p.YAMLHash, Kind: "change", Resume: resume, Atoms: []SetupAtom{}, Checks: []SetupCheck{}, Gates: []SetupGate{}}
	f, err := formula.Parse([]byte(p.YAML))
	if err != nil {
		v.Truncated = true
		return v
	}
	_, review, _ := f.ReviewAtom()
	switch {
	case p.ActiveIDBefore == "":
		v.Kind = "bootstrap"
	case review:
		v.Kind = "enable-review"
	}
	v.Formula, v.Binding = setupText(f.Name), setupText(f.Harness.Binding)
	for i, a := range f.Atoms {
		if i == maxSetupItems {
			v.Truncated = true
			break
		}
		v.Atoms = append(v.Atoms, SetupAtom{ID: setupText(a.ID), Type: setupText(a.Type), Label: atomLabel(a)})
		if a.Type == "gate" && len(v.Gates) < maxSetupItems {
			mode := "human"
			if policy, ok := a.DeclaresPolicy(); ok {
				mode = setupText(policy)
			}
			v.Gates = append(v.Gates, SetupGate{Purpose: setupText(a.Purpose), Mode: mode})
		}
	}
	for i, c := range f.RequiredChecks {
		if i == maxSetupItems {
			v.Truncated = true
			break
		}
		v.Checks = append(v.Checks, SetupCheck{ID: setupText(c.ID), Command: setupText(strings.Join(c.Argv, " ")), Dir: setupText(c.Dir), Workflow: setupText(c.Workflow)})
	}
	return v
}

// atomLabel says what an Atom does, in plain words.
func atomLabel(a formula.Atom) string {
	switch {
	case a.Type == "intake":
		return "Read the Issue"
	case a.Type == "gate" && a.Purpose == "scope":
		return "You approve the plan"
	case a.Type == "harness":
		return "The coding agent writes the change"
	case a.Type == "check":
		return "Checks run in a fresh checkout"
	case a.Type == "review":
		return "A second agent reviews the change"
	case a.Type == "gate" && a.Purpose == "review":
		return "You review before a draft PR"
	case a.Type == "ship":
		return "Publish a draft PR"
	case a.Type == "document":
		return "Record the outcome on the Issue"
	case a.Type == "learn":
		return "Learn from finished runs"
	}
	return "A " + setupText(a.Type) + " Atom"
}
