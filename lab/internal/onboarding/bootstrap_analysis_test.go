package onboarding

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

func bootstrapCandidate(id, executable, readiness string) Candidate {
	c := Candidate{ID: id, Executable: executable, Readiness: readiness, Detail: id + " is " + readiness + ".", Capabilities: harness.AdapterCapabilities(id)}
	if c.Capabilities.Analysis.State == "supported" {
		c.Capabilities.AnalysisControls = harness.Capability{State: "supported", Reason: "controls verified"}
	}
	return c
}

// prepareCursorBootstrap runs a fresh-Lab init --harness cursor with the given
// planner candidates.
func prepareCursorBootstrap(t *testing.T, planners ...Candidate) (Outcome, string, error) {
	t.Helper()
	root, run := repoFixture(t)
	dir := filepath.Join(t.TempDir(), "lab")
	candidates := append([]Candidate{bootstrapCandidate("cursor", "/bin/cursor-agent", "ready")}, planners...)
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Preferred: "cursor", Candidates: candidates, Runner: run})
	return out, dir, err
}

// assertPlannedBy checks a pending Cursor bootstrap declares harnesses.analysis on
// planner, saves it verifiably, and after approval intake resolves to planner.
func assertPlannedBy(t *testing.T, out Outcome, dir, planner, executable string) {
	t.Helper()
	if out.Status != "pending" || out.Proposal == nil {
		t.Fatalf("no pending proposal: %+v", out)
	}
	f, err := formula.Parse([]byte(out.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if f.Harness.Binding != "cursor" || !slices.Equal(f.Harness.Command, []string{"/installed/forgecell", "__adapter", "cursor", "/bin/cursor-agent"}) {
		t.Fatalf("coding binding changed: %+v", f.Harness)
	}
	analysis, declared := f.Harnesses[string(formula.AnalysisHarness)]
	if !declared || len(f.Harnesses) != 1 || analysis.Binding != planner || !slices.Equal(analysis.Command, []string{"/installed/forgecell", "__adapter", planner, executable}) {
		t.Fatalf("harnesses = %+v, want only analysis on %s", f.Harnesses, planner)
	}
	for _, want := range []string{"cursor cannot do read-only ticket analysis", "binds planning to " + planner, "forgecell init --harness ID --role analysis"} {
		if !strings.Contains(out.Note, want) {
			t.Fatalf("note lacks %q: %s", want, out.Note)
		}
	}
	if err = ValidateRoleBindings(f); err != nil {
		t.Fatal(err)
	}
	saved, err := InspectProposal(dir, out.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence := saved.CapabilityEvidence
	if evidence == nil || evidence.Candidate.ID != "cursor" || evidence.Workflow.State != "conditional" {
		t.Fatalf("saved evidence must keep the coding candidate and the planner's workflow: %+v", evidence)
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	approved, err := formula.Load(dir, "")
	if err != nil || !approved.Approved {
		t.Fatalf("approval: %+v %v", approved, err)
	}
	if b, _ := approved.Formula.ExecutionBinding(formula.IntakeRole); b.Binding != planner {
		t.Fatalf("intake resolves to %q, want %s", b.Binding, planner)
	}
	if b, _ := approved.Formula.RoleBinding(formula.CodingHarness); b.Binding != "cursor" {
		t.Fatalf("coding resolves to %q, want cursor", b.Binding)
	}
}

func TestBootstrapCursorPairsClaudeCode(t *testing.T) {
	out, dir, err := prepareCursorBootstrap(t, bootstrapCandidate("codex", "/bin/codex", "ready"), bootstrapCandidate("claude-code", "/bin/claude", "ready"))
	if err != nil {
		t.Fatal(err)
	}
	assertPlannedBy(t, out, dir, "claude-code", "/bin/claude")
}

func TestBootstrapCursorPairsCodexWhenClaudeCodeNotReady(t *testing.T) {
	for name, claude := range map[string]Candidate{
		"login required": bootstrapCandidate("claude-code", "/bin/claude", "login-required"),
		"not installed":  bootstrapCandidate("claude-code", "", "missing"),
	} {
		t.Run(name, func(t *testing.T) {
			out, dir, err := prepareCursorBootstrap(t, claude, bootstrapCandidate("codex", "/bin/codex", "ready"))
			if err != nil {
				t.Fatal(err)
			}
			assertPlannedBy(t, out, dir, "codex", "/bin/codex")
		})
	}
}

func TestBootstrapCursorRefusesWithoutPlanner(t *testing.T) {
	for name, planners := range map[string][]Candidate{
		"none discovered": nil,
		"none ready":      {bootstrapCandidate("claude-code", "", "missing"), bootstrapCandidate("codex", "/bin/codex", "login-required")},
		"no executable":   {bootstrapCandidate("codex", "", "ready")},
	} {
		t.Run(name, func(t *testing.T) {
			out, dir, err := prepareCursorBootstrap(t, planners...)
			if err == nil {
				t.Fatalf("Cursor bootstrap without a planner was proposed: %+v", out)
			}
			for _, want := range []string{harness.AdapterCapabilities("cursor").Analysis.Reason, "install or sign in to Claude Code or Codex", "no proposal saved"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal lacks %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), "/bin/") || strings.Contains(err.Error(), "/installed/") {
				t.Fatalf("refusal leaks a local path: %v", err)
			}
			if out.Proposal != nil {
				t.Fatalf("refusal returned a proposal: %+v", out.Proposal)
			}
			if entries, e := os.ReadDir(filepath.Join(dir, "assays")); e == nil && len(entries) > 0 || e != nil && !os.IsNotExist(e) {
				t.Fatalf("refusal wrote assays: %v %v", entries, e)
			}
			if _, e := os.Stat(filepath.Join(dir, resources.ConfigFile)); !os.IsNotExist(e) {
				t.Fatalf("refusal wrote %s: %v", resources.ConfigFile, e)
			}
		})
	}
}

func TestBootstrapNativeAnalysisUnchanged(t *testing.T) {
	for _, id := range []string{"claude-code", "codex"} {
		t.Run(id, func(t *testing.T) {
			root, run := repoFixture(t)
			dir := filepath.Join(t.TempDir(), "lab")
			candidates := []Candidate{bootstrapCandidate("claude-code", "/bin/claude", "ready"), bootstrapCandidate("codex", "/bin/codex", "ready"), bootstrapCandidate("cursor", "/bin/cursor-agent", "ready")}
			out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Preferred: id, Candidates: candidates, Runner: run})
			if err != nil {
				t.Fatal(err)
			}
			f, err := formula.Parse([]byte(out.Proposal.YAML))
			if err != nil {
				t.Fatal(err)
			}
			if f.Harness.Binding != id || len(f.Harnesses) != 0 || strings.Contains(out.Proposal.YAML, "harnesses") {
				t.Fatalf("%s bootstrap gained role bindings:\n%s", id, out.Proposal.YAML)
			}
			if strings.Contains(out.Note, "harnesses.analysis") || !strings.HasPrefix(out.Note, "Bootstrap Assay is read-only.") {
				t.Fatalf("%s bootstrap note changed: %s", id, out.Note)
			}
			if out.Binding == nil || out.Binding.Workflow != out.Binding.Candidate.WorkflowCapability() {
				t.Fatalf("%s workflow must come from the coding candidate: %+v", id, out.Binding)
			}
		})
	}
}
