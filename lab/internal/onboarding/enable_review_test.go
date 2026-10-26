package onboarding

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
)

// reviewLab approves a bootstrap Formula coding with the given harness and
// learning through codex, then approves it again with a hand-written comment the
// proposal must keep byte for byte.
func reviewLab(t *testing.T, coding string) (root string, runner Runner, dir string) {
	t.Helper()
	root, runner = repoFixture(t)
	dir = t.TempDir()
	boot, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Preferred: coding, MetaHarness: "codex", Runner: runner})
	if err != nil || boot.Proposal == nil {
		t.Fatalf("%+v %v", boot, err)
	}
	if _, err = Review(dir, boot.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	active, err := formula.Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	approveText(t, dir, "# reviewed by hand\n"+active.Formula.YAML)
	return root, runner, dir
}

func enableReview(root string, runner Runner, dir string, mutate ...func(*InitOptions)) (Outcome, error) {
	o := InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), EnableReview: true, Runner: runner}
	for _, m := range mutate {
		m(&o)
	}
	return Prepare(context.Background(), o)
}

// learnRoleFormula is roleFormula with a codex learn Atom, so a reviewer resolves
// from the meta binding; the coding command is the same codex command.
func learnRoleFormula() string {
	return roleFormula("") + "  - {id: learn, type: learn, binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 900000}\n"
}

