package evaluation

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mergedChangeHarness is a stub coding harness that writes the merged change for the
// replayed PR (a brand-new package's non-test file) into the workspace and reports a
// completed outcome. Task A's base has no widget, so it writes widget; task B's base already
// holds A's widget, so it writes gadget. The hidden tests are applied only at grading.
const mergedChangeHarness = "#!/bin/sh\ncat >/dev/null\nif [ -f lab/widget/widget.go ]; then\n mkdir -p lab/gadget\n printf 'package gadget\\n\\nfunc New() int { return 9 }\\n' > lab/gadget/gadget.go\nelse\n mkdir -p lab/widget\n printf 'package widget\\n\\nfunc New() int { return 7 }\\n' > lab/widget/widget.go\nfi\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'\n"

// noopHarness reports completion without changing anything: the negative control.
const noopHarness = "#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'\n"

// e2eFormulaYAML mirrors the custom-binding Formula shape used by the cohort tests, pinned to
// the given coding-harness script. Two variants with different script paths differ in bytes,
// satisfying the distinct-variants rule while behaving identically.
func e2eFormulaYAML(script string) string {
	command, _ := json.Marshal([]string{script})
	return "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: owner/repo}\nharness:\n  binding: custom\n  command: " + string(command) + "\n" +
		"atoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"
}

