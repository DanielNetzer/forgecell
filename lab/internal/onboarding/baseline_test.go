package onboarding

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

const baselineCIWorkflow = `name: CI
on:
  pull_request:
  push:
    branches: [main]
jobs:
  go-core:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: service
    steps:
      - uses: actions/checkout@v4
      - name: Keep agent-tool folders out of product source
        working-directory: .
        run: |
          sh scripts/check-repo-hygiene.sh
          sh scripts/build-release.sh
      - run: go test ./...
      - run: go vet ./...
      - run: go build -o "${RUNNER_TEMP}/svc" ./cmd/svc
      - name: Check Go formatting
        run: test -z "$(gofmt -l internal cmd)"
      - run: echo "${{ matrix.os }}" && go test ./...
      - if: always()
        run: go vet ./...
      - run: go test ./... -run Short
`

const baselineReleaseWorkflow = `name: Release
on:
  push:
    tags: ['v*']
jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - run: go test ./...
`

var baselineTracked = map[string]bool{"scripts/check-repo-hygiene.sh": true, "scripts/build-release.sh": true, "scripts/lint.sh": true}

func baselineRepository(workflows map[string]string) Repository {
	var r Repository
	paths := make([]string, 0, len(workflows))
	for file := range workflows {
		paths = append(paths, file)
	}
	sort.Strings(paths)
	for i, file := range paths {
		r.Workflows = append(r.Workflows, file)
		r.Evidence = append(r.Evidence, readiness.FileEvidence{Evidence: readiness.Evidence{ID: fmt.Sprintf("file-%d", i+1), Path: file}, Content: workflows[file]})
	}
	return r
}

func baselineSummary(checks []formula.RequiredCheck) []string {
	out := []string{}
	for _, c := range checks {
		out = append(out, fmt.Sprintf("%s %s %q", c.ID, c.Dir, c.Argv))
	}
	return out
}

func baselineStepsWorkflow(on, jobExtra, steps string) string {
	return "on: " + on + "\njobs:\n  j:\n    runs-on: ubuntu-latest\n" + jobExtra + "    steps:\n" + steps
}

func TestBaselineDerivesPullRequestWorkflowChecks(t *testing.T) {
	r := baselineRepository(map[string]string{".github/workflows/ci.yml": baselineCIWorkflow, ".github/workflows/release.yml": baselineReleaseWorkflow})
	checks, err := DeriveBaseline(r, baselineTracked)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`baseline-script-scripts-check-repo-hygiene . ["sh" "scripts/check-repo-hygiene.sh"]`,
		`baseline-go-test-service service ["go" "test" "./..."]`,
		`baseline-go-vet-service service ["go" "vet" "./..."]`,
		`baseline-gofmt-service service ["sh" "-c" "test -z \"$(gofmt -l internal cmd)\""]`,
	}
	if got := baselineSummary(checks); !reflect.DeepEqual(got, want) {
		t.Fatalf("derived baseline:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, c := range checks {
		if err := c.Validate(); err != nil || c.Workflow != ".github/workflows/ci.yml" || !strings.Contains(c.Reason, "pull requests") || !strings.Contains(c.Reason, "go-core") {
			t.Fatalf("%+v %v", c, err)
		}
	}
	again, _ := DeriveBaseline(r, baselineTracked)
	if !reflect.DeepEqual(checks, again) {
		t.Fatal("derivation is not deterministic")
	}
}

