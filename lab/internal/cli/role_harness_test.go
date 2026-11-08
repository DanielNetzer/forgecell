package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// roleHarnessRepo is a hermetic checkout of acme/widgets the CLI runs init in.
func roleHarnessRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	gsWrite(t, filepath.Join(repo, "code.txt"), "base", 0600)
	gsEnv(t, "\"repo view\"*) printf '%s' '{\"nameWithOwner\":\"acme/widgets\",\"defaultBranchRef\":{\"name\":\"main\"}}'; exit 0;;\n")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}, {"add", "."}, {"commit", "-qm", "base"}, {"remote", "add", "origin", "https://github.com/acme/widgets.git"}} {
		gsGit(t, repo, args...)
	}
	t.Chdir(repo)
	return root
}

func TestHarnessRoleFlagConflictsWriteNothing(t *testing.T) {
	lab := t.TempDir()
	cases := map[string]struct {
		args []string
		want string
	}{
		"role alone":            {[]string{"--role", "analysis"}, "--role needs --model ID to pin a model, or --harness ID"},
		"role with rebind":      {[]string{"--role", "analysis", "--rebind", "codex"}, "--role cannot be combined with --rebind"},
		"harness role rebind":   {[]string{"--harness", "claude-code", "--role", "analysis", "--rebind", "codex"}, "--role cannot be combined with --rebind"},
		"harness role meta":     {[]string{"--harness", "claude-code", "--role", "analysis", "--meta-harness", "codex"}, "cannot be combined with --meta-harness or --meta-command"},
		"harness role command":  {[]string{"--harness", "claude-code", "--role", "analysis", "--meta-command", `["/bin/x"]`}, "cannot be combined with --meta-harness or --meta-command"},
		"harness role timeout":  {[]string{"--harness", "claude-code", "--role", "analysis", "--harness-timeout-ms", "1000"}, "cannot be combined with the timeout flags"},
		"harness role max":      {[]string{"--harness", "claude-code", "--role", "analysis", "--max-timeout-ms", "1000"}, "cannot be combined with the timeout flags"},
		"harness role review":   {[]string{"--harness", "claude-code", "--role", "analysis", "--review", "assay-x"}, "review, approve or dismiss the saved proposal in a separate command"},
		"harness role approve":  {[]string{"--harness", "claude-code", "--role", "analysis", "--approve", "assay-x"}, "review, approve or dismiss the saved proposal in a separate command"},
		"harness role dismiss":  {[]string{"--harness", "claude-code", "--role", "analysis", "--dismiss", "assay-x"}, "review, approve or dismiss the saved proposal in a separate command"},
		"model role harness":    {[]string{"--model", "m", "--role", "coding", "--harness", "claude-code"}, "--model changes only the model pin"},
		"model role still pins": {[]string{"--model", "--bad", "--role", "coding"}, "--model"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := append([]string{"init", "--lab", lab}, c.args...)
			if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "dev"); code != 1 || !strings.Contains(stderr.String(), c.want) {
				t.Fatalf("exit %d stderr %q, want %q", code, stderr.String(), c.want)
			}
			if entries, _ := os.ReadDir(lab); len(entries) != 0 {
				t.Fatalf("a refused init wrote to the Lab: %v", entries)
			}
		})
	}
}

func TestHarnessRoleReachesTheRoleProposal(t *testing.T) {
	root := roleHarnessRepo(t)

	// Without an approved Formula the request is refused as a role harness change,
	// not as a --role without --model, and nothing is written.
	empty := filepath.Join(root, "empty-lab")
	if err := os.Mkdir(empty, 0700); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := gsRun([]string{"init", "--lab", empty, "--harness", "claude-code", "--role", "analysis"})
	if code != 1 || !strings.Contains(errOut, "--harness --role needs an approved Formula") {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 0 {
		t.Fatalf("a refused init wrote to the Lab: %v", entries)
	}

	// With a pending proposal the request is refused and names that proposal.
	lab := filepath.Join(root, "lab")
	gsApproveFormula(t, lab, modelPinFormula)
	code, out, errOut = gsRun([]string{"init", "--lab", lab, "--model", "claude-opus-4-8"})
	if code != 0 {
		t.Fatalf("init --model exit %d: %s %s", code, out, errOut)
	}
	id := regexp.MustCompile(`assay-[a-f0-9]{64}`).FindString(out)
	if id == "" {
		t.Fatalf("no proposal id:\n%s", out)
	}
	before, _ := os.ReadDir(filepath.Join(lab, "assays"))
	code, out, errOut = gsRun([]string{"init", "--lab", lab, "--harness", "claude-code", "--role", "coding"})
	if code != 1 || !strings.Contains(errOut, "already pending") || !strings.Contains(errOut, id) || !strings.Contains(errOut, "before --harness --role") {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if after, _ := os.ReadDir(filepath.Join(lab, "assays")); len(after) != len(before) {
		t.Fatal("a refused init saved a proposal")
	}
}

func TestHarnessRoleIsDocumentedInHelp(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"help"}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"init --harness ID --role analysis|coding|meta", "init --model ID [--role analysis|coding|meta|review]"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help omits %q", want)
		}
	}
}
