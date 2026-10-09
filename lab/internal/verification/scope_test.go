package verification

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %s %v", args, b, e)
	}
	return strings.TrimSpace(string(b))
}
func sourceFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-b", "main")
	gitTest(t, dir, "config", "user.email", "test@example.test")
	gitTest(t, dir, "config", "user.name", "Test")
	for file, data := range map[string]string{"code.txt": "base", ".gitignore": "ignored.txt\noutput/\n", "check.sh": "#!/bin/sh\ntest x = x\n"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, dir, "add", ".")
	gitTest(t, dir, "commit", "-m", "base")
	return dir, gitTest(t, dir, "rev-parse", "HEAD")
}
func TestCaptureIncludesIgnoredWritesAndDoesNotStage(t *testing.T) {
	dir, base := sourceFixture(t)
	os.WriteFile(filepath.Join(dir, "code.txt"), []byte("changed"), 0600)
	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("hidden"), 0600)
	before := gitTest(t, dir, "write-tree")
	got, err := Capture(context.Background(), dir, base, []string{"code.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tree == "" || len(got.Violations) != 1 || got.Violations[0] != "ignored.txt" {
		t.Fatalf("%+v", got)
	}
	if gitTest(t, dir, "write-tree") != before {
		t.Fatal("capture changed the user's index")
	}
	if gitTest(t, dir, "show", got.Tree+":ignored.txt") != "hidden" {
		t.Fatal("ignored violation not retained in captured tree")
	}
	again, err := Capture(context.Background(), dir, base, []string{"code.txt", "ignored.txt"}, nil)
	if err != nil || len(again.Violations) != 0 {
		t.Fatalf("%+v %v", again, err)
	}
}
func TestArtifactsDoNotAuthorizeSourceChanges(t *testing.T) {
	dir, base := sourceFixture(t)
	os.Mkdir(filepath.Join(dir, "output"), 0700)
	os.WriteFile(filepath.Join(dir, "output", "cache"), []byte("build"), 0600)
	got, err := Capture(context.Background(), dir, base, []string{"code.txt"}, []readiness.ArtifactRoot{{Path: "output", CommandID: "build"}})
	if err != nil || len(got.Changes) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := Capture(context.Background(), dir, base, []string{"code.txt"}, []readiness.ArtifactRoot{{Path: "code.txt", CommandID: "build"}}); err == nil {
		t.Fatal("tracked source excluded as artifact")
	}
	os.Symlink(t.TempDir(), filepath.Join(dir, "redirect"))
	if _, err := Capture(context.Background(), dir, base, []string{"code.txt"}, []readiness.ArtifactRoot{{Path: "redirect", CommandID: "build"}}); err == nil {
		t.Fatal("symlink artifact accepted")
	}
}
func TestRenameAndSymlinkScope(t *testing.T) {
	dir, base := sourceFixture(t)
	os.Rename(filepath.Join(dir, "code.txt"), filepath.Join(dir, "new.txt"))
	got, err := Capture(context.Background(), dir, base, []string{"new.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Violations) != 1 || got.Violations[0] != "code.txt" {
		t.Fatalf("rename dropped old path: %+v", got)
	}
	os.Symlink("check.sh", filepath.Join(dir, "link"))
	got, err = Capture(context.Background(), dir, base, []string{"code.txt", "new.txt", "link"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Violations) != 1 || got.Violations[0] != "link" {
		t.Fatalf("symlink allowed as source: %+v", got)
	}
}

func TestCaptureDoesNotRunCleanFiltersOrTrustIndexFlags(t *testing.T) {
	dir, base := sourceFixture(t)
	// A candidate can change attributes. Auditing must not execute their filters.
	marker := filepath.Join(dir, "filter-called")
	gitTest(t, dir, "config", "filter.trap.clean", "touch "+marker+"; cat")
	os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("code.txt filter=trap\n"), 0600)
	os.WriteFile(filepath.Join(dir, "code.txt"), []byte("changed"), 0600)
	gitTest(t, dir, "update-index", "--assume-unchanged", "code.txt")
	got, err := Capture(context.Background(), dir, base, []string{"code.txt", ".gitattributes"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("capture executed a clean filter")
	}
	if gitTest(t, dir, "show", got.Tree+":code.txt") != "changed" {
		t.Fatal("index flag hid source modification")
	}
}

func TestCaptureIncludesStagedNewFiles(t *testing.T) {
	dir, base := sourceFixture(t)
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new"), 0600)
	gitTest(t, dir, "add", "new.txt")
	got, err := Capture(context.Background(), dir, base, []string{"new.txt"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != 1 || gitTest(t, dir, "show", got.Tree+":new.txt") != "new" {
		t.Fatalf("staged new path lost: %+v", got)
	}
}

func TestArtifactInventoryIsBoundedAndRejectsEscapes(t *testing.T) {
	for _, scenario := range []string{"bounded", "escape", "nested-repository", "symlink-loop"} {
		t.Run(scenario, func(t *testing.T) {
			dir, base := sourceFixture(t)
			root := filepath.Join(dir, "output")
			os.Mkdir(root, 0700)
			a := readiness.ArtifactRoot{Path: "output", CommandID: "build", MaxFiles: 2, MaxBytes: 8, MaxDepth: 3}
			switch scenario {
			case "bounded":
				os.WriteFile(filepath.Join(root, "one"), []byte("123456789"), 0600)
			case "escape":
				os.Symlink(t.TempDir(), filepath.Join(root, "outside"))
			case "nested-repository":
				os.Mkdir(filepath.Join(root, ".git"), 0700)
			case "symlink-loop":
				os.Symlink("loop", filepath.Join(root, "loop"))
			}
			if _, err := Capture(context.Background(), dir, base, nil, []readiness.ArtifactRoot{a}); err == nil {
				t.Fatal("unsafe artifact inventory accepted")
			}
		})
	}
	dir, base := sourceFixture(t)
	os.Mkdir(filepath.Join(dir, "output"), 0700)
	os.WriteFile(filepath.Join(dir, "output", "file"), []byte("ok"), 0600)
	got, err := Capture(context.Background(), dir, base, nil, []readiness.ArtifactRoot{{Path: "output", CommandID: "build"}})
	if err != nil || len(got.Artifacts) != 1 || got.Artifacts[0].Files != 1 || got.Artifacts[0].Bytes != 2 || len(got.Artifacts[0].SHA256) != 64 {
		t.Fatalf("missing bounded inventory: %+v %v", got, err)
	}
}
