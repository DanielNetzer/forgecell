package molecule

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testGit(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = cwd
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, b, err)
	}
	return strings.TrimSpace(string(b))
}
func fixture(t *testing.T) (Options, string) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.Mkdir(repo, 0700)
	testGit(t, repo, "init", "-q")
	testGit(t, repo, "config", "user.name", "Test")
	testGit(t, repo, "config", "user.email", "test@example.test")
	os.WriteFile(filepath.Join(repo, "code.txt"), []byte("base"), 0600)
	os.WriteFile(filepath.Join(repo, "package.json"), []byte(`{"scripts":{"test":"test x = x"}}`), 0600)
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "-qm", "base")
	testGit(t, repo, "remote", "add", "origin", "https://github.com/acme/widgets.git")
	lab := filepath.Join(root, "lab")
	os.MkdirAll(filepath.Join(lab, "formulas"), 0700)
	harness := filepath.Join(root, "harness")
	os.WriteFile(harness, []byte("#!/bin/sh\ncat > request.json\nprintf changed > code.txt\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"Implemented and tested\",\"paths\":[]}'\n"), 0700)
	command, _ := json.Marshal([]string{harness})
	recipe := "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: acme/widgets}\nharness:\n  binding: custom\n  command: " + string(command) + "\natoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"
	os.WriteFile(filepath.Join(lab, "formulas", "sample.yaml"), []byte(recipe), 0600)
	approveFixture(t, lab, recipe, "fixture")
	return Options{LabDir: lab, SourceRoot: repo, Issue: "12", TargetBranch: "main", Analyze: func(_ context.Context, _ formula.Binding, _ Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		id := ""
		for _, e := range s.Evidence {
			if e.Path == "package.json" {
				id = e.ID
			}
		}
		return readiness.Analysis{Summary: "Change code", Scope: []readiness.ScopedPath{{Path: "code.txt", Reason: "Implementation", Evidence: []string{"issue"}}, {Path: "request.json", Reason: "Fixture harness trace", Evidence: []string{id}}}, Acceptance: []readiness.Criterion{{Description: "Code contains changed", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "check", Category: "candidate", Argv: []string{"/bin/sh", "-c", "test \"$(cat code.txt)\" = changed"}, Dir: ".", TimeoutMS: 5000, Required: true, Definitions: []string{id}, Reason: "Fixture acceptance", Evidence: []string{"issue", id}}}}, nil
	}, ReadIssue: func(context.Context, string, int64, string) (Issue, error) {
		return Issue{Number: 12, Repo: "acme/widgets", Title: "Change code", Body: "Write changed", URL: "https://github.com/acme/widgets/issues/12", State: "OPEN"}, nil
	}}, repo
}
func TestIsolatedMoleculeLeavesLedgerAndWaits(t *testing.T) {
	o, repo := fixture(t)
	os.WriteFile(filepath.Join(repo, "code.txt"), []byte("user edits"), 0600)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "waiting" || r.Workspace.BaseCommit == "" {
		t.Fatalf("%+v", r)
	}
	data, _ := os.ReadFile(filepath.Join(repo, "code.txt"))
	if string(data) != "user edits" {
		t.Fatal("original checkout changed")
	}
	data, _ = os.ReadFile(filepath.Join(r.Workspace.Path, "code.txt"))
	if string(data) != "changed" {
		t.Fatal("harness did not execute in isolation")
	}
	data, err = os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Record
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.FormulaSnapshot.SHA256 == "" || saved.Atoms[2].Status != "done" || saved.Atoms[5].Status != "skipped" {
		t.Fatalf("%+v", saved)
	}
}
func TestWrongRepoAndDraftCannotExecute(t *testing.T) {
	o, _ := fixture(t)
	o.Issue = "other/repo#12"
	if _, err := Run(context.Background(), o); err == nil {
		t.Fatal("wrong repository accepted")
	}
	o.Issue = "12"
	os.Remove(filepath.Join(o.LabDir, "lab.json"))
	if _, err := Run(context.Background(), o); err == nil {
		t.Fatal("unapproved Formula accepted")
	}
}
func TestMismatchedIssueResponseBlocksHarness(t *testing.T) {
	o, _ := fixture(t)
	o.ReadIssue = func(context.Context, string, int64, string) (Issue, error) {
		return Issue{Number: 99, Repo: "acme/widgets"}, nil
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" || r.Atoms[1].Status != "skipped" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("harness ran despite failed intake")
	}
}

