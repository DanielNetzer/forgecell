package verification

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func checkPlan(dir, base string) readiness.Plan {
	data, _ := os.ReadFile(filepath.Join(dir, "check.sh"))
	return readiness.Plan{SchemaVersion: "v1", MoleculeID: "mol-test", Revision: 1, Inputs: readiness.Inputs{Repository: "owner/repo", TargetBranch: "main", BaseCommit: base, FormulaSHA256: strings.Repeat("a", 64), EvidenceSHA256: strings.Repeat("b", 64), BindingSHA256: strings.Repeat("c", 64), Issue: readiness.Issue{Repository: "owner/repo", Number: 1, URL: "https://github.com/owner/repo/issues/1", State: "OPEN", Title: "Update code"}}, Evidence: []readiness.Evidence{{ID: "check", Path: "check.sh", SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}}, Analysis: readiness.Analysis{Summary: "Update code", Acceptance: []readiness.Criterion{{Description: "Code returns changed", Evidence: []string{"issue"}}}, Scope: []readiness.ScopedPath{{Path: "code.txt", Reason: "Implementation", Evidence: []string{"issue"}}}, Checks: []readiness.Check{{ID: "regression", Category: "regression", Dir: ".", Argv: []string{"/bin/sh", "check.sh"}, TimeoutMS: 1000, Required: true, Reason: "Verify implementation", Evidence: []string{"check"}, Definitions: []string{"check"}}}}, Policy: readiness.ExecutionPolicy{Environment: []string{"PATH"}, SetupNetwork: "unrestricted", CheckNetwork: "unrestricted", Containment: "filtered-environment"}}
}
func TestFreshVerificationDoesNotReuseIgnoredFilesOrSecrets(t *testing.T) {
	dir, base := sourceFixture(t)
	// Put the checker in the reviewed baseline so candidate checks cannot invent
	// their own oracle. Existing ignored content and host credentials must not leak.
	script := "#!/bin/sh\ntest ! -e ignored.txt && test -z \"$FORGECELL_TEST_SECRET\" && test \"$(cat code.txt)\" = changed\n"
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte(script), 0600)
	gitTest(t, dir, "add", "check.sh")
	gitTest(t, dir, "commit", "-m", "acceptance")
	base = gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	os.WriteFile(filepath.Join(dir, "code.txt"), []byte("changed"), 0600)
	capture, err := Capture(context.Background(), dir, base, []string{"code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("coding checkout only"), 0600)
	t.Setenv("FORGECELL_TEST_SECRET", "not-forwarded")
	dest := filepath.Join(t.TempDir(), "verification")
	result := Run(context.Background(), dir, dest, p, capture)
	if !result.RequiredChecksPassed || result.SourceTree != capture.Tree || len(result.Checks) != 1 {
		t.Fatalf("%+v", result)
	}
	if result.IndependentAcceptanceVerified {
		t.Fatal("regression promoted to independent acceptance")
	}
	if _, err := os.Stat(filepath.Join(dest, "ignored.txt")); !os.IsNotExist(err) {
		t.Fatal("ignored coding data copied")
	}
}
func TestChangedCheckDefinitionStopsBeforeExecution(t *testing.T) {
	dir, base := sourceFixture(t)
	p := checkPlan(dir, base)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "check.sh", Reason: "Checker", Evidence: []string{"check"}})
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte("touch should-not-run\n"), 0600)
	c, err := Capture(context.Background(), dir, base, []string{"check.sh", "code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if v.RequiredChecksPassed || len(v.Checks) != 0 || v.Error == "" {
		t.Fatalf("changed checker accepted: %+v", v)
	}
}
func TestFailedChecksAndMutatingChecksCannotPass(t *testing.T) {
	for _, script := range []string{"exit 7\n", "printf tampered > code.txt\n"} {
		t.Run(script, func(t *testing.T) {
			dir, _ := sourceFixture(t)
			os.WriteFile(filepath.Join(dir, "check.sh"), []byte(script), 0600)
			gitTest(t, dir, "add", "check.sh")
			gitTest(t, dir, "commit", "-m", "checker")
			base := gitTest(t, dir, "rev-parse", "HEAD")
			p := checkPlan(dir, base)
			c, err := Capture(context.Background(), dir, base, []string{"code.txt"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
			if v.RequiredChecksPassed {
				t.Fatalf("invalid check passed: %+v", v)
			}
		})
	}
}

func TestWeakenedHelperCannotReplaceProtectedRegression(t *testing.T) {
	dir, _ := sourceFixture(t)
	os.Mkdir(filepath.Join(dir, "tests"), 0700)
	os.WriteFile(filepath.Join(dir, "tests", "assert.sh"), []byte("test \"$(cat code.txt)\" = base\n"), 0600)
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte(". tests/assert.sh\n"), 0600)
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "baseline regression")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "tests/assert.sh", Reason: "Update test", Evidence: []string{"issue"}})
	os.WriteFile(filepath.Join(dir, "code.txt"), []byte("broken"), 0600)
	os.WriteFile(filepath.Join(dir, "tests", "assert.sh"), []byte("true\n"), 0600)
	c, err := Capture(context.Background(), dir, base, []string{"code.txt", "tests/assert.sh"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	v := Run(context.Background(), dir, filepath.Join(t.TempDir(), "verify"), p, c)
	if v.RequiredChecksPassed || v.Protected == nil || v.Protected.RequiredChecksPassed || len(v.Checks) != 1 || !v.Checks[0].Result.OK || v.Protected.SourceTree == v.SourceTree {
		t.Fatalf("weakened test hid regression: %+v", v)
	}
}

func TestHistoryPersistenceFailureStopsProtectedExecution(t *testing.T) {
	dir, _ := sourceFixture(t)
	os.Mkdir(filepath.Join(dir, "tests"), 0700)
	os.WriteFile(filepath.Join(dir, "tests", "assert.sh"), []byte("true\n"), 0600)
	os.WriteFile(filepath.Join(dir, "check.sh"), []byte(". tests/assert.sh\n"), 0600)
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "baseline")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	p := checkPlan(dir, base)
	p.Analysis.Scope = append(p.Analysis.Scope, readiness.ScopedPath{Path: "tests/assert.sh", Reason: "Change test", Evidence: []string{"issue"}})
	os.WriteFile(filepath.Join(dir, "tests", "assert.sh"), []byte("echo candidate\n"), 0600)
	c, e := Capture(context.Background(), dir, base, []string{"code.txt", "tests/assert.sh"}, nil)
	if e != nil {
		t.Fatal(e)
	}
	dest := filepath.Join(t.TempDir(), "verify")
	v := RunObserved(context.Background(), dir, dest, p, c, func(Observation) error { return fmt.Errorf("disk full") })
	if v.RequiredChecksPassed || v.Protected != nil {
		t.Fatal("continued after evidence persistence failure")
	}
	if _, e = os.Stat(dest + "-protected"); !os.IsNotExist(e) {
		t.Fatal("protected commands started after persistence failure")
	}
}
