package molecule

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// numstatRepo is a Lab with one workspace repository: a base commit and the tree
// of a second commit that edits one file, adds a text and a binary file and
// deletes one.
func numstatRepo(t *testing.T) (lab, workspace, base, tree string) {
	t.Helper()
	lab = resolvedTempDir(t)
	workspace = filepath.Join(lab, "workspaces", "issue-1")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		return testGit(t, workspace, append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false"}, args...)...)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(workspace, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	write("edit.txt", "one\ntwo\nthree\n")
	write("gone.txt", "a\nb\n")
	git("add", ".")
	git("commit", "-qm", "base")
	base = git("rev-parse", "HEAD")
	write("edit.txt", "one\n2\nthree\nfour\n")
	write("new.txt", "x\ny\nz\n")
	write("blob.bin", "\x00\x01\x02binary")
	if err := os.Remove(filepath.Join(workspace, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "change")
	tree = git("rev-parse", "HEAD^{tree}")
	return lab, workspace, base, tree
}

func TestDiffNumstatCountsTheCapturedTreeAgainstTheBase(t *testing.T) {
	lab, workspace, base, tree := numstatRepo(t)
	got, err := DiffNumstat(context.Background(), lab, workspace, base, tree)
	if err != nil {
		t.Fatal(err)
	}
	// edit.txt +2 -1, new.txt +3, gone.txt -2, blob.bin binary.
	if want := (DiffStat{Files: 4, Added: 5, Removed: 3, Binary: 1}); got != want {
		t.Fatalf("numstat %+v, want %+v", got, want)
	}
	// The same tree against itself is no change at all.
	if same, err := DiffNumstat(context.Background(), lab, workspace, base, testGit(t, workspace, "rev-parse", base+"^{tree}")); err != nil || same != (DiffStat{}) {
		t.Fatalf("unchanged tree: %+v %v", same, err)
	}
}

func TestDiffNumstatRefusesInvalidIdsAndWorkspaces(t *testing.T) {
	lab, workspace, base, tree := numstatRepo(t)
	for name, c := range map[string][2]string{
		"short":      {base[:12], tree},
		"upper":      {strings.ToUpper(base), tree},
		"option":     {"--output=/tmp/x", tree},
		"expression": {base, "HEAD~1"},
		"range":      {base, base + ".." + tree},
		"empty":      {"", tree},
	} {
		if _, err := DiffNumstat(context.Background(), lab, workspace, c[0], c[1]); !errors.Is(err, ErrDiffStatInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	outside := resolvedTempDir(t)
	link := filepath.Join(lab, "workspaces", "link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"relative":    "workspaces/issue-1",
		"outside":     outside,
		"lab root":    lab,
		"nested":      filepath.Join(workspace, "sub"),
		"traversal":   filepath.Join(lab, "workspaces", "..", "workspaces", "..", "x"),
		"symlink":     link,
		"missing":     filepath.Join(lab, "workspaces", "absent"),
		"dot entries": filepath.Join(lab, "workspaces", ".hidden"),
	} {
		if _, err := DiffNumstat(context.Background(), lab, dir, base, tree); !errors.Is(err, ErrDiffStatInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An object that does not exist fails safe without a size.
	if got, err := DiffNumstat(context.Background(), lab, workspace, base, strings.Repeat("0", 40)); !errors.Is(err, ErrDiffStatFailed) || got != (DiffStat{}) {
		t.Fatalf("missing object: %+v %v", got, err)
	}
}

func TestDiffNumstatParseBoundsFailSafe(t *testing.T) {
	if got := parseNumstat("1\t2\ta.go\x00-\t-\tb.png\x00", true); got != (DiffStat{Files: 2, Added: 1, Removed: 2, Binary: 1}) {
		t.Fatalf("plain: %+v", got)
	}
	// Output cut by the byte bound counts only complete records and is truncated.
	if got := parseNumstat("1\t2\ta.go\x003\t4\tb.g", false); got != (DiffStat{Files: 1, Added: 1, Removed: 2, Truncated: true}) {
		t.Fatalf("cut: %+v", got)
	}
	// Malformed and hostile records are never counted as sizes.
	for _, raw := range []string{"x\ty\ta.go\x00", "1\ta.go\x00", "-1\t2\ta.go\x00", "1\t2\t\x00", "99999999999999999999\t1\ta.go\x00"} {
		if got := parseNumstat(raw, true); !got.Truncated || got.Files != 0 || got.Added != 0 {
			t.Errorf("%q: %+v", raw, got)
		}
	}
	// More files than the bound stop at the bound, truncated.
	var b strings.Builder
	for i := 0; i <= MaxDiffStatFiles; i++ {
		b.WriteString("1\t0\tf\x00")
	}
	if got := parseNumstat(b.String(), true); !got.Truncated || got.Files != MaxDiffStatFiles || got.Added != MaxDiffStatFiles {
		t.Fatalf("file bound: %+v", got)
	}
}

// A change whose numstat output exceeds the byte bound is reported truncated with
// the complete records read, never as the whole change.
func TestDiffNumstatOutputBoundIsTruncated(t *testing.T) {
	lab, workspace, base, _ := numstatRepo(t)
	// Long names make the output exceed MaxDiffStatBytes with few files.
	name := strings.Repeat("n", 200)
	files := MaxDiffStatBytes/len(name) + 64
	for i := 0; i < files; i++ {
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("%s-%06d", name, i)), []byte("x\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	testGit(t, workspace, "add", "-A")
	testGit(t, workspace, "-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false", "commit", "-qm", "many")
	tree := testGit(t, workspace, "rev-parse", "HEAD^{tree}")
	got, err := DiffNumstat(context.Background(), lab, workspace, base, tree)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.Files == 0 || got.Files >= files {
		t.Fatalf("over the byte bound: %+v of %d files", got, files)
	}
}
