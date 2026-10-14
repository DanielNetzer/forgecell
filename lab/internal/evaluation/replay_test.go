package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
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
		"repos/owner/repo/issues/42":     `{"number":42,"title":"Make Value return 2","body":"Value should return 2."}`,
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

	// source.git is history-free: it has the base commit but not the merge commit.
	sg := filepath.Join(out, "source.git")
	if !gitHas(sg, base) {
		t.Fatal("source.git is missing the base commit")
	}
	if gitHas(sg, merge) {
		t.Fatal("source.git leaked the merge commit")
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

func TestReplaySkipsBuildErrorAtBase(t *testing.T) {
	f, base, def := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	// Feature adds a new symbol and a test for it; at base the symbol does not exist, so the
	// hidden test fails to COMPILE rather than fail — this must be a build-error skip.
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
	if len(report.Built) != 0 || len(report.Skipped) != 1 || report.Skipped[0].PR != 12 {
		t.Fatalf("report = %+v", report)
	}
	if !strings.HasPrefix(report.Skipped[0].Reason, "build failed at base") {
		t.Fatalf("skip reason = %q", report.Skipped[0].Reason)
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
	// The rest of the allowlist stays as before.
	for _, want := range []string{"HOME=/private/home", "GOFLAGS=-mod=readonly", "GOPROXY=off"} {
		if !contains(env, want) {
			t.Fatalf("scrubbed env missing %q: %v", want, env)
		}
	}
}

// TestParseGoEnvCache covers the pure parser used by resolveGoCache: two absolute lines parse,
// CRLF endings and a trailing blank line are tolerated, and anything short of two non-empty
// absolute paths is rejected.
func TestParseGoEnvCache(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		wantMod string
		wantCac string
		wantOK  bool
	}{
		{name: "two absolute lines", stdout: "/go/pkg/mod\n/go-build\n", wantMod: "/go/pkg/mod", wantCac: "/go-build", wantOK: true},
		{name: "crlf endings", stdout: "/go/pkg/mod\r\n/go-build\r\n", wantMod: "/go/pkg/mod", wantCac: "/go-build", wantOK: true},
		{name: "trailing blank line", stdout: "/go/pkg/mod\n/go-build\n\n", wantMod: "/go/pkg/mod", wantCac: "/go-build", wantOK: true},
		{name: "one line only", stdout: "/go/pkg/mod\n", wantOK: false},
		{name: "empty value", stdout: "/go/pkg/mod\n\n", wantOK: false},
		{name: "relative path", stdout: "go/pkg/mod\n/go-build\n", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mod, cac, ok := parseGoEnvCache(c.stdout)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (mod=%q cache=%q)", ok, c.wantOK, mod, cac)
			}
			if !c.wantOK {
				return
			}
			if mod != c.wantMod || cac != c.wantCac {
				t.Fatalf("got mod=%q cache=%q, want mod=%q cache=%q", mod, cac, c.wantMod, c.wantCac)
			}
		})
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
