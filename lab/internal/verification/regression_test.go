package verification

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func TestProtectedTreeProtectedExemptPathsKeepCandidateVersion(t *testing.T) {
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "config", "user.email", "test@example.test")
	gitTest(t, dir, "config", "user.name", "Test")
	write := func(file, data string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("a_test.go", "package a // base a")
	write("b_test.go", "package a // base b")
	write("code.go", "package a")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "base")
	base := gitTest(t, dir, "rev-parse", "HEAD")
	write("a_test.go", "package a // candidate a")
	write("b_test.go", "package a // candidate b")
	write("c_test.go", "package a // new exempt c")
	write("d_test.go", "package a // new protected d")
	write("code.go", "package a // candidate")
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "candidate")
	candidate := gitTest(t, dir, "rev-parse", "HEAD^{tree}")

	p := readiness.Plan{Inputs: readiness.Inputs{BaseCommit: base}, ProtectedExemptPaths: []string{"a_test.go", "c_test.go"}}
	tree, err := ProtectedTree(context.Background(), dir, p, candidate)
	if err != nil {
		t.Fatal(err)
	}
	blob := func(rev, file string) string { return gitTest(t, dir, "rev-parse", rev+":"+file) }
	if blob(tree, "a_test.go") != blob(candidate, "a_test.go") {
		t.Fatal("exempt a_test.go was restored to base")
	}
	if blob(tree, "b_test.go") != blob(base, "b_test.go") {
		t.Fatal("non-exempt b_test.go kept its candidate version")
	}
	if blob(tree, "c_test.go") != blob(candidate, "c_test.go") {
		t.Fatal("exempt new c_test.go was not kept")
	}
	if gitTest(t, dir, "ls-tree", "--name-only", tree, "--", "d_test.go") != "" {
		t.Fatal("non-exempt new d_test.go was not removed")
	}
	if blob(tree, "code.go") != blob(candidate, "code.go") {
		t.Fatal("candidate source was not kept")
	}

	p.ProtectedExemptPaths = nil
	plain, err := ProtectedTree(context.Background(), dir, p, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if blob(plain, "a_test.go") != blob(base, "a_test.go") || gitTest(t, dir, "ls-tree", "--name-only", plain, "--", "c_test.go") != "" {
		t.Fatal("without exemptions every changed test must be restored as before")
	}
}
