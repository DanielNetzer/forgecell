package molecule

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

// gitlabBaselineRepo commits a small repository with a root .gitlab-ci.yml and a
// GitHub workflow, under the test's own temporary directory.
func gitlabBaselineRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for p, content := range map[string]string{
		"go.mod":                    "module example\n\ngo 1.22\n",
		".gitlab-ci.yml":            "unit:\n  script: [go test ./...]\n",
		".github/workflows/ci.yml":  "name: ci\n",
		"internal/example/a.go":     "package example\n",
		"internal/example/a_doc.md": "notes\n",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func gitlabRequiredCheck() formula.RequiredCheck {
	return formula.RequiredCheck{ID: "baseline-gitlab-unit", Argv: []string{"go", "test", "./..."}, Dir: ".", TimeoutMS: 60000, Workflow: ".gitlab-ci.yml", Reason: "GitLab CI runs go test"}
}

func githubRequiredCheck() formula.RequiredCheck {
	return formula.RequiredCheck{ID: "baseline-github-unit", Argv: []string{"go", "test", "./..."}, Dir: ".", TimeoutMS: 60000, Workflow: ".github/workflows/ci.yml", Reason: "GitHub CI runs go test"}
}

func TestFormulaCollectOptionsFollowTheReviewedBaseline(t *testing.T) {
	cases := map[string]struct {
		checks []formula.RequiredCheck
		want   bool
	}{
		"no checks":   {nil, false},
		"github only": {[]formula.RequiredCheck{githubRequiredCheck()}, false},
		"gitlab":      {[]formula.RequiredCheck{gitlabRequiredCheck()}, true},
		"both":        {[]formula.RequiredCheck{githubRequiredCheck(), gitlabRequiredCheck()}, true},
		"lookalike": {[]formula.RequiredCheck{func() formula.RequiredCheck {
			c := githubRequiredCheck()
			c.Workflow = ".github/workflows/.gitlab-ci.yml"
			return c
		}()}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := formulaCollectOptions(formula.Formula{RequiredChecks: c.checks}); got.GitLabCI != c.want {
				t.Fatalf("GitLabCI = %v, want %v", got.GitLabCI, c.want)
			}
		})
	}
}

func TestGitLabBaselineResolvesFromTheAppendedEvidence(t *testing.T) {
	dir := gitlabBaselineRepo(t)
	ctx := context.Background()
	ticket := "Change `internal/example/a.go`."
	f := formula.Formula{RequiredChecks: []formula.RequiredCheck{githubRequiredCheck(), gitlabRequiredCheck()}}

	// Without the Formula's options the stray file is not frozen, so the baseline
	// fails closed exactly as before.
	plain, err := readiness.CollectForTicket(ctx, dir, "HEAD", ticket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := formulaBaselineChecks(f, plain.References()); err == nil || !strings.Contains(err.Error(), ".gitlab-ci.yml") {
		t.Fatalf("an unfrozen GitLab workflow should fail closed: %v", err)
	}

	snapshot, err := readiness.CollectForTicketFor(ctx, dir, "HEAD", ticket, formulaCollectOptions(f))
	if err != nil {
		t.Fatal(err)
	}
	walk, err := readiness.Collect(ctx, dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	refs := snapshot.References()
	if !reflect.DeepEqual(refs[:len(walk.Evidence)], walk.References()) {
		t.Fatal("tree-walk evidence ids shifted")
	}
	if refs[len(walk.Evidence)].Path != ".gitlab-ci.yml" {
		t.Fatalf("the GitLab workflow is not appended after the tree walk: %+v", refs)
	}
	gitlabID := refs[len(walk.Evidence)].ID
	p := readiness.Plan{Evidence: refs}
	if err := applyFormulaBaseline(f, &p); err != nil {
		t.Fatal(err)
	}
	var got readiness.Check
	for _, c := range p.Analysis.Checks {
		if c.ID == "baseline-gitlab-unit" {
			got = c
		}
	}
	if !reflect.DeepEqual(got.Definitions, []string{gitlabID}) || !reflect.DeepEqual(got.Evidence, []string{gitlabID}) || got.Category != "regression" || !got.Required {
		t.Fatalf("baseline check does not cite the frozen GitLab workflow: %+v", got)
	}
	if !reflect.DeepEqual(p.FormulaBaseline, []string{"baseline-github-unit", "baseline-gitlab-unit"}) {
		t.Fatalf("baseline labels: %v", p.FormulaBaseline)
	}
}

func TestGitHubFormulaEvidenceIsUnchangedByAStrayGitLabFile(t *testing.T) {
	dir := gitlabBaselineRepo(t)
	ctx := context.Background()
	ticket := "Change `internal/example/a.go`."
	f := formula.Formula{RequiredChecks: []formula.RequiredCheck{githubRequiredCheck()}}
	plain, err := readiness.CollectForTicket(ctx, dir, "HEAD", ticket)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readiness.CollectForTicketFor(ctx, dir, "HEAD", ticket, formulaCollectOptions(f))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := plain.Digest()
	b, _ := got.Digest()
	if a != b || !reflect.DeepEqual(plain, got) {
		t.Fatal("a GitHub Formula's evidence changed")
	}
}
