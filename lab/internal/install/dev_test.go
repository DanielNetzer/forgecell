package install

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

const fakeProtocol = "fake-protocol-9"

func fakeMainFor(protocol string) string {
	return "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nvar version = \"dev-go\"\n\nfunc main() {\n\tif len(os.Args) > 1 && os.Args[1] == \"--protocol\" {\n\t\tfmt.Println(\"" + protocol + "\")\n\t\treturn\n\t}\n\tfmt.Println(version)\n}\n"
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=forgecell-test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir, cmd.Env = dir, gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", message)
}

func newCheckout(t *testing.T, protocol string) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"lab/go.mod": "module fakeforgecell\n\ngo 1.21\n", "lab/cmd/forgecell/main.go": fakeMainFor(protocol)})
	gitRun(t, dir, "init", "-q")
	commitAll(t, dir, "initial")
	return dir
}

func devCheckout(t *testing.T) string {
	t.Helper()
	return newCheckout(t, harness.AdapterProtocolVersion)
}

func revOf(t *testing.T, dir string) string {
	t.Helper()
	return gitRun(t, dir, "rev-parse", "--short", "HEAD")
}

func fakeBuildProtocol(content, protocol string) BuildFunc {
	return func(_ context.Context, _, version, output string) error {
		script := "#!/bin/sh\ncase \"$1\" in\n--protocol) printf '%s\\n' '" + protocol + "';;\n*) printf '%s\\n' '" + version + "';;\nesac\n# " + content + "\n"
		return os.WriteFile(output, []byte(script), 0755)
	}
}

func fakeBuild(content string) BuildFunc {
	return fakeBuildProtocol(content, harness.AdapterProtocolVersion)
}

func failingBuild(_ context.Context, _, _, _ string) error { return errors.New("compiler exploded") }

func noProtocolBuild(_ context.Context, _, version, output string) error {
	return os.WriteFile(output, []byte("#!/bin/sh\ncase \"$1\" in\n--protocol) exit 2;;\n*) printf '%s\\n' '"+version+"';;\nesac\n"), 0755)
}

func devOptions(t *testing.T, checkout string) DevOptions {
	t.Helper()
	return DevOptions{Home: t.TempDir(), Bin: filepath.Join(t.TempDir(), "bin"), Checkout: checkout, RunningVersion: "0.2.0-preview.2", Build: fakeBuild("one")}
}

func linkTargets(t *testing.T, home string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, name := range []string{"current", "previous"} {
		target, err := os.Readlink(filepath.Join(home, name))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		got[name] = target
	}
	return got
}

func releaseNames(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, "releases"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func assertNoInstallLeftovers(t *testing.T, home string) {
	t.Helper()
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Fatalf("install leftover: %s", e.Name())
		}
	}
}

