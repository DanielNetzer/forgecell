package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// cleanFixture is a Forgecell home with eight staged releases (the oldest active) and a
// Lab holding one finished Molecule older than 14 days, one recent finished Molecule and
// one old Molecule still waiting for review.
type cleanFixture struct {
	home, lab string
	releases  []string
	oldTrees  []string
	keptTrees []string
	ledgers   []string
}

func newCleanFixture(t *testing.T) cleanFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// doctor --clean never looks for orphaned check TMPDIRs in the user's real temp dirs.
	setLabTempRoots(t)
	f := cleanFixture{home: filepath.Join(root, "home"), lab: filepath.Join(root, "lab")}
	base := time.Now().Add(-100 * time.Hour)
	for i := 0; i < 8; i++ {
		dir := filepath.Join(f.home, "releases", fmt.Sprintf("1.%d.0", i))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "forgecell"), []byte("bin"), 0555); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
		f.releases = append(f.releases, dir)
	}
	if err := os.Symlink(f.releases[0], filepath.Join(f.home, "current")); err != nil {
		t.Fatal(err)
	}
	molecules := []struct {
		id, status, phase string
		age               time.Duration
		old, proposal     bool
	}{
		{"mol-old", "failed", "", 20 * 24 * time.Hour, true, false},
		{"mol-recent", "blocked", "", 2 * 24 * time.Hour, false, false},
		{"mol-waiting", "waiting", "", 30 * 24 * time.Hour, false, false},
		// Interrupted Molecules stay recoverable, whatever their age.
		{"mol-interrupted", "interrupted", "", 30 * 24 * time.Hour, false, false},
		{"mol-paused", "failed", "interrupted", 30 * 24 * time.Hour, false, false},
		// A pending recovery proposal still needs the trees.
		{"mol-proposal", "completed", "", 30 * 24 * time.Hour, false, true},
	}
	for _, m := range molecules {
		workspace := filepath.Join(f.lab, "workspaces", m.id+"-ws")
		verification := filepath.Join(f.lab, "verification", m.id+"-r1")
		for _, dir := range []string{workspace, verification} {
			if err := os.MkdirAll(filepath.Join(dir, "home", "go", "pkg", "mod", "m@v1"), 0700); err != nil {
				t.Fatal(err)
			}
			// A read-only module cache, as Go leaves it, must not defeat removal.
			if err := os.Chmod(filepath.Join(dir, "home", "go", "pkg", "mod", "m@v1"), 0555); err != nil {
				t.Fatal(err)
			}
		}
		raw, _ := json.Marshal(map[string]any{"id": m.id, "status": m.status, "finishedAt": time.Now().Add(-m.age).UTC().Format(time.RFC3339Nano), "workspace": map[string]string{"path": workspace}, "readiness": map[string]string{"phase": m.phase}})
		if m.proposal {
			proposal := filepath.Join(f.lab, "recovery-proposals", m.id+".json")
			if err := os.MkdirAll(filepath.Dir(proposal), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(proposal, []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		ledger := filepath.Join(f.lab, "ledgers", m.id+".json")
		if err := os.MkdirAll(filepath.Dir(ledger), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ledger, raw, 0600); err != nil {
			t.Fatal(err)
		}
		f.ledgers = append(f.ledgers, ledger)
		if m.old {
			f.oldTrees = append(f.oldTrees, workspace, verification)
		} else {
			f.keptTrees = append(f.keptTrees, workspace, verification)
		}
	}
	history := filepath.Join(f.lab, "ledgers", "mol-old", "history.json")
	if err := os.MkdirAll(filepath.Dir(history), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	f.ledgers = append(f.ledgers, history)
	return f
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestDoctorCleanDryRunListsWithoutDeleting(t *testing.T) {
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"doctor", "--clean", "--lab", f.lab}, strings.NewReader("n\n"), &out, &stderr, "dev")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	text := out.String()
	for _, want := range []string{f.releases[1], f.releases[2], f.oldTrees[0], f.oldTrees[1], "[y/N]", "Nothing removed.",
		"keep release 1.0.0 (current)", "keep release 1.7.0 (newest)",
		"warning: mol-old is failed; after removal it can no longer be amended or recovered."} {
		if !strings.Contains(text, want) {
			t.Fatalf("listing omits %q:\n%s", want, text)
		}
	}
	for _, path := range append(append(append([]string{}, f.releases...), f.oldTrees...), f.keptTrees...) {
		if !exists(path) {
			t.Fatalf("dry run removed %s", path)
		}
	}
	if strings.Contains(text, "remove release "+f.releases[0]) {
		t.Fatalf("listing offers the active release:\n%s", text)
	}
	for _, kept := range f.keptTrees {
		if strings.Contains(text, kept) {
			t.Fatalf("listing offers kept tree %s:\n%s", kept, text)
		}
	}
}

func TestDoctorCleanKeepsReleaseReferencedByLabFormula(t *testing.T) {
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	formulas := filepath.Join(f.home, "labs", "pinned-lab", "formulas")
	if err := os.MkdirAll(formulas, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulas, "f.yaml"), []byte("launcher: "+f.releases[1]+"/forgecell\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", f.lab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	if !strings.Contains(out.String(), "keep release 1.1.0 (referenced by Lab pinned-lab)") {
		t.Fatalf("listing does not say why 1.1.0 is kept:\n%s", out.String())
	}
	if !exists(f.releases[1]) || exists(f.releases[2]) {
		t.Fatalf("1.1.0 kept=%v (want true), 1.2.0 kept=%v (want false)", exists(f.releases[1]), exists(f.releases[2]))
	}
}

func TestDoctorCleanSkipsLockedMolecule(t *testing.T) {
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	unlock, locked, err := molecule.TryLockForCleanup(f.lab, "mol-old")
	if err != nil || locked {
		t.Fatalf("locked=%v err=%v", locked, err)
	}
	defer unlock()
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", f.lab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	if !strings.Contains(out.String(), "skipped mol-old: it is locked") {
		t.Fatalf("output does not report the skip:\n%s", out.String())
	}
	for _, path := range f.oldTrees {
		if !exists(path) {
			t.Fatalf("tree %s of a locked Molecule was removed", path)
		}
	}
	// Releases are still pruned.
	if exists(f.releases[1]) {
		t.Fatal("a locked Molecule must not stop release pruning")
	}
}

// answerAfter answers the confirmation prompt after running change, which stands for
// another process acting between the listing and the removal.
type answerAfter struct {
	change func()
	done   bool
}

func (a *answerAfter) Read(p []byte) (int, error) {
	if !a.done {
		a.done = true
		a.change()
	}
	return copy(p, "y\n"), io.EOF
}

func TestDoctorCleanKeepsTreesWhenTheLedgerChangesAfterTheListing(t *testing.T) {
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	ledger := filepath.Join(f.lab, "ledgers", "mol-old.json")
	in := &answerAfter{change: func() {
		// The Molecule is resumed between the listing and the removal.
		raw, _ := json.Marshal(map[string]any{"id": "mol-old", "status": "running", "workspace": map[string]string{"path": f.oldTrees[0]}})
		if err := os.WriteFile(ledger, raw, 0600); err != nil {
			t.Error(err)
		}
	}}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--clean", "--lab", f.lab}, in, &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	if !strings.Contains(out.String(), f.oldTrees[0]) {
		t.Fatalf("the listing did not offer mol-old's trees:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "skipped mol-old: its status changed from failed to running since the listing") {
		t.Fatalf("output does not say why mol-old was skipped:\n%s", out.String())
	}
	for _, path := range f.oldTrees {
		if !exists(path) {
			t.Fatalf("tree %s of a Molecule whose ledger changed was removed", path)
		}
	}
}

func TestDoctorCleanKeepsWorkspaceAnotherLedgerTakesAfterTheListing(t *testing.T) {
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	in := &answerAfter{change: func() {
		raw, _ := json.Marshal(map[string]any{"id": "mol-new", "status": "running", "workspace": map[string]string{"path": f.oldTrees[0]}})
		if err := os.WriteFile(filepath.Join(f.lab, "ledgers", "mol-new.json"), raw, 0600); err != nil {
			t.Error(err)
		}
	}}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--clean", "--lab", f.lab}, in, &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	if !strings.Contains(out.String(), "skipped mol-old: its workspace tree "+f.oldTrees[0]+" changed or is referenced elsewhere") {
		t.Fatalf("output does not say why mol-old was skipped:\n%s", out.String())
	}
	for _, path := range f.oldTrees {
		if !exists(path) {
			t.Fatalf("tree %s was removed although another ledger took the workspace", path)
		}
	}
}

func TestDoctorCleanKeepsReleaseReferencedByTheGivenLabOutsideHome(t *testing.T) {
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	// f.lab lies outside <home>/labs; a Formula there and a pending suggestion each pin
	// a release.
	if err := os.MkdirAll(filepath.Join(f.lab, "formulas"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.lab, "formulas", "f.yaml"), []byte("launcher: "+f.releases[1]+"/forgecell\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.lab, "suggestions"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]string{"id": "suggestion-x", "status": "pending", "proposedYaml": "launcher: " + f.releases[2] + "/forgecell\n"})
	if err := os.WriteFile(filepath.Join(f.lab, "suggestions", "suggestion-x.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", f.lab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	for _, want := range []string{"keep release 1.1.0 (referenced by Lab " + f.lab + ")", "keep release 1.2.0 (referenced by pending suggestion suggestion-x in Lab " + f.lab + ")"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("listing omits %q:\n%s", want, out.String())
		}
	}
	if !exists(f.releases[1]) || !exists(f.releases[2]) {
		t.Fatal("a release the given Lab references was removed")
	}
}

func TestStaleLabTreesMatchesOnlyTheMoleculesVerificationTrees(t *testing.T) {
	f := newCleanFixture(t)
	names := []string{"mol-old-r2-protected", "mol-old-r12", "mol-old-rx", "mol-old-r1-other", "mol-old-r1x", "mol-older-r1"}
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(f.lab, "verification", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	trees, err := staleLabTrees(f.lab, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, tree := range trees {
		got[filepath.Base(tree.Path)] = true
	}
	for _, name := range names {
		want := name == "mol-old-r2-protected" || name == "mol-old-r12"
		if got[name] != want {
			t.Fatalf("%s listed=%v, want %v (listed %v)", name, got[name], want, got)
		}
	}
}

func TestDoctorCleanYesKeepsActivePlusFiveNewest(t *testing.T) {
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", f.lab}, strings.NewReader(""), &out, &stderr, "dev")
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Fatal("--yes must not prompt")
	}
	// Active is 1.0.0 (the oldest); the 5 newest are 1.3.0 through 1.7.0.
	for i, path := range f.releases {
		if want := i == 0 || i >= 3; exists(path) != want {
			t.Fatalf("release %s kept=%v, want %v", filepath.Base(path), exists(path), want)
		}
	}
	for _, path := range f.oldTrees {
		if exists(path) {
			t.Fatalf("old finished Molecule tree %s was not removed", path)
		}
	}
	for _, path := range append(append([]string{}, f.keptTrees...), f.ledgers...) {
		if !exists(path) {
			t.Fatalf("%s must never be pruned", path)
		}
	}
}

func TestDoctorYesRequiresClean(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--yes", "--lab", t.TempDir()}, strings.NewReader(""), &out, &stderr, "dev"); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
}

func TestDoctorResourcesSectionReportsFloorsAndSizes(t *testing.T) {
	f := newCleanFixture(t)
	d := inspectResources(f.lab, f.home)
	if len(d.Volumes) == 0 || d.MemorySoft != "3.0 GiB" || d.MemoryHard != "1.5 GiB" {
		t.Fatalf("resources = %+v", d)
	}
	if d.Releases != 8 || d.ReleasesPrune != 2 || d.Lab.Bytes == 0 {
		t.Fatalf("releases %d (prunable %d), lab %d bytes", d.Releases, d.ReleasesPrune, d.Lab.Bytes)
	}
	var out bytes.Buffer
	renderResources(&out, d)
	for _, want := range []string{"Resources:", "Disk ", "Memory:", "Lab:", "Releases: 8 staged", "Stale temp directories:"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("Resources section omits %q:\n%s", want, out.String())
		}
	}
}

// TestDoctorWarnsAboutLargeGoCacheWithoutCleaningIt measures the shared cache checks use
// (resources.GoCacheDir), presented here as 25 GiB without walking it.
func TestDoctorWarnsAboutLargeGoCacheWithoutCleaningIt(t *testing.T) {
	cache := resources.GoCacheDir()
	if cache == "" {
		t.Skip("no Go build cache known")
	}
	existed := exists(cache)
	previous, measured := goCacheSizes.Load(cache)
	goCacheSizes.Store(cache, resourceSize{Path: cache, Size: "25.0 GiB", Bytes: 25 * resources.GiB, Complete: true})
	t.Cleanup(func() {
		if measured {
			goCacheSizes.Store(cache, previous)
		} else {
			goCacheSizes.Delete(cache)
		}
	})
	d := inspectResources(t.TempDir(), t.TempDir())
	found := false
	for _, w := range d.Warnings {
		found = found || strings.Contains(w, "go clean -cache")
	}
	if !found || d.GoCache == nil || d.GoCache.Path != cache || exists(cache) != existed {
		t.Fatalf("warnings %v cache %+v; want the go clean -cache hint and the cache untouched", d.Warnings, d.GoCache)
	}
}

// TestDoctorCleanListsAndRemovesStaleLabTmpEntries: doctor --clean lists entries under the
// Lab's check TMPDIR (under the system temp dir) older than 24h and a marked legacy
// <lab>/tmp, removes them only once confirmed, and keeps recent ones and the check TMPDIR
// itself.
func TestDoctorCleanListsAndRemovesStaleLabTmpEntries(t *testing.T) {
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	// The check TMPDIR follows a usable $TMPDIR outside the home.
	callerTmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(resources.LabTempRootEnv, "")
	t.Setenv("TMPDIR", callerTmp)
	tmp, err := resources.LabTempDir(f.lab)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resources.RemoveTree(tmp) })
	if filepath.Dir(tmp) != callerTmp {
		t.Fatalf("check TMPDIR %s does not follow $TMPDIR %s", tmp, callerTmp)
	}
	if path, err := resources.LabTempPath(f.lab); err != nil || path != tmp {
		t.Fatalf("LabTempPath %s %v, want %s", path, err, tmp)
	}
	legacy := filepath.Join(f.lab, "tmp")
	if err := os.MkdirAll(filepath.Join(legacy, "TestFresh"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, resources.LabTempMarker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(tmp, "TestLeak123")
	recent := filepath.Join(tmp, "TestRunning456")
	for _, dir := range []string{filepath.Join(stale, "001"), recent} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--clean", "--lab", f.lab}, strings.NewReader("n\n"), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(out.String(), "remove "+stale) || strings.Contains(out.String(), recent) || !exists(stale) {
		t.Fatalf("dry run listing:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "check TMPDIR "+tmp) || !strings.Contains(out.String(), "remove "+legacy+" (") || !exists(legacy) {
		t.Fatalf("dry run must name the check TMPDIR and list the legacy <lab>/tmp:\n%s", out.String())
	}
	out.Reset()
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", f.lab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	if exists(stale) || !exists(recent) || !exists(tmp) || !strings.Contains(out.String(), "removed "+stale) {
		t.Fatalf("stale %t recent %t tmp %t:\n%s", exists(stale), exists(recent), exists(tmp), out.String())
	}
	if exists(legacy) || !strings.Contains(out.String(), "removed "+legacy+"\n") {
		t.Fatalf("legacy <lab>/tmp not removed after confirmation:\n%s", out.String())
	}
}

// TestDoctorCleanLeavesForeignTmpAlone: doctor --clean --yes --lab touches the check
// TMPDIR and a legacy <lab>/tmp only for a directory that looks like a Lab and only when
// Forgecell's marker is there; a symbolic link in place of <lab>/tmp is refused and its
// target untouched.
func TestDoctorCleanLeavesForeignTmpAlone(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour)
	oldEntry := func(path string) string {
		t.Helper()
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	// The Lab's own tmp/, without the marker: nothing there is removed.
	own := oldEntry(filepath.Join(f.lab, "tmp", "my-work"))
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", f.lab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	if !exists(own) || strings.Contains(out.String(), own) {
		t.Fatalf("unmarked <lab>/tmp entry touched:\n%s", out.String())
	}

	// A directory that is not a Lab (no ledgers/) keeps even a marked check TMPDIR and a
	// marked legacy tmp/.
	notLab := t.TempDir()
	tmp, err := resources.LabTempDir(notLab)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resources.RemoveTree(tmp) })
	marked := oldEntry(filepath.Join(tmp, "TestLeak"))
	legacy := oldEntry(filepath.Join(notLab, "tmp"))
	if err := os.WriteFile(filepath.Join(legacy, resources.LabTempMarker), nil, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", notLab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	if !exists(marked) || !exists(legacy) {
		t.Fatalf("tmp of a non-Lab directory pruned:\n%s", out.String())
	}

	// A symbolic link at <lab>/tmp is refused with a warning, and the rest is still cleaned.
	linkLab := newCleanFixture(t)
	if err := os.Symlink(tmp, filepath.Join(linkLab.lab, "tmp")); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", linkLab.lab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 || !strings.Contains(stderr.String(), "warning: skipping the legacy") || !strings.Contains(stderr.String(), "symbolic link") {
		t.Fatalf("symlinked <lab>/tmp not refused with a warning: exit %d %s\n%s", code, stderr.String(), out.String())
	}
	if !exists(marked) {
		t.Fatal("symlink target entry removed")
	}
	if !strings.Contains(out.String(), "removed ") {
		t.Fatalf("the rest of the Lab was not cleaned:\n%s", out.String())
	}

	// A file at <lab>/tmp is warned about too, without failing.
	fileLab := newCleanFixture(t)
	if err := os.WriteFile(filepath.Join(fileLab.lab, "tmp"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", fileLab.lab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 || !strings.Contains(stderr.String(), "not a directory") {
		t.Fatalf("a file at <lab>/tmp: exit %d %s\n%s", code, stderr.String(), out.String())
	}

	// No usable temp root ($TMPDIR and the per-user default both lie inside the home) makes
	// LabTempPath fail: the section is skipped with a warning and doctor still succeeds.
	homeLab := newCleanFixture(t)
	def, _ := resources.DefaultLabTempRoot()
	resolvedDef, err := filepath.EvalSymlinks(def)
	if err != nil {
		t.Skipf("no per-user default temp root: %v", err)
	}
	t.Setenv("HOME", resolvedDef)
	t.Setenv("TMPDIR", resolvedDef)
	t.Setenv(resources.LabTempRootEnv, "")
	out.Reset()
	stderr.Reset()
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", homeLab.lab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 || !strings.Contains(stderr.String(), "warning: skipping the Lab's check TMPDIR") {
		t.Fatalf("an unusable temp root: exit %d %s\n%s", code, stderr.String(), out.String())
	}
}

