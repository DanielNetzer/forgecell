package formula

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const devRoot = "/home/op/.forgecell/releases/"

// withCommitTimes replaces the recorded dev-build commit times with times, keyed
// by launcher; a launcher missing from times records none.
func withCommitTimes(t *testing.T, times map[string]int64) {
	t.Helper()
	previous := devCommitTime
	devCommitTime = func(launcher string) int64 { return times[launcher] }
	t.Cleanup(func() { devCommitTime = previous })
}

func TestDevBuildRebindNewerIsAcceptedOlderIsRefusedWithReason(t *testing.T) {
	early, late := devRoot+"0.16.9-dev.ffff/forgecell", devRoot+"0.16.9-dev.0000/forgecell"
	untimed, dirty := devRoot+"0.16.9-dev.abcd/forgecell", devRoot+"0.16.9-dev.abcd.dirty.12345678/forgecell"
	twin := devRoot + "0.16.9-dev.1234/forgecell"
	withCommitTimes(t, map[string]int64{early: 1_700_000_000, late: 1_700_000_100, twin: 1_700_000_000})
	for name, c := range map[string]struct {
		current, next, order string
	}{
		"newer dev build of a newer version":           {devRoot + "0.16.8-dev.ffff/forgecell", devRoot + "0.16.9-dev.0000/forgecell", LauncherNewer},
		"dev build after its release":                  {devRoot + "0.16.8/forgecell", devRoot + "0.16.9-dev.abcd/forgecell", LauncherNewer},
		"release after its dev build":                  {devRoot + "0.16.9-dev.abcd/forgecell", devRoot + "0.16.9/forgecell", LauncherNewer},
		"older dev build":                              {devRoot + "0.16.9-dev.0000/forgecell", devRoot + "0.16.8-dev.ffff/forgecell", LauncherOlder},
		"dev build of the current release":             {devRoot + "0.16.9/forgecell", devRoot + "0.16.9-dev.abcd/forgecell", LauncherOlder},
		"same version, later commit, lower hash":       {early, late, LauncherNewer},
		"same version, earlier commit, higher hash":    {late, early, LauncherOlder},
		"same version, the next records no time":       {early, untimed, LauncherUnordered},
		"same version, the current records no time":    {untimed, late, LauncherUnordered},
		"same version, neither records a time":         {untimed, dirty, LauncherUnordered},
		"same version, equal commit times":             {early, twin, LauncherUnordered},
		"identical":                                    {early, early, LauncherSame},
		"other installation":                           {devRoot + "0.16.8/forgecell", "/opt/forgecell/releases/0.16.9/forgecell", LauncherUnrelated},
		"not a release":                                {"/usr/local/bin/forgecell", devRoot + "0.16.9/forgecell", LauncherUnrelated},
		"other installation, even with recorded times": {early, "/opt/forgecell/releases/0.16.9-dev.0000/forgecell", LauncherUnrelated},
	} {
		order, reason := CompareLaunchers(c.current, c.next)
		if order != c.order {
			t.Errorf("%s: order %s, want %s (%s)", name, order, c.order, reason)
		}
		if reason == "" || strings.Contains(reason, "/home/op") || strings.Contains(reason, "/opt/") || strings.Contains(reason, "/usr/") {
			t.Errorf("%s: reason must be plain and path-free: %q", name, reason)
		}
		// The rebase check agrees: only a newer build is a launcher upgrade.
		if IsReleaseLauncher(c.current) && IsReleaseLauncher(c.next) {
			if got := launcherUpgrade(c.current, c.next); got != (c.order == LauncherNewer) {
				t.Errorf("%s: launcherUpgrade %v, want %v", name, got, c.order == LauncherNewer)
			}
		}
	}
	if _, reason := CompareLaunchers(early, untimed); !strings.Contains(reason, "records no commit time") || !strings.Contains(reason, "cannot tell which is newer") {
		t.Errorf("a missing commit time needs the plain reason, got %q", reason)
	}
	if _, reason := CompareLaunchers(early, twin); !strings.Contains(reason, "same recorded commit time") {
		t.Errorf("equal commit times need the plain reason, got %q", reason)
	}
	if _, reason := CompareLaunchers(late, early); !strings.Contains(reason, "older") || !strings.Contains(reason, "0.16.9-dev.ffff") {
		t.Errorf("an older dev build needs a reason naming its version, got %q", reason)
	}
	if _, reason := CompareLaunchers(devRoot+"0.16.9/forgecell", devRoot+"0.16.8/forgecell"); !strings.Contains(reason, "older") || !strings.Contains(reason, "0.16.8") {
		t.Errorf("an older build needs a reason naming its version, got %q", reason)
	}
}

