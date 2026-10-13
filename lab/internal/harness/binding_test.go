package harness

import (
	"context"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const completedProvider = `touch started
cat > /dev/null
printf '{"structured_output":{"schemaVersion":"v1","outcome":"completed","reason":"implemented","paths":[]}}'
`

// fakeBuild is a launcher that records any execution in a sentinel, beside a release
// manifest. An empty protocol writes a manifest without protocolVersion.
func fakeBuild(t *testing.T, version, protocol string) (launcher, sentinel string) {
	t.Helper()
	dir := t.TempDir()
	sentinel = filepath.Join(dir, "executed")
	launcher = filepath.Join(dir, "forgecell")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\ntouch '"+sentinel+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"version":%q,"sha256":"x"`, version)
	if protocol != "" {
		manifest += fmt.Sprintf(`,"protocolVersion":%q`, protocol)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest+"}"), 0600); err != nil {
		t.Fatal(err)
	}
	return launcher, sentinel
}

func invoke(t *testing.T, launcher, provider string) (process.Result, bool) {
	t.Helper()
	dir := t.TempDir()
	r := InvokeBinding(context.Background(), []string{launcher, "__adapter", "claude-code", provider}, dir, 30*time.Second, map[string]any{"kind": "molecule"})
	_, err := os.Stat(filepath.Join(dir, "started"))
	return r, err == nil
}

func TestUpgradeWithinTheSameProtocolKeepsTheBindingRunnable(t *testing.T) {
	provider := fake(t, completedProvider)
	for _, version := range []string{"1.0.0", "1.1.0"} {
		launcher, sentinel := fakeBuild(t, version, AdapterProtocolVersion)
		r, started := invoke(t, launcher, provider)
		if !r.OK || !started {
			t.Fatalf("build %s did not run: %+v started=%t", version, r, started)
		}
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatalf("saved launcher %s was executed", launcher)
		}
	}
}

func TestAdapterProtocolBumpBlocksWithAnActionableMessage(t *testing.T) {
	provider := fake(t, completedProvider)
	launcher, sentinel := fakeBuild(t, "1.0.0", "4")
	r, started := invoke(t, launcher, provider)
	running, _ := os.Executable()
	if r.OK || r.Code != -1 || started {
		t.Fatalf("incompatible protocol ran: %+v started=%t", r, started)
	}
	for _, want := range []string{launcher, running, "version 1.0.0", "adapter protocol 4", "adapter protocol " + AdapterProtocolVersion, "forgecell init --lab DIR --harness claude-code"} {
		if !strings.Contains(r.Error, want) {
			t.Fatalf("message omits %q: %s", want, r.Error)
		}
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("saved launcher was executed")
	}
}

