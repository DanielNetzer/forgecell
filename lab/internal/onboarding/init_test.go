package onboarding

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"go.yaml.in/yaml/v4"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoFixture(t *testing.T) (string, Runner) {
	root := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = root
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v", b, err)
		}
	}
	run("init", "-q")
	run("config", "user.name", "Test")
	run("config", "user.email", "test@example.test")
	run("remote", "add", "origin", "https://github.com/acme/widgets.git")
	os.Mkdir(filepath.Join(root, "service"), 0700)
	os.WriteFile(filepath.Join(root, "service", "go.mod"), []byte("module example/service\n\ngo 1.27\n"), 0600)
	os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"scripts":{"test":"run test","build":"build"}}`), 0600)
	run("add", ".")
	run("commit", "-qm", "base")
	runner := func(ctx context.Context, o process.Options) process.Result {
		if o.Argv[0] == "gh" {
			return process.Result{OK: true, Stdout: `{"nameWithOwner":"acme/widgets","defaultBranchRef":{"name":"main"}}`}
		}
		return process.Run(ctx, o)
	}
	return root, runner
}
func TestInitProposesRepositoryBoundRecipeWithoutExecuting(t *testing.T) {
	root, runner := repoFixture(t)
	dir := filepath.Join(t.TempDir(), "lab")
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "pending" || out.Proposal == nil || !strings.Contains(out.Proposal.YAML, "/installed/forgecell") || out.Repository.Repo != "acme/widgets" {
		t.Fatalf("%+v", out)
	}
	var proposed struct {
		Lab struct {
			Dir string `yaml:"dir"`
		} `yaml:"lab"`
	}
	if err := yaml.Unmarshal([]byte(out.Proposal.YAML), &proposed); err != nil || proposed.Lab.Dir != dir {
		t.Fatalf("Formula Lab directory = %q, want %q: %v", proposed.Lab.Dir, dir, err)
	}
	if len(out.Repository.Components) != 2 {
		t.Fatalf("%+v", out.Repository)
	}
	if _, err = os.Stat(filepath.Join(dir, "lab.json")); !os.IsNotExist(err) {
		t.Fatal("init bypassed approval")
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	second, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/different/forgecell", Candidates: []Candidate{{ID: "codex", Readiness: "ready"}}, Runner: runner})
	if err != nil || second.Status != "unchanged" || second.Proposal != nil || second.Binding == nil || second.Binding.Candidate.ID != "codex" {
		t.Fatalf("%+v %v", second, err)
	}
}
func TestAmbiguousInitCannotProposeExecution(t *testing.T) {
	root, runner := repoFixture(t)
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: t.TempDir(), Launcher: "/cli", Candidates: []Candidate{{ID: "codex", Readiness: "ready"}, {ID: "cursor", Readiness: "ready"}}, Runner: runner})
	if err != nil || out.Status != "ambiguous" || out.Proposal != nil {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestRepositoryRetainsCommittedEvidenceAndScripts(t *testing.T) {
	root, runner := repoFixture(t)
	os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0700)
	os.WriteFile(filepath.Join(root, ".github", "workflows", "ci.yml"), []byte("name: CI\n"), 0600)
	os.WriteFile(filepath.Join(root, "CODEOWNERS"), []byte("/service/ @owner\n"), 0600)
	os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"scripts":{"test":"node --test"},"dependencies":{"local":"workspace:*"}}`), 0600)
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "evidence"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("%s %v", b, e)
		}
	}
	r, err := ReadRepository(context.Background(), root, runner)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Evidence) != 4 || r.EvidenceSHA256 == "" {
		t.Fatalf("missing frozen evidence: %+v", r)
	}
	found := false
	for _, c := range r.Components {
		if c.Manifest == "package.json" {
			found = true
			if c.Scripts["test"] != "node --test" || c.DeclaredDependencies["local"] != "workspace:*" {
				t.Fatalf("lost manifest provenance: %+v", c)
			}
		}
	}
	if !found {
		t.Fatal("missing root component")
	}
	for _, e := range r.Evidence {
		if e.Content == "" || e.SHA256 == "" {
			t.Fatalf("missing evidence content/hash: %+v", e)
		}
	}
}

