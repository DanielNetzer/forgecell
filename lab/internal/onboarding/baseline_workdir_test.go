package onboarding

import (
	"reflect"
	"strings"
	"testing"
)

func workdirWorkflow(dir, run string) string {
	step := "      - run: " + run + "\n"
	if dir != "" {
		step = "      - working-directory: " + dir + "\n        run: " + run + "\n"
	}
	return baselineStepsWorkflow("pull_request", "", step)
}

func deriveWorkdir(t *testing.T, tracked map[string]bool, dir, run string) []string {
	t.Helper()
	r := baselineRepository(map[string]string{".github/workflows/ci.yml": workdirWorkflow(dir, run)})
	checks, err := DeriveBaseline(r, tracked)
	if err != nil {
		t.Fatal(err)
	}
	return baselineSummary(checks)
}

func TestBaselineWorkingDirectoryResolvesScriptsAgainstTheStepDirectory(t *testing.T) {
	nested := map[string]bool{"service/scripts/check.sh": true}
	want := []string{`baseline-script-scripts-check-service service ["sh" "scripts/check.sh"]`}
	if got := deriveWorkdir(t, nested, "service", "sh scripts/check.sh"); !reflect.DeepEqual(got, want) {
		t.Fatalf("a script tracked under the working directory must be recognized: %v", got)
	}
	rootOnly := map[string]bool{"scripts/check.sh": true}
	if got := deriveWorkdir(t, rootOnly, "service", "sh scripts/check.sh"); len(got) != 0 {
		t.Fatalf("a script that exists only root-relative must not be recognized from service: %v", got)
	}
	if got := deriveWorkdir(t, nested, "", "sh scripts/check.sh"); len(got) != 0 {
		t.Fatalf("a script tracked only under service must not be recognized from the root: %v", got)
	}
	wantRoot := []string{`baseline-script-scripts-check . ["sh" "scripts/check.sh"]`}
	if got := deriveWorkdir(t, rootOnly, "", "sh scripts/check.sh"); !reflect.DeepEqual(got, wantRoot) {
		t.Fatalf("a root script must still be recognized: %v", got)
	}
	if got := deriveWorkdir(t, rootOnly, ".", "sh scripts/check.sh"); !reflect.DeepEqual(got, wantRoot) {
		t.Fatalf("an explicit root directory must still be recognized: %v", got)
	}
}

func TestBaselineWorkingDirectoryRecognizeLineKeepsItsSignature(t *testing.T) {
	var recognize func(string, map[string]bool) (baselineCommand, bool) = recognizeBaselineLine
	if c, ok := recognize("sh scripts/check.sh", map[string]bool{"scripts/check.sh": true}); !ok || c.display != "sh scripts/check.sh" {
		t.Fatalf("%+v %v", c, ok)
	}
}

func TestBaselineWorkingDirectoryComplexWords(t *testing.T) {
	tracked := map[string]bool{"scripts/check.sh": true}
	for _, prefix := range []string{
		"export GOFLAGS=-mod=mod", "set -e", "set -x", "source ./env.sh", ". ./env.sh", "unset GOFLAGS", "GOFLAGS=-mod=mod", "CGO_ENABLED=0", "_X=1",
	} {
		run := "|\n          " + prefix + "\n          go test ./...\n          sh scripts/check.sh\n"
		if got := deriveWorkdir(t, tracked, "", run); len(got) != 0 {
			t.Fatalf("a block starting with %q must be skipped as complex: %v", prefix, got)
		}
	}
	for _, line := range []string{"go test ./...", "sh scripts/check.sh"} {
		if baselineComplex([]string{line}) {
			t.Fatalf("%q is a plain command", line)
		}
	}
	if baselineComplex([]string{"go test ./... -run X=1"}) {
		t.Fatal("an assignment-like later word must not make a command complex")
	}
	if !strings.Contains(strings.Join(workdirKeys(baselineComplexWords), " "), "unset") {
		t.Fatal("unset missing from the complex words")
	}
}

func TestBaselineWorkingDirectoryComplexWordsSplitOnAnyWhitespace(t *testing.T) {
	for _, line := range []string{"export\tGOFLAGS=-mod=mod", "set\t-e", "eval \"$X\"", "eval\t\"$X\"", "exec go test", "exit 0", "readonly X", "declare -x X=1", "GOFLAGS=-mod=mod\tgo test ./..."} {
		if !baselineComplex([]string{line}) {
			t.Fatalf("%q must make a block complex", line)
		}
	}
}

func workdirKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
