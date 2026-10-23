package install

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stagedReleases creates n release directories under home/releases, the first oldest,
// and points current at the given index (and previous, when non-negative).
func stagedReleases(t *testing.T, n, current, previous int) (string, []string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Duration(n) * time.Hour)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		dir := filepath.Join(home, "releases", fmt.Sprintf("0.%d.0", i+1))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		// A release's executable is not writable; removal must still succeed.
		if err := os.WriteFile(filepath.Join(dir, "forgecell"), []byte("binary"), 0555); err != nil {
			t.Fatal(err)
		}
		at := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(dir, at, at); err != nil {
			t.Fatal(err)
		}
		paths[i] = dir
	}
	if err := os.Symlink(paths[current], filepath.Join(home, "current")); err != nil {
		t.Fatal(err)
	}
	if previous >= 0 {
		if err := os.Symlink(paths[previous], filepath.Join(home, "previous")); err != nil {
			t.Fatal(err)
		}
	}
	return home, paths
}

func TestInspectReleasesKeepsActivePlusFiveNewest(t *testing.T) {
	// Ten releases; the active one is the oldest.
	home, paths := stagedReleases(t, 10, 0, -1)
	r, err := InspectReleases(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Releases) != 10 {
		t.Fatalf("listed %d releases, want 10", len(r.Releases))
	}
	want := map[string]bool{paths[1]: true, paths[2]: true, paths[3]: true, paths[4]: true}
	if len(r.Prune) != len(want) {
		t.Fatalf("prune = %v, want %v", r.Prune, want)
	}
	for _, p := range r.Prune {
		if !want[p.Path] {
			t.Fatalf("would prune %s, which is active or among the 5 newest", p.Path)
		}
	}
	// Inspection only reads.
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("inspection removed %s", p)
		}
	}
}

func TestPruneReleasesRemovesOnlyTheListedOldReleases(t *testing.T) {
	home, paths := stagedReleases(t, 9, 0, 1)
	r, err := InspectReleases(home)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := PruneReleases(home, r.Prune)
	if err != nil {
		t.Fatal(err)
	}
	// Active (0) and previous (1) are kept with the 5 newest (4..8).
	if len(removed) != 2 {
		t.Fatalf("removed %v, want releases 0.3.0 and 0.4.0", removed)
	}
	for i, p := range paths {
		_, err := os.Stat(p)
		gone := os.IsNotExist(err)
		if (i == 2 || i == 3) != gone {
			t.Fatalf("release %d: removed=%v", i, gone)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".install-lock")); !os.IsNotExist(err) {
		t.Fatal("pruning left the install lock behind")
	}
}

func TestReleaseReferencedOnlyByALabFormulaSurvivesPruneReleases(t *testing.T) {
	// Ten releases; the active one is the newest, so 0.1.0..0.5.0 are old.
	home, paths := stagedReleases(t, 10, 9, -1)
	formulas := filepath.Join(home, "labs", "demo-1234", "formulas")
	if err := os.MkdirAll(formulas, 0700); err != nil {
		t.Fatal(err)
	}
	formula := "id: pinned\nlauncher: " + filepath.Join(home, "releases", "0.2.0", "forgecell") + " analyze\n"
	if err := os.WriteFile(filepath.Join(formulas, "pinned.yaml"), []byte(formula), 0400); err != nil {
		t.Fatal(err)
	}
	r, err := InspectReleases(home)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string][]string{}
	for _, rel := range r.Releases {
		reasons[rel.Version] = rel.Kept
	}
	if got := reasons["0.2.0"]; len(got) != 1 || got[0] != "referenced by Lab demo-1234" {
		t.Fatalf("0.2.0 kept because %v, want referenced by Lab demo-1234", got)
	}
	if got := reasons["0.10.0"]; len(got) != 2 || got[0] != "current" || got[1] != "newest" {
		t.Fatalf("0.10.0 kept because %v, want current and newest", got)
	}
	for _, p := range r.Prune {
		if p.Version == "0.2.0" {
			t.Fatal("inspection would prune a release a Lab Formula references")
		}
	}
	// Even when asked explicitly, pruning re-scans the Formulas and keeps it.
	removed, err := PruneReleases(home, append(r.Prune, Release{Version: "0.2.0", Path: paths[1]}))
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 4 {
		t.Fatalf("removed %v, want 0.1.0, 0.3.0, 0.4.0 and 0.5.0", removed)
	}
	if _, err := os.Stat(paths[1]); err != nil {
		t.Fatal("the Formula-referenced release 0.2.0 was removed")
	}
}

