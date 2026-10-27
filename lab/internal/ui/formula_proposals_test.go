package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// formulaClock pins the proposal clock, so a UI-created and a CLI-created
// proposal carry the same createdAt and so the same identity.
func formulaClock(t *testing.T) {
	t.Helper()
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	t.Cleanup(onboarding.SetClock(func() time.Time { return at }))
}

func formulaCandidates() []onboarding.Candidate {
	var all []onboarding.Candidate
	for _, id := range []string{"codex", "claude-code"} {
		all = append(all, onboarding.Candidate{ID: id, Executable: "/" + id, Readiness: "ready", Capabilities: harness.AdapterCapabilities(id)})
	}
	return all
}

func formulaWorkflow(run string) string {
	return "on: pull_request\njobs:\n  core:\n    runs-on: ubuntu-latest\n    steps:\n      - run: " + run + "\n        working-directory: service\n"
}

func formulaGit(t *testing.T, root string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = root
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s %v", args, b, err)
	}
}

// formulaCommitWorkflow writes and commits the pull-request workflow.
func formulaCommitWorkflow(t *testing.T, root, run string) {
	t.Helper()
	os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0700)
	if err := os.WriteFile(filepath.Join(root, ".github", "workflows", "ci.yml"), []byte(formulaWorkflow(run)), 0600); err != nil {
		t.Fatal(err)
	}
	formulaGit(t, root, "add", ".")
	formulaGit(t, root, "commit", "-qm", "workflow")
}

