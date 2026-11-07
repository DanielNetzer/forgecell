package resources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHarnessGoCacheCreatesPrivateDirsAndReusesThem(t *testing.T) {
	lab, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c, err := HarnessGoCache(lab)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(lab, "caches", "harness")
	if c != (HarnessCache{Root: root, BuildCache: filepath.Join(root, "go-build"), TmpDir: filepath.Join(root, "gotmp")}) {
		t.Fatalf("%+v", c)
	}
	for _, dir := range []string{filepath.Dir(root), root, c.BuildCache, c.TmpDir} {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatalf("%s: %v %v", dir, info, err)
		}
	}
	kept := filepath.Join(c.BuildCache, "kept")
	if err = os.WriteFile(kept, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(c.TmpDir, 0755); err != nil {
		t.Fatal(err)
	}
	again, err := HarnessGoCache(lab)
	if err != nil || again != c {
		t.Fatalf("reuse: %+v %v", again, err)
	}
	if _, err = os.Stat(kept); err != nil {
		t.Fatalf("reuse emptied the cache: %v", err)
	}
	if info, err := os.Lstat(c.TmpDir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("permissions were not restored: %v %v", info, err)
	}
}

func TestHarnessGoCacheRefusesUnsafeDirs(t *testing.T) {
	for _, name := range []string{"caches", "caches/harness", "caches/harness/go-build", "caches/harness/gotmp"} {
		t.Run(name, func(t *testing.T) {
			for _, plant := range []string{"symlink", "file"} {
				lab, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(lab, filepath.FromSlash(name))
				if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				want := "not a directory"
				if plant == "symlink" {
					want = "symbolic link"
					err = os.Symlink(t.TempDir(), path)
				} else {
					err = os.WriteFile(path, []byte("x"), 0600)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err = HarnessGoCache(lab); err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), path) {
					t.Fatalf("%s %s accepted or unnamed: %v", plant, name, err)
				}
			}
		})
	}
}

func TestHarnessGoCacheRefusesAnotherUsersDir(t *testing.T) {
	lab, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(lab, "caches", "harness")
	if err = os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	orig := ownedByUser
	ownedByUser = func(p string, info os.FileInfo) bool { return p != path && orig(p, info) }
	t.Cleanup(func() { ownedByUser = orig })
	if _, err = HarnessGoCache(lab); err == nil || !strings.Contains(err.Error(), "another user") || !strings.Contains(err.Error(), path) {
		t.Fatalf("another user's directory was accepted: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(path, "go-build")); !os.IsNotExist(err) {
		t.Fatalf("another user's directory was written: %v", err)
	}
}
