package resources

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeOwner(t *testing.T, dir string, pid int) {
	t.Helper()
	raw, _ := json.Marshal(owner{PID: pid, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err := os.WriteFile(filepath.Join(dir, OwnerFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// isolateSystemTemp points the Lab temp root at a fresh directory, spelled through a
// symbolic link, so Lab check TMPDIRs a test creates stay inside it, and HOME at a sibling
// directory, so the root never lies inside it. It returns the resolved directory.
func isolateSystemTemp(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "system-tmp")
	if err := os.Symlink(resolved, link); err != nil {
		t.Fatal(err)
	}
	setLabTempRoot(t, link)
	t.Setenv("HOME", t.TempDir())
	return resolved
}

// setLabTempRoot makes root the Lab temp root until the test ends.
func setLabTempRoot(t *testing.T, root string) {
	t.Helper()
	orig := labTempRoot
	labTempRoot = func() (string, string) { return root, "" }
	t.Cleanup(func() { labTempRoot = orig })
}

// TestDefaultLabTempRootIsFixedPerUser: the per-user default root does not follow the
// caller's $TMPDIR (on Linux it is /tmp, on macOS the per-user temp dir getconf reports).
func TestDefaultLabTempRootIsFixedPerUser(t *testing.T) {
	before, note := systemDefaultLabTempRoot()
	if note != "" {
		t.Skipf("no fixed per-user temp root here: %s", note)
	}
	t.Setenv("TMPDIR", t.TempDir())
	after, _ := systemDefaultLabTempRoot()
	if after != before || !filepath.IsAbs(after) {
		t.Fatalf("root %s became %s after TMPDIR changed", before, after)
	}
	switch runtime.GOOS {
	case "linux":
		if after != "/tmp" {
			t.Fatalf("linux root %s, want /tmp", after)
		}
	case "darwin":
		if !strings.HasSuffix(filepath.Clean(after), string(filepath.Separator)+"T") {
			t.Fatalf("darwin root %s is not the per-user temp dir", after)
		}
	}
}

// setDefaultLabTempRoot makes root the per-user default root until the test ends.
func setDefaultLabTempRoot(t *testing.T, root string) {
	t.Helper()
	orig := defaultLabTempRoot
	defaultLabTempRoot = func() (string, string) { return root, "" }
	t.Cleanup(func() { defaultLabTempRoot = orig })
}

// TestLabTempRootFollowsTheEnvironment: the root is $FORGECELL_LAB_TMP_ROOT, then $TMPDIR,
// each only when set, usable and outside the home, then the per-user default. A nested run
// inside a check view, whose TMPDIR is the view's, so creates its Lab temp dir under that
// TMPDIR and never in the user's real temp dir; a passed-over root is noted.
func TestLabTempRootFollowsTheEnvironment(t *testing.T) {
	resolve := func(dir string) string {
		t.Helper()
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	home, def, viewTmp, override := resolve(t.TempDir()), resolve(t.TempDir()), resolve(t.TempDir()), resolve(t.TempDir())
	t.Setenv("HOME", home)
	setDefaultLabTempRoot(t, def)
	lab := t.TempDir()

	// A nested run inside a view: TMPDIR is the view's, so the Lab temp dir lands there.
	t.Setenv(LabTempRootEnv, "")
	t.Setenv("TMPDIR", viewTmp)
	dir, err := LabTempDir(lab)
	if err != nil || filepath.Dir(dir) != viewTmp {
		t.Fatalf("nested run got %s %v, want it under TMPDIR %s", dir, err, viewTmp)
	}
	if note := LabTempRootNote(); note != "" {
		t.Fatalf("unexpected note %q", note)
	}
	if roots := LabTempRoots(); !slices.Equal(roots, []string{viewTmp, def}) {
		t.Fatalf("roots %v, want the current one then the default", roots)
	}

	// $FORGECELL_LAB_TMP_ROOT comes first.
	t.Setenv(LabTempRootEnv, override)
	if path, err := LabTempPath(lab); err != nil || filepath.Dir(path) != override {
		t.Fatalf("override got %s %v, want it under %s", path, err, override)
	}

	// A root inside the home, or one that does not exist, is passed over with a note.
	t.Setenv(LabTempRootEnv, mkdir(t, filepath.Join(home, "lab-tmp")))
	t.Setenv("TMPDIR", filepath.Join(viewTmp, "missing"))
	if path, err := LabTempPath(lab); err != nil || filepath.Dir(path) != def {
		t.Fatalf("got %s %v, want the default %s", path, err, def)
	}
	if note := LabTempRootNote(); !strings.Contains(note, "$"+LabTempRootEnv) || !strings.Contains(note, "inside the home") || !strings.Contains(note, "$TMPDIR") {
		t.Fatalf("note %q", note)
	}
	if roots := LabTempRoots(); !slices.Equal(roots, []string{def}) {
		t.Fatalf("roots %v, want only the default", roots)
	}

	// A home of "/" holds nothing, so it refuses no root.
	t.Setenv("HOME", "/")
	t.Setenv(LabTempRootEnv, "")
	t.Setenv("TMPDIR", viewTmp)
	if path, err := LabTempPath(lab); err != nil || filepath.Dir(path) != viewTmp {
		t.Fatalf("HOME=/ got %s %v", path, err)
	}
}

// TestInsideHome: the home check compares whole path components, never treats "/" as a
// home, and ignores case on macOS.
func TestInsideHome(t *testing.T) {
	for _, c := range []struct {
		home, path string
		want       bool
	}{
		{"/Users/me", "/Users/me", true},
		{"/Users/me", "/Users/me/tmp", true},
		{"/Users/me/", "/Users/me/tmp", true},
		{"/Users/me", "/Users/meander/tmp", false},
		{"/Users/me", "/var/folders/x/T", false},
		{"/", "/tmp", false},
		{"", "/tmp", false},
		{"/Users/me", "/USERS/ME/tmp", runtime.GOOS == "darwin"},
	} {
		if got := insideHome(c.home, c.path); got != c.want {
			t.Errorf("insideHome(%q, %q) = %t, want %t", c.home, c.path, got, c.want)
		}
	}
}

// TestLabTempPathRefusesARootInsideTheHome: a temp root inside os.UserHomeDir (such as
// TMPDIR=~/tmp) is refused, so the views fall back to a private TMPDIR rather than put test
// temp dirs under the real home again.
func TestLabTempPathRefusesARootInsideTheHome(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	setLabTempRoot(t, mkdir(t, filepath.Join(home, "tmp")))
	lab := t.TempDir()
	if _, err := LabTempPath(lab); err == nil || !strings.Contains(err.Error(), "inside the home") {
		t.Fatalf("a root inside the home was accepted: %v", err)
	}
	if _, err := LabTempDir(lab); err == nil {
		t.Fatal("LabTempDir accepted a root inside the home")
	}
	setLabTempRoot(t, home)
	if _, err := LabTempPath(lab); err == nil {
		t.Fatal("the home itself was accepted as the root")
	}
	t.Setenv("HOME", filepath.Join(home, "elsewhere"))
	if _, err := LabTempPath(lab); err != nil {
		t.Fatalf("a root outside the home was refused: %v", err)
	}
}

// TestLabTempDirRestoresALostMarker: when the marker or lab file of the user's 0700 check
// TMPDIR goes missing, the next LabTempDir writes it again, so pruning resumes.
func TestLabTempDirRestoresALostMarker(t *testing.T) {
	isolateSystemTemp(t)
	lab := t.TempDir()
	tmp, err := LabTempDir(lab)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{LabTempMarker, LabTempLabFile} {
		if err := os.Remove(filepath.Join(tmp, name)); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	leak := mkdir(t, filepath.Join(tmp, "TestLeak"))
	if err := os.Chtimes(leak, old, old); err != nil {
		t.Fatal(err)
	}
	if removed, err := PruneLabTemp(tmp, now); err != nil || len(removed) != 0 {
		t.Fatalf("an unmarked dir was pruned: %v %v", removed, err)
	}
	if again, err := LabTempDir(lab); err != nil || again != tmp {
		t.Fatalf("reuse: %s %v", again, err)
	}
	if info, err := os.Lstat(filepath.Join(tmp, LabTempMarker)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("marker not restored: %v", err)
	}
	resolved, _ := filepath.EvalSymlinks(lab)
	if raw, err := os.ReadFile(filepath.Join(tmp, LabTempLabFile)); err != nil || string(raw) != resolved+"\n" {
		t.Fatalf("lab file not restored: %q %v", raw, err)
	}
	if removed, err := PruneLabTemp(tmp, now); err != nil || !slices.Equal(removed, []string{leak}) {
		t.Fatalf("pruning did not resume: %v %v", removed, err)
	}
}

// TestLabTempDirIsStablePrivateAndUnregistered covers the stable per-Lab check TMPDIR:
// <resolved system temp>/forgecell-lab-<16 hex of sha256(resolved lab)> at 0700, outside
// the Lab, the same path on every call and for every spelling of the Lab, another path for
// another Lab, the marker and lab file written, and never registered for exit removal.
func TestLabTempDirIsStablePrivateAndUnregistered(t *testing.T) {
	sys := isolateSystemTemp(t)
	lab := mkdir(t, filepath.Join(t.TempDir(), "home", ".forgecell", "labs", "one"))
	resolvedLab, err := filepath.EvalSymlinks(lab)
	if err != nil {
		t.Fatal(err)
	}
	path, err := LabTempPath(lab)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("LabTempPath created %s: %v", path, err)
	}
	first, err := LabTempDir(lab)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(resolvedLab))
	want := filepath.Join(sys, "forgecell-lab-"+hex.EncodeToString(sum[:])[:16])
	if first != want || path != want || !filepath.IsAbs(first) {
		t.Fatalf("got %s (path %s), want %s", first, path, want)
	}
	if strings.HasPrefix(first, resolvedLab+string(filepath.Separator)) || strings.HasPrefix(first, lab+string(filepath.Separator)) {
		t.Fatalf("%s is under the Lab", first)
	}
	if err := os.Chmod(first, 0755); err != nil {
		t.Fatal(err)
	}
	second, err := LabTempDir(lab)
	if err != nil || second != first {
		t.Fatalf("not stable: %s %v", second, err)
	}
	info, err := os.Stat(second)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("mode: %v %v", info, err)
	}
	if marker, err := os.Lstat(filepath.Join(first, LabTempMarker)); err != nil || !marker.Mode().IsRegular() {
		t.Fatalf("no marker: %v", err)
	}
	if raw, err := os.ReadFile(filepath.Join(first, LabTempLabFile)); err != nil || string(raw) != resolvedLab+"\n" {
		t.Fatalf("lab file %q %v, want %s", raw, err, resolvedLab)
	}
	for _, dir := range Registered() {
		if dir == first {
			t.Fatal("the shared Lab TMPDIR must not be registered for removal")
		}
	}
	linkedLab := filepath.Join(t.TempDir(), "lab-link")
	if err := os.Symlink(lab, linkedLab); err != nil {
		t.Fatal(err)
	}
	if again, err := LabTempDir(linkedLab); err != nil || again != first {
		t.Fatalf("a symlinked spelling of the Lab got %s %v, want %s", again, err, first)
	}
	other, err := LabTempDir(mkdir(t, filepath.Join(t.TempDir(), "two")))
	if err != nil || other == first || filepath.Dir(other) != sys {
		t.Fatalf("another Lab got %s %v (first %s)", other, err, first)
	}
}

// TestLabTempDirRefusesWhatIsNotItsOwn: in the shared system temp dir a symbolic link, a
// non-directory or another user's directory at the Lab's path is refused and left as it
// is, and a system temp dir that does not exist is an error, so verification falls back to
// a private TMPDIR.
func TestLabTempDirRefusesWhatIsNotItsOwn(t *testing.T) {
	isolateSystemTemp(t)
	lab := t.TempDir()
	path, err := LabTempPath(lab)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LabTempDir(lab); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("a symbolic link was accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, LabTempMarker)); !os.IsNotExist(err) {
		t.Fatalf("the symlink target was written: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LabTempDir(lab); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("a file was accepted: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mkdir(t, path)
	orig := ownedByUser
	ownedByUser = func(p string, info os.FileInfo) bool { return p != path && orig(p, info) }
	t.Cleanup(func() { ownedByUser = orig })
	if _, err := LabTempDir(lab); err == nil || !strings.Contains(err.Error(), "another user") {
		t.Fatalf("another user's directory was accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(path, LabTempMarker)); !os.IsNotExist(err) {
		t.Fatalf("another user's directory was written: %v", err)
	}
	ownedByUser = orig

	setLabTempRoot(t, filepath.Join(t.TempDir(), "missing"))
	if _, err := LabTempDir(lab); err == nil {
		t.Fatal("a missing system temp dir was accepted")
	}
}

// TestLabTempLegacyIsReportedOnlyWhenMarked: <lab>/tmp counts as an earlier Forgecell's
// check TMPDIR only when it is the user's real directory holding the marker; a symbolic
// link there is refused.
func TestLabTempLegacyIsReportedOnlyWhenMarked(t *testing.T) {
	lab := t.TempDir()
	dir, legacy, err := LegacyLabTemp(lab)
	if err != nil || legacy || dir != filepath.Join(lab, "tmp") {
		t.Fatalf("missing: %s %t %v", dir, legacy, err)
	}
	mkdir(t, dir)
	if _, legacy, err := LegacyLabTemp(lab); err != nil || legacy {
		t.Fatalf("unmarked: %t %v", legacy, err)
	}
	if err := os.WriteFile(filepath.Join(dir, LabTempMarker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, legacy, err := LegacyLabTemp(lab); err != nil || !legacy {
		t.Fatalf("marked: %t %v", legacy, err)
	}
	orig := ownedByUser
	ownedByUser = func(p string, info os.FileInfo) bool { return p != dir && orig(p, info) }
	if _, legacy, err := LegacyLabTemp(lab); err != nil || legacy {
		t.Fatalf("another user's: %t %v", legacy, err)
	}
	ownedByUser = orig
	linked := t.TempDir()
	if err := os.Symlink(dir, filepath.Join(linked, "tmp")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LegacyLabTemp(linked); err == nil {
		t.Fatal("a symbolic link was accepted")
	}
}

// TestPruneLabTempRemovesOnlyOldEntriesOfTheUser: entries under a Lab's check TMPDIR older
// than 24h that belong to the user go (read-only trees included); recent ones, other users'
// and the directory itself stay. A missing directory has nothing stale.
func TestPruneLabTempRemovesOnlyOldEntriesOfTheUser(t *testing.T) {
	isolateSystemTemp(t)
	tmp, err := LabTempDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-25 * time.Hour)
	oldDir := mkdir(t, filepath.Join(tmp, "TestLeak123", "001"))
	if err := os.Chmod(oldDir, 0500); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(tmp, "leaked.txt")
	if err := os.WriteFile(oldFile, nil, 0400); err != nil {
		t.Fatal(err)
	}
	recent := mkdir(t, filepath.Join(tmp, "TestRunning456"))
	foreign := mkdir(t, filepath.Join(tmp, "someone-else"))
	for _, p := range []string{filepath.Join(tmp, "TestLeak123"), oldFile, foreign} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	orig := ownedByUser
	ownedByUser = func(path string, info os.FileInfo) bool { return path != foreign && orig(path, info) }
	t.Cleanup(func() { ownedByUser = orig })
	stale, err := StaleLabTempEntries(tmp, now)
	want := []string{filepath.Join(tmp, "TestLeak123"), oldFile}
	if err != nil || !slices.Equal(stale, want) {
		t.Fatalf("stale %v %v, want %v", stale, err, want)
	}
	removed, err := PruneLabTemp(tmp, now)
	if err != nil || !slices.Equal(removed, want) {
		t.Fatalf("removed %v %v", removed, err)
	}
	for _, p := range want {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the prune", p)
		}
	}
	for _, p := range []string{tmp, recent, foreign} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s must be kept: %v", p, err)
		}
	}
	if stale, err := StaleLabTempEntries(filepath.Join(t.TempDir(), "missing"), now); err != nil || len(stale) != 0 {
		t.Fatalf("missing dir: %v %v", stale, err)
	}
}

// TestPruneLabTempTreatsGoTempAsContainer: the shared gotmp in a Lab's check TMPDIR is
// never removed, even when old, while its old children (go-build* work dirs, leaked test
// temp dirs) go and fresh ones stay, even when gotmp's own mtime is fresh. A gotmp of
// another user is left alone with its children.
func TestPruneLabTempTreatsGoTempAsContainer(t *testing.T) {
	isolateSystemTemp(t)
	tmp, err := LabTempDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-25 * time.Hour)
	gotmp := mkdir(t, filepath.Join(tmp, LabGoTempName))
	oldBuild := mkdir(t, filepath.Join(gotmp, "go-build123", "b001"))
	if err := os.Chmod(oldBuild, 0500); err != nil {
		t.Fatal(err)
	}
	oldTest := mkdir(t, filepath.Join(gotmp, "TestKilled456"))
	fresh := mkdir(t, filepath.Join(gotmp, "go-build789"))
	for _, p := range []string{filepath.Join(gotmp, "go-build123"), oldTest} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{oldTest, filepath.Join(gotmp, "go-build123")}
	removed, err := PruneLabTemp(tmp, now)
	if err != nil || !slices.Equal(removed, want) {
		t.Fatalf("removed %v %v, want %v", removed, err, want)
	}
	for _, p := range want {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the prune", p)
		}
	}
	for _, p := range []string{tmp, gotmp, fresh} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s must be kept: %v", p, err)
		}
	}

	// An old gotmp itself is still kept, and only its old children are listed.
	if err := os.Chtimes(gotmp, old, old); err != nil {
		t.Fatal(err)
	}
	if stale, err := StaleLabTempEntries(tmp, now); err != nil || len(stale) != 0 {
		t.Fatalf("old gotmp listed: %v %v", stale, err)
	}

	// A gotmp that belongs to another user is not looked into.
	leaked := mkdir(t, filepath.Join(gotmp, "TestOther"))
	if err := os.Chtimes(leaked, old, old); err != nil {
		t.Fatal(err)
	}
	orig := ownedByUser
	ownedByUser = func(path string, info os.FileInfo) bool { return path != gotmp && orig(path, info) }
	t.Cleanup(func() { ownedByUser = orig })
	if stale, err := StaleLabTempEntries(tmp, now); err != nil || len(stale) != 0 {
		t.Fatalf("foreign gotmp children listed: %v %v", stale, err)
	}
	ownedByUser = orig

	// A symbolic link named gotmp is never followed: its target's entries stay.
	if err := RemoveTree(gotmp); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	victim := mkdir(t, filepath.Join(target, "victim"))
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, gotmp); err != nil {
		t.Fatal(err)
	}
	if stale, err := StaleLabTempEntries(tmp, now); err != nil || len(stale) != 0 {
		t.Fatalf("symlinked gotmp followed: %v %v", stale, err)
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("symlink target entry removed: %v", err)
	}
}

