package ui

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
)

// plannerCandidate is a probed coding agent with the given readiness.
func plannerCandidate(id, readiness string) onboarding.Candidate {
	auth := "verified"
	if readiness != "ready" {
		auth = readiness
	}
	return onboarding.Candidate{ID: id, Executable: localLeak + "/bin/" + id, Readiness: readiness, Detail: id + " probe at " + localLeak + " is " + readiness + ".", Installed: harness.Capability{State: "verified"}, Authentication: harness.Capability{State: auth}}
}

// plannerFormula is the bootstrap Formula the fake Prepare proposes for o: the
// shared one, recoded with o.Preferred, and with harnesses.analysis on the
// chosen planner (or Claude Code, the engine's default) for Cursor.
func plannerFormula(o onboarding.InitOptions) string {
	coding := o.Preferred
	yaml := strings.ReplaceAll(setupFormula, "claude-code", coding)
	if coding != "cursor" {
		return yaml
	}
	planner := o.Planner
	if planner == "" {
		planner = "claude-code"
	}
	return strings.Replace(yaml, "atoms:\n", "harnesses:\n  analysis: {binding: "+planner+", command: [/cli, __adapter, "+planner+", /"+planner+"]}\natoms:\n", 1)
}

// plannerHandler serves setup with the fakeSetup seams, except that Prepare
// proposes plannerFormula, so the proposal shows the bindings it was given.
func plannerHandler(t *testing.T, f *fakeSetup) http.Handler {
	t.Helper()
	o := f.options_()
	o.Prepare = func(_ context.Context, io onboarding.InitOptions) (onboarding.Outcome, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unchanged("before Prepare")
		defer func() { f.last = snapshot(f.t, f.lab) }()
		f.prepares++
		f.options = append(f.options, io)
		p, err := onboarding.SaveProposal(io.LabDir, plannerFormula(io))
		if err != nil {
			f.t.Fatal(err)
		}
		return onboarding.Outcome{Status: "pending", Proposal: &p}, nil
	}
	return testHandler(t, Options{LabDir: f.lab, SourceRoot: "/src/checkout", Setup: o})
}

func (f *fakeSetup) lastOptions(t *testing.T) onboarding.InitOptions {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.options) == 0 {
		t.Fatal("Prepare was not called")
	}
	return f.options[len(f.options)-1]
}

// The agent step marks which agents can plan, says why Cursor codes only, and
// serves the engine's default planner: Claude Code, then Codex, else none.
func TestUISetupPlannerDefaultFollowsTheEngine(t *testing.T) {
	for name, tc := range map[string]struct {
		candidates []onboarding.Candidate
		want       string
	}{
		"claude code first": {[]onboarding.Candidate{plannerCandidate("codex", "ready"), plannerCandidate("claude-code", "ready"), plannerCandidate("cursor", "ready")}, "claude-code"},
		"codex second":      {[]onboarding.Candidate{plannerCandidate("claude-code", "login-required"), plannerCandidate("codex", "ready"), plannerCandidate("cursor", "ready")}, "codex"},
		"none ready":        {[]onboarding.Candidate{plannerCandidate("claude-code", "login-required"), plannerCandidate("cursor", "ready")}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			lab := t.TempDir()
			f := newFakeSetup(t, lab)
			f.candidates = tc.candidates
			h := plannerHandler(t, f)
			_, v, _ := setupPost(t, h, lab, "check", `{}`)
			if v.Planner != tc.want {
				t.Fatalf("default planner %q, want %q", v.Planner, tc.want)
			}
			if len(v.Agents) != 3 || !v.Agents[0].Plans || !v.Agents[1].Plans || v.Agents[2].ID != "cursor" || v.Agents[2].Plans {
				t.Fatalf("agents %+v", v.Agents)
			}
			if v.Agents[0].PlanReason != "" || v.Agents[2].PlanReason == "" || strings.HasSuffix(v.Agents[2].PlanReason, ".") {
				t.Fatalf("plan reasons %q %q", v.Agents[0].PlanReason, v.Agents[2].PlanReason)
			}
			if v := setupGet(t, h, lab); v.Planner != tc.want {
				t.Fatalf("reload default planner %q", v.Planner)
			}
			// Without a choice, Create leaves the planner to the engine's default.
			if tc.want == "" {
				return
			}
			code, v, _ := setupPost(t, h, lab, "create", `{"harness":"cursor"}`)
			if code != http.StatusOK || v.Proposal == nil {
				t.Fatalf("create: %d %+v", code, v)
			}
			if o := f.lastOptions(t); o.Preferred != "cursor" || o.Planner != "" {
				t.Fatalf("Prepare got %+v", o)
			}
		})
	}
}

