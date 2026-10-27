package ui

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// setupFormula is the bootstrap Formula the fake Prepare proposes, and
// setupReviewFormula the same Formula with the review Atom inserted.
const setupFormula = `kind: formula
id: widgets-github-issue
name: acme/widgets issue workflow
intake: {source: github-issues, repo: acme/widgets}
harness: {binding: claude-code, command: [/cli, __adapter, claude-code, /claude]}
atoms:
  - {id: intake, type: intake, source: github-issues}
  - {id: scope, type: gate, purpose: scope, mode: human}
  - {id: harness, type: harness, binding: claude-code}
  - {id: check, type: check}
  - {id: gate, type: gate, purpose: review, mode: human}
  - {id: ship, type: ship, via: github}
  - {id: document, type: document, target: github-issue}
requiredChecks:
  - {id: go-test, argv: [go, test, ./...], dir: lab, timeoutMs: 600000, workflow: .github/workflows/lab.yml, reason: CI runs it on pull requests}
`

var setupReviewFormula = strings.NewReplacer(
	"  - {id: gate,", "  - {id: review, type: review}\n  - {id: gate,",
	"atoms:\n", "harnesses:\n  review: {binding: codex, command: [/cli, __adapter, codex, /codex]}\natoms:\n",
).Replace(setupFormula)

// localLeak is a local path the fakes put in errors and probe detail; no
// response may carry it.
const localLeak = "/Users/someone/private"

// fakeSetup holds the engine seams. Every Lab write happens inside Prepare or
// Review: each seam checks on entry that the Lab is exactly as the last seam
// left it, so a write by anything else fails the test.
type fakeSetup struct {
	t   *testing.T
	lab string
	mu  sync.Mutex
	// last is the Lab snapshot the last seam left.
	last                                string
	repoErr, prepareErr, reviewErr      error
	prepares, reviews, reads, discovers int
	options                             []onboarding.InitOptions
	resources                           []string
	entered, release                    chan struct{}
	candidates                          []onboarding.Candidate
}

func newFakeSetup(t *testing.T, lab string) *fakeSetup {
	f := &fakeSetup{t: t, lab: lab, last: snapshot(t, lab)}
	f.candidates = []onboarding.Candidate{
		{ID: "claude-code", Executable: localLeak + "/bin/claude", Readiness: "ready", Detail: "CLI capabilities and authentication verified.", Installed: harness.Capability{State: "verified"}, Authentication: harness.Capability{State: "verified"}},
		{ID: "codex", Executable: localLeak + "/bin/codex", Readiness: "login-required", Detail: "Probe step auth failed at " + localLeak + "/bin/codex — provider reports it is not logged in.", Installed: harness.Capability{State: "verified"}, Authentication: harness.Capability{State: "login-required"}},
	}
	return f
}

func (f *fakeSetup) unchanged(where string) {
	f.t.Helper()
	if now := snapshot(f.t, f.lab); now != f.last {
		f.t.Errorf("%s: the Lab changed outside an engine call:\n%s\n---\n%s", where, f.last, now)
	}
}

func (f *fakeSetup) options_() *SetupOptions {
	return &SetupOptions{
		Launcher: "/cli",
		Env:      map[string]string{"CLAUDECODE": ""},
		ReadRepository: func(context.Context, string) (onboarding.Repository, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reads++
			if f.repoErr != nil {
				return onboarding.Repository{}, f.repoErr
			}
			return onboarding.Repository{Root: "/src/checkout", Repo: "acme/widgets", DefaultBranch: "main"}, nil
		},
		Discover: func(context.Context, string) []onboarding.Candidate {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.discovers++
			return f.candidates
		},
		Prepare: func(_ context.Context, o onboarding.InitOptions) (onboarding.Outcome, error) {
			if f.entered != nil {
				f.entered <- struct{}{}
				<-f.release
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.unchanged("before Prepare")
			defer func() { f.last = snapshot(f.t, f.lab) }()
			f.prepares++
			f.options = append(f.options, o)
			if f.prepareErr != nil {
				return onboarding.Outcome{}, f.prepareErr
			}
			yaml := setupFormula
			if o.EnableReview {
				yaml = setupReviewFormula
			}
			p, err := onboarding.SaveProposal(o.LabDir, yaml)
			if err != nil {
				f.t.Fatal(err)
			}
			return onboarding.Outcome{Status: "pending", Proposal: &p}, nil
		},
		Review: func(dir, id, decision string) (onboarding.Proposal, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.unchanged("before Review")
			defer func() { f.last = snapshot(f.t, f.lab) }()
			f.reviews++
			if f.reviewErr != nil {
				return onboarding.Proposal{}, f.reviewErr
			}
			return onboarding.Review(dir, id, decision)
		},
		Resources: func(lab string) (*resources.Guard, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.resources = append(f.resources, lab)
			return nil, nil
		},
	}
}

func (f *fakeSetup) calls() (prepares, reviews int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares, f.reviews
}

func setupHandler(t *testing.T, f *fakeSetup) http.Handler {
	t.Helper()
	return testHandler(t, Options{LabDir: f.lab, SourceRoot: "/src/checkout", Setup: f.options_()})
}

// setupDo sends one request and checks the response never carries a local path.
func setupDo(t *testing.T, h http.Handler, r request, lab string) (int, SetupView, gateRefusal) {
	t.Helper()
	w := do(t, h, r)
	body := w.Body.String()
	for _, leak := range []string{lab, localLeak, "/src/checkout", "/cli"} {
		if leak != "" && strings.Contains(body, leak) {
			t.Errorf("%s %s serves the local path %q: %s", r.method, r.path, leak, body)
		}
	}
	var v SetupView
	var refusal gateRefusal
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v %s", r.path, err, body)
		}
	} else {
		_ = json.Unmarshal(w.Body.Bytes(), &refusal)
	}
	return w.Code, v, refusal
}