func TestFailedHarnessSkipsLaterAtoms(t *testing.T) {
	o, _ := fixture(t)
	script := filepath.Join(filepath.Dir(o.SourceRoot), "harness")
	os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf failed >&2\nexit 7\n"), 0700)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "failed" || r.Atoms[2].Status != "failed" || r.Atoms[3].Status != "skipped" || r.FinishedAt == "" {
		t.Fatalf("%+v", r)
	}
	if r.Atoms[2].ExitCode == nil || *r.Atoms[2].ExitCode != 7 {
		t.Fatal("lost harness exit code")
	}
}

func TestPreparationFailureRetainsLedgerWithoutCallingHarness(t *testing.T) {
	o, _ := fixture(t)
	o.PrepareWorkspace = func(context.Context, Workspace) error { return fmt.Errorf("dependency setup blocked") }
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "failed" || r.Atoms[2].Status != "failed" {
		t.Fatalf("%+v", r)
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("harness ran despite setup failure")
	}
	if _, err = os.Stat(filepath.Join(o.LabDir, "ledgers", r.ID+".json")); err != nil {
		t.Fatal(err)
	}
}

func runApprovedFixture(t *testing.T, o Options) (Record, error) {
	t.Helper()
	r, err := Run(context.Background(), o)
	if err != nil {
		return r, err
	}
	if r.Readiness == nil {
		t.Fatalf("fixture not ready: %+v", r)
	}
	o.Approve = r.Readiness.Plans[0].Digest
	return Run(context.Background(), o)
}

func TestBareFormulaNeverInvokesAnalysis(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			o, _ := fixture(t)
			os.RemoveAll(filepath.Join(o.LabDir, "formula-decisions"))
			if explicit {
				o.FormulaID = "sample"
			}
			calls := 0
			analyze := o.Analyze
			o.Analyze = func(c context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
				calls++
				return analyze(c, b, i, s)
			}
			_, err := Run(context.Background(), o)
			if err == nil || calls != 0 {
				t.Fatalf("unapproved recipe: error=%v analysis calls=%d", err, calls)
			}
		})
	}
}

func approveFixture(t *testing.T, dir, yaml, id string) {
	t.Helper()
	f, e := formula.Parse([]byte(yaml))
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := formula.FileHash(filepath.Join(dir, "lab.json"))
	if e != nil {
		t.Fatal(e)
	}
	target, e := formula.Inspect(dir, f.ID)
	before := "absent"
	if e == nil {
		before = target.Formula.SHA256
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: id, SourceKind: "test-review", SourceID: id}
	if e = formula.BeginActivation(dir, yaml, a, cfg, before); e != nil {
		t.Fatal(e)
	}
	if e = formula.ApplyActivation(dir, a); e != nil {
		t.Fatal(e)
	}
	if e = formula.CompleteActivation(dir, a); e != nil {
		t.Fatal(e)
	}
}