// formulaLab is a fixture checkout of acme/widgets (gh stubbed) and a Lab whose
// approved Formula codes with claude-code on launcher and learns with codex.
func formulaLab(t *testing.T, launcher string) (root, lab string, runner onboarding.Runner) {
	t.Helper()
	root, lab = t.TempDir(), t.TempDir()
	formulaGit(t, root, "init", "-q")
	formulaGit(t, root, "config", "user.name", "Test")
	formulaGit(t, root, "config", "user.email", "test@example.test")
	formulaGit(t, root, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	os.Mkdir(filepath.Join(root, "service"), 0700)
	os.WriteFile(filepath.Join(root, "service", "go.mod"), []byte("module example/service\n\ngo 1.27\n"), 0600)
	formulaCommitWorkflow(t, root, "go test ./...")
	runner = func(ctx context.Context, o process.Options) process.Result {
		if o.Argv[0] == "gh" {
			return process.Result{OK: true, Stdout: `{"nameWithOwner":"acme/widgets","defaultBranchRef":{"name":"main"}}`}
		}
		return process.Run(ctx, o)
	}
	boot, err := onboarding.Prepare(context.Background(), onboarding.InitOptions{Cwd: root, LabDir: lab, Launcher: launcher, Candidates: formulaCandidates(), Preferred: "claude-code", MetaHarness: "codex", Runner: runner})
	if err != nil || boot.Proposal == nil {
		t.Fatalf("%+v %v", boot, err)
	}
	if _, err = onboarding.Review(lab, boot.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	return root, lab, runner
}

// formulaHandler serves lab from root with launcher as this build, counting
// engine proposal calls.
func formulaHandler(t *testing.T, root, lab, launcher string, runner onboarding.Runner) (*handler, *int32) {
	t.Helper()
	h := testHandler(t, Options{LabDir: lab, SourceRoot: root, Launcher: launcher}).(*handler)
	h.formulaRunner, h.formulaCandidates = runner, formulaCandidates()
	calls := new(int32)
	prepare := h.prepare
	h.prepare = func(ctx context.Context, o onboarding.InitOptions) (onboarding.Outcome, error) {
		atomic.AddInt32(calls, 1)
		return prepare(ctx, o)
	}
	return h, calls
}

func getFormula(t *testing.T, h http.Handler) FormulaPage {
	t.Helper()
	return viewOf[FormulaPage](t, do(t, h, authed("/api/formula")), http.StatusOK)
}

func option(v FormulaPage, kind string) (CreateOptionView, bool) {
	for _, o := range v.Create {
		if o.Kind == kind {
			return o, true
		}
	}
	return CreateOptionView{}, false
}

// formulaRefusalOf checks a fixed refusal with a reason and no local paths.
func formulaRefusalOf(t *testing.T, w interface {
	Result() *http.Response
}, body string, code int, category string, leaks ...string) gateRefusal {
	t.Helper()
	var v gateRefusal
	if w.Result().StatusCode != code || json.Unmarshal([]byte(body), &v) != nil || v.Category != category || v.Error == "" || v.Reason == "" {
		t.Fatalf("status %d, want %d %s: %s", w.Result().StatusCode, code, category, body)
	}
	noLeak(t, body, leaks...)
	return v
}

// mainView is the view without what only Details shows.
func mainView(t *testing.T, v FormulaPage) string {
	t.Helper()
	v.Active.Details = ActiveFormulaExtra{}
	if v.Pending != nil {
		p := *v.Pending
		p.Details = ProposalDetailView{}
		v.Pending = &p
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func noMainLeak(t *testing.T, v FormulaPage, root, lab string) {
	t.Helper()
	text := mainView(t, v)
	for _, leak := range []string{root, lab, "/cli", "/opt/", "/claude-code", "/codex", "assay-", "kind: formula", "schemaVersion", v.Active.Details.SHA256} {
		if leak != "" && strings.Contains(text, leak) {
			t.Errorf("the main view carries %q: %s", leak, text)
		}
	}
	if v.Pending != nil && (strings.Contains(text, v.Pending.Details.Digest) || strings.Contains(text, v.Pending.Details.ID)) {
		t.Errorf("the main view carries the proposal's full id or digest: %s", text)
	}
}

func TestUIFormulaProposalsMatchTheCLIBytesForEachKind(t *testing.T) {
	formulaClock(t)
	for _, c := range []struct {
		kind, wantKind, body string
		prepare              func(*onboarding.InitOptions)
		before               func(t *testing.T, root string)
	}{
		// /cli and /cli2 are not release launchers, so the rebind needs Rebind Anyway.
		{ProposeRebind, onboarding.KindRebind, `"kind":"rebind","anyway":true`, func(o *onboarding.InitOptions) { o.Rebind = "claude-code" }, nil},
		{ProposeBaseline, onboarding.KindBaseline, `"kind":"baseline"`, func(o *onboarding.InitOptions) { o.RefreshBaseline = true }, func(t *testing.T, root string) { formulaCommitWorkflow(t, root, "go test -timeout 20m ./...") }},
		{ProposeReview, onboarding.KindEnableReview, `"kind":"enable-review"`, func(o *onboarding.InitOptions) { o.EnableReview = true }, nil},
		{ProposeModel, onboarding.KindModelPin, `"kind":"model","model":"claude-opus-4-8"`, func(o *onboarding.InitOptions) { o.Model, o.ModelSet = "claude-opus-4-8", true }, nil},
	} {
		t.Run(c.kind, func(t *testing.T) {
			root, lab, runner := formulaLab(t, "/cli")
			if c.before != nil {
				c.before(t, root)
			}
			h, _ := formulaHandler(t, root, lab, "/cli2", runner)
			v := getFormula(t, h)
			if o, ok := option(v, c.kind); !ok || !o.Available || o.NeedsAnyway != (c.kind == ProposeRebind) {
				t.Fatalf("%s is not offered: %+v", c.kind, v.Create)
			}
			w := do(t, h, gatePost("/api/formula/proposals", `{`+c.body+`,"digest":"`+v.Active.Details.SHA256+`"}`))
			proposed := viewOf[FormulaPage](t, w, http.StatusOK)
			if proposed.Outcome != "proposed" || proposed.Pending == nil || proposed.Pending.Kind != c.wantKind || len(proposed.Pending.Changes) == 0 {
				t.Fatalf("%+v", proposed.Pending)
			}
			noMainLeak(t, proposed, root, lab)
			if o, _ := option(proposed, c.kind); o.Available || o.Reason == "" {
				t.Fatalf("a pending proposal does not block new ones with a reason: %+v", o)
			}
			id := proposed.Pending.Details.ID
			file := filepath.Join(lab, "assays", id+".json")
			ui, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.Remove(file); err != nil {
				t.Fatal(err)
			}
			// The CLI path: forgecell init builds these InitOptions.
			o := onboarding.InitOptions{Cwd: root, LabDir: lab, Launcher: "/cli2", Candidates: formulaCandidates(), Runner: runner, Env: map[string]string{"CLAUDECODE": ""}}
			c.prepare(&o)
			out, err := onboarding.Prepare(context.Background(), o)
			if err != nil || out.Proposal == nil || out.Proposal.ID != id {
				t.Fatalf("CLI path %+v %v, want %s", out.Proposal, err, id)
			}
			cli, err := os.ReadFile(file)
			if err != nil || string(cli) != string(ui) {
				t.Fatalf("the UI saved other bytes than the CLI:\nUI  %s\nCLI %s", ui, cli)
			}
		})
	}
}

func TestUIFormulaDecisionsBindTheExactIdAndDigest(t *testing.T) {
	formulaClock(t)
	root, lab, runner := formulaLab(t, "/cli")
	h, _ := formulaHandler(t, root, lab, "/cli2", runner)
	v := getFormula(t, h)
	active := v.Active.Details.SHA256
	if v.Active.Name == "" || len(v.Active.Atoms) == 0 || v.Active.Coding.Agent != "Claude Code" || v.Active.ApprovedBy != "Not recorded" || v.Active.ApprovedAt == "" {
		t.Fatalf("active %+v", v.Active)
	}
	noMainLeak(t, v, root, lab)

	// A stale active Formula digest is refused before the engine runs.
	before := snapshot(t, lab)
	w := do(t, h, gatePost("/api/formula/proposals", `{"kind":"enable-review","digest":"`+strings.Repeat("0", 64)+`"}`))
	formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "stale", root, lab)
	if snapshot(t, lab) != before {
		t.Fatal("a stale create wrote to the Lab")
	}

	proposed := viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals", `{"kind":"enable-review","digest":"`+active+`"}`)), http.StatusOK)
	p := proposed.Pending
	if p == nil || p.Kind != onboarding.KindEnableReview || !strings.Contains(strings.Join(p.Notes, " "), "Reviewer: Codex") {
		t.Fatalf("%+v", p)
	}
	if len(p.Details.Lines) == 0 || len(p.Details.Keys) == 0 {
		t.Fatal("details lack the exact YAML diff")
	}

	// A pending proposal blocks every new one, with the reason.
	w = do(t, h, gatePost("/api/formula/proposals", `{"kind":"model","model":"claude-opus-4-8","digest":"`+active+`"}`))
	if r := formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "pending", root, lab); !strings.Contains(r.Error, "already pending") {
		t.Fatalf("refusal %+v", r)
	}

	// A mismatched digest or another id is refused and writes nothing.
	before = snapshot(t, lab)
	for name, req := range map[string]request{
		"wrong digest":       gatePost("/api/formula/proposals/"+p.Details.ID+"/approve", `{"digest":"`+strings.Repeat("a", 64)+`"}`),
		"active digest":      gatePost("/api/formula/proposals/"+p.Details.ID+"/approve", `{"digest":"`+active+`"}`),
		"other id":           gatePost("/api/formula/proposals/assay-"+strings.Repeat("b", 64)+"/approve", `{"digest":"`+p.Details.Digest+`"}`),
		"dismiss other id":   gatePost("/api/formula/proposals/assay-"+strings.Repeat("b", 64)+"/dismiss", `{"digest":"`+p.Details.Digest+`"}`),
		"dismiss bad digest": gatePost("/api/formula/proposals/"+p.Details.ID+"/dismiss", `{"digest":"`+strings.Repeat("c", 64)+`"}`),
	} {
		w := do(t, h, req)
		formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "stale", root, lab)
		if snapshot(t, lab) != before {
			t.Fatalf("%s wrote to the Lab", name)
		}
	}

	// Dismiss goes through the engine's flow.
	dismissed := viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals/"+p.Details.ID+"/dismiss", `{"digest":"`+p.Details.Digest+`"}`)), http.StatusOK)
	if dismissed.Outcome != "dismissed" || dismissed.Pending != nil {
		t.Fatalf("%+v", dismissed)
	}
	if saved, err := onboarding.InspectProposal(lab, p.Details.ID); err != nil || saved.Status != "dismissed" {
		t.Fatalf("%+v %v", saved.Status, err)
	}

	// Approve activates exactly the proposal shown.
	proposed = viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals", `{"kind":"model","model":"claude-opus-4-8","digest":"`+active+`"}`)), http.StatusOK)
	p = proposed.Pending
	approved := viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals/"+p.Details.ID+"/approve", `{"digest":"`+p.Details.Digest+`"}`)), http.StatusOK)
	if approved.Outcome != "approved" || approved.Pending != nil || approved.Active.Coding.Model != "claude-opus-4-8" {
		t.Fatalf("%+v", approved)
	}
	saved, err := onboarding.InspectProposal(lab, p.Details.ID)
	live, lerr := formula.Load(lab, "")
	if err != nil || lerr != nil || !live.Approved || live.Formula.YAML != saved.YAML || live.Formula.SHA256 != p.Details.Digest {
		t.Fatalf("approval did not activate the exact proposal: %v %v", err, lerr)
	}
	// The old active digest is now stale for a new proposal.
	w = do(t, h, gatePost("/api/formula/proposals", `{"kind":"baseline","digest":"`+active+`"}`))
	formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "stale", root, lab)
}