func setupGet(t *testing.T, h http.Handler, lab string) SetupView {
	t.Helper()
	code, v, _ := setupDo(t, h, authed("/api/setup"), lab)
	if code != http.StatusOK {
		t.Fatalf("GET /api/setup: %d", code)
	}
	return v
}

func setupPost(t *testing.T, h http.Handler, lab, action, body string) (int, SetupView, gateRefusal) {
	t.Helper()
	return setupDo(t, h, gatePost("/api/setup/"+action, body), lab)
}

func approveBody(p *SetupProposal) string {
	return `{"proposal":"` + p.ID + `","digest":"` + p.Digest + `"}`
}

// Every step's happy path, from no Lab files at all to an approved Formula:
// the steps come from the Lab's state, each write is one engine call, and every
// other API route is refused until the Lab is ready.
func TestUISetupStepsFromAFreshLabToAnApprovedFormula(t *testing.T) {
	lab := t.TempDir()
	f := newFakeSetup(t, lab)
	h := setupHandler(t, f)

	v := setupGet(t, h, lab)
	if !v.Active || v.Step != "repository" || v.Checked || v.Repository != nil || v.Lab != filepath.Base(lab) {
		t.Fatalf("first view %+v", v)
	}
	for _, path := range []string{"/api/lab", "/api/molecules", "/api/loop", "/api/issues"} {
		if code, _, refusal := setupDo(t, h, authed(path), lab); code != http.StatusConflict || refusal.Category != "setup" {
			t.Errorf("GET %s during setup: %d %+v", path, code, refusal)
		}
	}
	if code, _, refusal := setupPost(t, h, lab, "../runs", `{"issue":1}`); code == http.StatusOK {
		t.Errorf("a non-setup POST during setup: %d %+v", code, refusal)
	}
	if code, _, refusal := setupDo(t, h, gatePost("/api/runs", `{"issue":1}`), lab); code != http.StatusConflict || refusal.Category != "setup" {
		t.Errorf("POST /api/runs during setup: %d %+v", code, refusal)
	}

	code, v, _ := setupPost(t, h, lab, "check", `{}`)
	if code != http.StatusOK || v.Step != "agent" || !v.Checked || v.Repository == nil || !v.Repository.OK || v.Repository.Repo != "acme/widgets" || v.Repository.DefaultBranch != "main" || v.Repository.GitHub != "signed-in" {
		t.Fatalf("check: %d %+v %+v", code, v, v.Repository)
	}
	if len(v.Agents) != 3 || v.Agents[0].ID != "claude-code" || !v.Agents[0].Ready || v.Agents[1].ID != "codex" || v.Agents[1].Ready || v.Agents[1].SignIn != "login-required" || v.Agents[2].ID != "cursor" || v.Agents[2].Readiness != "missing" {
		t.Fatalf("agents %+v", v.Agents)
	}
	if !strings.Contains(v.Agents[1].Detail, "not logged in") || !strings.Contains(v.Agents[2].Detail, "Not found") {
		t.Fatalf("unavailable agents are not explained: %+v", v.Agents)
	}
	// A reload at the agent step stays there.
	if v := setupGet(t, h, lab); v.Step != "agent" {
		t.Fatalf("reload after check: %+v", v)
	}

	code, v, _ = setupPost(t, h, lab, "create", `{"harness":"claude-code"}`)
	if code != http.StatusOK || v.Step != "review" || v.Proposal == nil || v.Proposal.Kind != "bootstrap" || v.Proposal.Resume {
		t.Fatalf("create: %d %+v", code, v)
	}
	p := v.Proposal
	if len(p.Atoms) != 7 || p.Atoms[0].Label != "Read the Issue" || p.Atoms[1].Label != "You approve the plan" || len(p.Checks) != 1 || p.Checks[0].Command != "go test ./..." || p.Checks[0].Dir != "lab" || len(p.Gates) != 2 || p.Gates[0].Mode != "human" || p.Binding != "claude-code" {
		t.Fatalf("proposal %+v", p)
	}
	if !assayID.MatchString(p.ID) || !setupDigest.MatchString(p.Digest) {
		t.Fatalf("proposal identity %q %q", p.ID, p.Digest)
	}
	f.mu.Lock()
	o := f.options[0]
	f.mu.Unlock()
	if o.Preferred != "claude-code" || o.LabDir != lab || o.Cwd != "/src/checkout" || o.Launcher != "/cli" || o.EnableReview || len(o.Candidates) != 2 {
		t.Fatalf("Prepare got %+v", o)
	}

	code, v, _ = setupPost(t, h, lab, "approve", approveBody(p))
	if code != http.StatusOK || v.Step != "done" || v.Active || v.ReviewAtom != "available" {
		t.Fatalf("approve: %d %+v", code, v)
	}
	if prepares, reviews := f.calls(); prepares != 1 || reviews != 1 {
		t.Fatalf("engine calls: %d prepares, %d reviews", prepares, reviews)
	}
	if !NeedsSetup(lab) == false || NeedsSetup(lab) {
		t.Fatal("an approved Lab still needs setup")
	}
	// Once ready, the resource guard is open for this Lab and the Lab is served.
	if len(f.resources) != 1 || f.resources[0] != lab {
		t.Fatalf("resource guard opened for %q", f.resources)
	}
	if w := do(t, h, authed("/api/lab")); w.Code != http.StatusOK {
		t.Fatalf("GET /api/lab after setup: %d", w.Code)
	}
	f.unchanged("after setup")
}

