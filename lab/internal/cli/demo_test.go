package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/demo"
)

func runDemoCLI(args ...string) (int, string, string) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "demo-test")
	return code, out.String(), stderr.String()
}

// stubDemoRunner replaces the demo runner and records the options it receives.
func stubDemoRunner(t *testing.T, result error) *[]demo.Options {
	t.Helper()
	calls := &[]demo.Options{}
	previous := runDemoWith
	runDemoWith = func(_ context.Context, o demo.Options) error {
		*calls = append(*calls, o)
		return result
	}
	t.Cleanup(func() { runDemoWith = previous })
	return calls
}

func TestDemoIsTheFirstCommandInTheUsage(t *testing.T) {
	code, out, _ := runDemoCLI("--help")
	if code != 0 {
		t.Fatalf("help exited %d", code)
	}
	lines := strings.Split(out, "\n")
	if len(lines) < 3 || lines[2] != "demo [--keep DIR]" {
		t.Fatalf("the first usage line is not the demo: %q", lines[:min(3, len(lines))])
	}
	if !strings.Contains(out, "demo is the first command to run after install") || !strings.Contains(out, "never runs gh") {
		t.Fatal("the help does not explain the demo")
	}
}

func TestDemoCommandParsesKeepAndReportsTheExitCode(t *testing.T) {
	calls := stubDemoRunner(t, nil)
	if code, _, stderr := runDemoCLI("demo"); code != 0 || stderr != "" {
		t.Fatalf("demo exited %d: %s", code, stderr)
	}
	if code, _, stderr := runDemoCLI("demo", "--keep", "kept"); code != 0 || stderr != "" {
		t.Fatalf("demo --keep exited %d: %s", code, stderr)
	}
	if len(*calls) != 2 || (*calls)[0].Keep != "" || (*calls)[1].Keep != "kept" || (*calls)[1].Out == nil || len((*calls)[1].StubCommand) != 0 {
		t.Fatalf("the runner received %+v", *calls)
	}
	for _, args := range [][]string{{"demo", "extra"}, {"demo", "--keep"}, {"demo", "--keep="}, {"demo", "--lab", "x"}} {
		if code, out, _ := runDemoCLI(args...); code == 0 || out != "" {
			t.Fatalf("%v was accepted: %d %q", args, code, out)
		}
	}
	if len(*calls) != 2 {
		t.Fatalf("a refused invocation reached the runner: %+v", *calls)
	}

	stubDemoRunner(t, errors.New("the demo Molecule stopped"))
	if code, _, stderr := runDemoCLI("demo"); code != 1 || !strings.Contains(stderr, "forgecell: demo: the demo Molecule stopped") {
		t.Fatalf("a failed demo exited %d: %s", code, stderr)
	}
}

