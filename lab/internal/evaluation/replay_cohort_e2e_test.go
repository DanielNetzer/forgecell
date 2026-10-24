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

// modifiedTestHarness writes the merged change of a replayed PR that changes both the
// implementation and its existing test.
var modifiedTestHarness = "#!/bin/sh\ncat >/dev/null\ncat > lab/calc/calc.go <<'FORGECELL_EOF'\n" + valueGo(2) + "FORGECELL_EOF\ncat > lab/calc/calc_test.go <<'FORGECELL_EOF'\n" + valueTest(2) + "FORGECELL_EOF\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'\n"

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
	skipToolchainInShort(t)
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

// flakyRegressionHarness writes task A's merged widget change plus a candidate version of
// the PR's own test file holding an extra failing test, standing in for a flake in the
// replayed code: hidden acceptance (graded with the PR's real test file) passes while the
// go-test regression check fails.
const flakyRegressionHarness = "#!/bin/sh\ncat >/dev/null\nmkdir -p lab/widget\nprintf 'package widget\\n\\nfunc New() int { return 7 }\\n' > lab/widget/widget.go\nprintf 'package widget\\n\\nimport \"testing\"\\n\\nfunc TestNew(t *testing.T) {\\n\\tif New() != 7 {\\n\\t\\tt.Fatal(\"no\")\\n\\t}\\n}\\n\\nfunc TestFlakyHistorical(t *testing.T) { t.Fatal(\"flake\") }\\n' > lab/widget/widget_test.go\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'\n"

// wrongWidgetHarness writes a widget that hidden acceptance rejects.
const wrongWidgetHarness = "#!/bin/sh\ncat >/dev/null\nmkdir -p lab/widget\nprintf 'package widget\\n\\nfunc New() int { return 8 }\\n' > lab/widget/widget.go\nprintf '%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}'\n"