// A reload, or a restart of forgecell ui, at each step resumes from the Lab's
// own state; a half-made Lab is continued and never duplicated.
func TestUISetupResumesFromTheLabsOwnState(t *testing.T) {
	lab := t.TempDir()
	f := newFakeSetup(t, lab)
	h := setupHandler(t, f)
	setupPost(t, h, lab, "check", `{}`)
	_, created, _ := setupPost(t, h, lab, "create", `{"harness":"claude-code"}`)
	if created.Proposal == nil {
		t.Fatalf("create %+v", created)
	}
	// A new launch reads the waiting proposal without checking anything again.
	restarted := setupHandler(t, f)
	v := setupGet(t, restarted, lab)
	if v.Step != "review" || v.Proposal == nil || v.Proposal.ID != created.Proposal.ID || v.Checked {
		t.Fatalf("restart at review: %+v", v)
	}
	if !NeedsSetup(lab) {
		t.Fatal("a Lab with only a waiting proposal does not need setup")
	}
	// Create again (a double click across a reload) continues that proposal.
	code, again, _ := setupPost(t, restarted, lab, "create", `{"harness":"claude-code"}`)
	if code != http.StatusOK || again.Proposal == nil || again.Proposal.ID != created.Proposal.ID {
		t.Fatalf("create after restart: %d %+v", code, again)
	}
	if prepares, _ := f.calls(); prepares != 1 {
		t.Fatalf("a half-made Lab was proposed again: %d prepares", prepares)
	}
	entries, _ := os.ReadDir(filepath.Join(lab, "assays"))
	if len(entries) != 1 {
		t.Fatalf("assays %v", entries)
	}
	if code, _, _ := setupPost(t, restarted, lab, "approve", approveBody(again.Proposal)); code != http.StatusOK {
		t.Fatalf("approve after restart: %d", code)
	}
	// After approval a new launch finds the Lab done, and the Lab opens.
	done := setupGet(t, setupHandler(t, f), lab)
	if done.Step != "done" || done.Active {
		t.Fatalf("restart after approval: %+v", done)
	}
	// Approving twice is refused as stale: nothing waits any more.
	if code, _, refusal := setupPost(t, restarted, lab, "approve", approveBody(again.Proposal)); code != http.StatusConflict || refusal.Category != "stale" {
		t.Fatalf("second approval: %d %+v", code, refusal)
	}
	if _, reviews := f.calls(); reviews != 1 {
		t.Fatalf("reviews %d", reviews)
	}
}

