package onboarding

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
)

func refreshWorkflow(run string) string {
	return "on: pull_request\njobs:\n  core:\n    runs-on: ubuntu-latest\n    steps:\n      - run: " + run + "\n        working-directory: service\n"
}

// commitWorkflow writes .github/workflows/ci.yml and commits it.
func commitWorkflow(t *testing.T, root, content string) {
	t.Helper()
	os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0700)
	if err := os.WriteFile(filepath.Join(root, ".github", "workflows", "ci.yml"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "workflow"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v", b, err)
		}
	}
}

// refreshLab approves a learn-bound codex Formula whose baseline is `go test ./...`
// at 900000 ms, then hand-edits and approves it with instructions, learned
// guidance, a role binding and a comment the refresh must keep byte for byte.
func refreshLab(t *testing.T) (root string, runner Runner, dir string) {
	t.Helper()
	root, runner = repoFixture(t)
	commitWorkflow(t, root, refreshWorkflow("go test ./..."))
	dir = t.TempDir()
	boot, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Preferred: "codex", MetaHarness: "codex", Runner: runner})
	if err != nil || boot.Proposal == nil {
		t.Fatalf("%+v %v", boot, err)
	}
	if _, err = Review(dir, boot.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	active, err := formula.Load(dir, "")
	if err != nil || len(active.Formula.RequiredChecks) != 1 || active.Formula.RequiredChecks[0].TimeoutMS != 900000 || !slices.Equal(active.Formula.RequiredChecks[0].Argv, []string{"go", "test", "./..."}) {
		t.Fatalf("bootstrap baseline %+v %v", active.Formula.RequiredChecks, err)
	}
	doc := decodeYAML(t, active.Formula.YAML)
	doc["harness"].(map[string]any)["instructions"] = "learned guidance: run the narrowest test first"
	doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": []string{"/cli", "__adapter", "claude-code", "/claude-code"}}}
	edited, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p, err := SaveProposal(dir, "# reviewed by hand\n"+string(edited))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, p.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	return root, runner, dir
}

func refresh(root string, runner Runner, dir string, mutate ...func(*InitOptions)) (Outcome, error) {
	o := InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), RefreshBaseline: true, Runner: runner}
	for _, m := range mutate {
		m(&o)
	}
	return Prepare(context.Background(), o)
}

// withoutRequiredChecks drops the top-level requiredChecks block from Formula text.
func withoutRequiredChecks(text string) []string {
	var kept []string
	inBlock := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "requiredChecks:") {
			inBlock = true
			continue
		}
		if inBlock && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "-")) {
			continue
		}
		inBlock = false
		kept = append(kept, line)
	}
	return kept
}

func TestRefreshBaselineProposesOnlyTheChangedCheck(t *testing.T) {
	root, runner, dir := refreshLab(t)
	approved, err := formula.Load(dir, "")
	if err != nil || !approved.Approved {
		t.Fatal(err)
	}
	workflow := refreshWorkflow("go test -timeout 20m ./...")
	commitWorkflow(t, root, workflow)
	next, err := refresh(root, runner, dir)
	if err != nil || next.Status != "pending" || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	proposed, err := formula.Parse([]byte(next.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(proposed.RequiredChecks) != 1 {
		t.Fatalf("requiredChecks %+v", proposed.RequiredChecks)
	}
	was, now := approved.Formula.RequiredChecks[0], proposed.RequiredChecks[0]
	if !slices.Equal(now.Argv, []string{"go", "test", "-timeout", "20m", "./..."}) || now.TimeoutMS != 1500000 {
		t.Fatalf("refreshed check %+v", now)
	}
	was.Argv, was.TimeoutMS, was.Reason = now.Argv, now.TimeoutMS, now.Reason
	if !reflect.DeepEqual(was, now) || now.ID != "baseline-go-test-service" || now.Dir != "service" {
		t.Fatalf("the check changed more than argv and timeoutMs: %+v vs %+v", approved.Formula.RequiredChecks[0], now)
	}
	if !slices.Equal(withoutRequiredChecks(approved.Formula.YAML), withoutRequiredChecks(next.Proposal.YAML)) || !strings.HasPrefix(next.Proposal.YAML, "# reviewed by hand\n") {
		t.Fatalf("lines outside requiredChecks changed:\nbefore %s\nafter  %s", approved.Formula.YAML, next.Proposal.YAML)
	}
	before, after := decodeYAML(t, approved.Formula.YAML), decodeYAML(t, next.Proposal.YAML)
	for _, key := range []string{"harness", "harnesses", "atoms", "repositoryContext", "lab", "intake"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Fatalf("%s changed", key)
		}
	}
	if after["harness"].(map[string]any)["instructions"] != "learned guidance: run the narrowest test first" || !LearnBound(proposed) {
		t.Fatal("learned guidance or the learn Atom was not preserved")
	}
	if err = verifyBaselineDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(workflow)))
	for _, want := range []string{"changed baseline-go-test-service", `argv ["go","test","./..."] -> ["go","test","-timeout","20m","./..."]`, "timeoutMs 900000 -> 1500000", ".github/workflows/ci.yml sha256 " + sum} {
		if !strings.Contains(next.Note, want) {
			t.Fatalf("note omits %q: %s", want, next.Note)
		}
	}
	if strings.Contains(next.Note, root) || strings.Contains(next.Note, dir) {
		t.Fatalf("note leaks a local path: %s", next.Note)
	}
	if now, _ := formula.Load(dir, ""); now.Formula.YAML != approved.Formula.YAML {
		t.Fatal("the proposal activated without --approve")
	}
	if _, err = Review(dir, next.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	live, err := formula.Load(dir, "")
	if err != nil || !live.Approved || live.Formula.YAML != next.Proposal.YAML || live.Formula.RequiredChecks[0].TimeoutMS != 1500000 {
		t.Fatalf("approval did not activate the exact proposal: %v", err)
	}
}