func TestUnknownProtocolBlocksAndNeverExecutesTheLauncher(t *testing.T) {
	provider := fake(t, completedProvider)
	manifest := func(dir, content string) {
		os.Remove(filepath.Join(dir, "manifest.json"))
		os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(content), 0600)
	}
	valid := `{"version":"1.0.0","protocolVersion":"` + AdapterProtocolVersion + `"}`
	for _, tc := range []struct {
		name  string
		write func(dir string)
	}{
		{"no manifest", func(dir string) { os.Remove(filepath.Join(dir, "manifest.json")) }},
		{"no protocolVersion", func(string) {}},
		{"malformed", func(dir string) { manifest(dir, `{`) }},
		{"invalid protocol", func(dir string) { manifest(dir, `{"version":"1.0.0","protocolVersion":"1; rm"}`) }},
		{"oversized", func(dir string) { manifest(dir, strings.TrimSuffix(valid, "}")+strings.Repeat(" ", 8192)+"}") }},
		{"directory manifest", func(dir string) {
			os.Remove(filepath.Join(dir, "manifest.json"))
			os.Mkdir(filepath.Join(dir, "manifest.json"), 0700)
		}},
		{"symlinked manifest", func(dir string) {
			target := filepath.Join(t.TempDir(), "real.json")
			os.WriteFile(target, []byte(valid), 0600)
			os.Remove(filepath.Join(dir, "manifest.json"))
			os.Symlink(target, filepath.Join(dir, "manifest.json"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launcher, sentinel := fakeBuild(t, "1.0.0", "")
			tc.write(filepath.Dir(launcher))
			r, started := invoke(t, launcher, provider)
			if r.OK || r.Code != -1 || started || !strings.Contains(r.Error, "cannot be established") || !strings.Contains(r.Error, launcher) {
				t.Fatalf("%+v started=%t", r, started)
			}
			if _, err := os.Stat(sentinel); err == nil {
				t.Fatal("saved launcher was executed")
			}
		})
	}
}

func TestMissingLauncherBlocksWithItsPathAndVersion(t *testing.T) {
	provider := fake(t, completedProvider)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"1.2.3","protocolVersion":"`+AdapterProtocolVersion+`"}`), 0600)
	launcher := filepath.Join(dir, "forgecell")
	r, started := invoke(t, launcher, provider)
	if r.OK || r.Code != -1 || started || !strings.Contains(r.Error, launcher) || !strings.Contains(r.Error, "was not found") || strings.Contains(r.Error, "different") {
		t.Fatalf("%+v started=%t", r, started)
	}
}

func TestAbsolutePathOfTheRunningExecutableKeepsWorking(t *testing.T) {
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if id := ResolveLauncher(running); id.Class != LauncherRunning || !id.Runnable() {
		t.Fatalf("%+v", id)
	}
}

// managedHome builds the installer layout: bin/forgecell wrapper, releases/VERSION
// and a current link, with a bin directory exposing the wrapper.
func managedHome(t *testing.T) (home, exposed string) {
	t.Helper()
	home = t.TempDir()
	os.MkdirAll(filepath.Join(home, "bin"), 0700)
	wrapper := "#!/bin/sh\n" + managedLauncherMark + "\nexec '" + filepath.Join(home, "current", "forgecell") + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(home, "bin", "forgecell"), []byte(wrapper), 0755); err != nil {
		t.Fatal(err)
	}
	exposedDir := t.TempDir()
	exposed = filepath.Join(exposedDir, "forgecell")
	if err := os.Symlink(filepath.Join(home, "bin", "forgecell"), exposed); err != nil {
		t.Fatal(err)
	}
	return home, exposed
}

func activate(t *testing.T, home, version, protocol string) (sentinel string) {
	t.Helper()
	release := filepath.Join(home, "releases", version)
	os.MkdirAll(release, 0700)
	sentinel = filepath.Join(release, "executed")
	os.WriteFile(filepath.Join(release, "forgecell"), []byte("#!/bin/sh\ntouch '"+sentinel+"'\n"), 0755)
	manifest := fmt.Sprintf(`{"version":%q,"sha256":"x","protocolVersion":%q}`, version, protocol)
	os.WriteFile(filepath.Join(release, "manifest.json"), []byte(manifest), 0600)
	link := filepath.Join(home, "current")
	os.Remove(link)
	if err := os.Symlink(release, link); err != nil {
		t.Fatal(err)
	}
	return sentinel
}

func TestManagedLauncherResolvesToTheActiveReleaseAcrossUpgrades(t *testing.T) {
	provider := fake(t, completedProvider)
	home, exposed := managedHome(t)
	if path, ok := ManagedLauncher(filepath.Dir(exposed)); !ok || path != exposed {
		t.Fatalf("managed launcher not recognised: %q %t", path, ok)
	}
	for _, version := range []string{"1.0.0", "1.1.0"} {
		sentinel := activate(t, home, version, AdapterProtocolVersion)
		r, started := invoke(t, exposed, provider)
		if !r.OK || !started {
			t.Fatalf("active release %s did not serve the binding: %+v", version, r)
		}
		id := ResolveLauncher(exposed)
		if !id.Managed || id.Manifest.Version != version || id.Class != LauncherCompatible {
			t.Fatalf("%+v", id)
		}
		if _, err := os.Stat(sentinel); err == nil {
			t.Fatal("active release was executed")
		}
	}
	activate(t, home, "2.0.0", "4")
	if r, started := invoke(t, exposed, provider); r.OK || started || !strings.Contains(r.Error, "adapter protocol 4") {
		t.Fatalf("a protocol bump in the active release must block: %+v", r)
	}
	os.Remove(filepath.Join(home, "current"))
	if r, started := invoke(t, exposed, provider); r.OK || started || !strings.Contains(r.Error, "no active release") {
		t.Fatalf("a missing active release must block: %+v", r)
	}
}

func TestOtherScriptsAreNotManagedLaunchers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "forgecell"), []byte("#!/bin/sh\nexit 0\n"), 0755)
	if _, ok := ManagedLauncher(dir); ok {
		t.Fatal("an arbitrary script was treated as the managed launcher")
	}
	if _, ok := ManagedLauncher("relative"); ok {
		t.Fatal("a relative directory was accepted")
	}
}

func TestProvenanceRecordsVersionAndExecutableDigest(t *testing.T) {
	SetRunningVersion("9.8.7")
	t.Cleanup(func() { SetRunningVersion("") })
	launcher, _ := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
	p := RecordProvenance([]string{launcher, "__adapter", "codex", "/provider"})
	if p.ForgecellVersion != "9.8.7" || len(p.ExecutableSHA256) != 64 || p.ProtocolVersion != AdapterProtocolVersion || p.SavedLauncher != launcher || p.LauncherClass != LauncherCompatible {
		t.Fatalf("%+v", p)
	}
	if c := RecordProvenance([]string{"custom-script"}); c.LauncherClass != "custom" || c.SavedLauncher != "" || c.ForgecellVersion != "9.8.7" {
		t.Fatalf("%+v", c)
	}
}