// A second submit while one runs is refused and never reaches the engine.
func TestUISetupDoubleSubmitRunsOneEngineCall(t *testing.T) {
	lab := t.TempDir()
	f := newFakeSetup(t, lab)
	h := setupHandler(t, f)
	setupPost(t, h, lab, "check", `{}`)
	f.entered, f.release = make(chan struct{}), make(chan struct{})
	done := make(chan int, 1)
	go func() {
		w := do(t, h, gatePost("/api/setup/create", `{"harness":"claude-code"}`))
		done <- w.Code
	}()
	<-f.entered
	for _, action := range []string{"create", "check", "approve", "review"} {
		body := `{}`
		switch action {
		case "create":
			body = `{"harness":"claude-code"}`
		case "approve":
			body = `{"proposal":"assay-` + strings.Repeat("a", 64) + `","digest":"` + strings.Repeat("b", 64) + `"}`
		}
		if code, _, refusal := setupPost(t, h, lab, action, body); code != http.StatusConflict || refusal.Category != "busy" {
			t.Errorf("%s while create runs: %d %+v", action, code, refusal)
		}
	}
	if v := setupGet(t, h, lab); !v.Busy {
		t.Errorf("the view does not report the running step: %+v", v)
	}
	close(f.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first create: %d", code)
	}
	if prepares, reviews := f.calls(); prepares != 1 || reviews != 0 {
		t.Fatalf("engine calls: %d prepares, %d reviews", prepares, reviews)
	}
	f.mu.Lock()
	reads := f.reads
	f.mu.Unlock()
	if reads != 1 {
		t.Fatalf("the refused check read the repository: %d reads", reads)
	}
}

// Approval binds the shown proposal: a changed digest, another proposal id or a
// proposal replaced after it was shown approves nothing and never reaches Review.
func TestUISetupApprovalBindsTheShownDigest(t *testing.T) {
	lab := t.TempDir()
	f := newFakeSetup(t, lab)
	h := setupHandler(t, f)
	setupPost(t, h, lab, "check", `{}`)
	_, v, _ := setupPost(t, h, lab, "create", `{"harness":"claude-code"}`)
	p := *v.Proposal
	for name, body := range map[string]string{
		"other digest":   `{"proposal":"` + p.ID + `","digest":"` + strings.Repeat("0", 64) + `"}`,
		"other proposal": `{"proposal":"assay-` + strings.Repeat("0", 64) + `","digest":"` + p.Digest + `"}`,
	} {
		if code, _, refusal := setupPost(t, h, lab, "approve", body); code != http.StatusConflict || refusal.Category != "stale" {
			t.Errorf("%s: %d %+v", name, code, refusal)
		}
	}
	for name, body := range map[string]string{
		"no digest":       `{"proposal":"` + p.ID + `"}`,
		"short digest":    `{"proposal":"` + p.ID + `","digest":"abc"}`,
		"path as id":      `{"proposal":"../lab.json","digest":"` + p.Digest + `"}`,
		"extra field":     `{"proposal":"` + p.ID + `","digest":"` + p.Digest + `","force":true}`,
		"two objects":     approveBody(&p) + approveBody(&p),
		"upper-case hash": `{"proposal":"` + p.ID + `","digest":"` + strings.ToUpper(p.Digest) + `"}`,
	} {
		if code, _, refusal := setupPost(t, h, lab, "approve", body); code != http.StatusBadRequest || refusal.Category != "invalid" {
			t.Errorf("%s: %d %+v", name, code, refusal)
		}
	}
	// The proposal shown is dismissed through the engine and another one saved:
	// the old id and digest no longer approve anything.
	if _, err := onboarding.Review(lab, p.ID, "dismiss"); err != nil {
		t.Fatal(err)
	}
	f.last = snapshot(t, lab)
	if code, _, refusal := setupPost(t, h, lab, "approve", approveBody(&p)); code != http.StatusConflict || refusal.Category != "stale" {
		t.Fatalf("dismissed proposal: %d %+v", code, refusal)
	}
	if _, reviews := f.calls(); reviews != 0 {
		t.Fatalf("a stale approval reached Review %d times", reviews)
	}
	if NeedsSetup(lab) == false {
		t.Fatal("a Lab without lab.json does not need setup")
	}
	f.unchanged("after refusals")
}