func TestRefreshBaselineAddsChecksToAFormulaWithoutThem(t *testing.T) {
	root, runner := repoFixture(t)
	commitWorkflow(t, root, refreshWorkflow("go vet ./..."))
	dir := t.TempDir()
	p, err := SaveProposal(dir, roleFormula(""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, p.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	next, err := refresh(root, runner, dir)
	if err != nil || next.Proposal == nil || !strings.Contains(next.Note, "added baseline-go-vet-service") {
		t.Fatalf("%+v %v", next, err)
	}
	if !strings.HasPrefix(next.Proposal.YAML, roleFormula("")) {
		t.Fatalf("the approved text was not kept as a prefix:\n%s", next.Proposal.YAML)
	}
}

func TestRefreshBaselineUnchangedSavesNothing(t *testing.T) {
	root, runner, dir := refreshLab(t)
	count := assayCount(dir)
	out, err := refresh(root, runner, dir)
	if err != nil || out.Status != "unchanged" || out.Proposal != nil || !strings.Contains(strings.ToLower(out.Note), "baseline unchanged") {
		t.Fatalf("%+v %v", out, err)
	}
	if assayCount(dir) != count {
		t.Fatal("an unchanged baseline saved a proposal")
	}
}

func TestRefreshBaselineRefusesAnEmptyBaseline(t *testing.T) {
	root, runner, dir := refreshLab(t)
	count := assayCount(dir)
	commitWorkflow(t, root, refreshWorkflow("echo nothing recognized"))
	if _, err := refresh(root, runner, dir); err == nil || !strings.Contains(err.Error(), "dropping every protected check") {
		t.Fatalf("empty baseline: %v", err)
	}
	if assayCount(dir) != count {
		t.Fatal("a refused refresh saved a proposal")
	}
}

func TestRefreshBaselineRefusesATamperedProposal(t *testing.T) {
	root, runner, dir := refreshLab(t)
	approved, _ := formula.Load(dir, "")
	commitWorkflow(t, root, refreshWorkflow("go test -timeout 20m ./..."))
	next, err := refresh(root, runner, dir)
	if err != nil || next.Proposal == nil {
		t.Fatal(err)
	}
	if err = verifyBaselineDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
	doc := decodeYAML(t, next.Proposal.YAML)
	doc["harness"].(map[string]any)["timeoutMs"] = 1
	tampered, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyBaselineDiff(approved.Formula.YAML, string(tampered)); err == nil || !strings.Contains(err.Error(), "no proposal saved") {
		t.Fatalf("a proposal that also changes harness.timeoutMs was accepted: %v", err)
	}
	proposed, _ := formula.Parse([]byte(next.Proposal.YAML))
	if err = verifyBaselineExact(approved.Formula.YAML, next.Proposal.YAML, proposed.RequiredChecks); err != nil {
		t.Fatal(err)
	}
	if err = verifyBaselineExact(approved.Formula.YAML, next.Proposal.YAML, approved.Formula.RequiredChecks); err == nil || !strings.Contains(err.Error(), "no proposal saved") {
		t.Fatalf("requiredChecks other than the derived ones were accepted: %v", err)
	}
}

func TestRefreshBaselineGuards(t *testing.T) {
	root, runner, dir := refreshLab(t)
	count := assayCount(dir)
	for name, mutate := range map[string]func(*InitOptions){
		"with harness":      func(o *InitOptions) { o.Preferred = "codex" },
		"with rebind":       func(o *InitOptions) { o.Rebind = "claude-code" },
		"with meta harness": func(o *InitOptions) { o.MetaHarness = "codex" },
		"with meta command": func(o *InitOptions) { o.MetaCommand = []string{"/bin/meta"} },
		"with model":        func(o *InitOptions) { o.ModelSet, o.Model = true, "claude-opus-4-8" },
		"with timeouts":     func(o *InitOptions) { o.HarnessTimeoutMS = 1000 },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := refresh(root, runner, dir, mutate); err == nil || !strings.Contains(err.Error(), "--refresh-baseline changes only requiredChecks") {
				t.Fatalf("error %v", err)
			}
			if assayCount(dir) != count {
				t.Fatal("a refused refresh saved a proposal")
			}
		})
	}
	commitWorkflow(t, root, refreshWorkflow("go test -timeout 20m ./..."))
	if _, err := refresh(root, runner, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := refresh(root, runner, dir); err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("second refresh beside a pending proposal: %v", err)
	}
}

func TestRefreshBaselineRequiresCurrentExactApproval(t *testing.T) {
	root, runner := repoFixture(t)
	if _, err := refresh(root, runner, t.TempDir()); err == nil || !strings.Contains(err.Error(), "approved Formula") {
		t.Fatalf("no Formula: %v", err)
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
	os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"sample"}`), 0600)
	os.WriteFile(filepath.Join(dir, "formulas", "sample.yaml"), []byte("kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex]}\natoms: [{id: harness, type: harness, binding: codex}]\n"), 0600)
	if _, err := refresh(root, runner, dir); err == nil || !strings.Contains(err.Error(), "exact approval") {
		t.Fatalf("unapproved Formula: %v", err)
	}
	if assayCount(dir) != 0 {
		t.Fatal("a refused refresh saved a proposal")
	}
}
