package onboarding

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// prepareExplicitPlanner runs a fresh-Lab init --harness coding with an explicit
// Planner over the given candidates.
func prepareExplicitPlanner(t *testing.T, coding, planner string, candidates ...Candidate) (Outcome, string, error) {
	t.Helper()
	root, run := repoFixture(t)
	dir := filepath.Join(t.TempDir(), "lab")
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Preferred: coding, Planner: planner, Candidates: candidates, Runner: run})
	return out, dir, err
}

// assertNothingSaved checks a refused Prepare returned no proposal and wrote no
// assays and no resource configuration.
func assertNothingSaved(t *testing.T, out Outcome, dir string) {
	t.Helper()
	if out.Proposal != nil {
		t.Fatalf("refusal returned a proposal: %+v", out.Proposal)
	}
	if entries, e := os.ReadDir(filepath.Join(dir, "assays")); e == nil && len(entries) > 0 || e != nil && !os.IsNotExist(e) {
		t.Fatalf("refusal wrote assays: %v %v", entries, e)
	}
	if _, e := os.Stat(filepath.Join(dir, resources.ConfigFile)); !os.IsNotExist(e) {
		t.Fatalf("refusal wrote %s: %v", resources.ConfigFile, e)
	}
}

func allReady() []Candidate {
	return []Candidate{bootstrapCandidate("cursor", "/bin/cursor-agent", "ready"), bootstrapCandidate("claude-code", "/bin/claude", "ready"), bootstrapCandidate("codex", "/bin/codex", "ready")}
}

func TestBootstrapExplicitPlannerCodex(t *testing.T) {
	out, dir, err := prepareExplicitPlanner(t, "cursor", "codex", allReady()...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Note, "the planner you chose") {
		t.Fatalf("note does not say the planner was chosen: %s", out.Note)
	}
	assertPlannedBy(t, out, dir, "codex", "/bin/codex")
}

func TestBootstrapExplicitPlannerClaudeCode(t *testing.T) {
	candidates := []Candidate{bootstrapCandidate("cursor", "/bin/cursor-agent", "ready"), bootstrapCandidate("codex", "/bin/codex", "ready"), bootstrapCandidate("claude-code", "/bin/claude", "ready")}
	out, dir, err := prepareExplicitPlanner(t, "cursor", "claude-code", candidates...)
	if err != nil {
		t.Fatal(err)
	}
	assertPlannedBy(t, out, dir, "claude-code", "/bin/claude")
}

func TestBootstrapExplicitPlannerRefusesUnreadyOrUnableToPlan(t *testing.T) {
	cursorReason := harness.AdapterCapabilities("cursor").Analysis.Reason
	for name, tc := range map[string]struct {
		planner    string
		candidates []Candidate
		want       []string
	}{
		"cursor plans":      {"cursor", allReady(), []string{"cannot be used for read-only ticket analysis", cursorReason, "no proposal saved"}},
		"codex signed out":  {"codex", []Candidate{bootstrapCandidate("cursor", "/bin/cursor-agent", "ready"), bootstrapCandidate("claude-code", "/bin/claude", "ready"), bootstrapCandidate("codex", "/bin/codex", "login-required")}, []string{"cannot bind harnesses.analysis to codex", "codex is login-required.", "no proposal saved"}},
		"claude no binary":  {"claude-code", []Candidate{bootstrapCandidate("cursor", "/bin/cursor-agent", "ready"), bootstrapCandidate("claude-code", "", "ready")}, []string{"cannot bind harnesses.analysis to claude-code", "no proposal saved"}},
		"not discovered":    {"codex", []Candidate{bootstrapCandidate("cursor", "/bin/cursor-agent", "ready"), bootstrapCandidate("claude-code", "/bin/claude", "ready")}, []string{"no such harness was discovered", "no proposal saved"}},
		"unknown harness":   {"other", allReady(), []string{"no such harness was discovered", "no proposal saved"}},
		"coding plans self": {"codex", allReady(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			coding := "cursor"
			if name == "coding plans self" {
				coding = "claude-code"
				tc.want = []string{"claude-code can do read-only ticket analysis itself", "no proposal saved"}
			}
			out, dir, err := prepareExplicitPlanner(t, coding, tc.planner, tc.candidates...)
			if err == nil {
				t.Fatalf("planner %s was accepted: %+v", tc.planner, out)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal lacks %q: %v", want, err)
				}
			}
			assertNothingSaved(t, out, dir)
		})
	}
}

func TestBootstrapExplicitPlannerOnlyForABootstrapWithHarness(t *testing.T) {
	for name, o := range map[string]InitOptions{
		"no harness":    {Planner: "codex"},
		"enable review": {Preferred: "cursor", Planner: "codex", EnableReview: true},
		"role":          {Preferred: "cursor", Planner: "codex", RoleHarness: true, Role: "analysis"},
		"rebind":        {Rebind: "cursor", Planner: "codex"},
	} {
		t.Run(name, func(t *testing.T) {
			root, run := repoFixture(t)
			dir := filepath.Join(t.TempDir(), "lab")
			o.Cwd, o.LabDir, o.Launcher, o.Candidates, o.Runner = root, dir, "/installed/forgecell", allReady(), run
			out, err := Prepare(context.Background(), o)
			if err == nil || !strings.Contains(err.Error(), "a planner is chosen only for a GitHub bootstrap") {
				t.Fatalf("planner with %s: %v", name, err)
			}
			assertNothingSaved(t, out, dir)
		})
	}
	// A Lab that already has a Formula takes no bootstrap planner.
	root, run := repoFixture(t)
	dir := filepath.Join(t.TempDir(), "lab")
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Preferred: "cursor", Planner: "codex", Candidates: allReady(), Runner: run})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err = Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Preferred: "cursor", Planner: "claude-code", Candidates: allReady(), Runner: run}); err == nil || !strings.Contains(err.Error(), "only for a bootstrap") {
		t.Fatalf("planner on an approved Lab: %v", err)
	}
}

func TestBootstrapExplicitPlannerDefaultMatchesTheEngine(t *testing.T) {
	for name, tc := range map[string]struct {
		candidates []Candidate
		want       string
	}{
		"claude first":   {allReady(), "claude-code"},
		"codex second":   {[]Candidate{bootstrapCandidate("claude-code", "/bin/claude", "login-required"), bootstrapCandidate("codex", "/bin/codex", "ready")}, "codex"},
		"none ready":     {[]Candidate{bootstrapCandidate("claude-code", "", "missing"), bootstrapCandidate("cursor", "/bin/cursor-agent", "ready")}, ""},
		"none at all":    {nil, ""},
		"relative paths": {[]Candidate{bootstrapCandidate("claude-code", "bin/claude", "ready")}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := DefaultBootstrapPlanner(tc.candidates, "/installed/forgecell"); got != tc.want {
				t.Fatalf("default planner %q, want %q", got, tc.want)
			}
		})
	}
	if DefaultBootstrapPlanner(allReady(), "") != "" {
		t.Fatal("a default planner without a launcher")
	}
	for id, want := range map[string]string{"claude-code": "supported", "codex": "supported", "cursor": "unsupported"} {
		if got := PlanningCapability(id); got.State != want || got.Reason == "" {
			t.Fatalf("%s planning capability %+v, want %s", id, got, want)
		}
	}
	if got := PlanningCapability("cursor").Reason; got != harness.AdapterCapabilities("cursor").Analysis.Reason {
		t.Fatalf("cursor reason %q", got)
	}
}