// setLabTempRoots makes roots the only places doctor --clean looks for orphaned check
// TMPDIRs until the test ends.
func setLabTempRoots(t *testing.T, roots ...string) {
	t.Helper()
	saved := labTempRoots
	labTempRoots = func() []string { return roots }
	t.Cleanup(func() { labTempRoots = saved })
}

// TestDoctorCleanRemovesOrphanedLabTemps: doctor --clean looks under both the per-user
// default root and the current one for marked check TMPDIRs older than 24h whose Lab is gone
// or that have no lab file, removes them once confirmed, and keeps a live Lab's one and a
// recent orphan.
func TestDoctorCleanRemovesOrphanedLabTemps(t *testing.T) {
	f := newCleanFixture(t)
	t.Setenv("FORGECELL_HOME", f.home)
	currentRoot, defaultRoot := t.TempDir(), t.TempDir()
	setLabTempRoots(t, currentRoot, defaultRoot)
	old := time.Now().Add(-48 * time.Hour)
	labTemp := func(root, hash, lab string, at time.Time) string {
		t.Helper()
		dir := filepath.Join(root, resources.LabTempPrefix+hash)
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, resources.LabTempMarker), nil, 0600); err != nil {
			t.Fatal(err)
		}
		if lab != "" {
			if err := os.WriteFile(filepath.Join(dir, resources.LabTempLabFile), []byte(lab+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	gone := labTemp(defaultRoot, "0000000000000001", filepath.Join(t.TempDir(), "removed-lab"), old)
	noLab := labTemp(currentRoot, "0000000000000002", "", old)
	live := labTemp(defaultRoot, "0000000000000003", f.lab, old)
	recent := labTemp(currentRoot, "0000000000000004", filepath.Join(t.TempDir(), "removed-too"), time.Now())
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"doctor", "--clean", "--lab", f.lab}, strings.NewReader("n\n"), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(out.String(), "remove "+gone+" (") || !strings.Contains(out.String(), "remove "+noLab+" (") || strings.Contains(out.String(), live) || strings.Contains(out.String(), recent) {
		t.Fatalf("dry run listing:\n%s", out.String())
	}
	if !exists(gone) || !exists(noLab) {
		t.Fatal("the dry run removed an orphan")
	}
	out.Reset()
	if code := Run(context.Background(), []string{"doctor", "--clean", "--yes", "--lab", f.lab}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr.String(), out.String())
	}
	if exists(gone) || exists(noLab) || !strings.Contains(out.String(), "removed "+gone+"\n") || !strings.Contains(out.String(), "removed "+noLab+"\n") {
		t.Fatalf("orphans not removed after confirmation:\n%s", out.String())
	}
	if !exists(live) || !exists(recent) {
		t.Fatalf("a live Lab's or a recent check TMPDIR was removed:\n%s", out.String())
	}
}

