package delivery

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// The protected identity delivery rebuilds must honour the plan's exemptions, or a
// verified evaluation run could never be re-checked.
func TestValidateReadinessHonoursProtectedExemptPaths(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		b, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s %v", args, b, e)
		}
		return strings.TrimSpace(string(b))
	}
	write := func(file, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	checker := "grep -q after a_test.go\n"
	git("init", "-b", "main")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	write("a.txt", "before\n")
	write("a_test.go", "before\n")
	write("b_test.go", "before\n")
	write("check.sh", checker)
	git("add", ".")
	git("commit", "-m", "base")
	sha := git("rev-parse", "HEAD")
	write("a.txt", "after\n")
	write("a_test.go", "after\n")
	write("b_test.go", "after\n")

	formulaSHA := strings.Repeat("a", 64)
	plan := func(exempt []string) readiness.Plan {
		return readiness.Plan{SchemaVersion: "v1", MoleculeID: "mol-exempt", Revision: 1,
			Inputs:   readiness.Inputs{Repository: "owner/repo", TargetBranch: "main", BaseCommit: sha, FormulaSHA256: formulaSHA, EvidenceSHA256: strings.Repeat("b", 64), BindingSHA256: strings.Repeat("c", 64), Issue: readiness.Issue{Repository: "owner/repo", Number: 9, Title: "Change a", URL: "https://github.com/owner/repo/issues/9", State: "OPEN"}},
			Evidence: []readiness.Evidence{{ID: "checker", Path: "check.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(checker)))}},
			Analysis: readiness.Analysis{Summary: "Change a and its test",
				Scope:      []readiness.ScopedPath{{Path: "a.txt", Reason: "Requested change", Evidence: []string{"issue"}}, {Path: "a_test.go", Reason: "Updated test", Evidence: []string{"issue"}}, {Path: "b_test.go", Reason: "Other test", Evidence: []string{"issue"}}},
				Acceptance: []readiness.Criterion{{Description: "a contains after", Evidence: []string{"issue"}}},
				Checks:     []readiness.Check{{ID: "regression", Category: "regression", Argv: []string{"/bin/sh", "check.sh"}, Dir: ".", TimeoutMS: 10000, Required: true, Definitions: []string{"checker"}, Reason: "Protected checker", Evidence: []string{"issue"}}}},
			Policy:               readiness.ExecutionPolicy{Environment: []string{"PATH"}, SetupNetwork: "unrestricted", CheckNetwork: "unrestricted", Containment: "filtered-environment"},
			ProtectedExemptPaths: exempt,
		}
	}
	record := func(p readiness.Plan) molecule.Record {
		t.Helper()
		s, err := readiness.NewState(p, "now")
		if err != nil {
			t.Fatal(err)
		}
		d, err := p.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if s, err = s.Approve(d, p.Inputs, "approve"); err != nil {
			t.Fatal(err)
		}
		s, _ = s.StartAttempt("start")
		s, _ = s.FinishAttempt("check-pending", "captured", "end")
		s, _ = s.RecordVerification(true, "fixture verified", "checked")
		return molecule.Record{ID: p.MoleculeID, FormulaSnapshot: molecule.Snapshot{SHA256: formulaSHA}, Workspace: molecule.Workspace{Path: root, BaseCommit: sha}, Issue: molecule.Issue{Repo: "owner/repo", Number: 9}, Readiness: &s}
	}

	exempt := plan([]string{"a_test.go"})
	c, err := verification.Capture(context.Background(), root, sha, []string{"a.txt", "a_test.go", "b_test.go"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := verification.Run(context.Background(), root, filepath.Join(t.TempDir(), "verify"), exempt, c)
	if !v.RequiredChecksPassed || v.Protected == nil || !v.Protected.RequiredChecksPassed || v.Protected.SourceTree == v.SourceTree {
		t.Fatalf("exempt protected run: %+v", v)
	}
	r := record(exempt)
	r.Verification = &v
	if err := validateReadiness(context.Background(), Options{BaseBranch: "main"}, r, ""); err != nil {
		t.Fatalf("exempt protected identity refused: %v", err)
	}

	// The same evidence under a plan without the exemption no longer matches the
	// rebuilt protected tree, which restores a_test.go to base.
	r = record(plan(nil))
	r.Verification = &v
	if err := validateReadiness(context.Background(), Options{BaseBranch: "main"}, r, ""); err == nil || err.Error() != "missing successful protected regression evidence" {
		t.Fatalf("unexempt plan accepted exempt evidence: %v", err)
	}
}