// An explicit Claude Code or Codex planner reaches Prepare, and the review step
// shows both bindings of the proposal: plans with it, codes with Cursor.
func TestUISetupPlannerExplicitChoiceReachesPrepare(t *testing.T) {
	for _, planner := range []string{"claude-code", "codex"} {
		t.Run(planner, func(t *testing.T) {
			lab := t.TempDir()
			f := newFakeSetup(t, lab)
			f.candidates = []onboarding.Candidate{plannerCandidate("claude-code", "ready"), plannerCandidate("codex", "ready"), plannerCandidate("cursor", "ready")}
			h := plannerHandler(t, f)
			setupPost(t, h, lab, "check", `{}`)
			code, v, refusal := setupPost(t, h, lab, "create", `{"harness":"cursor","planner":"`+planner+`"}`)
			if code != http.StatusOK || v.Step != "review" || v.Proposal == nil {
				t.Fatalf("create: %d %+v %+v", code, v, refusal)
			}
			o := f.lastOptions(t)
			if o.Preferred != "cursor" || o.Planner != planner || o.LabDir != lab || o.Launcher != "/cli" || len(o.Candidates) != 3 {
				t.Fatalf("Prepare got %+v", o)
			}
			if p := v.Proposal; p.Planning != planner || p.Coding != "cursor" || p.Binding != "cursor" {
				t.Fatalf("review bindings: planning %q, coding %q, binding %q", p.Planning, p.Coding, p.Binding)
			}
			// A reload shows the same bindings, read from the saved proposal.
			if again := setupGet(t, h, lab); again.Proposal == nil || again.Proposal.Planning != planner || again.Proposal.Coding != "cursor" || again.Proposal.Digest != v.Proposal.Digest {
				t.Fatalf("reload %+v", again.Proposal)
			}
		})
	}
}

// A planner that cannot plan, is not ready or is not offered is refused before
// Prepare with the adapter's reason or a fixed next step; nothing is written.
func TestUISetupPlannerRefusesUnreadyOrUnableToPlan(t *testing.T) {
	cursorReason := strings.TrimRight(harness.AdapterCapabilities("cursor").Analysis.Reason, ". ")
	for name, tc := range map[string]struct {
		body     string
		code     int
		category string
		reason   string
		probes   bool
	}{
		"cursor plans":         {`{"harness":"cursor","planner":"cursor"}`, http.StatusUnprocessableEntity, "planner", cursorReason, false},
		"codex signed out":     {`{"harness":"cursor","planner":"codex"}`, http.StatusConflict, "planner", "Sign in to Codex", true},
		"gone":                 {`{"harness":"cursor","planner":"claude-code"}`, http.StatusConflict, "planner", "Sign in to Claude Code", true},
		"not offered":          {`{"harness":"cursor","planner":"../evil"}`, http.StatusBadRequest, "invalid", "", false},
		"coding agent plans":   {`{"harness":"codex","planner":"claude-code"}`, http.StatusBadRequest, "invalid", "", false},
		"planner as an object": {`{"harness":"cursor","planner":{"id":"codex"}}`, http.StatusBadRequest, "invalid", "", false},
		"extra field":          {`{"harness":"cursor","planner":"codex","x":1}`, http.StatusBadRequest, "invalid", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			lab := t.TempDir()
			f := newFakeSetup(t, lab)
			f.candidates = []onboarding.Candidate{plannerCandidate("codex", "login-required"), plannerCandidate("cursor", "ready")}
			if name != "gone" {
				f.candidates = append(f.candidates, plannerCandidate("claude-code", "ready"))
			}
			h := plannerHandler(t, f)
			setupPost(t, h, lab, "check", `{}`)
			f.mu.Lock()
			discovers := f.discovers
			f.mu.Unlock()
			code, _, refusal := setupPost(t, h, lab, "create", tc.body)
			if code != tc.code || refusal.Category != tc.category || !strings.Contains(refusal.Reason, tc.reason) {
				t.Fatalf("create %s: %d %+v", tc.body, code, refusal)
			}
			if tc.reason != "" && strings.Contains(refusal.Reason, localLeak) {
				t.Fatalf("refusal leaks probe detail: %+v", refusal)
			}
			f.mu.Lock()
			probed := f.discovers - discovers
			f.mu.Unlock()
			if want := map[bool]int{true: 1, false: 0}[tc.probes]; probed != want {
				t.Fatalf("create probed %d times, want %d", probed, want)
			}
			if prepares, _ := f.calls(); prepares != 0 {
				t.Fatalf("a refused planner reached Prepare %d times", prepares)
			}
			if entries, _ := os.ReadDir(lab); len(entries) != 0 {
				t.Fatalf("a refused planner wrote %v", entries)
			}
			if v := setupGet(t, h, lab); v.Step != "agent" {
				t.Fatalf("after a refused planner: %+v", v)
			}
			f.unchanged("after a refused planner")
		})
	}
}