func TestWatchSkipsPassBelowSoftFloor(t *testing.T) {
	savedLoop, savedWait := runLoop, watchWait
	defer func() { runLoop, watchWait = savedLoop, savedWait }()
	calls, waits := 0, 0
	runLoop = func(context.Context, molecule.LoopOptions) (molecule.LoopReport, error) {
		calls++
		return molecule.LoopReport{}, nil
	}
	watchWait = func(context.Context, time.Duration) bool {
		waits++
		return waits < 2
	}
	samples := 0
	guard := &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
		samples++
		free := 50 * resources.GiB
		if samples == 1 {
			free = resources.GiB
		}
		return resources.Readings{Volumes: []resources.Volume{{Path: "/lab", Free: free, Total: 100 * resources.GiB}}, Memory: 8 * resources.GiB, MemoryKnown: true}
	})}
	var out, stderr bytes.Buffer
	options := molecule.LoopOptions{Template: molecule.Options{LabDir: t.TempDir(), Resources: guard}, MaxConcurrent: 1}
	watchLoop(context.Background(), options, time.Minute, false, options.Template.LabDir, &out, &stderr)
	if !strings.Contains(out.String(), "Watch pass 1\nPass skipped: resources: low disk") {
		t.Fatalf("pass report does not log the skip:\n%s", out.String())
	}
	if calls != 1 {
		t.Fatalf("loop ran %d time(s), want only the second pass", calls)
	}
}
