package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// fixtureRepo is a tiny git fixture: a base commit whose calc.Value returns 1, plus whatever
// shape a test builds on top of it. The feature's test file is the hidden acceptance.
type fixtureRepo struct {
	t   *testing.T
	dir string
}

func valueGo(n int) string {
	return fmt.Sprintf("package calc\n\n// Value is the implementation under test.\nfunc Value() int { return %d }\n", n)
}

func valueTest(n int) string {
	return fmt.Sprintf("package calc\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) {\n\tif Value() != %d {\n\t\tt.Fatalf(\"got %%d\", Value())\n\t}\n}\n", n)
}

// newFixture initializes a repo with a go module and a base commit (Value()==1) on the
// default branch, and returns the fixture plus the base SHA and default branch name.
func newFixture(t *testing.T) (*fixtureRepo, string, string) {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "lab", "calc"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fixtureRepo{t: t, dir: repo}
	f.git("init", "-q")
	f.git("config", "user.email", "test@example.test")
	f.git("config", "user.name", "Test")
	f.write("lab/go.mod", "module forgecellreplayfixture\n\ngo 1.21\n")
	f.write("lab/calc/calc.go", valueGo(1))
	f.git("add", ".")
	f.git("commit", "-qm", "base")
	return f, f.head(), f.git("rev-parse", "--abbrev-ref", "HEAD")
}

func (f *fixtureRepo) git(args ...string) string {
	f.t.Helper()
	c := exec.Command("git", args...)
	c.Dir = f.dir
	b, e := c.CombinedOutput()
	if e != nil {
		f.t.Fatalf("git %v: %s %v", args, b, e)
	}
	return strings.TrimSpace(string(b))
}

func (f *fixtureRepo) write(rel, body string) {
	f.t.Helper()
	p := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixtureRepo) rm(rel string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.dir, filepath.FromSlash(rel))); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixtureRepo) head() string { return f.git("rev-parse", "HEAD") }

// ghFixture serves canned JSON for the endpoints BuildReplay reads. It matches the request
// path with its query string stripped against its keys exactly, so map iteration order can
// never decide which response a request receives (otherwise the key "…/pulls/N" could answer
// the "…/pulls/N/files?…" request), and it fails on any unmatched request.
func ghFixture(responses map[string]string) func(context.Context, string) ([]byte, error) {
	return func(_ context.Context, endpoint string) ([]byte, error) {
		path := endpoint
		if i := strings.IndexByte(path, '?'); i >= 0 {
			path = path[:i]
		}
		if body, ok := responses[path]; ok {
			return []byte(body), nil
		}
		return nil, fmt.Errorf("unexpected endpoint %q", endpoint)
	}
}

func prJSON(merge, base, ref, body, title string) string {
	return fmt.Sprintf(`{"merged":true,"merge_commit_sha":%q,"body":%q,"title":%q,"state":"closed","base":{"sha":%q,"ref":%q}}`, merge, body, title, base, ref)
}