// TestPruneLabTempTouchesOnlyMarkedDirectories: LabTempDir writes its marker when it
// creates the check TMPDIR or reuses one of the user's; a
// directory without the marker is never pruned, an old marker
// (or lab file) itself is never listed, and a symbolic link in place of it is refused.
func TestPruneLabTempTouchesOnlyMarkedDirectories(t *testing.T) {
	isolateSystemTemp(t)
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	tmp, err := LabTempDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tmp, LabTempMarker)
	if info, err := os.Lstat(marker); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("no marker written on creation: %v", err)
	}
	labFile := filepath.Join(tmp, LabTempLabFile)
	for _, p := range []string{marker, labFile} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if removed, err := PruneLabTemp(tmp, now); err != nil || len(removed) != 0 {
		t.Fatalf("the marker or lab file was pruned: %v %v", removed, err)
	}
	for _, p := range []string{marker, labFile} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s removed: %v", p, err)
		}
	}

	// A directory of the user already at the forgecell-lab-* path that is not 0700 is made
	// 0700 and marked, since the name is Forgecell's, so its old entries are pruned.
	lab := t.TempDir()
	path, err := LabTempPath(lab)
	if err != nil {
		t.Fatal(err)
	}
	own := mkdir(t, path)
	leak := mkdir(t, filepath.Join(own, "TestLeak"))
	if err := os.Chtimes(leak, old, old); err != nil {
		t.Fatal(err)
	}
	if removed, err := PruneLabTemp(own, now); err != nil || len(removed) != 0 {
		t.Fatalf("unmarked tmp pruned: %v %v", removed, err)
	}
	if err := os.Chmod(own, 0755); err != nil {
		t.Fatal(err)
	}
	if got, err := LabTempDir(lab); err != nil || got != own {
		t.Fatalf("reuse: %s %v", got, err)
	}
	if info, err := os.Lstat(own); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("reused directory mode: %v %v", info, err)
	}
	for _, name := range []string{LabTempMarker, LabTempLabFile} {
		if info, err := os.Lstat(filepath.Join(own, name)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("reused directory lacks %s: %v", name, err)
		}
	}
	if removed, err := PruneLabTemp(own, now); err != nil || !slices.Equal(removed, []string{leak}) {
		t.Fatalf("reused marked tmp not pruned: %v %v", removed, err)
	}

	// A symbolic link to a marked directory is refused, and its target is untouched.
	target, err := LabTempDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	victim := mkdir(t, filepath.Join(target, "victim"))
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "tmp")
	if err := os.Symlink(target, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := StaleLabTempEntries(linked, now); err == nil {
		t.Fatal("a symbolic link was listed")
	}
	if _, err := PruneLabTemp(linked, now); err == nil {
		t.Fatal("a symbolic link was pruned")
	}
	if _, err := os.Lstat(victim); err != nil {
		t.Fatalf("symlink target entry removed: %v", err)
	}
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// deadPID returns the pid of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

// readOnlyModuleTree builds what Go's module cache leaves under a private HOME: files
// and directories without write permission.
func readOnlyModuleTree(t *testing.T, root string) {
	t.Helper()
	mod := mkdir(t, filepath.Join(root, "go", "pkg", "mod", "example.com", "m@v1.0.0"))
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module example.com/m\n"), 0444); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{mod, filepath.Dir(mod)} {
		if err := os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveTreeRemovesReadOnlyModuleCache(t *testing.T) {
	home := mkdir(t, filepath.Join(t.TempDir(), "forgecell-check-home-1"))
	readOnlyModuleTree(t, home)
	if os.Getuid() != 0 {
		if err := os.RemoveAll(home); err == nil {
			t.Fatal("fixture should defeat a plain os.RemoveAll")
		}
	}
	if err := RemoveTree(home); err != nil {
		t.Fatalf("RemoveTree: %v", err)
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Fatalf("tree still exists: %v", err)
	}
	if err := RemoveTree(home); err != nil {
		t.Fatalf("a missing tree is not an error: %v", err)
	}
}

func TestRemoveTreeNeverFollowsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := mkdir(t, filepath.Join(root, "outside"))
	if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0444); err != nil {
		t.Fatal(err)
	}
	tree := mkdir(t, filepath.Join(root, "forgecell-tree"))
	if err := os.Symlink(outside, filepath.Join(tree, "link")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveTree(tree); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(outside, "keep"))
	if err != nil || info.Mode().Perm() != 0444 {
		t.Fatalf("symlink target was touched: %v %v", info, err)
	}
}