// e2eCohort derives a runnable cohort file from the replay's cohort.stub.json: it strips the
// documentary source field, keeps only the named tasks and adds two variants, each a Formula
// pinned to a harness script with the given body.
func e2eCohort(t *testing.T, out, name string, tasks []string, bodies [2]string) CohortOptions {
	t.Helper()
	var stub map[string]any
	if err := json.Unmarshal(mustRead(t, filepath.Join(out, "cohort.stub.json")), &stub); err != nil {
		t.Fatal(err)
	}
	delete(stub, "source")
	keep := map[string]bool{}
	for _, n := range tasks {
		keep[n] = true
	}
	kept := []any{}
	for _, task := range stub["tasks"].([]any) {
		if keep[task.(map[string]any)["name"].(string)] {
			kept = append(kept, task)
		}
	}
	if len(kept) != len(tasks) {
		t.Fatalf("cohort stub lacks tasks %v: %v", tasks, stub["tasks"])
	}
	stub["tasks"] = kept
	variants := []any{}
	for i, v := range []string{"base", "cand"} {
		script := filepath.Join(out, name+"-"+v+".sh")
		if err := os.WriteFile(script, []byte(bodies[i]), 0o700); err != nil {
			t.Fatal(err)
		}
		yaml := e2eFormulaYAML(script)
		if err := os.WriteFile(filepath.Join(out, name+"-"+v+".yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		variants = append(variants, map[string]any{"name": v, "formula": name + "-" + v + ".yaml", "formulaSha256": Hash([]byte(yaml))})
	}
	stub["variants"] = variants
	raw, err := json.Marshal(stub)
	if err != nil {
		t.Fatal(err)
	}
	cohortPath := filepath.Join(out, name+".json")
	if err := os.WriteFile(cohortPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	o := CohortOptions{Cohort: cohortPath, SourceRoot: filepath.Join(out, "source"), Output: filepath.Join(t.TempDir(), name+"-results")}
	approveCohort(t, &o)
	return o
}

// TestReplayCohortEndToEnd is the required integration test (issue #164, point 5): it builds a
// replay from the fixture repo for two PRs that each add a new file and close their issue (B
// merged after A), then runs `evaluate --cohort` over a cohort derived from the replay's
// cohort.stub.json (with two trivially different stub Formulas) and <out>/source using the
// stub coding harness that writes the merged change. It asserts each attempt reaches the
// coding stage and is graded — never stopping at intake (the closed issue is frozen OPEN) or
// readiness (the new path cites the lab/go.mod manifest evidence) — and that the correct stub
// change scores hiddenAcceptancePass, while a no-op harness does not. It also proves task A's
// attempt cannot see PR B's base commit (which holds A's solution) or its replay ref.
func TestReplayCohortEndToEnd(t *testing.T) {
	f, base, def := newFixture(t)
	addPR := func(branch, rel, body, testRel, testBody string) string {
		f.git("checkout", "-q", "-b", branch)
		f.write(rel, body)
		f.write(testRel, testBody)
		f.git("add", ".")
		f.git("commit", "-qm", "add "+branch)
		f.git("checkout", "-q", def)
		f.git("merge", "-q", "--no-ff", "-m", "merge "+branch, branch)
		return f.head()
	}
	mergeA := addPR("widget", "lab/widget/widget.go", "package widget\n\nfunc New() int { return 7 }\n",
		"lab/widget/widget_test.go", "package widget\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {\n\tif New() != 7 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n")
	mergeB := addPR("gadget", "lab/gadget/gadget.go", "package gadget\n\nfunc New() int { return 9 }\n",
		"lab/gadget/gadget_test.go", "package gadget\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {\n\tif New() != 9 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n")

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/15/files": `[{"filename":"lab/widget/widget.go","status":"added","additions":3,"deletions":0},{"filename":"lab/widget/widget_test.go","status":"added","additions":7,"deletions":0}]`,
		"repos/owner/repo/pulls/15":       prJSON(mergeA, base, def, "Closes #5", "add widget"),
		"repos/owner/repo/issues/5":       `{"number":5,"title":"add widget","body":"Add a widget.","state":"closed"}`,
		"repos/owner/repo/pulls/16/files": `[{"filename":"lab/gadget/gadget.go","status":"added","additions":3,"deletions":0},{"filename":"lab/gadget/gadget_test.go","status":"added","additions":7,"deletions":0}]`,
		"repos/owner/repo/pulls/16":       prJSON(mergeB, mergeA, def, "Closes #6", "add gadget"),
		"repos/owner/repo/issues/6":       `{"number":6,"title":"add gadget","body":"Add a gadget.","state":"closed"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{15, 16}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 2 {
		t.Fatalf("replay did not build both PRs: built=%+v skipped=%+v", report.Built, report.Skipped)
	}
	baseB := report.Built[1].BaseCommit
	if baseB != mergeA {
		t.Fatalf("PR B base = %s, want PR A's merge %s", baseB, mergeA)
	}

	o := e2eCohort(t, out, "cohort", []string{"pr-15", "pr-16"}, [2]string{mergedChangeHarness, mergedChangeHarness})
	// Keep the trees so the per-attempt sources can be inspected for leakage.
	o.KeepWorkspaces = true
	r, err := RunCohort(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "complete" || len(r.Attempts) != 4 {
		t.Fatalf("status=%s attempts=%d", r.Status, len(r.Attempts))
	}
	for _, ca := range r.Attempts {
		a := ca.Attempt
		// Intake succeeded (the frozen, closed issue was accepted as OPEN).
		if a.Molecule.Issue.Number == 0 {
			t.Fatalf("attempt %s/%s did not reach intake: molecule=%+v error=%q", ca.Task, ca.Variant, a.Molecule, a.Error)
		}
		// Readiness produced a plan (the new path cited the manifest evidence, so scope
		// validation passed) and the coding harness ran to completion.
		if a.Molecule.Readiness == nil {
			t.Fatalf("attempt %s/%s stopped before readiness: error=%q", ca.Task, ca.Variant, a.Error)
		}
		if !completed(a) {
			t.Fatalf("attempt %s/%s did not reach the coding stage: status=%q error=%q", ca.Task, ca.Variant, a.Molecule.Status, a.Error)
		}
		// The correct stub change is graded as a hidden-acceptance pass.
		if !a.Correct {
			t.Fatalf("attempt %s/%s not graded correct: error=%q verification=%+v", ca.Task, ca.Variant, a.Error, a.Verification)
		}
	}
	for _, v := range r.Variants {
		if v.HiddenAcceptancePass != 2 {
			t.Fatalf("variant %s hiddenAcceptancePass = %d, want 2", v.Name, v.HiddenAcceptancePass)
		}
	}

	// Task A's attempt ran from its own source holding only A's base: PR B's base commit (which
	// contains A's solution) and refs/replay/pr-16 are unreachable from its workspace.
	for _, variant := range []string{"base", "cand"} {
		attemptSource := filepath.Join(o.Output, "attempts", "pr-15", variant, "rep-1", "source")
		if gitHas(attemptSource, baseB) {
			t.Fatalf("task A attempt (%s) can resolve PR B's base commit", variant)
		}
		if refs := gitOut(t, attemptSource, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "refs/replay/") {
			t.Fatalf("task A attempt (%s) lists replay refs:\n%s", variant, refs)
		}
		if out := gitOut(t, attemptSource, "rev-list", "--all", "--count"); out != "1" {
			t.Fatalf("task A attempt source rev-list --all --count = %q, want 1", out)
		}
		if refspec := gitTry(attemptSource, "config", "--get-all", "remote.origin.fetch"); refspec != noFetchRefspec {
			t.Fatalf("attempt source fetch refspec = %q", refspec)
		}
		assertNoPathLeak(t, filepath.Join(attemptSource, ".git"), filepath.Join(out, "source"))
		// The kept workspace is a worktree of that per-attempt source, so it cannot see B either.
		workspaces, _ := filepath.Glob(filepath.Join(o.Output, "attempts", "pr-15", variant, "rep-1", "lab", "workspaces", "*"))
		if len(workspaces) == 0 {
			t.Fatalf("no kept workspace for task A (%s)", variant)
		}
		for _, w := range workspaces {
			c := exec.Command("git", "cat-file", "-e", baseB+"^{commit}")
			c.Dir = w
			if c.Run() == nil {
				t.Fatalf("task A workspace %s can resolve PR B's base commit", w)
			}
		}
	}
	// --source itself is only read: no worktree is registered in it.
	if wl := gitOut(t, filepath.Join(out, "source"), "worktree", "list", "--porcelain"); strings.Count(wl, "worktree ") != 1 {
		t.Fatalf("--source gained worktrees:\n%s", wl)
	}

	// Negative control: a no-op harness runs (the molecule may refuse its no-change
	// completion) but is never graded a pass.
	neg := e2eCohort(t, out, "negative", []string{"pr-15"}, [2]string{mergedChangeHarness, noopHarness})
	nr, err := RunCohort(context.Background(), neg)
	if err != nil {
		t.Fatal(err)
	}
	if nr.Status != "complete" || len(nr.Attempts) != 2 {
		t.Fatalf("negative status=%s attempts=%d", nr.Status, len(nr.Attempts))
	}
	for _, ca := range nr.Attempts {
		if len(ca.Attempt.Molecule.HarnessAttempts) == 0 {
			t.Fatalf("negative attempt %s did not reach the coding stage: error=%q", ca.Variant, ca.Attempt.Error)
		}
		if want := ca.Variant == "base"; ca.Attempt.Correct != want {
			t.Fatalf("negative attempt %s graded correct=%v, want %v (error=%q)", ca.Variant, ca.Attempt.Correct, want, ca.Attempt.Error)
		}
	}
	for _, v := range nr.Variants {
		want := 0
		if v.Name == "base" {
			want = 1
		}
		if v.HiddenAcceptancePass != want {
			t.Fatalf("negative variant %s hiddenAcceptancePass = %d, want %d", v.Name, v.HiddenAcceptancePass, want)
		}
	}
	// Without --keep-workspaces every per-attempt source is removed after its attempt, with no
	// spurious cleanup error or manual-removal note.
	assertCleanCleanup(t, nr)
	for _, variant := range []string{"base", "cand"} {
		if _, e := os.Stat(filepath.Join(neg.Output, "attempts", "pr-15", variant, "rep-1", "source")); !os.IsNotExist(e) {
			t.Fatalf("per-attempt source kept without --keep-workspaces: %v", e)
		}
	}
}