func TestReplayBuildsTaskFromMergedPR(t *testing.T) {
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", valueTest(2))
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/7/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/7":       prJSON(merge, base, def, "Closes #42", "Make Value return 2"),
		"repos/owner/repo/issues/42":     `{"number":42,"title":"Make Value return 2","body":"Value should return 2.","state":"closed"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{7}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 1 || len(report.Skipped) != 0 {
		t.Fatalf("built=%+v skipped=%+v", report.Built, report.Skipped)
	}
	built := report.Built[0]
	if built.BaseCommit != base {
		t.Fatalf("baseCommit = %s, want %s", built.BaseCommit, base)
	}
	if !strings.Contains(built.BaseRule, "merge-base") {
		t.Fatalf("baseRule = %q", built.BaseRule)
	}

	hiddenDir := filepath.Join(out, "hidden", "pr-7")
	planRaw := mustRead(t, filepath.Join(hiddenDir, "plan.json"))
	if Hash(planRaw) != built.PlanSHA256 {
		t.Fatal("plan sha mismatch")
	}
	var plan Plan
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		t.Fatal(err)
	}
	if err := validatePlan(plan); err != nil {
		t.Fatalf("generated plan invalid: %v", err)
	}
	if plan.TargetBranch != def || !plan.RequireInitialFailure {
		t.Fatalf("plan fields: %+v", plan)
	}
	wantAllowed := []string{"lab/calc/calc.go", "lab/calc/calc_test.go"}
	if strings.Join(plan.AllowedFiles, ",") != strings.Join(wantAllowed, ",") {
		t.Fatalf("allowedFiles = %v", plan.AllowedFiles)
	}
	if strings.Join(plan.OriginalCheckFiles, ",") != "lab/calc/calc_test.go" {
		t.Fatalf("originalCheckFiles = %v", plan.OriginalCheckFiles)
	}
	// The replay plan exempts exactly the PR's changed test files.
	if strings.Join(plan.CandidateTestFiles, ",") != "lab/calc/calc_test.go" {
		t.Fatalf("candidateTestFiles = %v", plan.CandidateTestFiles)
	}
	if err := validateCandidateTestFiles(plan); err != nil {
		t.Fatalf("generated candidate test files invalid: %v", err)
	}
	if got := protectedExemptPaths(plan); strings.Join(got, ",") != strings.Join(plan.CandidateTestFiles, ",") {
		t.Fatalf("replay plan exempts %v, want candidateTestFiles %v", got, plan.CandidateTestFiles)
	}
	acc := 0
	for _, c := range plan.Checks {
		if c.Name == "acceptance" {
			acc++
		}
	}
	if acc != 1 {
		t.Fatalf("expected exactly one acceptance check, got %d", acc)
	}

	// issue.json parses as a molecule.Issue the cohort runner will accept.
	issueRaw := mustRead(t, filepath.Join(hiddenDir, "issue.json"))
	if Hash(issueRaw) != plan.TaskSHA256 {
		t.Fatal("issue sha mismatch")
	}
	var issue molecule.Issue
	if err := json.Unmarshal(issueRaw, &issue); err != nil {
		t.Fatal(err)
	}
	if issue.Number != 42 || issue.Repo != "owner/repo" || issue.URL != "https://github.com/owner/repo/issues/42" {
		t.Fatalf("issue identity: %+v", issue)
	}
	// The source issue is closed, but issue.json freezes State as OPEN so intake accepts it.
	if issue.State != "OPEN" {
		t.Fatalf("issue.json State = %q, want OPEN", issue.State)
	}
	// approval.md records the live (closed) state separately.
	if ap := string(mustRead(t, filepath.Join(out, "tasks", "pr-7", "approval.md"))); !strings.Contains(ap, "Live issue state: CLOSED") {
		t.Fatalf("approval.md missing live state:\n%s", ap)
	}
	if Hash(mustRead(t, filepath.Join(hiddenDir, "acceptance.sh"))) != plan.AcceptanceSHA256 {
		t.Fatal("acceptance sha mismatch")
	}
	if _, err := os.Stat(filepath.Join(hiddenDir, "files", "lab", "calc", "calc_test.go")); err != nil {
		t.Fatalf("raw hidden test not captured: %v", err)
	}

	// task.md carries the issue text under tasks/; it must never leak hidden test content.
	taskDir := filepath.Join(out, "tasks", "pr-7")
	taskMD := mustRead(t, filepath.Join(taskDir, "task.md"))
	if !strings.Contains(string(taskMD), "Value should return 2.") {
		t.Fatal("task.md missing issue body")
	}
	// Leakage: nothing under tasks/ may contain hidden test content or be a test file.
	assertNoHiddenLeak(t, filepath.Join(out, "tasks"))

	// cohort.stub.json pins the built task by sha256 at its hidden plan path.
	stubRaw := mustRead(t, filepath.Join(out, "cohort.stub.json"))
	var stub CohortFile
	if err := json.Unmarshal(stubRaw, &stub); err != nil {
		t.Fatal(err)
	}
	if len(stub.Tasks) != 1 || stub.Tasks[0].Name != "pr-7" || stub.Tasks[0].PlanSHA256 != built.PlanSHA256 {
		t.Fatalf("cohort stub tasks = %+v", stub.Tasks)
	}
	if stub.Tasks[0].Plan != filepath.Join("hidden", "pr-7", "plan.json") {
		t.Fatalf("cohort stub plan path = %q", stub.Tasks[0].Plan)
	}

	// <out>/source is a non-bare, history-free checkout usable directly as the cohort
	// --source: it has the base commit but not the merge commit.
	src := filepath.Join(out, "source")
	if report.Source != src {
		t.Fatalf("report.Source = %q, want %q", report.Source, src)
	}
	if !gitHas(src, base) {
		t.Fatal("source is missing the base commit")
	}
	if gitHas(src, merge) {
		t.Fatal("source leaked the merge commit")
	}
	// Non-bare: a work tree with a clean status.
	if out := gitOut(t, src, "rev-parse", "--is-bare-repository"); out != "false" {
		t.Fatalf("source is bare: %q", out)
	}
	if out := gitOut(t, src, "status", "--porcelain"); out != "" {
		t.Fatalf("source work tree is dirty:\n%s", out)
	}
	// Exactly the distinct built base commits (one here), and a detached HEAD at that base.
	if out := gitOut(t, src, "rev-list", "--all", "--count"); out != "1" {
		t.Fatalf("source rev-list --all --count = %q, want 1", out)
	}
	if out := gitOut(t, src, "rev-parse", "HEAD"); out != base {
		t.Fatalf("source HEAD = %q, want base %q", out, base)
	}
	// symbolic-ref -q HEAD exits non-zero (empty output) when HEAD is detached.
	if out := gitTry(src, "symbolic-ref", "-q", "HEAD"); out != "" {
		t.Fatalf("source HEAD is not detached: %q", out)
	}
	// origin is set to the repo URL whose only fetch refspec matches nothing on GitHub.
	if out := gitOut(t, src, "remote", "get-url", "origin"); out != "https://github.com/owner/repo.git" {
		t.Fatalf("source origin = %q", out)
	}
	if out := gitTry(src, "config", "--get-all", "remote.origin.fetch"); out != noFetchRefspec {
		t.Fatalf("source fetch refspec = %q, want only %q", out, noFetchRefspec)
	}
	// No FETCH_HEAD (or anything else under .git) records the full-history source's path.
	assertNoPathLeak(t, filepath.Join(src, ".git"), f.dir)
	// Only the replay ref remains besides a detached HEAD.
	if out := gitOut(t, src, "for-each-ref", "--format=%(refname)"); out != "refs/replay/pr-7" {
		t.Fatalf("source refs = %q, want only refs/replay/pr-7", out)
	}

	// The source checkout is untouched (still at the merge tree with Value()==2).
	if raw := mustRead(t, filepath.Join(f.dir, "lab", "calc", "calc.go")); !strings.Contains(string(raw), "return 2") {
		t.Fatal("source checkout changed")
	}
}

func TestReplayBuildsFromSquashMerge(t *testing.T) {
	f, base, def := newFixture(t)
	// A squash/rebase merge is a single-parent commit on the default branch.
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", valueTest(2))
	f.git("add", ".")
	f.git("commit", "-qm", "squash: make Value return 2")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/5/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/5":       prJSON(merge, base, def, "Closes #40", "squash"),
		"repos/owner/repo/issues/40":     `{"number":40,"title":"squash","body":"Return 2."}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{5}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 1 {
		t.Fatalf("built=%+v skipped=%+v", report.Built, report.Skipped)
	}
	if report.Built[0].BaseCommit != base {
		t.Fatalf("baseCommit = %s, want %s", report.Built[0].BaseCommit, base)
	}
	if !strings.Contains(report.Built[0].BaseRule, "first-parent") {
		t.Fatalf("baseRule = %q", report.Built[0].BaseRule)
	}
}