// Each step's failure path is refused by category with a fixed next step; engine
// text, which names local paths, is never served, and nothing is written.
func TestUISetupFailurePathsAreRefusedWithoutWrites(t *testing.T) {
	lab := t.TempDir()
	f := newFakeSetup(t, lab)
	f.repoErr = errors.New("repository discovery failed for gh: exit 1 " + localLeak)
	h := setupHandler(t, f)

	_, v, _ := setupPost(t, h, lab, "check", `{}`)
	if v.Step != "repository" || v.Repository == nil || v.Repository.OK || v.Repository.GitHub != "sign-in-needed" || !strings.Contains(v.Repository.Message, "gh auth login") {
		t.Fatalf("gh failure: %+v %+v", v, v.Repository)
	}
	if code, _, refusal := setupPost(t, h, lab, "create", `{"harness":"claude-code"}`); code != http.StatusConflict || refusal.Category != "repository" {
		t.Fatalf("create before the repository reads: %d %+v", code, refusal)
	}
	for text, want := range map[string]string{
		"repository discovery failed for git: not a repository": "not a git checkout",
		"origin must identify a GitHub repository":              "not a GitHub repository",
		"something else at " + localLeak:                        "could not be read",
	} {
		if got := repositoryView(onboarding.Repository{}, errors.New(text)); got.OK || !strings.Contains(got.Message, want) || strings.Contains(got.Message, localLeak) {
			t.Errorf("%q: %+v", text, got)
		}
	}

	f.repoErr = nil
	setupPost(t, h, lab, "check", `{}`)
	for body, category := range map[string]string{
		`{"harness":"codex"}`:       "agent",
		`{"harness":"cursor"}`:      "agent",
		`{"harness":"../evil"}`:     "invalid",
		`{"harness":""}`:            "invalid",
		`{"harness":"codex","x":1}`: "invalid",
	} {
		code, _, refusal := setupPost(t, h, lab, "create", body)
		if refusal.Category != category || code == http.StatusOK {
			t.Errorf("create %s: %d %+v", body, code, refusal)
		}
	}
	f.prepareErr = errors.New("approved Formula belongs to another repository at " + localLeak)
	code, _, refusal := setupPost(t, h, lab, "create", `{"harness":"claude-code"}`)
	if code != http.StatusUnprocessableEntity || refusal.Category != "refused" || !strings.Contains(refusal.Reason, "another repository") {
		t.Fatalf("Prepare failure: %d %+v", code, refusal)
	}
	if code, _, refusal := setupPost(t, h, lab, "review", `{}`); code != http.StatusConflict || refusal.Category != "approve" {
		t.Fatalf("review Atom before approval: %d %+v", code, refusal)
	}

	f.prepareErr = nil
	_, v, _ = setupPost(t, h, lab, "create", `{"harness":"claude-code"}`)
	f.reviewErr = errors.New("Formula changed since proposal; stale approval refused at " + localLeak)
	code, _, refusal = setupPost(t, h, lab, "approve", approveBody(v.Proposal))
	if code != http.StatusUnprocessableEntity || refusal.Category != "refused" || !strings.Contains(refusal.Reason, "Check Again") {
		t.Fatalf("Review failure: %d %+v", code, refusal)
	}
	if v := setupGet(t, h, lab); v.Step != "review" {
		t.Fatalf("after a refused approval: %+v", v)
	}

	// Unsafe Lab state fails closed: a linked assays directory is a problem, not
	// an empty Lab.
	linked := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(linked, "assays")); err != nil {
		t.Fatal(err)
	}
	lf := newFakeSetup(t, linked)
	lh := setupHandler(t, lf)
	if v := setupGet(t, lh, linked); v.Step != "problem" || v.Problem == "" {
		t.Fatalf("linked assays: %+v", v)
	}
	setupPost(t, lh, linked, "check", `{}`)
	if code, _, refusal := setupPost(t, lh, linked, "create", `{"harness":"claude-code"}`); code != http.StatusUnprocessableEntity || refusal.Category != "lab" {
		t.Fatalf("create in a linked Lab: %d %+v", code, refusal)
	}
	if prepares, _ := lf.calls(); prepares != 0 {
		t.Fatal("an unsafe Lab reached Prepare")
	}
	f.unchanged("after failures")
}

// The review Atom is never approved implicitly: it is a second proposal on the
// approved Formula with its own Approve Formula click and digest.
func TestUISetupReviewAtomIsASecondApproval(t *testing.T) {
	lab := t.TempDir()
	f := newFakeSetup(t, lab)
	h := setupHandler(t, f)
	setupPost(t, h, lab, "check", `{}`)
	_, v, _ := setupPost(t, h, lab, "create", `{"harness":"claude-code"}`)
	bootstrap := v.Proposal
	setupPost(t, h, lab, "approve", approveBody(bootstrap))

	code, v, _ := setupPost(t, h, lab, "review", `{}`)
	if code != http.StatusOK || v.Step != "review" || v.Proposal == nil || v.Proposal.Kind != "enable-review" || v.Proposal.ID == bootstrap.ID || v.Proposal.Digest == bootstrap.Digest {
		t.Fatalf("review Atom proposal: %d %+v", code, v)
	}
	f.mu.Lock()
	last := f.options[len(f.options)-1]
	f.mu.Unlock()
	if !last.EnableReview || last.Preferred != "" || last.LabDir != lab {
		t.Fatalf("Prepare got %+v", last)
	}
	if code, _, refusal := setupPost(t, h, lab, "review", `{}`); code != http.StatusConflict || refusal.Category != "pending" {
		t.Fatalf("a second review proposal: %d %+v", code, refusal)
	}
	if _, reviews := f.calls(); reviews != 1 {
		t.Fatalf("the review Atom was approved without a click: %d reviews", reviews)
	}
	code, v, _ = setupPost(t, h, lab, "approve", approveBody(v.Proposal))
	if code != http.StatusOK || v.Step != "done" || v.ReviewAtom != "present" {
		t.Fatalf("approve review Atom: %d %+v", code, v)
	}
	if code, _, refusal := setupPost(t, h, lab, "review", `{}`); code != http.StatusConflict || refusal.Category != "present" {
		t.Fatalf("review Atom twice: %d %+v", code, refusal)
	}
}