func TestDevBuildRebaseOrdersSameVersionDevBuildsByCommitTime(t *testing.T) {
	first, second := devRoot+"0.9.2-dev.ffff/forgecell", devRoot+"0.9.2-dev.0000/forgecell"
	untimed := devRoot + "0.9.2-dev.abcd/forgecell"
	withCommitTimes(t, map[string]int64{first: 1_700_000_000, second: 1_700_000_100})
	recipe := strings.ReplaceAll(launcherRecipe, oldLauncher, first)
	if _, err := Parse([]byte(recipe)); err != nil {
		t.Fatal(err)
	}
	to := func(launcher string) []byte {
		return []byte(strings.Replace(recipe, first+", __adapter, claude", launcher+", __adapter, claude", 1))
	}
	if err := LauncherOnlyChange([]byte(recipe), to(second)); err != nil {
		t.Fatalf("a newer dev build of the same version (alphabetically lower) was refused: %v", err)
	}
	if err := LauncherOnlyChange(to(second), []byte(recipe)); err == nil {
		t.Fatal("an older dev build of the same version was carried as an upgrade")
	}
	if err := LauncherOnlyChange([]byte(recipe), to(untimed)); err == nil {
		t.Fatal("a dev build without a recorded commit time was carried as an upgrade")
	}
	if err := LauncherOnlyChange([]byte(recipe), to(devRoot+"0.9.3-dev.0000/forgecell")); err != nil {
		t.Fatalf("a dev build of a newer version was refused: %v", err)
	}
}

func writeDevBuild(t *testing.T, dir string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DevBuildFile), content, mode); err != nil {
		t.Fatal(err)
	}
}

func TestDevCommitTimeReadsOnlyATrustworthyRecord(t *testing.T) {
	releases := filepath.Join(t.TempDir(), "releases")
	record := func(version string, at int64) []byte {
		raw, _ := json.Marshal(DevBuild{Version: version, CommitTime: at})
		return raw
	}
	good := filepath.Join(releases, "0.9.2-dev.abcd")
	writeDevBuild(t, good, record("0.9.2-dev.abcd", 1_700_000_000), 0600)
	if got := devCommitTime(filepath.Join(good, "forgecell")); got != 1_700_000_000 {
		t.Fatalf("a valid record reads %d", got)
	}
	cases := map[string][]byte{
		"0.9.2-dev.0001": record("0.9.2-dev.other", 1_700_000_000),
		"0.9.2-dev.0002": record("0.9.2-dev.0002", 0),
		"0.9.2-dev.0003": record("0.9.2-dev.0003", -5),
		"0.9.2-dev.0004": []byte(`{"version":"0.9.2-dev.0004","commitTime":1700000000,"extra":1}`),
		"0.9.2-dev.0005": []byte(`{"version":"0.9.2-dev.0005","commitTime":1700000000}{}`),
		"0.9.2-dev.0006": []byte("not json"),
		"0.9.2-dev.0007": []byte(`{"version":"0.9.2-dev.0007","commitTime":1700000000,"pad":"` + strings.Repeat("x", 2000) + `"}`),
		"0.9.2-dev.0008": record("0.9.2-dev.0008", maxCommitTime+1),
	}
	for version, raw := range cases {
		writeDevBuild(t, filepath.Join(releases, version), raw, 0600)
		if got := devCommitTime(filepath.Join(releases, version, "forgecell")); got != 0 {
			t.Errorf("%s: an untrustworthy record read as %d", version, got)
		}
	}
	if got := devCommitTime(filepath.Join(releases, "0.9.2-dev.missing", "forgecell")); got != 0 {
		t.Errorf("a missing record read as %d", got)
	}
	linked := filepath.Join(releases, "0.9.2-dev.link")
	if err := os.MkdirAll(linked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(good, DevBuildFile), filepath.Join(linked, DevBuildFile)); err != nil {
		t.Fatal(err)
	}
	if got := devCommitTime(filepath.Join(linked, "forgecell")); got != 0 {
		t.Errorf("a symlinked record was followed: %d", got)
	}
	linkedDir := filepath.Join(releases, "0.9.2-dev.abce")
	if err := os.Symlink(good, linkedDir); err != nil {
		t.Fatal(err)
	}
	if got := devCommitTime(filepath.Join(linkedDir, "forgecell")); got != 0 {
		t.Errorf("a symlinked release directory was followed: %d", got)
	}
}