func TestEnableReviewInsertsOnlyTheReviewAtom(t *testing.T) {
	root, runner, dir := reviewLab(t, "claude-code")
	approved, err := formula.Load(dir, "")
	if err != nil || !approved.Approved {
		t.Fatal(err)
	}
	out, err := enableReview(root, runner, dir)
	if err != nil || out.Status != "pending" || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	before, after := strings.Split(approved.Formula.YAML, "\n"), strings.Split(out.Proposal.YAML, "\n")
	if len(after) != len(before)+2 {
		t.Fatalf("expected exactly two inserted lines:\nbefore %s\nafter  %s", approved.Formula.YAML, out.Proposal.YAML)
	}
	at := slices.IndexFunc(after, func(line string) bool { return strings.TrimSpace(line) == "- id: review" })
	if at < 0 || strings.TrimSpace(after[at+1]) != "type: review" || !slices.Equal(slices.Delete(slices.Clone(after), at, at+2), before) {
		t.Fatalf("lines other than the review Atom changed:\nbefore %s\nafter  %s", approved.Formula.YAML, out.Proposal.YAML)
	}
	proposed, err := formula.Parse([]byte(out.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if a := proposed.Atoms[4]; a.ID != "review" || a.Type != "review" || proposed.Atoms[3].Type != "check" || proposed.Atoms[5].Purpose != "review" {
		t.Fatalf("atoms %+v", proposed.Atoms)
	}
	roles, err := proposed.ExecutionRoles()
	if err != nil || roles[formula.Review].ID != "review" {
		t.Fatalf("roles %v %v", roles, err)
	}
	res, configured, err := ResolveReviewBinding(proposed)
	if err != nil || !configured || res.Binding.Binding != "codex" || res.Source != "learn-atom" || res.SameCommandAsCoding {
		t.Fatalf("reviewer %+v %v %v", res, configured, err)
	}
	if !strings.Contains(out.Note, "Reviewer: binding codex (from learn-atom).") || strings.Contains(out.Note, "WARNING") {
		t.Fatalf("note %s", out.Note)
	}
	if strings.Contains(out.Note, root) || strings.Contains(out.Note, dir) {
		t.Fatalf("note leaks a local path: %s", out.Note)
	}
	if err = verifyReviewExact(approved.Formula.YAML, out.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
	if now, _ := formula.Load(dir, ""); now.Formula.YAML != approved.Formula.YAML {
		t.Fatal("the proposal activated without --approve")
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	live, err := formula.Load(dir, "")
	if err != nil || !live.Approved || live.Formula.YAML != out.Proposal.YAML {
		t.Fatalf("approval did not activate the exact proposal: %v", err)
	}
	count := assayCount(dir)
	if _, err = enableReview(root, runner, dir); err == nil || !strings.Contains(err.Error(), "already has a review Atom") || !strings.Contains(err.Error(), "no proposal saved") {
		t.Fatalf("existing review Atom: %v", err)
	}
	if assayCount(dir) != count {
		t.Fatal("a refused --enable-review saved a proposal")
	}
}

func TestEnableReviewKeepsFlowItemStyleAndReportsSameCommandAsCoding(t *testing.T) {
	root, runner := repoFixture(t)
	dir := t.TempDir()
	approveText(t, dir, learnRoleFormula())
	out, err := enableReview(root, runner, dir)
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	want := strings.Replace(learnRoleFormula(), "  - {id: gate,", "  - {id: review, type: review}\n  - {id: gate,", 1)
	if out.Proposal.YAML != want {
		t.Fatalf("proposal:\n%s\nwant:\n%s", out.Proposal.YAML, want)
	}
	if !strings.Contains(out.Note, "WARNING: "+SameReviewerWarning) {
		t.Fatalf("same command as coding not reported: %s", out.Note)
	}
	proposed, _ := formula.Parse([]byte(out.Proposal.YAML))
	if res, _, err := ResolveReviewBinding(proposed); err != nil || !res.SameCommandAsCoding {
		t.Fatalf("reviewer %+v %v", res, err)
	}
}

func TestEnableReviewRefusals(t *testing.T) {
	t.Run("no resolvable binding", func(t *testing.T) {
		root, runner := repoFixture(t)
		dir := t.TempDir()
		approveText(t, dir, roleFormula(""))
		count := assayCount(dir)
		if _, err := enableReview(root, runner, dir); err == nil || !strings.Contains(err.Error(), ReviewMissingBindingInstruction) || !strings.Contains(err.Error(), "no proposal saved") {
			t.Fatalf("error %v", err)
		}
		if assayCount(dir) != count {
			t.Fatal("a refused --enable-review saved a proposal")
		}
	})
	t.Run("flow atoms list", func(t *testing.T) {
		root, runner := repoFixture(t)
		dir := t.TempDir()
		approveText(t, dir, "kind: formula\nid: roles\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex]}\natoms: [{id: intake, type: intake}, {id: scope, type: gate, purpose: scope}, {id: harness, type: harness, binding: codex}, {id: check, type: check}, {id: gate, type: gate, purpose: review}, {id: ship, type: ship}, {id: document, type: document}, {id: learn, type: learn, binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 900000}]\n")
		count := assayCount(dir)
		if _, err := enableReview(root, runner, dir); err == nil || !strings.Contains(err.Error(), "cannot be edited in place") {
			t.Fatalf("error %v", err)
		}
		if assayCount(dir) != count {
			t.Fatal("a refused --enable-review saved a proposal")
		}
	})
	t.Run("pending proposal", func(t *testing.T) {
		root, runner := repoFixture(t)
		dir := t.TempDir()
		approveText(t, dir, learnRoleFormula())
		if _, err := enableReview(root, runner, dir); err != nil {
			t.Fatal(err)
		}
		count := assayCount(dir)
		if _, err := enableReview(root, runner, dir); err == nil || !strings.Contains(err.Error(), "already pending") || !strings.Contains(err.Error(), "before --enable-review") {
			t.Fatalf("error %v", err)
		}
		if assayCount(dir) != count {
			t.Fatal("a refused --enable-review saved a proposal")
		}
	})
	t.Run("no approved Formula", func(t *testing.T) {
		root, runner := repoFixture(t)
		dir := t.TempDir()
		if _, err := enableReview(root, runner, dir); err == nil || !strings.Contains(err.Error(), "--enable-review needs an approved Formula") {
			t.Fatalf("error %v", err)
		}
		if assayCount(dir) != 0 {
			t.Fatal("a refused --enable-review saved a proposal")
		}
	})
	t.Run("no current exact approval", func(t *testing.T) {
		root, runner := repoFixture(t)
		dir := t.TempDir()
		os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
		os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"roles"}`), 0600)
		os.WriteFile(filepath.Join(dir, "formulas", "roles.yaml"), []byte(learnRoleFormula()), 0600)
		if _, err := enableReview(root, runner, dir); err == nil || !strings.Contains(err.Error(), "--enable-review needs a current exact approval") {
			t.Fatalf("error %v", err)
		}
		if assayCount(dir) != 0 {
			t.Fatal("a refused --enable-review saved a proposal")
		}
	})
	t.Run("combined options", func(t *testing.T) {
		root, runner := repoFixture(t)
		dir := t.TempDir()
		approveText(t, dir, learnRoleFormula())
		count := assayCount(dir)
		for name, mutate := range map[string]func(*InitOptions){
			"with harness":          func(o *InitOptions) { o.Preferred = "codex" },
			"with rebind":           func(o *InitOptions) { o.Rebind = "claude-code" },
			"with meta harness":     func(o *InitOptions) { o.MetaHarness = "codex" },
			"with meta command":     func(o *InitOptions) { o.MetaCommand = []string{"/bin/meta"} },
			"with model":            func(o *InitOptions) { o.ModelSet, o.Model = true, "claude-opus-4-8" },
			"with role":             func(o *InitOptions) { o.Role = "coding" },
			"with timeouts":         func(o *InitOptions) { o.HarnessTimeoutMS = 1000 },
			"with max timeout":      func(o *InitOptions) { o.MaxTimeoutMS = DefaultMaxTimeoutMS },
			"with refresh baseline": func(o *InitOptions) { o.RefreshBaseline = true },
		} {
			t.Run(name, func(t *testing.T) {
				if _, err := enableReview(root, runner, dir, mutate); err == nil || !strings.Contains(err.Error(), "--enable-review only inserts the review Atom") {
					t.Fatalf("error %v", err)
				}
				if assayCount(dir) != count {
					t.Fatal("a refused --enable-review saved a proposal")
				}
			})
		}
	})
}

func TestEnableReviewRefusesATamperedProposal(t *testing.T) {
	root, runner := repoFixture(t)
	dir := t.TempDir()
	approveText(t, dir, learnRoleFormula())
	out, err := enableReview(root, runner, dir)
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	before := learnRoleFormula()
	if err = verifyReviewExact(before, out.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
	tamper := func(change func(doc map[string]any)) string {
		doc := decodeYAML(t, out.Proposal.YAML)
		change(doc)
		b, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for name, c := range map[string]struct {
		text, want string
	}{
		"another key": {tamper(func(doc map[string]any) { doc["harness"].(map[string]any)["timeoutMs"] = 1 }), "more than the atoms list"},
		"moved Atom": {tamper(func(doc map[string]any) {
			atoms := doc["atoms"].([]any)
			atoms[4], atoms[5] = atoms[5], atoms[4]
		}), "other than by inserting"},
		"different Atom": {tamper(func(doc map[string]any) {
			doc["atoms"].([]any)[4].(map[string]any)["binding"] = "cursor"
		}), "other than by inserting"},
		"second Atom": {tamper(func(doc map[string]any) {
			doc["atoms"] = slices.Insert(doc["atoms"].([]any), 5, any(map[string]any{"id": "review2", "type": "review"}))
		}), "other than by inserting"},
		"unchanged": {before, "other than by inserting"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyReviewExact(before, c.text); err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "no proposal saved") {
				t.Fatalf("error %v, want %q", err, c.want)
			}
		})
	}
}