func TestUIFormulaEngineRefusalsShowFixedReasons(t *testing.T) {
	formulaClock(t)
	root, lab, runner := formulaLab(t, "/cli")
	h, _ := formulaHandler(t, root, lab, "/cli2", runner)
	active := getFormula(t, h).Active.Details.SHA256
	// The baseline already matches CI: nothing to propose.
	w := do(t, h, gatePost("/api/formula/proposals", `{"kind":"baseline","digest":"`+active+`"}`))
	formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "unchanged", root, lab)
	// The model pin on a role the Formula does not declare.
	w = do(t, h, gatePost("/api/formula/proposals", `{"kind":"model","model":"claude-opus-4-8","role":"review","digest":"`+active+`"}`))
	formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "role-undeclared", root, lab)
	if entries, _ := os.ReadDir(filepath.Join(lab, "assays")); len(entries) == 0 {
		t.Fatal("the fixture's bootstrap proposal is missing")
	}
	if p, err := onboarding.PendingProposal(lab); err != nil || p != nil {
		t.Fatalf("a refused proposal was saved: %+v %v", p, err)
	}
}

func TestUIFormulaDevBuildRebindOrdersBuilds(t *testing.T) {
	formulaClock(t)
	const current = "/opt/fc/releases/0.16.9-dev.0000/forgecell"
	root, lab, runner := formulaLab(t, current)

	// An older build is refused with the plain reason, unless Rebind Anyway is chosen.
	h, calls := formulaHandler(t, root, lab, "/opt/fc/releases/0.16.8/forgecell", runner)
	v := getFormula(t, h)
	o, ok := option(v, ProposeRebind)
	if !ok || !o.Available || !o.NeedsAnyway || !strings.Contains(o.Reason, "older") || o.Build != "0.16.8" {
		t.Fatalf("%+v", o)
	}
	before := snapshot(t, lab)
	w := do(t, h, gatePost("/api/formula/proposals", `{"kind":"rebind","digest":"`+v.Active.Details.SHA256+`"}`))
	if r := formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "older-build", root, lab, "/opt/"); !strings.Contains(r.Error, "older") || !strings.Contains(r.Reason, "Rebind Anyway") {
		t.Fatalf("%+v", r)
	}
	if snapshot(t, lab) != before || atomic.LoadInt32(calls) != 0 {
		t.Fatal("a refused older-build rebind reached the engine")
	}
	anyway := viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals", `{"kind":"rebind","anyway":true,"digest":"`+v.Active.Details.SHA256+`"}`)), http.StatusOK)
	if anyway.Pending == nil || anyway.Pending.Kind != onboarding.KindRebind {
		t.Fatalf("%+v", anyway.Pending)
	}
	noMainLeak(t, anyway, root, lab)
	p := anyway.Pending
	viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals/"+p.Details.ID+"/dismiss", `{"digest":"`+p.Details.Digest+`"}`)), http.StatusOK)

	// Same-version dev builds from other commits are unordered: never an upgrade.
	h, _ = formulaHandler(t, root, lab, "/opt/fc/releases/0.16.9-dev.ffff/forgecell", runner)
	if o, _ := option(getFormula(t, h), ProposeRebind); !o.NeedsAnyway || !strings.Contains(o.Reason, "cannot tell which is newer") {
		t.Fatalf("%+v", o)
	}

	// A newer dev build is rebound directly.
	h, _ = formulaHandler(t, root, lab, "/opt/fc/releases/0.17.0-dev.0000/forgecell", runner)
	h.opts.Home = "/opt/fc"
	v = getFormula(t, h)
	if o, _ := option(v, ProposeRebind); !o.Available || o.NeedsAnyway {
		t.Fatalf("%+v", o)
	}
	newer := viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals", `{"kind":"rebind","digest":"`+v.Active.Details.SHA256+`"}`)), http.StatusOK)
	if newer.Pending == nil || newer.Pending.Kind != onboarding.KindRebind {
		t.Fatalf("%+v", newer.Pending)
	}
	if text := mainView(t, newer); !strings.Contains(text, "Forgecell dev build 0.17.0-dev.0000") || !strings.Contains(text, "Forgecell dev build 0.16.9-dev.0000") {
		t.Fatalf("the readable diff does not name the new build: %s", mainView(t, newer))
	}
	// The --rebase-formula offer follows the engine's own launcher-only verdict.
	saved, err := onboarding.InspectProposal(lab, newer.Pending.Details.ID)
	if err != nil {
		t.Fatal(err)
	}
	prior, _ := onboarding.ProposalBefore(lab, saved)
	if want := formula.LauncherOnlyChange([]byte(prior), []byte(saved.YAML)) == nil; newer.Pending.Rebase != want {
		t.Fatalf("rebase offered %v, engine allows %v", newer.Pending.Rebase, want)
	}
}