func TestReplayHandlesRenamedTestFile(t *testing.T) {
	f, base, def := newFixture(t)
	// Base carries an existing test file so the rename has an old path to remove.
	f.write("lab/calc/calc_test.go", valueTest(1))
	f.git("add", ".")
	f.git("commit", "-qm", "add base test")
	base = f.head()

	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	f.rm("lab/calc/calc_test.go")
	f.write("lab/calc/calc_renamed_test.go", valueTest(2))
	f.git("add", "-A")
	f.git("commit", "-qm", "rename and change")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/6/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_renamed_test.go","status":"renamed","previous_filename":"lab/calc/calc_test.go","additions":1,"deletions":1}]`,
		"repos/owner/repo/pulls/6":       prJSON(merge, base, def, "Closes #41", "rename"),
		"repos/owner/repo/issues/41":     `{"number":41,"title":"rename","body":"Rename the test."}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{6}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 1 {
		t.Fatalf("built=%+v skipped=%+v", report.Built, report.Skipped)
	}
	plan := readPlan(t, filepath.Join(out, "hidden", "pr-6", "plan.json"))
	if strings.Join(plan.OriginalCheckFiles, ",") != "lab/calc/calc_renamed_test.go" {
		t.Fatalf("originalCheckFiles = %v", plan.OriginalCheckFiles)
	}
	for _, want := range []string{"lab/calc/calc.go", "lab/calc/calc_renamed_test.go", "lab/calc/calc_test.go"} {
		if !contains(plan.AllowedFiles, want) {
			t.Fatalf("allowedFiles %v missing %s", plan.AllowedFiles, want)
		}
	}
}

// TestReplayBuildsTestCompileTask: the hidden test calls a function the PR adds, so the test
// package fails to compile at base while the non-test package still builds. That is a
// test-compile initial failure: the PR is built, not skipped, and the task gains an interface
// contract listing the new function's signature.
func TestReplayBuildsTestCompileTask(t *testing.T) {
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(1)+"\nfunc Value2() int { return 2 }\n")
	f.write("lab/calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestValue2(t *testing.T) {\n\tif Value2() != 2 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n")
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/12/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":2,"deletions":0},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/12":       prJSON(merge, base, def, "Closes #4", "add Value2"),
		"repos/owner/repo/issues/4":       `{"number":4,"title":"add Value2","body":"Add Value2."}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{12}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 1 || len(report.Skipped) != 0 {
		t.Fatalf("built=%+v skipped=%+v", report.Built, report.Skipped)
	}
	if report.Built[0].TaskSource != "current-issue-body+interface-contract" {
		t.Fatalf("taskSource = %q", report.Built[0].TaskSource)
	}

	// issue.json's body carries the exact contract heading and the resolved signature.
	issueRaw := mustRead(t, filepath.Join(out, "hidden", "pr-12", "issue.json"))
	var issue molecule.Issue
	if err := json.Unmarshal(issueRaw, &issue); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issue.Body, "## Interface contract (from the merged change)") {
		t.Fatalf("issue body missing contract heading:\n%s", issue.Body)
	}
	if !strings.Contains(issue.Body, "// package forgecellreplayfixture/calc") {
		t.Fatalf("issue body missing package header:\n%s", issue.Body)
	}
	if !strings.Contains(issue.Body, "func Value2() int") {
		t.Fatalf("issue body missing signature:\n%s", issue.Body)
	}

	// task.md matches the issue body.
	taskMD := string(mustRead(t, filepath.Join(out, "tasks", "pr-12", "task.md")))
	if !strings.Contains(taskMD, "func Value2() int") || !strings.Contains(taskMD, "## Interface contract (from the merged change)") {
		t.Fatalf("task.md missing contract:\n%s", taskMD)
	}

	// approval.md records the test-compile kind and the extracted identifier.
	approvalMD := string(mustRead(t, filepath.Join(out, "tasks", "pr-12", "approval.md")))
	if !strings.Contains(approvalMD, "Initial failure: test-compile") {
		t.Fatalf("approval.md missing test-compile kind:\n%s", approvalMD)
	}
	if !strings.Contains(approvalMD, "`Value2`") {
		t.Fatalf("approval.md missing identifier:\n%s", approvalMD)
	}

	// The contract must never leak into the human task directory as runnable test content,
	// and the hidden test content stays under hidden/.
	assertNoHiddenLeak(t, filepath.Join(out, "tasks"))
}

// TestReplaySkipsBuildErrorAtBase: the non-test package itself fails to build at base (not the
// test), so the failure is a genuine build error and the PR is skipped.
func TestReplaySkipsBuildErrorAtBase(t *testing.T) {
	f, _, def := newFixture(t)
	// Make a base whose non-test calc.go does not compile.
	f.write("lab/calc/calc.go", "package calc\n\nfunc Value() int { return missingSymbol }\n")
	f.git("add", ".")
	f.git("commit", "-qm", "broken base")
	base := f.head()

	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", valueTest(2))
	f.git("add", ".")
	f.git("commit", "-qm", "fix and test")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/12/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/12":       prJSON(merge, base, def, "Closes #4", "fix Value"),
		"repos/owner/repo/issues/4":       `{"number":4,"title":"fix Value","body":"Fix Value."}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{12}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 0 || len(report.Skipped) != 1 || report.Skipped[0].PR != 12 {
		t.Fatalf("report = %+v", report)
	}
	if !strings.HasPrefix(report.Skipped[0].Reason, "build failed at base") {
		t.Fatalf("skip reason = %q", report.Skipped[0].Reason)
	}
}

// TestReplayBuildsBrandNewPackage: the PR creates a brand-new package with its test. At base
// the package directory has no non-test Go files, which is a test-compile initial failure, so
// the PR is built.
func TestReplayBuildsBrandNewPackage(t *testing.T) {
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/widget/widget.go", "package widget\n\nfunc New() int { return 7 }\n")
	f.write("lab/widget/widget_test.go", "package widget\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {\n\tif New() != 7 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n")
	f.git("add", ".")
	f.git("commit", "-qm", "add widget")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/15/files": `[{"filename":"lab/widget/widget.go","status":"added","additions":3,"deletions":0},{"filename":"lab/widget/widget_test.go","status":"added","additions":7,"deletions":0}]`,
		"repos/owner/repo/pulls/15":       prJSON(merge, base, def, "Closes #5", "add widget"),
		"repos/owner/repo/issues/5":       `{"number":5,"title":"add widget","body":"Add a widget."}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{15}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 1 || len(report.Skipped) != 0 {
		t.Fatalf("built=%+v skipped=%+v", report.Built, report.Skipped)
	}
	issue := molecule.Issue{}
	if err := json.Unmarshal(mustRead(t, filepath.Join(out, "hidden", "pr-15", "issue.json")), &issue); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issue.Body, "// package forgecellreplayfixture/widget") || !strings.Contains(issue.Body, "func New() int") {
		t.Fatalf("issue body missing widget contract:\n%s", issue.Body)
	}
}

