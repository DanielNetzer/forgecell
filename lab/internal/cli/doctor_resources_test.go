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

func TestDoctorWarnsAboutLargeGoCacheWithoutCleaningIt(t *testing.T) {
	cache := t.TempDir()
	goCacheSizes.Store(cache, resourceSize{Path: cache, Size: "25.0 GiB", Bytes: 25 * resources.GiB, Complete: true})
	t.Setenv("GOCACHE", cache)
	d := inspectResources(t.TempDir(), t.TempDir())
	found := false
	for _, w := range d.Warnings {
		found = found || strings.Contains(w, "go clean -cache")
	}
	if !found || !exists(cache) {
		t.Fatalf("warnings %v; want the go clean -cache hint and the cache untouched", d.Warnings)
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