// With several Labs for the repository the operator chooses one, by index;
// only directory names are shown, and nothing runs before the choice.
func TestUISetupChoosesAmongSeveralLabs(t *testing.T) {
	home := t.TempDir()
	first, second := filepath.Join(home, "widgets-a"), filepath.Join(home, "widgets-b")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	f := newFakeSetup(t, second)
	o := f.options_()
	o.Labs = []string{first, second}
	h := testHandler(t, Options{LabDir: "", SourceRoot: "/src/checkout", Setup: o})
	v := setupGet(t, h, home)
	if v.Step != "choose" || len(v.Labs) != 2 || v.Labs[0].Name != "widgets-a" || v.Labs[1].Name != "widgets-b" || v.Labs[1].Index != 1 {
		t.Fatalf("chooser %+v", v)
	}
	for action, body := range map[string]string{"create": `{"harness":"claude-code"}`, "approve": `{"proposal":"assay-` + strings.Repeat("a", 64) + `","digest":"` + strings.Repeat("b", 64) + `"}`, "review": `{}`} {
		if code, _, refusal := setupPost(t, h, home, action, body); code != http.StatusConflict || refusal.Category != "choose" {
			t.Errorf("%s before choosing: %d %+v", action, code, refusal)
		}
	}
	for _, body := range []string{`{"lab":2}`, `{"lab":-1}`, `{"lab":"` + first + `"}`, `{"path":"/"}`} {
		if code, _, _ := setupPost(t, h, home, "choose", body); code != http.StatusBadRequest {
			t.Errorf("choose %s: %d", body, code)
		}
	}
	code, v, _ := setupPost(t, h, home, "choose", `{"lab":1}`)
	if code != http.StatusOK || v.Step != "repository" || v.Lab != "widgets-b" {
		t.Fatalf("chosen: %d %+v", code, v)
	}
	if code, _, refusal := setupPost(t, h, home, "choose", `{"lab":0}`); code != http.StatusConflict || refusal.Category != "chosen" {
		t.Fatalf("choosing again: %d %+v", code, refusal)
	}
	setupPost(t, h, home, "check", `{}`)
	_, v, _ = setupPost(t, h, home, "create", `{"harness":"claude-code"}`)
	f.mu.Lock()
	got := f.options[0].LabDir
	f.mu.Unlock()
	if got != second || v.Step != "review" {
		t.Fatalf("created in %q: %+v", got, v)
	}
	if entries, _ := os.ReadDir(first); len(entries) != 0 {
		t.Fatalf("the Lab not chosen was written: %v", entries)
	}
}

// Setup POSTs pass the same write protection as the gates, never reach the
// engine when forged, are POST-only, and are refused without setup mode.
func TestUISetupRoutesNeedTheWriteProtection(t *testing.T) {
	lab := t.TempDir()
	f := newFakeSetup(t, lab)
	h := setupHandler(t, f)
	for action, body := range map[string]string{"check": `{}`, "choose": `{"lab":0}`, "create": `{"harness":"claude-code"}`, "approve": `{"proposal":"assay-` + strings.Repeat("a", 64) + `","digest":"` + strings.Repeat("b", 64) + `"}`, "review": `{}`} {
		path := "/api/setup/" + action
		for name, forged := range forgedWrites(path, body) {
			if w := do(t, h, forged); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d", name, action, w.Code)
			}
		}
		for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
			r := authed(path)
			r.method = method
			if w := do(t, h, r); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
				t.Errorf("%s %s: %d", method, path, w.Code)
			}
		}
		if w := do(t, h, authed(path)); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", path, w.Code)
		}
		plain := testHandler(t, Options{LabDir: t.TempDir()})
		if w := do(t, plain, gatePost(path, body)); w.Code != http.StatusNotFound {
			t.Errorf("%s without setup mode: %d", path, w.Code)
		}
	}
	f.mu.Lock()
	n := f.reads + f.discovers + f.prepares + f.reviews
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("refused setup writes reached the engine %d times", n)
	}
	// Without setup mode, or in demo mode, GET /api/setup says so and every
	// route is served as before.
	for _, o := range []Options{{LabDir: t.TempDir()}, {Demo: true}} {
		plain := testHandler(t, o)
		if v := setupGet(t, plain, ""); v.Active || v.Step != "none" {
			t.Errorf("setup view without setup mode: %+v", v)
		}
		if w := do(t, plain, authed("/api/lab")); w.Code != http.StatusOK {
			t.Errorf("/api/lab without setup mode: %d", w.Code)
		}
	}
	f.unchanged("after forged writes")
}