func installTwo(t *testing.T, o DevOptions) Options {
	t.Helper()
	base := Options{Home: o.Home, Bin: o.Bin, Source: fixture(t, "0.1.0"), Version: "0.1.0"}
	if _, err := Install(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	base.Source, base.Version = fixture(t, "0.2.0"), "0.2.0"
	if _, err := Install(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	return base
}

func reportedVersion(t *testing.T, exe string) string {
	t.Helper()
	out, err := exec.Command(exe, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func dirtyVersion(rev string) *regexp.Regexp {
	return regexp.MustCompile(`^0\.2\.0-dev\.` + rev + `\.dirty\.[0-9a-f]{8}$`)
}

func TestDevVersionDerivation(t *testing.T) {
	for _, c := range []struct {
		running string
		digest  string
		want    string
	}{
		{"0.2.0-preview.2", "", "0.2.0-dev.abc1234"},
		{"1.4.7", "", "1.4.7-dev.abc1234"},
		{"1.4.7", "0123456789abcdef", "1.4.7-dev.abc1234.dirty.01234567"},
		{"0.3.0-dev.ffff000", "", "0.3.0-dev.abc1234"},
		{"dev-go", "", "0.0.0-dev.abc1234"},
		{"unknown", "", "0.0.0-dev.abc1234"},
		{"", "", "0.0.0-dev.abc1234"},
		{"0.2", "", "0.0.0-dev.abc1234"},
		{"v1.2.3", "89abcdef", "0.0.0-dev.abc1234.dirty.89abcdef"},
	} {
		got := devVersion(c.running, "abc1234", c.digest)
		if got != c.want || !versionPattern.MatchString(got) {
			t.Errorf("devVersion(%q, digest=%q) = %q, want %q", c.running, c.digest, got, c.want)
		}
	}
}

func TestDevBuildEnvAllowsOnlyTheNamedVariables(t *testing.T) {
	env := buildEnv([]string{
		"PATH=/usr/bin", "HOME=/home/dev", "GOCACHE=/cache", "GOMODCACHE=/mod", "TMPDIR=/tmp", "HTTPS_PROXY=http://proxy", "GOPROXY=https://proxy.example",
		"GOFLAGS=-toolexec=/evil", "GOEXPERIMENT=boringcrypto", "GOWORK=/evil/go.work", "GOTOOLCHAIN=go9.99", "CGO_ENABLED=1", "GOENV=/evil/env", "GITHUB_TOKEN=secret", "ANTHROPIC_API_KEY=secret", "LD_PRELOAD=/evil.so",
	})
	want := []string{
		"PATH=/usr/bin", "HOME=/home/dev", "GOCACHE=/cache", "GOMODCACHE=/mod", "TMPDIR=/tmp", "HTTPS_PROXY=http://proxy", "GOPROXY=https://proxy.example",
		"GOFLAGS=-mod=readonly", "GOWORK=off", "GOTOOLCHAIN=local", "GOTELEMETRY=off", "CGO_ENABLED=0", "GOEXPERIMENT=", "GOENV=off",
	}
	sort.Strings(env)
	sort.Strings(want)
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("got %v\nwant %v", env, want)
	}
}

// skipRealBuildInShort skips a test that drives the real Go toolchain under
// go test -short; the full suite, as CI runs it, still builds.
func skipRealBuildInShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs a real Go build")
	}
}

func TestDevGoBuildIgnoresTheAmbientGoEnvironment(t *testing.T) {
	skipRealBuildInShort(t)
	marker := filepath.Join(t.TempDir(), "toolexec-ran")
	wrapper := filepath.Join(t.TempDir(), "toolexec.sh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\necho ran >> '"+marker+"'\nexec \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOFLAGS", "-toolexec="+wrapper)
	t.Setenv("GOEXPERIMENT", "forgecell-test-unknown-experiment")
	// The build keeps the shared GOCACHE: an empty one forces a full standard
	// library rebuild, and the pass-through of GOCACHE itself is covered by
	// TestDevBuildEnvAllowsOnlyTheNamedVariables. A leaked -toolexec changes
	// the build's cache key, so a warm cache cannot hide one.
	root := t.TempDir()
	lean := "package main\n\nimport \"os\"\n\nvar version = \"dev-go\"\n\nfunc main() { os.Stdout.WriteString(version + \"\\n\") }\n"
	writeTree(t, root, map[string]string{"lab/go.mod": "module fakeforgecell\n\ngo 1.21\n", "lab/cmd/forgecell/main.go": lean})
	out := filepath.Join(t.TempDir(), "forgecell")
	if err := goBuild(context.Background(), root, "0.2.0-dev.abc1234", out); err != nil {
		t.Fatalf("ambient Go settings reached the build: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("ambient GOFLAGS -toolexec reached the build")
	}
	if got := reportedVersion(t, out); got != "0.2.0-dev.abc1234" {
		t.Fatalf("%q", got)
	}
}

func TestDevInstallStagesARealBuildWithoutActivating(t *testing.T) {
	skipRealBuildInShort(t)
	checkout := devCheckout(t)
	o := devOptions(t, checkout)
	o.Build = nil
	r, err := DevInstall(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	version := "0.2.0-dev." + revOf(t, checkout)
	home, _ := filepath.EvalSymlinks(o.Home)
	dir := filepath.Join(home, "releases", version)
	if r.Version != version || r.Path != filepath.Join(dir, "forgecell") || r.Activated {
		t.Fatalf("%+v", r)
	}
	if got := reportedVersion(t, r.Path); got != version {
		t.Fatalf("the build must report its stamped version, got %q", got)
	}
	exe, err := os.ReadFile(r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if m := readManifest(t, o.Home, version); m.Version != version || m.SHA256 != sum(exe) || m.ProtocolVersion != harness.AdapterProtocolVersion {
		t.Fatalf("%+v", m)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil || r.ManifestSHA256 != sum(raw) || r.ManifestSHA256 != harness.ReadManifest(dir).Digest {
		t.Fatalf("manifest digest %s: %v", r.ManifestSHA256, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "THIRD-PARTY-NOTICES.txt")); err != nil {
		t.Fatal(err)
	}
	if got := linkTargets(t, o.Home); got["current"] != "" || got["previous"] != "" {
		t.Fatalf("staging activated something: %v", got)
	}
	for _, path := range []string{o.Bin, filepath.Join(o.Home, "bin")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("staging created %s", path)
		}
	}
	if id := harness.ResolveLauncher(r.Path); id.Class != harness.LauncherCompatible || id.Manifest.Version != version {
		t.Fatalf("a staged dev build must not read as unknown: %+v", id)
	}
	assertNoInstallLeftovers(t, o.Home)
}

func TestDevInstallRecordsTheBuiltBinarysProtocol(t *testing.T) {
	if fakeProtocol == harness.AdapterProtocolVersion {
		t.Fatal("the fake protocol must differ from the running constant")
	}
	t.Run("fake builder", func(t *testing.T) {
		o := devOptions(t, devCheckout(t))
		o.Build = fakeBuildProtocol("one", fakeProtocol)
		r, err := DevInstall(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if m := readManifest(t, o.Home, r.Version); m.ProtocolVersion != fakeProtocol {
			t.Fatalf("the manifest must record the built binary's protocol, not the running one: %+v", m)
		}
	})
	t.Run("real build of a checkout with another protocol", func(t *testing.T) {
		skipRealBuildInShort(t)
		o := devOptions(t, newCheckout(t, fakeProtocol))
		o.Build = nil
		r, err := DevInstall(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if m := readManifest(t, o.Home, r.Version); m.ProtocolVersion != fakeProtocol || m.Version != r.Version {
			t.Fatalf("%+v", m)
		}
		if id := harness.ResolveLauncher(r.Path); id.Class != harness.LauncherIncompatible {
			t.Fatalf("a build speaking another protocol is incompatible, never unknown or compatible: %+v", id)
		}
	})
	t.Run("a stored release is not reused under another protocol", func(t *testing.T) {
		o := Options{Home: t.TempDir(), Source: fixture(t, "0.3.0-dev.abc1234"), Version: "0.3.0-dev.abc1234", Protocol: harness.AdapterProtocolVersion}
		for range 2 {
			if _, err := Stage(context.Background(), o); err != nil {
				t.Fatal(err)
			}
		}
		o.Protocol = fakeProtocol
		if _, err := Stage(context.Background(), o); err == nil || !strings.Contains(err.Error(), "already exists with different content") {
			t.Fatalf("a release stored under one protocol was accepted under another: %v", err)
		}
	})
}

func TestDevInstallStageLeavesTheActiveReleasesAlone(t *testing.T) {
	o := devOptions(t, devCheckout(t))
	installTwo(t, o)
	before := linkTargets(t, o.Home)
	launcher, err := os.ReadFile(filepath.Join(o.Home, "bin", "forgecell"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DevInstall(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if after := linkTargets(t, o.Home); !reflect.DeepEqual(before, after) || filepath.Base(after["current"]) != "0.2.0" || filepath.Base(after["previous"]) != "0.1.0" {
		t.Fatalf("%v -> %v", before, after)
	}
	if after, _ := os.ReadFile(filepath.Join(o.Home, "bin", "forgecell")); string(after) != string(launcher) {
		t.Fatal("launcher changed")
	}
	if got := releaseNames(t, o.Home); len(got) != 3 {
		t.Fatalf("%v", got)
	}
	assertNoInstallLeftovers(t, o.Home)
}

func TestDevInstallActivateRollsBackThroughTheOrdinaryPath(t *testing.T) {
	checkout := devCheckout(t)
	o := devOptions(t, checkout)
	o.Activate = true
	base := Options{Home: o.Home, Bin: o.Bin, Source: fixture(t, "0.2.0"), Version: "0.2.0"}
	if _, err := Install(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	first := "0.2.0-dev." + revOf(t, checkout)
	r, err := DevInstall(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Activated || r.Version != first {
		t.Fatalf("%+v", r)
	}
	if got := linkTargets(t, o.Home); filepath.Base(got["current"]) != first || filepath.Base(got["previous"]) != "0.2.0" {
		t.Fatalf("%v", got)
	}
	id := harness.ResolveLauncher(filepath.Join(o.Bin, "forgecell"))
	if id.Class != harness.LauncherCompatible || !id.Managed || id.Manifest.Version != first {
		t.Fatalf("an activated dev build must be launcher-compatible: %+v", id)
	}

	writeTree(t, checkout, map[string]string{"change.txt": "two"})
	commitAll(t, checkout, "second")
	second := "0.2.0-dev." + revOf(t, checkout)
	if _, err = DevInstall(context.Background(), o); err != nil {
		t.Fatalf("a dev release that is current must not block the next one: %v", err)
	}
	if got := linkTargets(t, o.Home); filepath.Base(got["current"]) != second || filepath.Base(got["previous"]) != first {
		t.Fatalf("%v", got)
	}
	if _, err = Rollback(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	if got := linkTargets(t, o.Home); filepath.Base(got["current"]) != first || filepath.Base(got["previous"]) != second {
		t.Fatalf("%v", got)
	}
	assertNoInstallLeftovers(t, o.Home)
}

func TestDevInstallRefusesADirtyTreeUnlessAllowed(t *testing.T) {
	for _, c := range []struct {
		name      string
		committed map[string]string
		files     map[string]string
		want      string
	}{
		{"modified tracked file", nil, map[string]string{"lab/go.mod": "module fakeforgecell\n\ngo 1.22\n"}, "lab/go.mod"},
		{"untracked file", nil, map[string]string{"scratch.txt": "wip"}, "scratch.txt"},
		{"ignored Go source", map[string]string{".gitignore": "zz_ignored.go\n"}, map[string]string{"lab/cmd/forgecell/zz_ignored.go": "package main\n"}, "zz_ignored.go"},
	} {
		t.Run(c.name, func(t *testing.T) {
			checkout := devCheckout(t)
			if c.committed != nil {
				writeTree(t, checkout, c.committed)
				commitAll(t, checkout, "setup")
			}
			o := devOptions(t, checkout)
			installTwo(t, o)
			before := linkTargets(t, o.Home)
			writeTree(t, checkout, c.files)
			if _, err := DevInstall(context.Background(), o); err == nil || !strings.Contains(err.Error(), "--allow-dirty") || !strings.Contains(err.Error(), "uncommitted") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("dirty tree accepted or unexplained: %v", err)
			}
			if got := releaseNames(t, o.Home); len(got) != 2 || !reflect.DeepEqual(before, linkTargets(t, o.Home)) {
				t.Fatalf("%v %v", got, linkTargets(t, o.Home))
			}
			assertNoInstallLeftovers(t, o.Home)
		})
	}
	t.Run("allowed", func(t *testing.T) {
		checkout := devCheckout(t)
		o := devOptions(t, checkout)
		o.Build, o.AllowDirty = nil, true
		writeTree(t, checkout, map[string]string{"scratch.txt": "wip"})
		r, err := DevInstall(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		if !dirtyVersion(revOf(t, checkout)).MatchString(r.Version) || reportedVersion(t, r.Path) != r.Version {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("allowed on a clean tree adds no suffix", func(t *testing.T) {
		checkout := devCheckout(t)
		o := devOptions(t, checkout)
		o.AllowDirty = true
		r, err := DevInstall(context.Background(), o)
		if err != nil || r.Version != "0.2.0-dev."+revOf(t, checkout) {
			t.Fatalf("%+v %v", r, err)
		}
	})
	t.Run("ignored files the compiler never reads are not dirt", func(t *testing.T) {
		checkout := devCheckout(t)
		writeTree(t, checkout, map[string]string{".gitignore": "/lab/bin/\n/lab/forgecell\n"})
		commitAll(t, checkout, "ignore build output")
		writeTree(t, checkout, map[string]string{"lab/forgecell": "binary", "lab/bin/tool": "binary"})
		r, err := DevInstall(context.Background(), devOptions(t, checkout))
		if err != nil || r.Version != "0.2.0-dev."+revOf(t, checkout) {
			t.Fatalf("%+v %v", r, err)
		}
	})
}

func TestDevInstallDirtyVersionsCarryAContentHash(t *testing.T) {
	checkout := devCheckout(t)
	o := devOptions(t, checkout)
	o.AllowDirty = true
	pattern := dirtyVersion(revOf(t, checkout))
	versions := map[string]string{}
	for _, content := range []string{"one", "two", "one"} {
		writeTree(t, checkout, map[string]string{"scratch.txt": content})
		r, err := DevInstall(context.Background(), o)
		if err != nil {
			t.Fatalf("%q: %v", content, err)
		}
		if !pattern.MatchString(r.Version) {
			t.Fatalf("%q: %s", content, r.Version)
		}
		if prev, seen := versions[content]; seen && prev != r.Version {
			t.Fatalf("the same contents must give the same version: %s vs %s", prev, r.Version)
		}
		versions[content] = r.Version
	}
	if versions["one"] == versions["two"] {
		t.Fatalf("different uncommitted contents share %s", versions["one"])
	}
	if got := releaseNames(t, o.Home); len(got) != 2 {
		t.Fatalf("%v", got)
	}
	assertNoInstallLeftovers(t, o.Home)
}

func TestDevInstallFailuresStageNothing(t *testing.T) {
	notGit := t.TempDir()
	writeTree(t, notGit, map[string]string{"lab/go.mod": "module fakeforgecell\n\ngo 1.21\n", "lab/cmd/forgecell/main.go": fakeMainFor(harness.AdapterProtocolVersion)})
	noCommand := t.TempDir()
	writeTree(t, noCommand, map[string]string{"README.md": "x"})
	gitRun(t, noCommand, "init", "-q")
	commitAll(t, noCommand, "initial")
	brokenSource := devCheckout(t)
	writeTree(t, brokenSource, map[string]string{"lab/cmd/forgecell/main.go": "package main\n\nfunc main() { this does not compile }\n"})
	commitAll(t, brokenSource, "break")
	for _, c := range []struct {
		name     string
		checkout string
		build    BuildFunc
		want     string
	}{
		{"not a git repository", notGit, fakeBuild("one"), "git repository"},
		{"no forgecell command", noCommand, fakeBuild("one"), "lab/cmd/forgecell"},
		{"failing builder", devCheckout(t), failingBuild, "compiler exploded"},
		{"go build fails", brokenSource, nil, "go build failed"},
		{"executable without a protocol report", devCheckout(t), noProtocolBuild, "adapter protocol"},
		{"executable reports another version", devCheckout(t), func(_ context.Context, _, _, output string) error {
			return os.WriteFile(output, []byte("#!/bin/sh\nprintf 'wrong\\n'\n"), 0755)
		}, "smoke check failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := devOptions(t, c.checkout)
			o.Build = c.build
			installTwo(t, o)
			before := linkTargets(t, o.Home)
			for _, activate := range []bool{false, true} {
				o.Activate = activate
				if _, err := DevInstall(context.Background(), o); err == nil || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("activate=%v: want %q, got %v", activate, c.want, err)
				}
				if got := releaseNames(t, o.Home); !reflect.DeepEqual(got, []string{"0.1.0", "0.2.0"}) {
					t.Fatalf("a partial release remains: %v", got)
				}
				if !reflect.DeepEqual(before, linkTargets(t, o.Home)) {
					t.Fatalf("links changed: %v", linkTargets(t, o.Home))
				}
				assertNoInstallLeftovers(t, o.Home)
			}
		})
	}
}

func TestDevInstallAbortsWhenTheCheckoutChangesDuringTheBuild(t *testing.T) {
	for _, activate := range []bool{false, true} {
		for _, c := range []string{"a new file appears", "a commit moves HEAD"} {
			t.Run(c, func(t *testing.T) {
				checkout := devCheckout(t)
				o := devOptions(t, checkout)
				o.Activate = activate
				installTwo(t, o)
				before := linkTargets(t, o.Home)
				o.Build = func(ctx context.Context, root, version, output string) error {
					writeTree(t, root, map[string]string{"late.txt": "changed"})
					if c == "a commit moves HEAD" {
						commitAll(t, root, "late")
					}
					return fakeBuild("one")(ctx, root, version, output)
				}
				if _, err := DevInstall(context.Background(), o); err == nil || !strings.Contains(err.Error(), "changed while it was being built") {
					t.Fatalf("activate=%v: %v", activate, err)
				}
				if got := releaseNames(t, o.Home); !reflect.DeepEqual(got, []string{"0.1.0", "0.2.0"}) || !reflect.DeepEqual(before, linkTargets(t, o.Home)) {
					t.Fatalf("%v %v", got, linkTargets(t, o.Home))
				}
				assertNoInstallLeftovers(t, o.Home)
			})
		}
	}
}

func TestDevInstallDoesNotRunTheCheckoutsFsmonitor(t *testing.T) {
	checkout := devCheckout(t)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho ran >> '"+marker+"'\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, checkout, "config", "core.fsmonitor", hook)
	plain := exec.Command("git", "status", "--porcelain")
	plain.Dir, plain.Env = checkout, gitEnv()
	_ = plain.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Skip("this git does not run a core.fsmonitor command, so there is nothing to prove")
	}
	os.Remove(marker)
	if _, err := DevInstall(context.Background(), devOptions(t, checkout)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the checkout's fsmonitor command ran")
	}
}

func readDevBuild(t *testing.T, release string) (formula.DevBuild, bool) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(release), formula.DevBuildFile))
	if os.IsNotExist(err) {
		return formula.DevBuild{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var record formula.DevBuild
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return record, true
}

func TestDevInstallRecordsTheSourceCommitTime(t *testing.T) {
	checkout := devCheckout(t)
	o := devOptions(t, checkout)
	want, err := strconv.ParseInt(gitRun(t, checkout, "log", "-1", "--format=%ct", "HEAD"), 10, 64)
	if err != nil || want <= 0 {
		t.Fatalf("fixture commit time: %v", err)
	}
	r, err := DevInstall(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	record, found := readDevBuild(t, r.Path)
	if !found || record.Version != r.Version || record.CommitTime != want {
		t.Fatalf("dev build record %+v (found %v), want %s at %d", record, found, r.Version, want)
	}
	if st, err := os.Lstat(filepath.Join(filepath.Dir(r.Path), formula.DevBuildFile)); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("record mode: %v %v", st, err)
	}
	// The manifest the harness reads strictly is unchanged by the record.
	if m := harness.ReadManifest(filepath.Dir(r.Path)); !m.Found || m.Digest != r.ManifestSHA256 {
		t.Fatalf("%+v", m)
	}
	entries, err := os.ReadDir(filepath.Dir(r.Path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("recording left %s behind", e.Name())
		}
	}
	assertNoInstallLeftovers(t, o.Home)
}

func TestDevInstallRecordsAMissingCommitTimeAsAbsent(t *testing.T) {
	previous := headCommitTime
	headCommitTime = func(context.Context, string) int64 { return 0 }
	t.Cleanup(func() { headCommitTime = previous })
	o := devOptions(t, devCheckout(t))
	r, err := DevInstall(context.Background(), o)
	if err != nil {
		t.Fatalf("a missing commit time must not fail the install: %v", err)
	}
	if record, found := readDevBuild(t, r.Path); found {
		t.Fatalf("a missing commit time was recorded: %+v", record)
	}
	for _, out := range []string{"", "\n", "abc", "-5", "0", "12.5", "1700000000 1", "99999999999999", "32503680001"} {
		if got := parseCommitTime(out); got != 0 {
			t.Errorf("parseCommitTime(%q) = %d, want absent", out, got)
		}
	}
	if got := parseCommitTime("1700000000\n"); got != 1700000000 {
		t.Errorf("parseCommitTime = %d", got)
	}
}

func TestDevInstallRefusesAnUnmanagedDevBuildRecord(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, formula.DevBuildFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := recordDevBuild(dir, "0.2.0-dev.abcd", 1700000000); err == nil {
		t.Fatal("a directory in the record's place was replaced")
	}
}

func TestDevInstallIsIdempotentAndRefusesDivergentRebuilds(t *testing.T) {
	checkout := devCheckout(t)
	o := devOptions(t, checkout)
	first, err := DevInstall(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(first.Path)
	exe, _ := os.ReadFile(first.Path)
	second, err := DevInstall(context.Background(), o)
	if err != nil || second != first {
		t.Fatalf("reinstalling identical content: %+v %v", second, err)
	}
	if again, _ := os.ReadFile(first.Path); string(again) != string(exe) {
		t.Fatal("release changed")
	}

	writeTree(t, checkout, map[string]string{"scratch.txt": "wip"})
	o.AllowDirty = true
	dirty, err := DevInstall(context.Background(), o)
	if err != nil || !dirtyVersion(revOf(t, checkout)).MatchString(dirty.Version) {
		t.Fatalf("%+v %v", dirty, err)
	}
	before, _ := os.ReadFile(dirty.Path)
	o.Build = fakeBuild("two")
	if _, err = DevInstall(context.Background(), o); err == nil || !strings.Contains(err.Error(), "already exists with different content") {
		t.Fatalf("a different build under the same version was accepted: %v", err)
	}
	if after, _ := os.ReadFile(dirty.Path); string(after) != string(before) {
		t.Fatal("existing release was replaced")
	}
	if got := releaseNames(t, o.Home); len(got) != 2 || filepath.Base(dir) != first.Version {
		t.Fatalf("%v", got)
	}
	assertNoInstallLeftovers(t, o.Home)
}
