package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"go.yaml.in/yaml/v4"
)

var gitlabTracked = map[string]bool{"lab/go.mod": true, "lab/scripts/check-lab.sh": true, "scripts/check-repo-hygiene.sh": true, "scripts/build-release.sh": true}

func deriveGitLab(t *testing.T, content string) []string {
	t.Helper()
	checks, err := DeriveGitLabBaseline(content, gitlabTracked)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if err := c.Validate(); err != nil || c.Workflow != ".gitlab-ci.yml" || !strings.Contains(c.Reason, ".gitlab-ci.yml") {
			t.Fatalf("%+v %v", c, err)
		}
	}
	return baselineSummary(checks)
}

func wantGitLab(t *testing.T, content string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if got := deriveGitLab(t, content); !reflect.DeepEqual(got, want) {
		t.Fatalf("derived baseline:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestGitLabBaselineDerivesSimpleMergeRequestJob(t *testing.T) {
	content := "stages: [test]\ntest:\n  stage: test\n  script:\n    - go test ./...\n    - go vet ./...\n"
	wantGitLab(t, content, `baseline-go-test . ["go" "test" "./..."]`, `baseline-go-vet . ["go" "vet" "./..."]`)
	checks, _ := DeriveGitLabBaseline(content, gitlabTracked)
	if !strings.Contains(checks[0].Reason, "job test") || !strings.Contains(checks[0].Reason, "merge requests") {
		t.Fatalf("reason does not cite the job: %q", checks[0].Reason)
	}
	again, _ := DeriveGitLabBaseline(content, gitlabTracked)
	if !reflect.DeepEqual(checks, again) {
		t.Fatal("derivation is not deterministic")
	}
	wantGitLab(t, "single:\n  script: go vet ./...\n", `baseline-go-vet . ["go" "vet" "./..."]`)
}

func TestGitLabBaselineCdPrefixNeedsATrackedDirectory(t *testing.T) {
	wantGitLab(t, "lab:\n  script:\n    - cd lab && go test -timeout 20m ./...\n    - go vet ./...\n    - sh scripts/check-lab.sh\n",
		`baseline-go-test-lab lab ["go" "test" "-timeout" "20m" "./..."]`,
		`baseline-go-vet-lab lab ["go" "vet" "./..."]`,
		`baseline-script-scripts-check-lab-lab lab ["sh" "scripts/check-lab.sh"]`)
	for _, line := range []string{"cd web && go test ./...", "cd ../lab && go test ./...", "cd /lab && go test ./...", "cd ./lab && go test ./...", "cd lab/../lab && go test ./...", "cd $DIR && go test ./...", "cd lab"} {
		wantGitLab(t, fmt.Sprintf("j:\n  script:\n    - %q\n    - go vet ./...\n", line))
	}
	// Every line shares one shell, so a second cd is relative to the first.
	wantGitLab(t, "j:\n  script:\n    - cd lab && go vet ./...\n    - cd lab && go test ./...\n", `baseline-go-vet-lab lab ["go" "vet" "./..."]`)
}

func TestGitLabBaselineIgnoresHiddenJobsAndReservedKeys(t *testing.T) {
	content := `.template:
  script: [go test ./...]
variables:
  script: go test ./...
cache:
  script: go test ./...
workflow:
  script: go test ./...
image: golang
vet:
  script: [go vet ./...]
`
	wantGitLab(t, content, `baseline-go-vet . ["go" "vet" "./..."]`)
}

func TestGitLabBaselineMergeRequestTriggers(t *testing.T) {
	content := `only-mr:
  only: [merge_requests]
  script: [go test ./...]
rules-mr:
  rules:
    - if: '$CI_PIPELINE_SOURCE == "merge_request_event"'
  script: [go vet ./...]
iid:
  rules:
    - if: $CI_MERGE_REQUEST_IID
  script: ['test -z "$(gofmt -l internal cmd)"']
plain:
  script: [sh scripts/check-repo-hygiene.sh]
`
	wantGitLab(t, content,
		`baseline-go-test . ["go" "test" "./..."]`,
		`baseline-go-vet . ["go" "vet" "./..."]`,
		`baseline-gofmt . ["sh" "-c" "test -z \"$(gofmt -l internal cmd)\""]`,
		`baseline-script-scripts-check-repo-hygiene . ["sh" "scripts/check-repo-hygiene.sh"]`)
	for _, trigger := range []string{
		"only: [main]",
		"only: [merge_requests]\n  except: [main]",
		"except: [main]",
		"rules:\n    - if: $CI_COMMIT_BRANCH == \"main\"",
		"rules:\n    - when: never",
		"rules:\n    - if: $CI_MERGE_REQUEST_IID\n    - when: never",
		"rules:\n    - if: $CI_MERGE_REQUEST_IID\n      when: never",
		"rules:\n    - if: $CI_MERGE_REQUEST_IID\n      when: manual",
		"rules:\n    - if: $CI_MERGE_REQUEST_IID\n      allow_failure: true",
		"rules: $CI_MERGE_REQUEST_IID",
	} {
		wantGitLab(t, "j:\n  "+trigger+"\n  script: [go test ./...]\n")
	}
}

func TestGitLabBaselineMergeRequestConditionIsExact(t *testing.T) {
	for _, cond := range []string{
		`$CI_MERGE_REQUEST_IID`,
		`$CI_PIPELINE_SOURCE == "merge_request_event"`,
		`$CI_PIPELINE_SOURCE == 'merge_request_event'`,
		`$CI_MERGE_REQUEST_IID || $CI_PIPELINE_SOURCE == "merge_request_event"`,
		`  $CI_PIPELINE_SOURCE == 'merge_request_event'||$CI_MERGE_REQUEST_IID  `,
	} {
		wantGitLab(t, fmt.Sprintf("j:\n  rules:\n    - if: %q\n  script: [go test ./...]\n", cond), `baseline-go-test . ["go" "test" "./..."]`)
	}
	for _, cond := range []string{
		`$CI_COMMIT_BRANCH && !$CI_MERGE_REQUEST_IID`,
		`!$CI_MERGE_REQUEST_IID`,
		`$CI_MERGE_REQUEST_IID == null`,
		`$CI_MERGE_REQUEST_IID != null`,
		`$CI_PIPELINE_SOURCE != "merge_request_event"`,
		`$CI_PIPELINE_SOURCE == "merge_request_event" && $CI_COMMIT_BRANCH`,
		`$CI_PIPELINE_SOURCE =~ /merge_request_event/`,
		`($CI_MERGE_REQUEST_IID)`,
		`$CI_MERGE_REQUEST_IID || $CI_COMMIT_BRANCH`,
		`$CI_MERGE_REQUEST_IID ||`,
		`$CI_MERGE_REQUEST_IID ||| $CI_MERGE_REQUEST_IID`,
		`$CI_MERGE_REQUEST_IIDX`,
		`$CI_PIPELINE_SOURCE == "merge_request_event_x"`,
		`$CI_PIPELINE_SOURCE == "push"`,
		``,
	} {
		wantGitLab(t, fmt.Sprintf("j:\n  rules:\n    - if: %q\n  script: [go test ./...]\n", cond))
	}
}

func TestGitLabBaselineRulesAreFirstMatchAndFailSafe(t *testing.T) {
	// A rule that can only make the job run on success may come before the MR rule.
	wantGitLab(t, "j:\n  rules:\n    - if: $CI_COMMIT_BRANCH == \"main\"\n    - if: $CI_MERGE_REQUEST_IID\n  script: [go test ./...]\n", `baseline-go-test . ["go" "test" "./..."]`)
	wantGitLab(t, "j:\n  rules:\n    - if: $CI_MERGE_REQUEST_IID\n      when: always\n      allow_failure: false\n  script: [go test ./...]\n", `baseline-go-test . ["go" "test" "./..."]`)
	for _, rules := range []string{
		// A when: never rule before the first merge-request rule.
		"- if: $CI_COMMIT_BRANCH\n      when: never\n    - if: $CI_MERGE_REQUEST_IID",
		"- if: $CI_COMMIT_BRANCH && !$CI_MERGE_REQUEST_IID\n      when: never\n    - if: $CI_MERGE_REQUEST_IID",
		// A rule without an if matches every pipeline first.
		"- when: on_success\n    - if: $CI_MERGE_REQUEST_IID",
		"- {}\n    - if: $CI_MERGE_REQUEST_IID",
		// Keys other than if, when and allow_failure.
		"- if: $CI_MERGE_REQUEST_IID\n      changes: [lab/**/*]",
		"- if: $CI_MERGE_REQUEST_IID\n      exists: [lab/go.mod]",
		"- if: $CI_MERGE_REQUEST_IID\n      variables: {GOFLAGS: -run=X}",
		"- if: $CI_COMMIT_BRANCH\n      changes: [lab/**/*]\n    - if: $CI_MERGE_REQUEST_IID",
		"- if: $CI_MERGE_REQUEST_IID\n    - if: $CI_COMMIT_BRANCH\n      exists: [x]",
		"- if: $CI_MERGE_REQUEST_IID\n      <<: {when: manual}",
		"- if: [$CI_MERGE_REQUEST_IID]",
		"[]",
	} {
		wantGitLab(t, "j:\n  rules:\n    "+rules+"\n  script: [go test ./...]\n")
	}
}

func TestGitLabBaselineSkipsJobsWithUnreadableNames(t *testing.T) {
	content := "ok_job-1:\n  script: [go vet ./...]\n"
	for _, name := range []string{`"lint: go"`, `"build/test"`, `"a b"`, `"x.y"`, `"` + strings.Repeat("a", 65) + `"`, `"ünicode"`} {
		content += name + ":\n  script: [go test ./...]\n"
	}
	content += strings.Repeat("b", 64) + ":\n  script: [go test ./...]\n"
	wantGitLab(t, content, `baseline-go-vet . ["go" "vet" "./..."]`, `baseline-go-test . ["go" "test" "./..."]`)
	wantGitLab(t, "\""+strings.Repeat("a", 65)+"\":\n  script: [go test ./...]\n")
	wantGitLab(t, "\"a b\":\n  script: [go test ./...]\n")
}

func TestGitLabBaselineHonoursWorkflowRules(t *testing.T) {
	job := "j:\n  script: [go test ./...]\n"
	for _, workflow := range []string{
		"workflow:\n  rules:\n    - if: $CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH\n",
		"workflow:\n  rules:\n    - if: $CI_PIPELINE_SOURCE == \"push\"\n    - if: $CI_PIPELINE_SOURCE == \"schedule\"\n",
		"workflow:\n  rules:\n    - if: $CI_COMMIT_BRANCH\n      when: never\n    - if: $CI_MERGE_REQUEST_IID\n",
		"workflow:\n  rules:\n    - if: $CI_MERGE_REQUEST_IID\n      when: never\n    - when: always\n",
		"workflow:\n  rules:\n    - if: $CI_COMMIT_BRANCH && !$CI_MERGE_REQUEST_IID\n",
		"workflow:\n  rules:\n    - if: $CI_MERGE_REQUEST_IID\n      changes: [x]\n",
		"workflow:\n  rules: []\n",
		"workflow:\n  rules: !reference [.r, rules]\n",
		"workflow: push\n",
		".w: &w {rules: [{if: $CI_COMMIT_BRANCH}]}\nworkflow:\n  <<: *w\n",
	} {
		wantGitLab(t, workflow+job)
	}
	derived := `baseline-go-test . ["go" "test" "./..."]`
	for _, workflow := range []string{
		"workflow:\n  rules:\n    - if: $CI_PIPELINE_SOURCE == \"merge_request_event\"\n    - if: $CI_COMMIT_BRANCH && $CI_OPEN_MERGE_REQUESTS\n      when: never\n    - if: $CI_COMMIT_BRANCH\n",
		"workflow:\n  rules:\n    - if: $CI_COMMIT_TAG\n    - if: $CI_MERGE_REQUEST_IID\n      when: always\n",
		"workflow:\n  name: pipeline\n",
	} {
		wantGitLab(t, workflow+job, derived)
	}
}

func TestGitLabBaselineSkipsComplexJobs(t *testing.T) {
	content := `.base:
  script: [go test ./...]
ext:
  extends: .base
  script: [go test ./...]
ref:
  script:
    - !reference [.base, script]
    - go test ./...
refrules:
  rules: !reference [.rules, rules]
  script: [go test ./...]
trig:
  trigger: child.yml
  script: [go test ./...]
mat:
  parallel:
    matrix:
      - GO: ["1.26", "1.27"]
  script: [go test ./...]
merged:
  <<: *missing
`
	if _, err := DeriveGitLabBaseline(content, gitlabTracked); err == nil {
		t.Fatal("an unknown anchor was accepted")
	}
	wantGitLab(t, strings.TrimSuffix(content, "merged:\n  <<: *missing\n"))
	wantGitLab(t, ".base: &base\n  allow_failure: true\nmerged:\n  <<: *base\n  script: [go test ./...]\n")
	for _, extra := range []string{"allow_failure: true", "allow_failure:\n    exit_codes: [1]", "when: manual", "when: delayed", "include: child.yml", "before_script: [echo hi]"} {
		wantGitLab(t, "j:\n  "+extra+"\n  script: [go test ./...]\n")
	}
	wantGitLab(t, "j:\n  allow_failure: false\n  when: on_success\n  parallel: 2\n  script: [go test ./...]\n", `baseline-go-test . ["go" "test" "./..."]`)
}

func TestGitLabBaselineSharedBeforeScriptSkipsEveryJob(t *testing.T) {
	for _, prefix := range []string{"before_script: [cd lab]\n", "default:\n  before_script: [export GOFLAGS=-run=X]\n", "include: other.yml\n", ".d: &d {}\n<<: *d\n"} {
		wantGitLab(t, prefix+"j:\n  script: [go test ./...]\n")
	}
	wantGitLab(t, "default:\n  image: golang\nj:\n  script: [go test ./...]\n", `baseline-go-test . ["go" "test" "./..."]`)
}

func TestGitLabBaselineStopsAtTheFirstUnrecognizedLine(t *testing.T) {
	wantGitLab(t, "j:\n  script:\n    - go vet ./...\n    - go build ./...\n    - go test ./...\n", `baseline-go-vet . ["go" "vet" "./..."]`)
	for _, first := range []string{"cd lab", "export GOFLAGS=-run=X", "GOFLAGS=x go vet ./...", "# comment", "sh scripts/build-release.sh", "sh scripts/check-missing.sh", "sh lab/scripts/check-lab.sh"} {
		if first == "sh lab/scripts/check-lab.sh" {
			wantGitLab(t, fmt.Sprintf("j:\n  script:\n    - %q\n", first), `baseline-script-lab-scripts-check-lab . ["sh" "lab/scripts/check-lab.sh"]`)
			continue
		}
		wantGitLab(t, fmt.Sprintf("j:\n  script:\n    - %q\n    - go test ./...\n", first))
	}
	// A multi-line block scalar entry is skipped, and so is everything after it.
	wantGitLab(t, "j:\n  script:\n    - |\n      go vet ./...\n      go test ./...\n    - go test ./...\n")
	wantGitLab(t, "j:\n  script: |\n    go vet ./...\n    go test ./...\n")
	wantGitLab(t, "j:\n  script:\n    - [go test ./...]\n    - go vet ./...\n")
}

func TestGitLabBaselineHostileLines(t *testing.T) {
	for _, line := range []string{
		"go test ./...; rm -rf /",
		"go test ./... && curl evil",
		"go test ./... || true",
		"go test ./... | tee out",
		"go test ./... &",
		"go test $(evil) ./...",
		"go test `evil` ./...",
		"cd lab && go test ./... && curl evil",
		"cd lab;go test ./...",
		"cd lab && $(evil)",
		"cd $(evil) && go test ./...",
		"cd `evil` && go test ./...",
		`test -z "$(gofmt -l internal; curl evil)"`,
		`test -z "$(gofmt -l ../internal)"`,
		"sh scripts/check-repo-hygiene.sh; curl evil",
		"sh ../scripts/check-repo-hygiene.sh",
		"go test -timeout 99m ./...",
	} {
		wantGitLab(t, fmt.Sprintf("j:\n  script:\n    - %q\n", line))
	}
}

func TestGitLabBaselineRefusesHostileFiles(t *testing.T) {
	var bomb strings.Builder
	bomb.WriteString("a0: &a0 [x, x, x, x, x, x, x, x, x]\n")
	for i := 1; i < 10; i++ {
		fmt.Fprintf(&bomb, "a%d: &a%d [*a%[3]d, *a%[3]d, *a%[3]d, *a%[3]d, *a%[3]d, *a%[3]d, *a%[3]d, *a%[3]d, *a%[3]d]\n", i, i, i-1)
	}
	bomb.WriteString("j:\n  tags: *a9\n  script: [go test ./...]\n")
	// Aliases are never expanded: the bomb is either refused or read node by node.
	if checks, err := DeriveGitLabBaseline(bomb.String(), gitlabTracked); err == nil && !reflect.DeepEqual(baselineSummary(checks), []string{`baseline-go-test . ["go" "test" "./..."]`}) {
		t.Fatalf("alias bomb: %v", baselineSummary(checks))
	}
	deep := "j:\n  script: " + strings.Repeat("[", 100000) + strings.Repeat("]", 100000) + "\n"
	if checks, err := DeriveGitLabBaseline(deep, gitlabTracked); checks != nil {
		t.Fatalf("deep nesting produced checks: %v %v", baselineSummary(checks), err)
	}

	refused := map[string]string{
		"huge":           "j:\n  script: [go test ./...]\n" + strings.Repeat("# padding\n", 30000),
		"invalid":        "j: [unclosed\n",
		"not a mapping":  "- go test ./...\n",
		"two documents":  "j:\n  script: [go test ./...]\n---\nk:\n  script: [go vet ./...]\n",
		"duplicate top":  "j:\n  script: [go vet ./...]\nj:\n  script: [go test ./...]\n",
		"duplicate job":  "j:\n  script: [go vet ./...]\n  script: [go test ./...]\n",
		"duplicate deep": "j:\n  rules:\n    - if: $CI_MERGE_REQUEST_IID\n      if: $CI_COMMIT_BRANCH\n  script: [go test ./...]\n",
	}
	for name, content := range refused {
		t.Run(name, func(t *testing.T) {
			if checks, err := DeriveGitLabBaseline(content, gitlabTracked); err == nil || checks != nil {
				t.Fatalf("hostile file accepted: %v %v", checks, err)
			} else if strings.Contains(err.Error(), "go test") {
				t.Fatalf("error echoes file content: %v", err)
			}
		})
	}
	for _, empty := range []string{"", "# nothing\n"} {
		if checks, err := DeriveGitLabBaseline(empty, gitlabTracked); err != nil || checks != nil {
			t.Fatalf("empty file: %v %v", checks, err)
		}
	}
}

func TestGitLabBaselineDeduplicatesAndRefusesMoreThanTheCap(t *testing.T) {
	tracked := map[string]bool{}
	var many strings.Builder
	many.WriteString("j:\n  script:\n    - go test ./...\n    - go test ./...\n")
	many.WriteString("k:\n  script:\n    - go test ./...\n    - cd a && go test ./...\n")
	tracked["a/go.mod"] = true
	checks, err := DeriveGitLabBaseline(many.String(), tracked)
	if err != nil || len(checks) != 2 || checks[0].ID != "baseline-go-test" || checks[1].ID != "baseline-go-test-a" {
		t.Fatalf("%v %v", baselineSummary(checks), err)
	}
	var over strings.Builder
	for i := 0; i < 17; i++ {
		tracked[fmt.Sprintf("d%d/go.mod", i)] = true
		fmt.Fprintf(&over, "j%d:\n  script: [cd d%d && go test ./...]\n", i, i)
	}
	if checks, err := DeriveGitLabBaseline(over.String(), tracked); err == nil || checks != nil || !strings.Contains(err.Error(), "17 baseline checks") {
		t.Fatalf("a baseline larger than the Formula allows was not refused: %v %v", len(checks), err)
	}
	trimmed := strings.SplitAfter(over.String(), "]\n")
	if checks, err := DeriveGitLabBaseline(strings.Join(trimmed[:16], ""), tracked); err != nil || len(checks) != 16 {
		t.Fatalf("16 checks: %v %v", len(checks), err)
	}
}

func TestDeriveBaselineForSelectsByCIKind(t *testing.T) {
	r := baselineRepository(map[string]string{".github/workflows/ci.yml": baselineCIWorkflow})
	want, err := DeriveBaseline(r, baselineTracked)
	if err != nil || len(want) == 0 {
		t.Fatal(err)
	}
	gitlab := "j:\n  script: [go vet ./...]\n"
	for _, kind := range []string{"", "github-actions"} {
		got, err := DeriveBaselineFor(kind, r, baselineTracked, gitlab)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%q: %v %v", kind, baselineSummary(got), err)
		}
	}
	got, err := DeriveBaselineFor("gitlab-ci", r, baselineTracked, gitlab)
	if err != nil || !reflect.DeepEqual(baselineSummary(got), []string{`baseline-go-vet . ["go" "vet" "./..."]`}) {
		t.Fatalf("gitlab-ci: %v %v", baselineSummary(got), err)
	}
	if _, err := DeriveBaselineFor("gitlab-ci", r, baselineTracked, "- x\n"); err == nil {
		t.Fatal("gitlab-ci error was dropped")
	}
	for _, kind := range []string{"jenkins", "GitLab-CI", "gitlab-ci "} {
		if got, err := DeriveBaselineFor(kind, r, baselineTracked, gitlab); err == nil || got != nil || !strings.Contains(err.Error(), "CI kind") {
			t.Fatalf("%q accepted: %v %v", kind, got, err)
		}
	}
}

// baseRepositoryEvidence mirrors Repository before the GitLabCI field.
type baseRepositoryEvidence struct {
	Evidence       []readiness.FileEvidence `json:"evidence" yaml:"evidence"`
	EvidenceSHA256 string                   `json:"evidenceSha256" yaml:"evidenceSha256"`
	Root           string                   `json:"root" yaml:"root"`
	Repo           string                   `json:"repo" yaml:"repo"`
	Revision       string                   `json:"revision" yaml:"revision"`
	DefaultBranch  string                   `json:"defaultBranch" yaml:"defaultBranch"`
	Components     []Component              `json:"components" yaml:"components"`
	Workflows      []string                 `json:"workflows" yaml:"workflows"`
	PolicyFiles    []string                 `json:"policyFiles" yaml:"policyFiles"`
	Unknowns       []string                 `json:"unknowns" yaml:"unknowns"`
}

func TestRepositoryRecordsACommittedGitLabPipeline(t *testing.T) {
	root, runner := repoFixture(t)
	r, err := ReadRepository(context.Background(), root, runner)
	if err != nil {
		t.Fatal(err)
	}
	if r.GitLabCI {
		t.Fatal("gitlabCI set without a committed .gitlab-ci.yml")
	}
	base := baseRepositoryEvidence{r.Evidence, r.EvidenceSHA256, r.Root, r.Repo, r.Revision, r.DefaultBranch, r.Components, r.Workflows, r.PolicyFiles, r.Unknowns}
	for name, marshal := range map[string]func(any) ([]byte, error){"json": json.Marshal, "yaml": yaml.Marshal} {
		got, err := marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		want, err := marshal(base)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) || strings.Contains(strings.ToLower(string(got)), "gitlabci") {
			t.Fatalf("%s evidence changed:\n%s\nwant:\n%s", name, got, want)
		}
	}

	if err = os.WriteFile(filepath.Join(root, ".gitlab-ci.yml"), []byte("j:\n  script: [go test ./...]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if r, err = ReadRepository(context.Background(), root, runner); err != nil || r.GitLabCI {
		t.Fatalf("an uncommitted .gitlab-ci.yml set gitlabCI: %v", err)
	}
	if err = os.Remove(filepath.Join(root, ".gitlab-ci.yml")); err != nil {
		t.Fatal(err)
	}
	commitBaselineFiles(t, root, map[string]string{"service/.gitlab-ci.yml": "j:\n  script: [go test ./...]\n"})
	if r, err = ReadRepository(context.Background(), root, runner); err != nil || r.GitLabCI {
		t.Fatalf("a nested .gitlab-ci.yml set gitlabCI: %v", err)
	}
	commitBaselineFiles(t, root, map[string]string{".gitlab-ci.yml": "j:\n  script: [go test ./...]\n"})
	if r, err = ReadRepository(context.Background(), root, runner); err != nil || !r.GitLabCI {
		t.Fatalf("a committed .gitlab-ci.yml did not set gitlabCI: %v", err)
	}
	if text, _ := json.Marshal(r); !strings.Contains(string(text), `"gitlabCI":true`) {
		t.Fatalf("gitlabCI missing from evidence: %s", text)
	}
}