func TestDemoCommandRefusesAnUnsafeKeepTarget(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir("empty", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("empty", "link"); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runDemoCLI("demo", "--keep", "link")
	if code != 1 || !strings.Contains(stderr, "is a symbolic link") || !strings.HasPrefix(out, demo.Banner+"\n") || strings.Contains(out+stderr, root) {
		t.Fatalf("an unsafe --keep target was not refused: %d %q %q", code, out, stderr)
	}
	if entries, err := os.ReadDir(filepath.Join(root, "empty")); err != nil || len(entries) != 0 {
		t.Fatalf("the demo wrote through the symbolic link: %v %v", entries, err)
	}
}

// keptDemoLab is the state a kept demo Lab has for these commands: the demo's
// Formula activation and one ledger. PATH holds only a git and a gh that record
// any call and fail.
func keptDemoLab(t *testing.T) (lab, log string) {
	t.Helper()
	lab = t.TempDir()
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := `{"schemaVersion":"v0","activeFormulaId":"forgecell-demo","currentApproval":{"formulaId":"forgecell-demo","sha256":"` + strings.Repeat("a", 64) + `","decisionId":"demo-built-in-formula","sourceKind":"demo","sourceId":"forgecell demo built-in Formula"}}`
	if err := os.WriteFile(filepath.Join(lab, "lab.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := `{"schemaVersion":"v0","kind":"molecule","id":"mol-1-demo","formulaId":"forgecell-demo","status":"waiting"}`
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "mol-1-demo.json"), []byte(ledger), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	log = filepath.Join(t.TempDir(), "calls.log")
	for _, name := range []string{"git", "gh"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s %s\\n' "+name+" \"$*\" >> '"+log+"'\nexit 1\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv("FORGECELL_LAB", "")
	return lab, log
}

func TestGitHubCommandsRefuseAKeptDemoLabBeforeAnyProcess(t *testing.T) {
	lab, log := keptDemoLab(t)
	t.Chdir(t.TempDir())
	if err := os.Symlink(lab, "linked"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"deliver", "mol-1-demo", "--lab", lab, "--base", "main", "--", "greeting.go"},
		{"deliver", "mol-1-demo", "--lab", lab, "--base", "main", "--approve", strings.Repeat("a", 64), "--title", "Add Greeting", "--", "greeting.go"},
		{"run", "1", "--lab", lab},
		{"run", "1", "-lab=" + lab, "--approve", strings.Repeat("b", 64)},
		{"loop", "--lab", lab, "--once"},
		{"loop", "--lab=linked", "--watch"},
		{"issues", "--repo", "forgecell-demo/greeting", "--lab", lab},
		{"checks", "--lab", lab, "--molecule", "mol-1-demo", "--repo", "forgecell-demo/greeting", "--pr", "1", "--commit", strings.Repeat("c", 40), "--required", "ci"},
		// A later --lab cannot hide an earlier demo Lab.
		{"run", "1", "--lab", lab, "--lab", t.TempDir()},
	} {
		code, out, stderr := runDemoCLI(args...)
		if code == 0 || out != "" || !strings.Contains(stderr, "this is a forgecell demo Lab; it never contacts GitHub") {
			t.Fatalf("%v was not refused: %d %q %q", args[0], code, out, stderr)
		}
	}
	if raw, err := os.ReadFile(log); err == nil && len(strings.TrimSpace(string(raw))) != 0 {
		t.Fatalf("a refused command started a process:\n%s", raw)
	}
	if _, err := os.Lstat(filepath.Join(lab, ".delivery-lock")); !os.IsNotExist(err) {
		t.Fatalf("a refused command took the delivery lock: %v", err)
	}
}

func TestReadOnlyInspectionStillWorksOnAKeptDemoLab(t *testing.T) {
	lab, log := keptDemoLab(t)
	code, out, stderr := runDemoCLI("ledger", "mol-1-demo", "--lab", lab)
	if code != 0 || !strings.Contains(out, `"forgecell-demo"`) || strings.Contains(stderr, "demo Lab") {
		t.Fatalf("ledger on a kept demo Lab: %d %q %q", code, out, stderr)
	}
	if raw, err := os.ReadFile(log); err == nil && len(strings.TrimSpace(string(raw))) != 0 {
		t.Fatalf("ledger started a process:\n%s", raw)
	}
	// A Lab that is not a demo Lab is not refused by the demo check.
	if err := refuseDemoLab([]string{"run", "1", "--lab", t.TempDir()}, ""); err != nil {
		t.Fatalf("a Lab without lab.json was refused: %v", err)
	}
	if err := refuseDemoLab([]string{"ledger", "mol-1-demo", "--lab", lab}, ""); err != nil {
		t.Fatalf("ledger was refused: %v", err)
	}
	if err := refuseDemoLab([]string{"deliver", "mol-1-demo"}, lab); err == nil {
		t.Fatal("the default Lab was not checked")
	}
}

func TestDemoStubSubcommandRefusesAnythingButItsRequest(t *testing.T) {
	t.Chdir(t.TempDir())
	if code, out, stderr := runDemoCLI(demo.StubArg, "review"); code == 0 || out != "" || !strings.Contains(stderr, "only serves the coding request") {
		t.Fatalf("the stub served a review: %d %q %q", code, out, stderr)
	}
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{demo.StubArg}, strings.NewReader(`{"kind":"molecule","issue":{"repo":"acme/widgets","number":1}}`), &out, &stderr, "demo-test")
	if code == 0 || out.Len() != 0 || !strings.Contains(stderr.String(), "only the demo issue") {
		t.Fatalf("the stub served another repository: %d %q %q", code, out.String(), stderr.String())
	}
}