func TestMkdirTempWritesOwnerAndRegisters(t *testing.T) {
	dir, err := MkdirTemp(t.TempDir(), "forgecell-test-")
	if err != nil {
		t.Fatal(err)
	}
	o, ok := readOwner(dir)
	if !ok || o.PID != os.Getpid() || o.StartedAt == "" {
		t.Fatalf("owner file = %+v, %v", o, ok)
	}
	registered := false
	for _, d := range Registered() {
		registered = registered || d == dir
	}
	if !registered {
		t.Fatal("temp directory was not registered")
	}
	readOnlyModuleTree(t, dir)
	if err := CleanupAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("CleanupAll left the directory")
	}
	if len(Registered()) != 0 {
		t.Fatalf("registry not emptied: %v", Registered())
	}
}

func TestSweepRemovesOnlyDeadOwnerForgecellDirsOfCurrentUser(t *testing.T) {
	tmp := t.TempDir()
	now := time.Now()
	dead := mkdir(t, filepath.Join(tmp, "forgecell-check-home-dead"))
	writeOwner(t, dead, deadPID(t))
	readOnlyModuleTree(t, dead)

	live := exec.Command("sleep", "60")
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Process.Kill(); _ = live.Wait() }()
	alive := mkdir(t, filepath.Join(tmp, "forgecell-check-home-live"))
	writeOwner(t, alive, live.Process.Pid)

	oldNoOwner := mkdir(t, filepath.Join(tmp, "forgecell-capture-old"))
	newNoOwner := mkdir(t, filepath.Join(tmp, "forgecell-capture-new"))
	if err := os.Chtimes(oldNoOwner, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// An old Lab check TMPDIR of a live Lab has no owner file but is shared and kept; one
	// whose Lab is gone, or that has no lab file, is an orphan and goes.
	labTmp := orphanFixture(t, tmp, "0123456789abcdef", t.TempDir(), now.Add(-48*time.Hour))
	goneLab := orphanFixture(t, tmp, "1123456789abcdef", filepath.Join(t.TempDir(), "gone"), now.Add(-48*time.Hour))
	noLabFile := orphanFixture(t, tmp, "2123456789abcdef", "", now.Add(-48*time.Hour))
	foreign := mkdir(t, filepath.Join(tmp, "forgecell-check-home-foreign"))
	writeOwner(t, foreign, deadPID(t))
	other := mkdir(t, filepath.Join(tmp, "other-tool-dir"))
	writeOwner(t, other, deadPID(t))
	if err := os.Chtimes(other, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(tmp, "forgecell-file")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	target := mkdir(t, filepath.Join(t.TempDir(), "target"))
	writeOwner(t, target, deadPID(t))
	link := filepath.Join(tmp, "forgecell-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	mine, err := MkdirTemp(tmp, "forgecell-mine-")
	if err != nil {
		t.Fatal(err)
	}
	defer Remove(mine)

	saved := ownedByUser
	ownedByUser = func(path string, info os.FileInfo) bool {
		return filepath.Base(path) != "forgecell-check-home-foreign" && saved(path, info)
	}
	defer func() { ownedByUser = saved }()

	removed, err := SweepStale(tmp, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{dead: true, oldNoOwner: true, goneLab: true, noLabFile: true}
	if len(removed) != len(want) {
		t.Fatalf("removed %v, want exactly %v", removed, want)
	}
	for _, path := range removed {
		resolved, _ := filepath.Abs(path)
		if !want[resolved] {
			t.Fatalf("removed %s, which is not a dead-owner Forgecell directory", path)
		}
	}
	for path := range want {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep", path)
		}
	}
	for _, path := range []string{alive, newNoOwner, labTmp, foreign, other, file, link, target, mine} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("%s must be kept: %v", path, err)
		}
	}
}