// TestReplaySkipsTestMainPanicAtBase: the non-test package builds and the hidden test compiles,
// but its TestMain panics before any test runs. That is a runtime build-failure with no compile
// errors, so it is not reclassified as test-compile and stays the "build failed at base" skip.
func TestReplaySkipsTestMainPanicAtBase(t *testing.T) {
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestMain(m *testing.M) { panic(\"boom\") }\n\nfunc TestValue(t *testing.T) {\n\tif Value() != 2 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n")
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/16/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":9,"deletions":0}]`,
		"repos/owner/repo/pulls/16":       prJSON(merge, base, def, "Closes #6", "panic test"),
		"repos/owner/repo/issues/6":       `{"number":6,"title":"panic test","body":"x"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{16}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 0 || len(report.Skipped) != 1 || report.Skipped[0].PR != 16 {
		t.Fatalf("report = %+v", report)
	}
	// The non-test package builds and the test compiles, so the build failure yields no compile
	// errors and no interface identifiers: the distinct skip reason is used (prefix preserved; the
	// original build detail is appended after the phrase).
	if !strings.HasPrefix(report.Skipped[0].Reason, "build failed at base: no interface identifiers recognized") {
		t.Fatalf("skip reason = %q", report.Skipped[0].Reason)
	}
}

// TestReplaySkipsWhenOnlyPredeclaredIdents: at base the hidden test fails to compile only because
// of a type mismatch against a predeclared type (it assigns an int-returning F to a string). The
// non-test package builds, so the run is classified test-compile, but the single compile error
// names only the predeclared `string`, which is dropped: no interface identifier is recognized and
// the PR is skipped (prefix preserved, build detail appended).
func TestReplaySkipsWhenOnlyPredeclaredIdents(t *testing.T) {
	f, _, def := newFixture(t)
	f.write("lab/calc/calc.go", "package calc\n\nfunc F() int { return 0 }\n")
	f.git("add", ".")
	f.git("commit", "-qm", "base with F returning int")
	base := f.head()

	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", "package calc\n\nfunc F() string { return \"x\" }\n")
	f.write("lab/calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) {\n\tvar s string\n\ts = F()\n\t_ = s\n}\n")
	f.git("add", ".")
	f.git("commit", "-qm", "F returns string")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/20/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":7,"deletions":0}]`,
		"repos/owner/repo/pulls/20":       prJSON(merge, base, def, "Closes #10", "F returns string"),
		"repos/owner/repo/issues/10":      `{"number":10,"title":"F returns string","body":"x"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{20}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 0 || len(report.Skipped) != 1 || report.Skipped[0].PR != 20 {
		t.Fatalf("report = %+v", report)
	}
	if !strings.HasPrefix(report.Skipped[0].Reason, "build failed at base: no interface identifiers recognized") {
		t.Fatalf("skip reason = %q", report.Skipped[0].Reason)
	}
}

// TestReplayBuildsMainPackageTestCompile exercises the per-package non-test build's use of
// os.DevNull for its output. The hidden test is in a `package main` directory whose base source
// compiles but lacks a function the test calls (a test-compile initial failure). A plain
// `go build ./tool` would write an executable named after the package directory into the module
// root — colliding with the package directory itself and failing spuriously; `go build -o
// os.DevNull` discards the artifact, so the PR is correctly built rather than skipped.
func TestReplayBuildsMainPackageTestCompile(t *testing.T) {
	f, _, def := newFixture(t)
	f.write("lab/tool/main.go", "package main\n\nfunc main() {}\n")
	f.git("add", ".")
	f.git("commit", "-qm", "add tool base")
	base := f.head()

	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/tool/main.go", "package main\n\nfunc main() {}\n\nfunc Thing() int { return 2 }\n")
	f.write("lab/tool/main_test.go", "package main\n\nimport \"testing\"\n\nfunc TestThing(t *testing.T) {\n\tif Thing() != 2 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n")
	f.git("add", ".")
	f.git("commit", "-qm", "add Thing")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/19/files": `[{"filename":"lab/tool/main.go","status":"modified","additions":2,"deletions":0},{"filename":"lab/tool/main_test.go","status":"added","additions":7,"deletions":0}]`,
		"repos/owner/repo/pulls/19":       prJSON(merge, base, def, "Closes #9", "add Thing"),
		"repos/owner/repo/issues/9":       `{"number":9,"title":"add Thing","body":"Add Thing."}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{19}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 1 || len(report.Skipped) != 0 {
		t.Fatalf("built=%+v skipped=%+v", report.Built, report.Skipped)
	}
	issue := molecule.Issue{}
	if err := json.Unmarshal(mustRead(t, filepath.Join(out, "hidden", "pr-19", "issue.json")), &issue); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issue.Body, "// package forgecellreplayfixture/tool") || !strings.Contains(issue.Body, "func Thing() int") {
		t.Fatalf("issue body missing tool contract:\n%s", issue.Body)
	}
}

// TestReplaySkipsBrokenNonTestWithNewPackage: the PR fixes a broken existing package and adds a
// brand-new package. At base the existing package's non-test code does not build (a real build
// error) while the new package has no non-test files. The per-package build classification must
// treat the existing package's failure as a genuine build error and skip the PR.
func TestReplaySkipsBrokenNonTestWithNewPackage(t *testing.T) {
	f, _, def := newFixture(t)
	f.write("lab/calc/calc.go", "package calc\n\nfunc Value() int { return missingSymbol }\n")
	f.git("add", ".")
	f.git("commit", "-qm", "broken base")
	base := f.head()

	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", valueTest(2))
	f.write("lab/widget/widget.go", "package widget\n\nfunc New() int { return 7 }\n")
	f.write("lab/widget/widget_test.go", "package widget\n\nimport \"testing\"\n\nfunc TestNew(t *testing.T) {\n\tif New() != 7 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n")
	f.git("add", ".")
	f.git("commit", "-qm", "fix and add widget")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/17/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0},{"filename":"lab/widget/widget.go","status":"added","additions":3,"deletions":0},{"filename":"lab/widget/widget_test.go","status":"added","additions":7,"deletions":0}]`,
		"repos/owner/repo/pulls/17":       prJSON(merge, base, def, "Closes #7", "fix and widget"),
		"repos/owner/repo/issues/7":       `{"number":7,"title":"fix and widget","body":"x"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{17}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 0 || len(report.Skipped) != 1 || report.Skipped[0].PR != 17 {
		t.Fatalf("report = %+v", report)
	}
	if !strings.HasPrefix(report.Skipped[0].Reason, "build failed at base") {
		t.Fatalf("skip reason = %q", report.Skipped[0].Reason)
	}
}

// TestReplaySkipsWhenCompileCollectionIncomplete: when the compile-error collection run does not
// complete, the test-compile classification cannot be trusted, so the PR is skipped with a setup
// error rather than built from an unreliable contract. The collector is injected to simulate it.
func TestReplaySkipsWhenCompileCollectionIncomplete(t *testing.T) {
	orig := collectCompileErrors
	collectCompileErrors = func(context.Context, string, []string, []string) (string, bool) { return "", true }
	t.Cleanup(func() { collectCompileErrors = orig })

	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(1)+"\nfunc Value2() int { return 2 }\n")
	f.write("lab/calc/calc_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestValue2(t *testing.T) {\n\tif Value2() != 2 {\n\t\tt.Fatal(\"no\")\n\t}\n}\n")
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/18/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/18":       prJSON(merge, base, def, "Closes #8", "value2"),
		"repos/owner/repo/issues/8":       `{"number":8,"title":"value2","body":"x"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{18}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 0 || len(report.Skipped) != 1 || report.Skipped[0].Reason != "setup error: compile-error collection did not complete" {
		t.Fatalf("report = %+v", report)
	}
}

func TestReplaySkipsWhenGoMissing(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", valueTest(2))
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	// A PATH with git but not go: the build-time hidden-test run cannot find the toolchain.
	bin := t.TempDir()
	if err := os.Symlink(gitPath, filepath.Join(bin, "git")); err != nil {
		t.Skip("cannot symlink git")
	}
	t.Setenv("PATH", bin)

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/7/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/7":       prJSON(merge, base, def, "Closes #42", "Make Value return 2"),
		"repos/owner/repo/issues/42":     `{"number":42,"title":"t","body":"b"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{7}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].Reason != "go toolchain unavailable" {
		t.Fatalf("skipped = %+v", report.Skipped)
	}
}

// TestReplaySkipsOversizedAcceptanceScript: a hidden test file of about 70 KB makes the
// acceptance script exceed readiness.MaxCheckInputBytes, so intake would reject it. The PR is
// skipped with the script's length before any go toolchain run, and nothing is written for it.
func TestReplaySkipsOversizedAcceptanceScript(t *testing.T) {
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	padding := strings.Repeat("// padding to grow the hidden test file past the check-input bound.\n", 1020)
	testBody := valueTest(2) + padding
	if len(testBody) < 69000 || len(testBody) > 71000 {
		t.Fatalf("fixture test file is %d bytes, want about 70 KB", len(testBody))
	}
	f.write("lab/calc/calc_test.go", testBody)
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	merge := f.head()

	testFiles := []string{"lab/calc/calc_test.go"}
	pkgs, err := acceptancePackages(testFiles)
	if err != nil {
		t.Fatal(err)
	}
	script, err := buildAcceptanceScript(testFiles, map[string][]byte{testFiles[0]: []byte(testBody)}, pkgs)
	if err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/21/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":1025,"deletions":0}]`,
		"repos/owner/repo/pulls/21":       prJSON(merge, base, def, "Closes #42", "Make Value return 2"),
		"repos/owner/repo/issues/42":      `{"number":42,"title":"Make Value return 2","body":"Value should return 2.","state":"closed"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{21}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	assertSkip(t, report, 21, fmt.Sprintf("acceptance script exceeds the 64000-byte check-input bound (%d bytes)", len(script)))
	for _, dir := range []string{filepath.Join(out, "tasks", "pr-21"), filepath.Join(out, "hidden", "pr-21")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("%s written for a skipped PR: %v", dir, err)
		}
	}
}

// TestAcceptanceScriptRunsOnlyHiddenTests: the generated script runs go test with a -run
// pattern naming exactly the hidden test functions, sorted and regex-quoted, and with
// -count=1 so hidden acceptance is never served from the shared Go cache.
func TestAcceptanceScriptRunsOnlyHiddenTests(t *testing.T) {
	testFiles := []string{"lab/calc/a_test.go", "lab/calc/b_test.go"}
	contents := map[string][]byte{
		testFiles[0]: []byte("package calc\n\nimport \"testing\"\n\nfunc TestZeta(t *testing.T) {}\n\nfunc helper() {}\n\nfunc ExampleValue() {}\n"),
		testFiles[1]: []byte("package calc\n\nimport \"testing\"\n\nfunc TestAlpha(t *testing.T) {}\n\nfunc FuzzValue(f *testing.F) {}\n"),
	}
	script, err := buildAcceptanceScript(testFiles, contents, []string{"./calc"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "'go' 'test' '-count=1' '-buildvcs=false' '-run' '^(ExampleValue|FuzzValue|TestAlpha|TestZeta)$' './calc'\n"; !strings.HasSuffix(string(script), want) {
		t.Fatalf("acceptance script does not end with %q:\n%s", want, script)
	}
	if got, err := hiddenRunPattern([]string{"TestB", "TestA.x"}); err != nil || got != `^(TestA\.x|TestB)$` {
		t.Fatalf("hiddenRunPattern = %q, %v", got, err)
	}
}

// TestReplaySkipsOverlongRunPattern: when the joined -run pattern of the hidden test names
// exceeds 4,000 bytes the PR is skipped with "hidden test name pattern too long", before any
// go toolchain run, and nothing is written for it.
func TestReplaySkipsOverlongRunPattern(t *testing.T) {
	var names []string
	for len(strings.Join(names, "|")) <= replayMaxRunPatternBytes {
		names = append(names, fmt.Sprintf("TestGeneratedCaseWithALongName%04d", len(names)))
	}
	if _, err := hiddenRunPattern(names); !errors.Is(err, errHiddenRunPatternTooLong) {
		t.Fatalf("hiddenRunPattern(%d names) error = %v", len(names), err)
	}
	var body strings.Builder
	body.WriteString("package calc\n\nimport \"testing\"\n")
	for _, n := range names {
		fmt.Fprintf(&body, "\nfunc %s(t *testing.T) {}\n", n)
	}
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", body.String())
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/22/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/22":       prJSON(merge, base, def, "Closes #42", "Many tests"),
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{22}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	assertSkip(t, report, 22, "hidden test name pattern too long")
	if _, err := os.Stat(filepath.Join(out, "hidden", "pr-22")); !os.IsNotExist(err) {
		t.Fatalf("hidden directory written for a skipped PR: %v", err)
	}
}

// TestReplayAcceptanceIgnoresUnrelatedFailingTest: the base holds an always-failing test in
// the same package as the hidden test. Because acceptance and both build-time validation
// runs use the hidden tests' -run filter, the PR still builds (the initial failure is
// reproduced by its own test and the merge-pass validation is not failed by the unrelated
// one), and the generated acceptance fails on the base tree and passes on the merge tree.
func TestReplayAcceptanceIgnoresUnrelatedFailingTest(t *testing.T) {
	f, _, def := newFixture(t)
	f.write("lab/calc/unrelated_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestUnrelatedAlwaysFails(t *testing.T) { t.Fatal(\"unrelated\") }\n")
	f.git("add", ".")
	f.git("commit", "-qm", "unrelated failing test")
	base := f.head()
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", valueTest(2))
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/23/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/23":       prJSON(merge, base, def, "Closes #42", "Make Value return 2"),
		"repos/owner/repo/issues/42":      `{"number":42,"title":"Make Value return 2","body":"Value should return 2.","state":"closed"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{23}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 1 {
		t.Fatalf("built=%+v skipped=%+v", report.Built, report.Skipped)
	}
	script := filepath.Join(out, "hidden", "pr-23", "acceptance.sh")
	if !strings.Contains(string(mustRead(t, script)), "'-run' '^(TestValue)$'") {
		t.Fatalf("acceptance script lacks the hidden -run filter:\n%s", mustRead(t, script))
	}
	runAcceptance := func(commit string) (string, error) {
		dir := filepath.Join(t.TempDir(), "wt")
		f.git("worktree", "add", "-q", "--detach", dir, commit)
		c := exec.Command("/bin/sh", script)
		c.Dir = dir
		b, err := c.CombinedOutput()
		return string(b), err
	}
	if outText, err := runAcceptance(merge); err != nil {
		t.Fatalf("acceptance failed on the merge tree despite the unrelated test being filtered out: %v\n%s", err, outText)
	}
	if outText, err := runAcceptance(base); err == nil || !strings.Contains(outText, "TestValue") || strings.Contains(outText, "TestUnrelatedAlwaysFails") {
		t.Fatalf("acceptance on the base tree: err=%v, want a TestValue failure only\n%s", err, outText)
	}
}

func TestReplaySkipsWithoutTestChanges(t *testing.T) {
	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/8/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1}]`,
		"repos/owner/repo/pulls/8":       prJSON(strings.Repeat("a", 40), strings.Repeat("b", 40), "main", "Closes #1", "No tests"),
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: t.TempDir(), Out: out, PRs: []int{8}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	assertSkip(t, report, 8, "no test-file changes")
}

// TestReplayEmptyBuildCreatesNoSource: when nothing is built (here the only PR has no test
// changes), <out>/source is not created and cohort.stub.json carries no source hint and an
// empty tasks list.
func TestReplayEmptyBuildCreatesNoSource(t *testing.T) {
	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/8/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1}]`,
		"repos/owner/repo/pulls/8":       prJSON(strings.Repeat("a", 40), strings.Repeat("b", 40), "main", "Closes #1", "No tests"),
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: t.TempDir(), Out: out, PRs: []int{8}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 0 {
		t.Fatalf("expected no built tasks: %+v", report.Built)
	}
	if report.Source != "" {
		t.Fatalf("report.Source = %q, want empty on an empty build", report.Source)
	}
	if _, err := os.Stat(filepath.Join(out, "source")); !os.IsNotExist(err) {
		t.Fatalf("<out>/source created on an empty build: %v", err)
	}
	// The stub has no source hint (the field is omitted) and an empty tasks list.
	stubRaw := mustRead(t, filepath.Join(out, "cohort.stub.json"))
	if strings.Contains(string(stubRaw), "\"source\"") {
		t.Fatalf("cohort.stub.json kept a source hint on an empty build:\n%s", stubRaw)
	}
	var stub CohortFile
	if err := json.Unmarshal(stubRaw, &stub); err != nil {
		t.Fatal(err)
	}
	if len(stub.Tasks) != 0 {
		t.Fatalf("cohort stub tasks = %+v, want empty", stub.Tasks)
	}
}

func TestReplaySkipsWithoutLinkedIssue(t *testing.T) {
	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/9/files": `[{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/9":       prJSON(strings.Repeat("a", 40), strings.Repeat("b", 40), "main", "no reference here", "No link"),
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: t.TempDir(), Out: out, PRs: []int{9}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	assertSkip(t, report, 9, "no linked issue")
}

func TestReplaySkipsTooManyFiles(t *testing.T) {
	var files []string
	for i := 0; i < 26; i++ {
		files = append(files, fmt.Sprintf(`{"filename":"lab/pkg/f%d.go","status":"modified","additions":1,"deletions":0}`, i))
	}
	files = append(files, `{"filename":"lab/pkg/pkg_test.go","status":"added","additions":1,"deletions":0}`)
	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/10/files": "[" + strings.Join(files, ",") + "]",
		"repos/owner/repo/pulls/10":       prJSON(strings.Repeat("a", 40), strings.Repeat("b", 40), "main", "Fixes #2", "Big"),
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: t.TempDir(), Out: out, PRs: []int{10}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].PR != 10 || !strings.Contains(report.Skipped[0].Reason, "more than 25 changed files") {
		t.Fatalf("skipped = %+v", report.Skipped)
	}
}

func TestReplaySkipsInitialFailureNotReproduced(t *testing.T) {
	// The hidden test asserts Value()==1, which the base tree already satisfies.
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc_test.go", valueTest(1))
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()

	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/11/files": `[{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/11":       prJSON(merge, base, def, "Closes #3", "Already passes"),
		"repos/owner/repo/issues/3":       `{"number":3,"title":"Already passes","body":"x"}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{11}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	assertSkip(t, report, 11, "initial failure not reproduced")
}

func TestReplaySkipsLinkedReferenceIsPR(t *testing.T) {
	out := filepath.Join(t.TempDir(), "cohort")
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.write("lab/calc/calc.go", valueGo(2))
	f.write("lab/calc/calc_test.go", valueTest(2))
	f.git("add", ".")
	f.git("commit", "-qm", "feature")
	f.git("checkout", "-q", def)
	f.git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	merge := f.head()
	gh := ghFixture(map[string]string{
		"repos/owner/repo/pulls/13/files": `[{"filename":"lab/calc/calc.go","status":"modified","additions":1,"deletions":1},{"filename":"lab/calc/calc_test.go","status":"added","additions":5,"deletions":0}]`,
		"repos/owner/repo/pulls/13":       prJSON(merge, base, def, "Closes #99", "x"),
		"repos/owner/repo/issues/99":      `{"number":99,"title":"a pr","body":"b","pull_request":{"url":"https://api.github.com/repos/owner/repo/pulls/99"}}`,
	})
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: f.dir, Out: out, PRs: []int{13}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Skipped) != 1 || !strings.Contains(report.Skipped[0].Reason, "is a pull request, not an issue") {
		t.Fatalf("skipped = %+v", report.Skipped)
	}
}

func TestReplayRecordsPerPRInfraFailureAsSkip(t *testing.T) {
	// A gh failure for one PR is a recorded skip, not a fatal error; outputs are still written.
	out := filepath.Join(t.TempDir(), "cohort")
	gh := ghFixture(map[string]string{}) // every request is unmatched
	report, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: t.TempDir(), Out: out, PRs: []int{14}, GH: gh})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Built) != 0 || len(report.Skipped) != 1 || !strings.HasPrefix(report.Skipped[0].Reason, "gh access failed") {
		t.Fatalf("report = %+v", report)
	}
	if _, err := os.Stat(filepath.Join(out, "skipped.json")); err != nil {
		t.Fatalf("skipped.json not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "cohort.stub.json")); err != nil {
		t.Fatalf("cohort.stub.json not written: %v", err)
	}
}

func TestReplayRefusesExistingOutput(t *testing.T) {
	out := t.TempDir() // already exists
	_, err := BuildReplay(context.Background(), ReplayOptions{Repo: "owner/repo", Source: t.TempDir(), Out: out, PRs: []int{1}, GH: ghFixture(nil)})
	if err == nil || !strings.Contains(err.Error(), "new output directory") {
		t.Fatalf("existing output accepted: %v", err)
	}
}

// TestScrubbedGoEnvResolvesCache asserts that even when GOMODCACHE and GOCACHE are unset in the
// process environment (the normal case), the scrubbed build environment still carries both,
// populated from the values the injected resolver obtained from `go env` under the caller's
// environment. The resolver is injected as a function value so the test does not shell out.
func TestScrubbedGoEnvResolvesCache(t *testing.T) {
	// Empty then unset in the process env, so LookupEnv would not find them.
	t.Setenv("GOMODCACHE", "")
	t.Setenv("GOCACHE", "")
	os.Unsetenv("GOMODCACHE")
	os.Unsetenv("GOCACHE")

	const wantMod = "/resolved/go/pkg/mod"
	const wantCache = "/resolved/go-build"
	orig := resolveGoCache
	resolveGoCache = func(context.Context) (string, string, bool) { return wantMod, wantCache, true }
	t.Cleanup(func() { resolveGoCache = orig })

	gomodcache, gocache, ok := resolveGoCache(context.Background())
	if !ok {
		t.Fatal("injected resolver returned ok=false")
	}
	env := scrubbedGoEnv("/private/home", gomodcache, gocache)

	if !contains(env, "GOMODCACHE="+wantMod) {
		t.Fatalf("GOMODCACHE missing or wrong in scrubbed env: %v", env)
	}
	if !contains(env, "GOCACHE="+wantCache) {
		t.Fatalf("GOCACHE missing or wrong in scrubbed env: %v", env)
	}
	// The rest of the allowlist stays as before, with -trimpath so cached results serve
	// other checkouts.
	for _, want := range []string{"HOME=/private/home", "GOFLAGS=-trimpath", "GOPROXY=off", "GOTOOLCHAIN=local"} {
		if !contains(env, want) {
			t.Fatalf("scrubbed env missing %q: %v", want, env)
		}
	}
	// Base-flake re-runs merge the cohort's -p into the same GOFLAGS.
	env = resources.ApplyGoParallelism(resources.WithGoParallelism(context.Background(), 2), env)
	if !contains(env, "GOFLAGS=-trimpath -p=2") {
		t.Fatalf("-p dropped the cache flags: %v", env)
	}
}

// TestObserveEnvIsScrubbedWithSharedCache: evaluation commands no longer inherit the
// caller's environment; they get a private HOME, PATH, LANG, the stable TMPDIR ctx carries,
// the workspace, the shared cache with -trimpath and the cohort's -p. Hidden acceptance is
// recorded as fresh.
func TestObserveEnvIsScrubbedWithSharedCache(t *testing.T) {
	orig := resolveGoCache
	resolveGoCache = func(context.Context) (string, string, bool) { return "/shared/mod", "/shared/build", true }
	t.Cleanup(func() { resolveGoCache = orig })
	t.Setenv("FORGECELL_UNRELATED_CALLER_VAR", "leak")
	t.Setenv("GOCACHE", "")
	os.Unsetenv("GOCACHE")
	ctx := resources.WithGoParallelism(resources.WithCheckTempDir(context.Background(), "/stable/tmp"), 3)
	env, cache := observeEnv(ctx, "/private/home", "/work", false)
	if !cache {
		t.Fatal("cache not applied")
	}
	for _, want := range []string{"HOME=/private/home", "TMPDIR=/stable/tmp", "FORGECELL_EVAL_WORKSPACE=/work", "GOCACHE=/shared/build", "GOMODCACHE=/shared/mod", "GOPROXY=off", "GOFLAGS=-trimpath -p=3"} {
		if !contains(env, want) {
			t.Fatalf("observe env missing %q: %v", want, env)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "FORGECELL_UNRELATED_CALLER_VAR=") {
			t.Fatalf("caller variable inherited: %v", env)
		}
	}
	dir := t.TempDir()
	// Hidden acceptance is fresh by construction: its GOFLAGS carries -count=1 whatever
	// its argv runs, and it records "disabled (fresh)" regardless of argv. Any other non-go
	// argv records no cache state.
	if o := observe(ctx, Command{Name: "acceptance", Dir: ".", Argv: []string{"/bin/sh", "-c", "test \"$GOFLAGS\" = '-trimpath -count=1 -p=3'"}, TimeoutMS: 10000}, dir, ""); o.Cache != resources.CacheFresh || !o.Result.OK {
		t.Fatalf("acceptance: %+v", o)
	}
	if o := observe(ctx, Command{Name: "test", Dir: ".", Argv: []string{"/bin/sh", "-c", "test \"$GOCACHE\" = /shared/build"}, TimeoutMS: 10000}, dir, ""); o.Cache != "" || !o.Result.OK {
		t.Fatalf("regression: %+v", o)
	}
}

// --- helpers ---

func assertSkip(t *testing.T, report ReplayReport, pr int, reason string) {
	t.Helper()
	if len(report.Built) != 0 {
		t.Fatalf("expected no built tasks, got %+v", report.Built)
	}
	if len(report.Skipped) != 1 || report.Skipped[0].PR != pr || report.Skipped[0].Reason != reason {
		t.Fatalf("skipped = %+v, want pr %d reason %q", report.Skipped, pr, reason)
	}
}

func assertNoHiddenLeak(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			t.Fatalf("test file leaked under tasks/: %s", path)
		}
		if strings.Contains(string(mustRead(t, path)), "func TestValue") {
			t.Fatalf("hidden test content leaked under tasks/: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func gitHas(gitDir, sha string) bool {
	c := exec.Command("git", "cat-file", "-e", sha)
	c.Dir = gitDir
	return c.Run() == nil
}

// gitOut runs git in dir and fails the test on a non-zero exit, returning trimmed stdout.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	b, e := c.Output()
	if e != nil {
		t.Fatalf("git %v: %v", args, e)
	}
	return strings.TrimSpace(string(b))
}

// assertNoPathLeak fails when any file under gitDir contains source's absolute path (plain
// or symlink-resolved), which a FETCH_HEAD from a local fetch would record.
func assertNoPathLeak(t *testing.T, gitDir, source string) {
	t.Helper()
	needles := []string{source}
	if r, err := filepath.EvalSymlinks(source); err == nil && r != source {
		needles = append(needles, r)
	}
	if _, err := os.Stat(filepath.Join(gitDir, "FETCH_HEAD")); !os.IsNotExist(err) {
		t.Fatalf("FETCH_HEAD left in %s: %v", gitDir, err)
	}
	err := filepath.WalkDir(gitDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, n := range needles {
			if strings.Contains(string(b), n) {
				t.Errorf("%s contains the source path %s", p, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// gitTry runs git in dir and returns trimmed stdout, tolerating a non-zero exit (used for
// commands like `symbolic-ref -q HEAD` and `config --get-all` that signal "no value" with a
// non-zero status and empty output).
func gitTry(dir string, args ...string) string {
	c := exec.Command("git", args...)
	c.Dir = dir
	b, _ := c.Output()
	return strings.TrimSpace(string(b))
}

func readPlan(t *testing.T, path string) Plan {
	t.Helper()
	var p Plan
	if err := json.Unmarshal(mustRead(t, path), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