func TestUIFormulaLauncherOnlyRebindOffersRebase(t *testing.T) {
	formulaClock(t)
	const current, next = "/opt/fc/releases/0.16.9-dev.0000/forgecell", "/opt/fc/releases/0.17.0-dev.0000/forgecell"
	_, lab, _ := formulaLab(t, current)
	active, err := formula.Load(lab, "")
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.ReplaceAll(active.Formula.YAML, current, next)
	p := onboarding.Proposal{ID: "assay-" + strings.Repeat("d", 64), YAML: yaml, ActiveIDBefore: active.Formula.ID, ActiveHashBefore: active.Formula.SHA256, Status: "pending"}
	v := proposalView(lab, "", p)
	if v.Kind != onboarding.KindRebind || !v.Rebase || len(v.Changes) == 0 {
		t.Fatalf("%+v", v)
	}
	// An older build is never launcher-only: the engine refuses to carry it.
	p.YAML = strings.ReplaceAll(active.Formula.YAML, current, "/opt/fc/releases/0.16.8/forgecell")
	if v = proposalView(lab, "", p); v.Rebase {
		t.Fatalf("rebase offered for an older build: %+v", v)
	}
}

func TestUIFormulaGuardsAndDemoRefuseEveryWrite(t *testing.T) {
	formulaClock(t)
	root, lab, runner := formulaLab(t, "/cli")
	h, calls := formulaHandler(t, root, lab, "/cli2", runner)
	active := getFormula(t, h).Active.Details.SHA256
	id := "assay-" + strings.Repeat("a", 64)
	routes := map[string]string{
		"/api/formula/proposals":                    `{"kind":"enable-review","digest":"` + active + `"}`,
		"/api/formula/proposals/" + id + "/approve": `{"digest":"` + active + `"}`,
		"/api/formula/proposals/" + id + "/dismiss": `{"digest":"` + active + `"}`,
	}
	before := snapshot(t, lab)
	for path, body := range routes {
		for name, forged := range forgedWrites(path, body) {
			if w := do(t, h, forged); w.Code != http.StatusForbidden {
				t.Errorf("%s %s: %d %s", name, path, w.Code, w.Body)
			}
		}
		for name, req := range map[string]request{
			"no content type": func() request { q := gatePost(path, body); delete(q.header, "Content-Type"); return q }(),
			"unknown field":   gatePost(path, strings.TrimSuffix(body, "}")+`,"approve":true}`),
			"trailing data":   gatePost(path, body+`{}`),
			"oversized":       gatePost(path, strings.TrimSuffix(body, "}")+strings.Repeat(" ", maxGateBody)+`}`),
			"array":           gatePost(path, `[]`),
		} {
			if w := do(t, h, req); w.Code != http.StatusBadRequest {
				t.Errorf("%s %s: %d %s", name, path, w.Code, w.Body)
			}
		}
		if w := do(t, h, request{method: http.MethodPut, path: path, cookie: authed(path).cookie, origin: testOrigin}); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("PUT %s: %d", path, w.Code)
		}
	}
	for _, body := range []string{`{"kind":"other","digest":"` + active + `"}`, `{"kind":"model","digest":"` + active + `"}`, `{"kind":"model","model":"a b","digest":"` + active + `"}`, `{"kind":"model","model":"claude-opus-4-8","role":"owner","digest":"` + active + `"}`, `{"kind":"baseline","model":"x","digest":"` + active + `"}`, `{"kind":"baseline","anyway":true,"digest":"` + active + `"}`, `{"kind":"baseline","digest":"short"}`} {
		if w := do(t, h, gatePost("/api/formula/proposals", body)); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if w := do(t, h, gatePost("/api/formula/proposals/bad/approve", `{"digest":"`+active+`"}`)); w.Code != http.StatusBadRequest {
		t.Errorf("bad id: %d", w.Code)
	}
	if n := atomic.LoadInt32(calls); n != 0 {
		t.Fatalf("refused writes reached the engine %d times", n)
	}
	if snapshot(t, lab) != before {
		t.Fatal("a refused write changed the Lab")
	}

	// Without a source checkout nothing is proposed.
	bare := testHandler(t, Options{LabDir: lab, Launcher: "/cli2"}).(*handler)
	if v := getFormula(t, bare); v.Writable || v.Reason == "" {
		t.Fatalf("%+v", v)
	}
	w := do(t, bare, gatePost("/api/formula/proposals", routes["/api/formula/proposals"]))
	formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "unavailable", root, lab)

	// Demo mode refuses all of it and reads no Lab.
	demo := testHandler(t, Options{Demo: true})
	v := getFormula(t, demo)
	if !v.Sample || v.Writable || v.Pending != nil {
		t.Fatalf("%+v", v)
	}
	for _, o := range v.Create {
		if o.Available {
			t.Fatalf("sample data offers %s", o.Kind)
		}
	}
	for path, body := range routes {
		w := do(t, demo, gatePost(path, body))
		formulaRefusalOf(t, w, w.Body.String(), http.StatusNotFound, "sample")
	}
}