// NeedsSetup opens only a Lab with an approved Formula as before. A Lab with
// no lab.json, or a lab.json and no approved Formula with nothing waiting, is
// set up, so Create can propose the Formula to approve again.
func TestUISetupNeedsSetupFollowsTheLab(t *testing.T) {
	if !NeedsSetup(t.TempDir()) {
		t.Error("an empty Lab directory does not need setup")
	}
	if !NeedsSetup(filepath.Join(t.TempDir(), "missing")) {
		t.Error("a missing Lab directory does not need setup")
	}
	configured := t.TempDir()
	if err := os.WriteFile(filepath.Join(configured, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"gone"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !NeedsSetup(configured) {
		t.Fatal("a Lab with a lab.json but no approved Formula opens without setup")
	}
	// The active Formula is present but has no approval.
	unapproved := t.TempDir()
	if err := os.MkdirAll(filepath.Join(unapproved, "formulas"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unapproved, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"widgets-github-issue"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unapproved, "formulas", "widgets-github-issue.yaml"), []byte(setupFormula), 0600); err != nil {
		t.Fatal(err)
	}
	if !NeedsSetup(unapproved) {
		t.Fatal("a Lab whose Formula has no approval opens without setup")
	}
	// Set up, it reaches Create, and Create proposes a Formula to approve.
	configured = unapproved
	f := newFakeSetup(t, configured)
	h := setupHandler(t, f)
	if v := setupGet(t, h, configured); !v.Active || v.Step != "repository" {
		t.Fatalf("unapproved Lab: %+v", v)
	}
	setupPost(t, h, configured, "check", `{}`)
	if code, v, _ := setupPost(t, h, configured, "create", `{"harness":"claude-code"}`); code != http.StatusOK || v.Step != "review" || v.Proposal == nil {
		t.Fatalf("create in an unapproved Lab: %d %+v", code, v)
	}
	if prepares, _ := f.calls(); prepares != 1 {
		t.Fatalf("prepares %d", prepares)
	}
}

// A Lab whose proposals cannot be read safely needs setup, so the problem
// step explains it instead of the normal UI opening on it.
func TestUISetupNeedsSetupForAnUnreadableLab(t *testing.T) {
	linked := t.TempDir()
	if err := os.WriteFile(filepath.Join(linked, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"gone"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(linked, "assays")); err != nil {
		t.Fatal(err)
	}
	if _, err := readSetupLab(linked); err == nil {
		t.Fatal("a linked assays directory was read")
	}
	if !NeedsSetup(linked) {
		t.Fatal("an unreadable Lab opens without setup")
	}
	f := newFakeSetup(t, linked)
	if v := setupGet(t, setupHandler(t, f), linked); !v.Active || v.Step != "problem" || v.Problem == "" {
		t.Fatalf("unreadable Lab: %+v", v)
	}
	f.unchanged("after the problem step")
}

// Create probes the coding agents again: one that signed out after Check is
// refused with the plain reason, nothing is written, and the page shows the
// fresh probe.
func TestUISetupCreateReverifiesTheCodingAgent(t *testing.T) {
	lab := t.TempDir()
	f := newFakeSetup(t, lab)
	h := setupHandler(t, f)
	if _, v, _ := setupPost(t, h, lab, "check", `{}`); len(v.Agents) == 0 || !v.Agents[0].Ready {
		t.Fatalf("check %+v", v)
	}
	f.mu.Lock()
	f.candidates = []onboarding.Candidate{
		{ID: "claude-code", Executable: localLeak + "/bin/claude", Readiness: "login-required", Detail: "Probe step auth failed at " + localLeak + " — provider reports it is not logged in.", Installed: harness.Capability{State: "verified"}, Authentication: harness.Capability{State: "login-required"}},
	}
	discovers := f.discovers
	f.mu.Unlock()
	code, _, refusal := setupPost(t, h, lab, "create", `{"harness":"claude-code"}`)
	if code != http.StatusConflict || refusal.Category != "agent" || !strings.Contains(refusal.Reason, "Sign in") {
		t.Fatalf("create with a signed-out agent: %d %+v", code, refusal)
	}
	f.mu.Lock()
	again := f.discovers - discovers
	f.mu.Unlock()
	if prepares, _ := f.calls(); prepares != 0 || again != 1 {
		t.Fatalf("create reached Prepare %d times after %d probes", prepares, again)
	}
	if entries, _ := os.ReadDir(lab); len(entries) != 0 {
		t.Fatalf("a refused create wrote %v", entries)
	}
	if v := setupGet(t, h, lab); v.Step != "agent" || len(v.Agents) == 0 || v.Agents[0].Ready || v.Agents[0].SignIn != "login-required" {
		t.Fatalf("the page still shows the stale probe: %+v", v)
	}
	// Signed in again, Create proposes from the fresh probe.
	f.mu.Lock()
	f.candidates = newFakeSetup(t, lab).candidates
	f.mu.Unlock()
	if code, v, _ := setupPost(t, h, lab, "create", `{"harness":"claude-code"}`); code != http.StatusOK || v.Step != "review" {
		t.Fatalf("create after signing in: %d %+v", code, v)
	}
	f.mu.Lock()
	o := f.options[0]
	f.mu.Unlock()
	if len(o.Candidates) != 2 || o.Candidates[0].Readiness != "ready" {
		t.Fatalf("Prepare got stale candidates %+v", o.Candidates)
	}
}

// Choosing a Lab refuses one that became a link or a file since the scan.
func TestUISetupChooseRefusesALinkOrAFile(t *testing.T) {
	home := t.TempDir()
	plain, linked, file := filepath.Join(home, "widgets-a"), filepath.Join(home, "widgets-b"), filepath.Join(home, "widgets-c")
	if err := os.Mkdir(plain, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(plain, linked); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	f := newFakeSetup(t, plain)
	o := f.options_()
	o.Labs = []string{plain, linked, file, filepath.Join(home, "gone")}
	h := testHandler(t, Options{SourceRoot: "/src/checkout", Setup: o})
	for _, body := range []string{`{"lab":1}`, `{"lab":2}`, `{"lab":3}`} {
		if code, _, refusal := setupPost(t, h, home, "choose", body); code != http.StatusConflict || refusal.Category != "lab" {
			t.Errorf("choose %s: %d %+v", body, code, refusal)
		}
	}
	if v := setupGet(t, h, home); v.Step != "choose" {
		t.Fatalf("a refused choice was kept: %+v", v)
	}
	if code, v, _ := setupPost(t, h, home, "choose", `{"lab":0}`); code != http.StatusOK || v.Lab != "widgets-a" {
		t.Fatalf("choose the directory: %d %+v", code, v)
	}
	f.unchanged("after choosing")
}

// The view's module and stylesheet are served from the allowlist, render text
// only, and the entry mounts the view and loads nothing else while it shows.
func TestUISetupViewIsServedAndRendersTextOnly(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	js := do(t, h, authed("/js/setup.js"))
	css := do(t, h, authed("/css/setup.css"))
	if js.Code != http.StatusOK || css.Code != http.StatusOK || js.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || css.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatalf("setup.js %d, setup.css %d", js.Code, css.Code)
	}
	body := js.Body.String()
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "DOMParser", "eval(", "new Function", "javascript:", "import(", ".append(", "localStorage"} {
		if strings.Contains(body, sink) {
			t.Errorf("setup.js uses %s", sink)
		}
	}
	for _, want := range []string{"export function mount(root, ctx) {", "export function update(state) {", "export async function begin() {", "'Approve Formula'", "'Check Again'", "'Create Lab'", "{ proposal: id, digest: digest }", "autocomplete: 'off'", "'aria-describedby': 'setup-error-' + name", "if (setup.pending) return false;", "history.pushState"} {
		if !strings.Contains(body, want) {
			t.Errorf("setup.js lacks %q", want)
		}
	}
	entry := do(t, h, authed("/app.js")).Body.String()
	for _, want := range []string{"import * as setup from './js/setup.js';", "[setup, $('[data-view-panel=\"setup\"]')],", "if (state.setup) return;", "setup.begin().then("} {
		if !strings.Contains(entry, want) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	page := do(t, h, authed("/")).Body.String()
	for _, want := range []string{`<link rel="stylesheet" href="/css/setup.css">`, `<section id="view-setup" class="view setup" data-view-panel="setup" aria-labelledby="setup-title" hidden>`, `<p id="setup-live" class="sr-only" role="status" aria-live="polite"></p>`} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	sheet := css.Body.String()
	for _, want := range []string{"@media (pointer: coarse)", "@media (prefers-reduced-motion: reduce)", "@media (forced-colors: active)", "touch-action: manipulation", "min-height: 44px", ":focus-visible"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("setup.css lacks %s", want)
		}
	}
}