// TestReplayCohortHiddenAcceptanceIndependentOfRegressionFailure (issue #178): hidden
// acceptance runs first, so a failing regression check cannot hide it. An attempt whose
// acceptance passes while go-test fails records HiddenAcceptance true, Correct false and
// the failing test names, and the report counts it as a hidden-acceptance pass. A rejected
// change records false; a harness that never completes records nil and is excluded from
// HiddenAcceptanceN.
func TestReplayCohortHiddenAcceptanceIndependentOfRegressionFailure(t *testing.T) {
	skipToolchainInShort(t)
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "widget")
	f.write("lab/widget/widget.go", "package widget\n\nfunc New() int { return 7 }\n")
	f.write("lab/widget/widget_test.go", "package widget\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {\n\tif New() != 7 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n")
	f.git("add", ".")
	f.git("commit", "-qm", "add widget")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge widget", "widget")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/15/files": `[{"filename":"lab/widget/widget.go","status":"added","additions":3,"deletions":0},{"filename":"lab/widget/widget_test.go","status":"added","additions":7,"deletions":0}]`,
		"repos/owner/repo/pulls/15":       prJSON(merge, base, def, "Closes #5", "add widget"),
		"repos/owner/repo/issues/5":       `{"number":5,"title":"add widget","body":"Add a widget.","state":"closed"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{15}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 1 {
		t.Fatalf("replay did not build the PR: built=%+v skipped=%+v", report.Built, report.Skipped)
	}
	run := func(name string, bodies [2]string) (CohortReport, CohortOptions) {
		t.Helper()
		o := e2eCohort(t, out, name, []string{"pr-15"}, bodies)
		r, err := RunCohort(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "complete" || len(r.Attempts) != 2 {
			t.Fatalf("%s: status=%s attempts=%d", name, r.Status, len(r.Attempts))
		}
		// Hidden acceptance and Correct come from consistent runs.
		for _, ca := range r.Attempts {
			if ca.Correct && (ca.HiddenAcceptance == nil || !*ca.HiddenAcceptance) {
				t.Fatalf("%s: %s correct without a passing hidden acceptance", name, ca.Variant)
			}
		}
		return r, o
	}
	byVariant := func(r CohortReport) (map[string]Attempt, map[string]VariantSummary) {
		attempts, variants := map[string]Attempt{}, map[string]VariantSummary{}
		for _, ca := range r.Attempts {
			attempts[ca.Variant] = ca.Attempt
		}
		for _, v := range r.Variants {
			variants[v.Name] = v
		}
		return attempts, variants
	}

	r, o := run("flaky", [2]string{flakyRegressionHarness, wrongWidgetHarness})
	attempts, variants := byVariant(r)
	flaky := attempts["base"]
	if flaky.HiddenAcceptance == nil || !*flaky.HiddenAcceptance || flaky.Correct {
		t.Fatalf("flaky attempt hiddenAcceptance=%v correct=%v error=%q verification=%+v", flaky.HiddenAcceptance, flaky.Correct, flaky.Error, flaky.Molecule.Verification)
	}
	if v := flaky.Molecule.Verification; v == nil || len(v.Checks) < 2 || v.Checks[0].ID != "acceptance" || v.Checks[1].ID != "go-test" || v.Checks[1].Result.OK {
		t.Fatalf("candidate verification did not run acceptance first then fail go-test: %+v", flaky.Molecule.Verification)
	}
	if got := strings.Join(flaky.RegressionFailedTests, ","); got != "TestFlakyHistorical" {
		t.Fatalf("regression failed tests = %q", got)
	}
	if v := variants["base"]; v.HiddenAcceptancePass != 1 || v.HiddenAcceptanceN != 1 || v.Correct != 0 {
		t.Fatalf("flaky variant counts: %+v", v)
	}
	wrong := attempts["cand"]
	if wrong.HiddenAcceptance == nil || *wrong.HiddenAcceptance || wrong.Correct || wrong.RegressionFailedTests != nil {
		t.Fatalf("rejected attempt hiddenAcceptance=%v correct=%v regression=%v error=%q", wrong.HiddenAcceptance, wrong.Correct, wrong.RegressionFailedTests, wrong.Error)
	}
	if v := variants["cand"]; v.HiddenAcceptancePass != 0 || v.HiddenAcceptanceN != 1 {
		t.Fatalf("rejected variant counts: %+v", v)
	}
	md := string(mustRead(t, filepath.Join(o.Output, "report.md")))
	for _, want := range []string{"Hidden-acceptance pass 1 of 1-ran (100%); correct (acceptance and every required check): 0", "  - regression failed tests: TestFlakyHistorical\n"} {
		if !strings.Contains(md, want) {
			t.Fatalf("report.md missing %q:\n%s", want, md)
		}
	}

	r, _ = run("incomplete", [2]string{flakyRegressionHarness, failingHarness})
	attempts, variants = byVariant(r)
	if a := attempts["cand"]; completed(a) || a.HiddenAcceptance != nil {
		t.Fatalf("incomplete attempt completed=%v hiddenAcceptance=%v", completed(a), a.HiddenAcceptance)
	}
	if v := variants["cand"]; v.HiddenAcceptanceN != 0 || v.HiddenAcceptancePass != 0 || v.N.Attempts != 1 {
		t.Fatalf("incomplete variant counts: %+v", v)
	}
	if v := variants["base"]; v.HiddenAcceptancePass != 1 || v.HiddenAcceptanceN != 1 {
		t.Fatalf("flaky variant counts in second cohort: %+v", v)
	}
}

// TestReplayCohortExemptsModifiedExistingTest replays a PR that modifies an existing test
// (issue #176). The protected regression keeps the candidate's version of the PR's own test
// file, which hidden acceptance grades, so the merged solution is graded correct with the
// protected run passing. With the exemption stripped the protected run restores the old test
// and fails, so no attempt is graded correct.
func TestReplayCohortExemptsModifiedExistingTest(t *testing.T) {
	skipToolchainInShort(t)
	f, _, def := newFixture(t)
	f.write("lab/calc/calc_test.go", valueTest(1))
	f.git("add", ".")
	f.git("commit", "-qm", "existing test")
	base := f.head()
	f.git("checkout", "-q", "-b", "two")
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", valueTest(2))
	f.git("add", ".")
	f.git("commit", "-qm", "value two")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge two", "two")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/20/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"modified","additions":1,"deletions":1}]`,
		"repos/owner/repo/pulls/20":       prJSON(merge, base, def, "Closes #8", "Make Value return 2"),
		"repos/owner/repo/issues/8":       `{"number":8,"title":"Make Value return 2","body":"Value should return 2.","state":"closed"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{20}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 1 {
		t.Fatalf("replay did not build the PR: built=%+v skipped=%+v", report.Built, report.Skipped)
	}

	run := func(name string) CohortReport {
		t.Helper()
		o := e2eCohort(t, out, name, []string{"pr-20"}, [2]string{modifiedTestHarness, modifiedTestHarness})
		r, err := RunCohort(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != "complete" || len(r.Attempts) != 2 {
			t.Fatalf("%s: status=%s attempts=%d", name, r.Status, len(r.Attempts))
		}
		return r
	}
	// When every changed test is exempt the protected tree is the candidate tree, so the
	// candidate run is the protected run and no separate one is recorded.
	protectedPassed := func(ca CohortAttempt) bool {
		v := ca.Attempt.Molecule.Verification
		return v != nil && v.RequiredChecksPassed && (v.Protected == nil || (v.Protected.RequiredChecksPassed && v.Protected.Error == ""))
	}

	r := run("exempt")
	for _, ca := range r.Attempts {
		a := ca.Attempt
		if a.Molecule.Readiness == nil || len(a.Molecule.Readiness.Plans) == 0 {
			t.Fatalf("attempt %s stopped before readiness: error=%q", ca.Variant, a.Error)
		}
		if got := a.Molecule.Readiness.Plans[0].Plan.ProtectedExemptPaths; strings.Join(got, ",") != "lab/calc/calc_test.go" {
			t.Fatalf("attempt %s plan exempts %v", ca.Variant, got)
		}
		if !protectedPassed(ca) {
			t.Fatalf("attempt %s protected run did not pass: error=%q verification=%+v", ca.Variant, a.Error, a.Molecule.Verification)
		}
		if !a.Correct {
			t.Fatalf("attempt %s not graded correct: error=%q verification=%+v", ca.Variant, a.Error, a.Verification)
		}
	}
	for _, v := range r.Variants {
		if v.HiddenAcceptancePass != 1 {
			t.Fatalf("variant %s hiddenAcceptancePass = %d, want 1", v.Name, v.HiddenAcceptancePass)
		}
	}

	original := protectedExemptPaths
	protectedExemptPaths = func(Plan) []string { return nil }
	t.Cleanup(func() { protectedExemptPaths = original })
	stripped := run("stripped")
	for _, ca := range stripped.Attempts {
		a := ca.Attempt
		if a.Molecule.Readiness == nil || len(a.Molecule.Readiness.Plans) == 0 || len(a.Molecule.Readiness.Plans[0].Plan.ProtectedExemptPaths) != 0 {
			t.Fatalf("stripped attempt %s still has an exemption or no plan: error=%q", ca.Variant, a.Error)
		}
		// The restored old test must fail in the protected run itself, not merely leave
		// the attempt ungraded.
		if v := a.Molecule.Verification; v == nil || v.Protected == nil || v.Protected.RequiredChecksPassed {
			t.Fatalf("stripped attempt %s protected run did not fail: error=%q verification=%+v", ca.Variant, a.Error, a.Molecule.Verification)
		}
		if a.Correct {
			t.Fatalf("stripped attempt %s graded correct", ca.Variant)
		}
	}
}