func TestUIFormulaUnrelatedOrUnknownLauncherNeedsAnyway(t *testing.T) {
	formulaClock(t)
	root, lab, runner := formulaLab(t, "/opt/fc/releases/0.16.9/forgecell")

	// A build outside the Formula launcher's installation cannot be ordered: it
	// is offered only as Rebind Anyway, bound to the digest shown.
	h, calls := formulaHandler(t, root, lab, "/elsewhere/forgecell", runner)
	v := getFormula(t, h)
	o, ok := option(v, ProposeRebind)
	if !ok || !o.Available || !o.NeedsAnyway || !strings.Contains(o.Reason, "cannot order them") {
		t.Fatalf("%+v", o)
	}
	before := snapshot(t, lab)
	w := do(t, h, gatePost("/api/formula/proposals", `{"kind":"rebind","digest":"`+v.Active.Details.SHA256+`"}`))
	formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "unordered-build", root, lab, "/opt/", "/elsewhere")
	if snapshot(t, lab) != before || atomic.LoadInt32(calls) != 0 {
		t.Fatal("an unrelated rebind without Rebind Anyway reached the engine")
	}
	w = do(t, h, gatePost("/api/formula/proposals", `{"kind":"rebind","anyway":true,"digest":"`+strings.Repeat("e", 64)+`"}`))
	formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "stale", root, lab)
	anyway := viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals", `{"kind":"rebind","anyway":true,"digest":"`+v.Active.Details.SHA256+`"}`)), http.StatusOK)
	if anyway.Pending == nil || anyway.Pending.Kind != onboarding.KindRebind || !strings.Contains(mainView(t, anyway), "a Forgecell build outside the managed tree") {
		t.Fatalf("%s", mainView(t, anyway))
	}
	noMainLeak(t, anyway, root, lab)
	p := anyway.Pending
	viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals/"+p.Details.ID+"/dismiss", `{"digest":"`+p.Details.Digest+`"}`)), http.StatusOK)

	// A launcher ValidateLauncher refused: the view says so and offers no rebind.
	h, calls = formulaHandler(t, root, lab, "", runner)
	v = getFormula(t, h)
	if o, ok := option(v, ProposeRebind); !ok || o.Available || !strings.Contains(o.Reason, "launcher is unknown") {
		t.Fatalf("%+v", o)
	}
	w = do(t, h, gatePost("/api/formula/proposals", `{"kind":"rebind","anyway":true,"digest":"`+v.Active.Details.SHA256+`"}`))
	formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "rebind-unavailable", root, lab)
	if atomic.LoadInt32(calls) != 0 {
		t.Fatal("a rebind with an unknown launcher reached the engine")
	}
}