// keptBecause returns why InspectReleases keeps version, failing when it would prune it.
func keptBecause(t *testing.T, r Releases, version string) []string {
	t.Helper()
	for _, p := range r.Prune {
		if p.Version == version {
			t.Fatalf("inspection would prune %s; releases %+v", version, r.Releases)
		}
	}
	for _, rel := range r.Releases {
		if rel.Version == version {
			return rel.Kept
		}
	}
	t.Fatalf("release %s not listed", version)
	return nil
}

func TestReleaseReferencedOnlyByAPendingSuggestionSurvivesPruneReleases(t *testing.T) {
	home, paths := stagedReleases(t, 10, 9, -1)
	suggestions := filepath.Join(home, "labs", "demo-1234", "suggestions")
	if err := os.MkdirAll(suggestions, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(id, status, release string) {
		raw, _ := json.Marshal(map[string]string{"id": id, "status": status, "proposedYaml": "launcher: " + filepath.Join(home, "releases", release, "forgecell") + "\n"})
		if err := os.WriteFile(filepath.Join(suggestions, id+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("suggestion-a", "pending", "0.2.0")
	// A dismissed suggestion's Formula will never run, so it protects nothing.
	write("suggestion-b", "dismissed", "0.3.0")
	r, err := InspectReleases(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := keptBecause(t, r, "0.2.0"); len(got) != 1 || got[0] != "referenced by pending suggestion suggestion-a in Lab demo-1234" {
		t.Fatalf("0.2.0 kept because %v", got)
	}
	removed, err := PruneReleases(home, append(r.Prune, Release{Version: "0.2.0", Path: paths[1]}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[1]); err != nil {
		t.Fatal("the release a pending suggestion references was removed")
	}
	if _, err := os.Stat(paths[2]); !os.IsNotExist(err) {
		t.Fatalf("the release only a dismissed suggestion references was kept; removed %v", removed)
	}
}

func TestReleaseReferencedOnlyByAGivenLabOutsideHomeSurvivesPruneReleases(t *testing.T) {
	home, paths := stagedReleases(t, 10, 9, -1)
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The Lab is reached through a symlink, and so is its formulas directory.
	realLab := filepath.Join(outside, "real-lab")
	if err := os.MkdirAll(filepath.Join(outside, "real-formulas"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(realLab, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "real-formulas"), filepath.Join(realLab, "formulas")); err != nil {
		t.Fatal(err)
	}
	lab := filepath.Join(outside, "lab")
	if err := os.Symlink(realLab, lab); err != nil {
		t.Fatal(err)
	}
	// The Formula names the release through a symlink to the home.
	alias := filepath.Join(outside, "home-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	formula := "launcher: " + filepath.Join(alias, "releases", "0.2.0", "forgecell") + "\n"
	if err := os.WriteFile(filepath.Join(outside, "real-formulas", "f.yaml"), []byte(formula), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := InspectReleases(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Prune) != 5 {
		t.Fatalf("without the Lab, prune = %v; want 0.1.0..0.5.0", r.Prune)
	}
	r, err = InspectReleases(home, lab)
	if err != nil {
		t.Fatal(err)
	}
	if got := keptBecause(t, r, "0.2.0"); len(got) != 1 || got[0] != "referenced by Lab "+lab {
		t.Fatalf("0.2.0 kept because %v", got)
	}
	if _, err := PruneReleases(home, append(r.Prune, Release{Version: "0.2.0", Path: paths[1]}), lab); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths[1]); err != nil {
		t.Fatal("the release a given Lab references was removed")
	}
}

func TestPruneReleasesRefusesPathsOutsideReleases(t *testing.T) {
	home, paths := stagedReleases(t, 2, 1, -1)
	outside := filepath.Join(t.TempDir(), "1.0.0")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	removed, err := PruneReleases(home, []Release{{Path: outside}, {Path: paths[1]}})
	if err == nil || len(removed) != 0 {
		t.Fatalf("removed %v, err %v; want a refusal and nothing removed", removed, err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a path outside releases was removed")
	}
	if _, err := os.Stat(paths[1]); err != nil {
		t.Fatal("the active release was removed")
	}
}