func TestBaselineSkipsUnrecognizedAndUnsafeSteps(t *testing.T) {
	cases := []struct{ name, steps string }{
		{"extra go test argument", "      - run: go test ./... -run Short\n"},
		{"tolerated failure", "      - run: go test ./... || true\n"},
		{"gofmt parent directory", "      - run: test -z \"$(gofmt -l ../x)\"\n"},
		{"gofmt command substitution", "      - run: test -z \"$(gofmt -l $(pwd))\"\n"},
		{"gofmt chained command", "      - run: test -z \"$(gofmt -l .; rm -rf x)\"\n"},
		{"untracked script", "      - run: sh scripts/untracked-check.sh\n"},
		{"script outside the repository", "      - run: sh ../scripts/check.sh\n"},
		{"script that is not a check", "      - run: sh scripts/lint.sh\n"},
		{"shell string", "      - run: bash -c \"go test ./...\"\n"},
		{"directory change", "      - run: |\n          cd lab\n          go test ./...\n"},
		{"control flow", "      - run: |\n          if true; then\n            go test ./...\n          fi\n"},
		{"heredoc", "      - run: |\n          cat <<EOF\n          go test ./...\n          EOF\n"},
		{"line continuation", "      - run: |\n          go test \\\n          ./...\n"},
		{"expression in command", "      - run: \"go test ./... ${{ matrix.flags }}\"\n"},
		{"expression in directory", "      - working-directory: ${{ matrix.dir }}\n        run: go test ./...\n"},
		{"absolute directory", "      - working-directory: /tmp\n        run: go test ./...\n"},
		{"parent directory", "      - working-directory: ../x\n        run: go test ./...\n"},
		{"other shell", "      - shell: pwsh\n        run: go test ./...\n"},
		{"tolerated step failure", "      - continue-on-error: true\n        run: go test ./...\n"},
		{"conditional step", "      - if: github.ref == 'refs/heads/main'\n        run: go test ./...\n"},
		{"action step", "      - uses: actions/setup-go@v5\n"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			r := baselineRepository(map[string]string{".github/workflows/ci.yml": baselineStepsWorkflow("pull_request", "", tt.steps)})
			checks, err := DeriveBaseline(r, baselineTracked)
			if err != nil || len(checks) != 0 {
				t.Fatalf("derived %v from an unsafe or unrecognized step: %v", baselineSummary(checks), err)
			}
			// The same workflow must still be readable: a later recognized step is derived.
			r = baselineRepository(map[string]string{".github/workflows/ci.yml": baselineStepsWorkflow("pull_request", "", tt.steps+"      - run: go vet ./...\n")})
			checks, err = DeriveBaseline(r, baselineTracked)
			if want := []string{`baseline-go-vet . ["go" "vet" "./..."]`}; err != nil || !reflect.DeepEqual(baselineSummary(checks), want) {
				t.Fatalf("workflow was not parsed, derived %v: %v", baselineSummary(checks), err)
			}
		})
	}
	r := baselineRepository(map[string]string{".github/workflows/ci.yml": baselineStepsWorkflow("pull_request", "    if: github.actor != 'bot'\n", "      - run: go test ./...\n")})
	if checks, _ := DeriveBaseline(r, baselineTracked); len(checks) != 0 {
		t.Fatalf("a conditional job produced %v", baselineSummary(checks))
	}
	r = baselineRepository(map[string]string{".github/workflows/ci.yml": "on: [pull_request\njobs: {"})
	if checks, err := DeriveBaseline(r, baselineTracked); err != nil || len(checks) != 0 {
		t.Fatalf("an unreadable workflow produced %v: %v", baselineSummary(checks), err)
	}
}

func TestBaselineRequiresPullRequestTrigger(t *testing.T) {
	steps := "      - run: go test ./...\n"
	cases := []struct {
		on   string
		want int
	}{
		{"pull_request", 1},
		{"[push, pull_request]", 1},
		{"\n  pull_request:\n    branches: [main]\n  push:", 1},
		{"push", 0},
		{"[push, workflow_dispatch]", 0},
		{"pull_request_target", 0},
		{"\n  push:\n    tags: ['v*']", 0},
	}
	for _, tt := range cases {
		t.Run(strings.TrimSpace(tt.on), func(t *testing.T) {
			r := baselineRepository(map[string]string{".github/workflows/ci.yml": baselineStepsWorkflow(tt.on, "", steps)})
			checks, err := DeriveBaseline(r, baselineTracked)
			if err != nil || len(checks) != tt.want {
				t.Fatalf("derived %v, want %d: %v", baselineSummary(checks), tt.want, err)
			}
		})
	}
}

func TestBaselineHonoursWorkingDirectoryDefaults(t *testing.T) {
	workflow := `on: pull_request
defaults:
  run:
    working-directory: lab
jobs:
  a:
    steps:
      - run: go test ./...
  b:
    defaults:
      run:
        working-directory: service
    steps:
      - run: go test ./...
      - working-directory: .
        run: go vet ./...
`
	checks, err := DeriveBaseline(baselineRepository(map[string]string{".github/workflows/ci.yml": workflow}), baselineTracked)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`baseline-go-test-lab lab ["go" "test" "./..."]`,
		`baseline-go-test-service service ["go" "test" "./..."]`,
		`baseline-go-vet . ["go" "vet" "./..."]`,
	}
	if got := baselineSummary(checks); !reflect.DeepEqual(got, want) {
		t.Fatalf("got:\n%s", strings.Join(got, "\n"))
	}
}