func TestRejectedRecipesHaveZeroAnalysisCalls(t *testing.T) {
	for _, kind := range []string{"edited", "missing", "injected", "historical", "restored", "stale"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := fixture(t)
			calls := 0
			original := o.Analyze
			o.Analyze = func(c context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
				calls++
				return original(c, b, i, s)
			}
			file := filepath.Join(o.LabDir, "formulas/sample.yaml")
			raw, _ := os.ReadFile(file)
			switch kind {
			case "edited":
				os.WriteFile(file, append(raw, []byte("# comment\n")...), 0600)
			case "missing":
				os.Remove(file)
			case "injected":
				o.FormulaID = "injected"
				os.WriteFile(filepath.Join(o.LabDir, "formulas/injected.yaml"), []byte(strings.Replace(string(raw), "id: sample", "id: injected", 1)), 0600)
			case "historical":
				approveFixture(t, o.LabDir, strings.Replace(string(raw), "id: sample", "id: next", 1), "next")
				o.FormulaID = "sample"
			case "restored":
				approveFixture(t, o.LabDir, string(raw)+"# newly approved\n", "next")
				os.WriteFile(file, raw, 0600)
			case "stale":
				os.RemoveAll(filepath.Join(o.LabDir, "formula-decisions"))
			}
			if _, e := Run(context.Background(), o); e == nil || calls != 0 {
				t.Fatalf("error=%v analysis calls=%d", e, calls)
			}
		})
	}
}
func TestCallbackMutationPreventsSubsequentHarness(t *testing.T) {
	for _, kind := range []string{"issue", "analysis", "preparation", "same-byte-approval"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := fixture(t)
			file := filepath.Join(o.LabDir, "formulas/sample.yaml")
			raw, _ := os.ReadFile(file)
			calls := 0
			original := o.Analyze
			mutate := func() {
				if e := os.WriteFile(file, append(raw, []byte("# changed\n")...), 0600); e != nil {
					t.Fatal(e)
				}
			}
			o.Analyze = func(c context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
				calls++
				a, e := original(c, b, i, s)
				if kind == "analysis" {
					mutate()
				}
				return a, e
			}
			if kind == "issue" {
				read := o.ReadIssue
				o.ReadIssue = func(c context.Context, repo string, n int64, dir string) (Issue, error) {
					i, e := read(c, repo, n, dir)
					mutate()
					return i, e
				}
			}
			r, e := Run(context.Background(), o)
			if e != nil {
				t.Fatal(e)
			}
			if kind == "issue" || kind == "analysis" {
				if r.Status != "blocked" || (kind == "issue" && calls != 0) {
					t.Fatalf("%+v calls=%d", r, calls)
				}
			} else {
				if r.Readiness == nil {
					t.Fatalf("no scope plan: %+v", r)
				}
				o.Approve = r.Readiness.Plans[0].Digest
				o.PrepareWorkspace = func(context.Context, Workspace) error {
					if kind == "same-byte-approval" {
						approveFixture(t, o.LabDir, string(raw), "reactivated")
					} else {
						mutate()
					}
					return nil
				}
				r, e = Run(context.Background(), o)
				if e != nil {
					t.Fatal(e)
				}
				if r.Status != "failed" {
					t.Fatalf("mutation ran coding: %+v", r)
				}
			}
			if _, e = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(e) {
				t.Fatal("coding ran after mutation", e)
			}
		})
	}
}

func TestExplicitCurrentFormulaStillWaitsForScope(t *testing.T) {
	o, _ := fixture(t)
	o.FormulaID = "sample"
	r, e := Run(context.Background(), o)
	if e != nil {
		t.Fatal(e)
	}
	if r.Readiness == nil || r.Readiness.Phase != "scope-waiting" {
		t.Fatalf("%+v", r)
	}
	if _, e = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(e) {
		t.Fatal("Formula approval bypassed scope", e)
	}
}
func TestResumeRejectionsNeverInvokeCoding(t *testing.T) {
	for _, kind := range []string{"edited", "missing", "historical", "stale"} {
		t.Run(kind, func(t *testing.T) {
			o, _ := fixture(t)
			r, e := Run(context.Background(), o)
			if e != nil {
				t.Fatal(e)
			}
			o.Approve = r.Readiness.Plans[0].Digest
			file := filepath.Join(o.LabDir, "formulas/sample.yaml")
			raw, _ := os.ReadFile(file)
			switch kind {
			case "edited":
				os.WriteFile(file, append(raw, []byte("# changed\n")...), 0600)
			case "missing":
				os.Remove(file)
			case "historical":
				approveFixture(t, o.LabDir, strings.Replace(string(raw), "id: sample", "id: next", 1), "next")
				o.FormulaID = "sample"
			case "stale":
				os.RemoveAll(filepath.Join(o.LabDir, "formula-decisions"))
			}
			if _, e = Run(context.Background(), o); e == nil {
				t.Fatal("rejected snapshot resumed")
			}
			if _, e = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(e) {
				t.Fatal("rejection invoked coding", e)
			}
		})
	}
}

func TestResumeRejectsRetiredLoadedSnapshot(t *testing.T) {
	o, _ := fixture(t)
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := formula.Load(o.LabDir, "")
	if err != nil {
		t.Fatal(err)
	}
	o.Approve = r.Readiness.Plans[0].Digest
	// A new same-ID activation arrives after Run's initial load, before resume.
	approveFixture(t, o.LabDir, loaded.Formula.YAML+"# newly approved snapshot\n", "replacement")
	_, err = resume(context.Background(), o, loaded, 12, "acme/widgets")
	if err == nil {
		t.Error("retired snapshot accepted by resume")
	}
	if _, err = os.Stat(filepath.Join(r.Workspace.Path, "request.json")); !os.IsNotExist(err) {
		t.Fatal("retired harness was invoked", err)
	}
}

