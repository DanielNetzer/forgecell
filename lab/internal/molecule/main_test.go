package molecule

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// baseRepo is the committed fixture repository, built once per test binary.
// fixture copies it with os.CopyFS instead of running git for every test; a
// fresh .git holds no absolute paths, so a copy is a complete repository.
var baseRepo string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	// TMPDIR and GOTMPDIR are left as the environment spelled them, so the
	// package still runs Molecules under a symlinked temp root (macOS's
	// /var/folders); a test that needs resolved paths uses resolvedTempDir.
	dir, err := os.MkdirTemp("", "molecule-base-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	if baseRepo, err = buildBaseRepo(filepath.Join(dir, "repo")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// resolvedTempDir is t.TempDir with symlinks resolved, so a test does not
// depend on how the environment's temp roots are spelled.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func buildBaseRepo(repo string) (string, error) {
	if err := os.Mkdir(repo, 0700); err != nil {
		return "", err
	}
	run := func(args ...string) error {
		c := exec.Command("git", args...)
		c.Dir = repo
		if b, err := c.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %s %v", args, b, err)
		}
		return nil
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Test"}, {"config", "user.email", "test@example.test"}} {
		if err := run(args...); err != nil {
			return "", err
		}
	}
	for name, body := range map[string]string{"code.txt": "base", "package.json": `{"scripts":{"test":"test x = x"}}`} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
			return "", err
		}
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-qm", "base"}, {"remote", "add", "origin", "https://github.com/acme/widgets.git"}} {
		if err := run(args...); err != nil {
			return "", err
		}
	}
	// The sample hooks are never run; dropping them keeps each copy small.
	return repo, os.RemoveAll(filepath.Join(repo, ".git", "hooks"))
}