func TestBaselineDeduplicatesAndNumbersIdentifiers(t *testing.T) {
	same := baselineStepsWorkflow("pull_request", "", "      - run: go test ./...\n")
	two := baselineStepsWorkflow("pull_request", "", "      - run: |\n          test -z \"$(gofmt -l a)\"\n          test -z \"$(gofmt -l b)\"\n")
	checks, err := DeriveBaseline(baselineRepository(map[string]string{".github/workflows/a.yml": same, ".github/workflows/b.yml": same, ".github/workflows/c.yml": two}), baselineTracked)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, c := range checks {
		ids = append(ids, c.ID)
	}
	if want := []string{"baseline-go-test", "baseline-gofmt", "baseline-gofmt-2"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	var many strings.Builder
	for i := 0; i <= formula.MaxRequiredChecks; i++ {
		fmt.Fprintf(&many, "          test -z \"$(gofmt -l d%d)\"\n", i)
	}
	big := baselineStepsWorkflow("pull_request", "", "      - run: |\n"+many.String())
	if _, err := DeriveBaseline(baselineRepository(map[string]string{".github/workflows/ci.yml": big}), baselineTracked); err == nil {
		t.Fatal("a baseline larger than the Formula allows was truncated silently")
	}
}

func commitBaselineFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "ci"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v", b, err)
		}
	}
}

func TestBaselineBootstrapFormulaCarriesWorkflowChecks(t *testing.T) {
	root, runner := repoFixture(t)
	commitBaselineFiles(t, root, map[string]string{
		".github/workflows/ci.yml":      baselineCIWorkflow,
		".github/workflows/release.yml": baselineReleaseWorkflow,
		"scripts/check-repo-hygiene.sh": "exit 0\n",
		"scripts/build-release.sh":      "exit 0\n",
	})
	dir := filepath.Join(t.TempDir(), "lab")
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/installed/forgecell", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "pending" || out.Proposal == nil || !strings.Contains(out.Proposal.YAML, "requiredChecks:") || !strings.Contains(out.Note, "baseline-gofmt-service") {
		t.Fatalf("%+v", out)
	}
	f, err := formula.Parse([]byte(out.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	var gofmt *formula.RequiredCheck
	ids := []string{}
	for i, c := range f.RequiredChecks {
		ids = append(ids, c.ID)
		if c.ID == "baseline-gofmt-service" {
			gofmt = &f.RequiredChecks[i]
		}
	}
	if gofmt == nil || !reflect.DeepEqual(gofmt.Argv, []string{"sh", "-c", `test -z "$(gofmt -l internal cmd)"`}) || gofmt.Dir != "service" || gofmt.Workflow != ".github/workflows/ci.yml" {
		t.Fatalf("bootstrap Formula lacks the gofmt baseline: %v %+v", ids, f.RequiredChecks)
	}
	want := []string{"baseline-script-scripts-check-repo-hygiene", "baseline-go-test-service", "baseline-go-vet-service", "baseline-gofmt-service"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if _, err = os.Stat(filepath.Join(dir, "lab.json")); !os.IsNotExist(err) {
		t.Fatal("init bypassed approval")
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	loaded, err := formula.Load(dir, "")
	if err != nil || !loaded.Approved || !reflect.DeepEqual(loaded.Formula.RequiredChecks, f.RequiredChecks) {
		t.Fatalf("approved Formula lost the baseline: %+v %v", loaded.Formula.RequiredChecks, err)
	}
}

func TestBaselineBootstrapWithoutWorkflowsKeepsProposalUnchanged(t *testing.T) {
	root, runner := repoFixture(t)
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: filepath.Join(t.TempDir(), "lab"), Launcher: "/installed/forgecell", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner})
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	if strings.Contains(out.Proposal.YAML, "requiredChecks") || strings.Contains(out.Note, "baseline") {
		t.Fatalf("a repository without workflows gained a baseline:\n%s", out.Proposal.YAML)
	}
}