func TestDeferredAllocationUsesFrozenBaseAndPreservesDirtySource(t *testing.T) {
	o, repo := fixture(t)
	base := testGit(t, repo, "rev-parse", "HEAD")
	analyze := o.Analyze
	o.Analyze = func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if s.Commit != base {
			t.Fatal("analysis base changed")
		}
		os.WriteFile(filepath.Join(repo, "code.txt"), []byte("later commit"), 0600)
		testGit(t, repo, "add", "code.txt")
		testGit(t, repo, "commit", "-qm", "later")
		os.WriteFile(filepath.Join(repo, "code.txt"), []byte("dirty user edits"), 0600)
		return analyze(ctx, b, i, s)
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Workspace.BaseCommit != base || testGit(t, r.Workspace.Path, "rev-parse", "HEAD") != base {
		t.Fatal("allocation re-resolved HEAD")
	}
	if raw, _ := os.ReadFile(filepath.Join(repo, "code.txt")); string(raw) != "dirty user edits" {
		t.Fatal("dirty source lost")
	}
	o.Approve = r.Readiness.Plans[0].Digest
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("changed source base approved")
	}
}

func TestAllocationFailureRetainsProposalAndRefusesApproval(t *testing.T) {
	o, _ := fixture(t)
	if err := os.WriteFile(filepath.Join(o.LabDir, "workspaces"), []byte("user-owned blocker"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "blocked" || r.Readiness == nil || r.CheckoutStatus != "failed" || len(r.AnalysisAttempts) != 1 {
		t.Fatalf("lost failed allocation: %+v", r)
	}
	o.Approve = r.Readiness.Plans[0].Digest
	if _, err = Run(context.Background(), o); err == nil {
		t.Fatal("failed allocation approved")
	}
	if _, err = Amend(context.Background(), o.LabDir, r.ID, o.Approve, r.Readiness.Plans[0].Plan, ""); err == nil {
		t.Fatal("absent checkout amended")
	}
	if raw, _ := os.ReadFile(filepath.Join(o.LabDir, "workspaces")); string(raw) != "user-owned blocker" {
		t.Fatal("user file altered")
	}
}

func TestAllocationDurabilityRetainsPlannedDestination(t *testing.T) {
	for _, mode := range []string{"interrupted", "final-save-failure"} {
		t.Run(mode, func(t *testing.T) {
			o, _ := fixture(t)
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			r.Workspace.Path, r.Workspace.Branch = "", ""
			var persisted Record
			writes := 0
			persist := func(v Record) error {
				writes++
				if writes == 2 {
					// Simulate JSON replacement followed by a Markdown write failure.
					if err := save(v); err != nil {
						return err
					}
					return fmt.Errorf("synthetic persistence failure")
				}
				persisted = v
				return save(v)
			}
			allocate := func(_ context.Context, w Workspace) error {
				if persisted.Workspace.Path != w.Path || persisted.Workspace.Branch != w.Branch || w.Path == "" || w.Branch == "" || persisted.CheckoutStatus != "allocating" {
					t.Fatal("planned destination not durable before allocation")
				}
				if mode == "interrupted" {
					panic("synthetic allocation interruption")
				}
				return nil
			}
			func() {
				defer func() {
					if mode == "interrupted" && recover() == nil {
						t.Fatal("missing interruption")
					}
				}()
				var outcome Record
				outcome, err = allocateRecord(context.Background(), o, r, allocate, persist)
				if err != nil && outcome.CheckoutStatus == "available" {
					t.Fatal("failed persistence returned runnable checkout")
				}
			}()
			if mode == "final-save-failure" && err == nil {
				t.Fatal("persistence failure hidden")
			}
			if persisted.Workspace.Path == "" || persisted.Workspace.Branch == "" || persisted.CheckoutStatus != "allocating" {
				t.Fatal("allocation identity lost")
			}
			if _, err := findPending(o.LabDir, r.Readiness.Plans[0].Digest); err == nil {
				t.Fatal("unavailable checkout approved")
			}
		})
	}
}