func TestUIFormulaValidateLauncherAcceptsOnlyOwnedManagedBuilds(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	exe := func(path string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	link := func(path, target string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		return path
	}
	release := exe(filepath.Join(home, "releases", "0.17.0", "forgecell"))
	dev := exe(filepath.Join(home, "development", "build", "forgecell"))
	foreign := exe(filepath.Join(outside, "forgecell"))
	if err := os.MkdirAll(filepath.Join(home, "releases", "0.17.1", "forgecell"), 0700); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(home, "releases", "0.17.2", "forgecell")
	exe(plain)
	if err := os.Chmod(plain, 0600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		home, candidate string
		ok              bool
	}{
		"managed release":                {home, release, true},
		"managed dev build":              {home, dev, true},
		"link into the managed tree":     {home, link(filepath.Join(outside, "into"), release), true},
		"outside the managed tree":       {home, foreign, false},
		"link out of the managed tree":   {home, link(filepath.Join(home, "releases", "0.17.3", "forgecell"), foreign), false},
		"missing":                        {home, filepath.Join(home, "releases", "0.9.9", "forgecell"), false},
		"directory":                      {home, filepath.Join(home, "releases", "0.17.1", "forgecell"), false},
		"not executable":                 {home, plain, false},
		"relative":                       {home, "releases/0.17.0/forgecell", false},
		"traversal out of the tree":      {home, filepath.Join(home, "releases") + "/../../" + filepath.Base(outside) + "/forgecell", false},
		"no home":                        {"", release, false},
		"another home":                   {outside, release, false},
		"the releases directory itself":  {home, filepath.Join(home, "releases"), false},
		"empty":                          {home, "", false},
		"home file outside both trees":   {home, exe(filepath.Join(home, "bin", "forgecell")), false},
		"a symlinked home is resolved":   {link(filepath.Join(outside, "home-link"), home), release, true},
		"oversized candidate":            {home, "/" + strings.Repeat("a", 5000), false},
		"dev build under releases (dev)": {home, exe(filepath.Join(home, "releases", "0.17.0-dev.abcd", "forgecell")), true},
	} {
		got := ValidateLauncher(c.home, c.candidate)
		if c.ok && got != c.candidate || !c.ok && got != "" {
			t.Errorf("%s: ValidateLauncher = %q, want ok=%v", name, got, c.ok)
		}
	}
}

func TestUIFormulaReadableDiffNamesTheBuild(t *testing.T) {
	const home = "/h/.forgecell"
	for launcher, want := range map[string]string{
		home + "/releases/0.17.0/forgecell":          "Forgecell release 0.17.0",
		home + "/releases/0.17.0-dev.abcd/forgecell": "Forgecell dev build 0.17.0-dev.abcd",
		home + "/development/x/forgecell":            "a Forgecell dev build",
		"/opt/fc/releases/0.17.0/forgecell":          "Forgecell 0.17.0, outside the managed tree",
		"/usr/local/bin/forgecell":                   "a Forgecell build outside the managed tree",
		home + "/releases/../../x/forgecell":         "a Forgecell build outside the managed tree",
	} {
		if got := buildWord(home, launcher); got != want {
			t.Errorf("buildWord(%s) = %q, want %q", launcher, got, want)
		}
		raw, _ := json.Marshal([]string{launcher, "__adapter", "claude-code", "/bin/claude"})
		changes := readableChanges(home, []onboarding.KeyChange{{Key: "harness.command", Change: "changed", Old: string(raw), New: string(raw)}})
		if len(changes) != 1 || changes[0].New != want+" · Claude Code" || strings.Contains(changes[0].New, "/") {
			t.Errorf("%s: %+v", launcher, changes)
		}
	}
	if got := buildWord("", home+"/releases/0.17.0/forgecell"); got != "Forgecell 0.17.0, outside the managed tree" {
		t.Errorf("without a home every build is outside the managed tree: %q", got)
	}
}

func TestUIFormulaCreateStaysBoundToTheShownFormula(t *testing.T) {
	formulaClock(t)
	root, lab, runner := formulaLab(t, "/cli")
	h, _ := formulaHandler(t, root, lab, "/cli2", runner)
	shown := getFormula(t, h).Active.Details.SHA256
	prepare := h.prepare
	var cliID string
	h.prepare = func(ctx context.Context, o onboarding.InitOptions) (onboarding.Outcome, error) {
		// forgecell init --model … then --approve lands between this view's check
		// and the engine's own read of the active Formula.
		cli := o
		cli.EnableReview, cli.Model, cli.ModelSet = false, "claude-opus-4-8", true
		out, err := onboarding.Prepare(ctx, cli)
		if err != nil || out.Proposal == nil {
			t.Fatalf("CLI proposal %+v %v", out, err)
		}
		if _, err = onboarding.Review(lab, out.Proposal.ID, "approve"); err != nil {
			t.Fatal(err)
		}
		cliID = out.Proposal.ID
		return prepare(ctx, o)
	}
	w := do(t, h, gatePost("/api/formula/proposals", `{"kind":"enable-review","digest":"`+shown+`"}`))
	if r := formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "stale", root, lab); !strings.Contains(r.Error, "dismissed") {
		t.Fatalf("%+v", r)
	}
	h.prepare = prepare
	if p, err := onboarding.PendingProposal(lab); err != nil || p != nil {
		t.Fatalf("the proposal made from another Formula stays pending: %+v %v", p, err)
	}
	live, err := formula.Load(lab, "")
	if err != nil || !live.Approved || live.Formula.SHA256 == shown {
		t.Fatalf("the CLI's approval does not stand: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(lab, "assays"))
	if err != nil {
		t.Fatal(err)
	}
	dismissed := 0
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".json")
		if !assayID.MatchString(id) || id == cliID {
			continue
		}
		if p, err := onboarding.InspectProposal(lab, id); err == nil && p.Status == "dismissed" && p.ActiveHashBefore == live.Formula.SHA256 {
			dismissed++
		}
	}
	if dismissed != 1 {
		t.Fatalf("want the one stale proposal dismissed, found %d", dismissed)
	}

	// A CLI approval between showing a proposal and clicking it is reported as
	// already decided, never as this view's approval.
	v := getFormula(t, h)
	proposed := viewOf[FormulaPage](t, do(t, h, gatePost("/api/formula/proposals", `{"kind":"model","model":"claude-opus-4-9","digest":"`+v.Active.Details.SHA256+`"}`)), http.StatusOK)
	p := proposed.Pending
	if p == nil {
		t.Fatal("no proposal")
	}
	if _, err = onboarding.Review(lab, p.Details.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, lab)
	for _, decision := range []string{"approve", "dismiss"} {
		w = do(t, h, gatePost("/api/formula/proposals/"+p.Details.ID+"/"+decision, `{"digest":"`+p.Details.Digest+`"}`))
		if r := formulaRefusalOf(t, w, w.Body.String(), http.StatusConflict, "reviewed", root, lab); !strings.Contains(r.Error, "already decided outside this view") || strings.Contains(w.Body.String(), `"outcome"`) {
			t.Fatalf("%s: %s", decision, w.Body.String())
		}
	}
	if snapshot(t, lab) != before {
		t.Fatal("an already decided proposal was written again")
	}
}

func TestUIServedJSFormulaPage(t *testing.T) {
	js := servedJS(t)
	for _, want := range []string{
		"'/api/formula'", "'/api/formula/proposals'", "/approve', { digest: digest }", "/dismiss', { digest: digest }",
		"name: 'model', maxlength: '200', autocomplete: 'off', spellcheck: 'false', placeholder: 'claude-opus-4-8…'",
		"'Rebind to This Build'", "'Refresh Baseline from CI'", "'Enable In-Loop Review'", "'Rebind Anyway'", "'Approve Proposal'",
		"el('details', { class: 'formula-details' }",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the Formula view lacks %q", want)
		}
	}
	page := do(t, testHandler(t, Options{LabDir: t.TempDir()}), authed("/")).Body.String()
	for _, want := range []string{`id="formula-error" class="formula-error" role="alert"`, `id="formula-live" class="sr-only" role="status" aria-live="polite"`, `id="formula-suggestions"`} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
}