func TestLegacyInitRequiresExactReviewWithoutRegenerating(t *testing.T) {
	root, runner := repoFixture(t)
	dir := t.TempDir()
	yaml := strings.Replace(recipe, "harness:", "intake: {repo: acme/widgets}\nharness:", 1) + "# preserve legacy bytes\n"
	os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
	os.WriteFile(filepath.Join(dir, "formulas/sample.yaml"), []byte(yaml), 0600)
	config := []byte(`{"schemaVersion":"v0","activeFormulaId":"sample"}`)
	os.WriteFile(filepath.Join(dir, "lab.json"), config, 0600)
	out, e := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Runner: runner, Candidates: []Candidate{}})
	if e != nil || out.Proposal == nil || out.Proposal.YAML != yaml || out.Status != "pending" || out.Binding == nil || out.Proposal.CapabilityEvidence == nil {
		t.Fatalf("%+v %v", out, e)
	}
	saved, _ := os.ReadFile(filepath.Join(dir, "lab.json"))
	if string(saved) != string(config) {
		t.Fatal("legacy review activated implicitly")
	}
	if _, e = Review(dir, out.Proposal.ID, "approve"); e != nil {
		t.Fatal(e)
	}
}

func TestBootstrapDetachesEvidence(t *testing.T) {
	root, runner := repoFixture(t)
	dir := t.TempDir()
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Proposal.YAML, "content:") {
		t.Fatal("full evidence remains in recipe")
	}
	if out.Proposal.EvidencePath == "" {
		t.Fatal("missing detached evidence")
	}
	if _, err := InspectProposal(dir, out.Proposal.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRebindPreservesDetachedEvidenceAndProcess(t *testing.T) {
	root, runner := repoFixture(t)
	dir := t.TempDir()
	options := InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}, {ID: "cursor", Executable: "/cursor", Readiness: "ready"}}, Preferred: "codex", Runner: runner}
	out, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	options.Preferred = "cursor"
	next, err := Prepare(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if next.Proposal.EvidenceHash != out.Proposal.EvidenceHash || next.Proposal.Version != "v1" || next.Binding == nil || next.Binding.Candidate.ID != "cursor" || next.Proposal.CapabilityEvidence == nil {
		t.Fatal("rebind dropped evidence binding")
	}
	if _, err = InspectProposal(dir, next.Proposal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, next.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
}

func TestInitReportsCursorBeforeApprovalWithoutFallback(t *testing.T) {
	root, run := repoFixture(t)
	dir := t.TempDir()
	c := Candidate{ID: "cursor", Executable: "/cursor", Readiness: "ready", Capabilities: harness.AdapterCapabilities("cursor")}
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/forgecell", Preferred: "cursor", Candidates: []Candidate{c, {ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: run})
	if err != nil {
		t.Fatal(err)
	}
	if out.Selection.ID != "cursor" || out.Binding == nil || out.Binding.Candidate.WorkflowCapability().State != "unsupported" || out.Proposal.CapabilityEvidence == nil {
		t.Fatalf("%+v", out)
	}
	if strings.Contains(out.Proposal.YAML, "binding: codex") || strings.Contains(out.Proposal.YAML, "type: learn") {
		t.Fatal("changed selection or added learning")
	}
	saved, err := InspectProposal(dir, out.Proposal.ID)
	if err != nil || saved.CapabilityEvidence.Candidate.Capabilities.Analysis.Reason != c.Capabilities.Analysis.Reason {
		t.Fatalf("%+v %v", saved, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "lab.json")); !os.IsNotExist(err) {
		t.Fatal("activated Formula")
	}
}