// With no planner in the request, Claude Code and Codex setups reach Prepare
// with exactly the options they did before, and plan and code with themselves.
func TestUISetupPlannerUnchangedWithoutAChoice(t *testing.T) {
	for _, id := range []string{"claude-code", "codex"} {
		t.Run(id, func(t *testing.T) {
			lab := t.TempDir()
			f := newFakeSetup(t, lab)
			f.candidates = []onboarding.Candidate{plannerCandidate("claude-code", "ready"), plannerCandidate("codex", "ready"), plannerCandidate("cursor", "ready")}
			h := plannerHandler(t, f)
			setupPost(t, h, lab, "check", `{}`)
			code, v, _ := setupPost(t, h, lab, "create", `{"harness":"`+id+`"}`)
			if code != http.StatusOK || v.Proposal == nil {
				t.Fatalf("create: %d %+v", code, v)
			}
			o := f.lastOptions(t)
			want := onboarding.InitOptions{Cwd: "/src/checkout", LabDir: lab, Launcher: "/cli", Preferred: id}
			if o.Cwd != want.Cwd || o.LabDir != want.LabDir || o.Launcher != want.Launcher || o.Preferred != want.Preferred || o.Planner != "" || o.RoleHarness || o.Role != "" || o.EnableReview || len(o.Candidates) != 3 || len(o.Env) != 1 {
				t.Fatalf("Prepare got %+v", o)
			}
			if p := v.Proposal; p.Planning != id || p.Coding != id || p.Binding != id {
				t.Fatalf("review bindings: planning %q, coding %q, binding %q", p.Planning, p.Coding, p.Binding)
			}
		})
	}
}

// The page renders the codes-only note, the planning choice and both review
// bindings as text, and the stylesheet gives them the setup conventions.
func TestUISetupPlannerViewRendersTextOnly(t *testing.T) {
	h := testHandler(t, Options{LabDir: t.TempDir()})
	js := do(t, h, authed("/js/setup.js")).Body.String()
	for _, want := range []string{"'Codes only: it needs a second agent for planning.'", "name: 'planner'", "planner: chosen.value", "view.planner", "'Plans with '", "' · Codes with '", "choice.hidden = hide; choice.disabled = hide;"} {
		if !strings.Contains(js, want) {
			t.Errorf("setup.js lacks %q", want)
		}
	}
	css := do(t, h, authed("/css/setup.css")).Body.String()
	for _, want := range []string{".setup-codes-only", ".setup-planners", ".setup-option input:focus-visible"} {
		if !strings.Contains(css, want) {
			t.Errorf("setup.css lacks %s", want)
		}
	}
}