// orphanFixture creates root/forgecell-lab-<hash> holding the marker and, unless lab is
// empty, a lab file naming lab, last modified at at.
func orphanFixture(t *testing.T, root, hash, lab string, at time.Time) string {
	t.Helper()
	dir := mkdir(t, filepath.Join(root, LabTempPrefix+hash))
	if err := os.WriteFile(filepath.Join(dir, LabTempMarker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if lab != "" {
		if err := os.WriteFile(filepath.Join(dir, LabTempLabFile), []byte(lab+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSweepOrphanLabTempsRemovesOnlyOrphans: a marked check TMPDIR older than 24h whose Lab
// no longer exists, or that has no lab file, is swept; a live Lab's one (even old), a recent
// orphan, an unmarked one, another user's and a symbolic link are never swept.
func TestSweepOrphanLabTempsRemovesOnlyOrphans(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	gone := orphanFixture(t, root, "0000000000000001", filepath.Join(t.TempDir(), "removed-lab"), old)
	noLab := orphanFixture(t, root, "0000000000000002", "", old)
	liveLab := t.TempDir()
	live := orphanFixture(t, root, "0000000000000003", liveLab, old)
	recent := orphanFixture(t, root, "0000000000000004", filepath.Join(t.TempDir(), "removed-too"), now)
	unmarked := mkdir(t, filepath.Join(root, LabTempPrefix+"0000000000000005"))
	if err := os.Chtimes(unmarked, old, old); err != nil {
		t.Fatal(err)
	}
	foreign := orphanFixture(t, root, "0000000000000006", "", old)
	linkTarget := orphanFixture(t, t.TempDir(), "0000000000000007", "", old)
	link := filepath.Join(root, LabTempPrefix+"0000000000000008")
	if err := os.Symlink(linkTarget, link); err != nil {
		t.Fatal(err)
	}
	orig := ownedByUser
	ownedByUser = func(p string, info os.FileInfo) bool { return p != foreign && orig(p, info) }
	t.Cleanup(func() { ownedByUser = orig })

	removed, err := SweepOrphanLabTemps(root, now)
	if err != nil || !slices.Equal(removed, []string{gone, noLab}) {
		t.Fatalf("removed %v %v, want %v", removed, err, []string{gone, noLab})
	}
	for _, p := range []string{gone, noLab} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived the sweep", p)
		}
	}
	for _, p := range []string{live, recent, unmarked, foreign, link, linkTarget} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s must be kept: %v", p, err)
		}
	}
	if _, ok := OrphanLabTemp(live, now); ok {
		t.Fatal("a live Lab's check TMPDIR counts as an orphan")
	}
}

// TestSignalCleanupHelper is the child process of the SIGINT tests; it does nothing
// unless the parent test starts it.
func TestSignalCleanupHelper(t *testing.T) {
	mode := os.Getenv("FORGECELL_SIGNAL_HELPER")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	os.Exit(RunWithCleanup(func(ctx context.Context) int {
		dir, err := MkdirTemp(os.Getenv("FORGECELL_SIGNAL_TMP"), "forgecell-check-home-")
		if err != nil {
			fmt.Println("error", err)
			return 1
		}
		readOnlyModuleTree(t, dir)
		fmt.Println(dir)
		if mode == "stuck" {
			// A run that ignores cancellation: only the second signal ends it.
			select {}
		}
		<-ctx.Done()
		return 130
	}, os.Stderr))
}

func runSignalHelper(t *testing.T, mode string, signals int) {
	tmp := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSignalCleanupHelper$")
	cmd.Env = append(os.Environ(), "FORGECELL_SIGNAL_HELPER="+mode, "FORGECELL_SIGNAL_TMP="+tmp)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper output: %v", err)
	}
	dir := strings.TrimSpace(line)
	if _, err := os.Stat(filepath.Join(dir, OwnerFile)); err != nil {
		t.Fatalf("helper temp dir %q has no owner file: %v", dir, err)
	}
	for i := 0; i < signals; i++ {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not exit after SIGINT")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("SIGINT left the registered temp directory %s behind", dir)
	}
	entries, _ := os.ReadDir(tmp)
	if len(entries) != 0 {
		t.Fatalf("temp parent still holds %d entries", len(entries))
	}
}

func TestSIGINTRemovesRegisteredTempDirs(t *testing.T) {
	runSignalHelper(t, "cancel", 1)
}

func TestSecondSIGINTRemovesRegisteredTempDirsOfAStuckRun(t *testing.T) {
	runSignalHelper(t, "stuck", 2)
}
