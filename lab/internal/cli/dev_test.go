package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

const devFakeProtocol = "fake-protocol-9"

func devFakeMainFor(protocol string) string {
	return "package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nvar version = \"dev-go\"\n\nfunc main() {\n\tif len(os.Args) > 1 && os.Args[1] == \"--protocol\" {\n\t\tfmt.Println(\"" + protocol + "\")\n\t\treturn\n\t}\n\tfmt.Println(version)\n}\n"
}

func devGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=forgecell-test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func devFakeCheckout(t *testing.T, protocol string) (dir, rev string) {
	t.Helper()
	dir = t.TempDir()
	for name, content := range map[string]string{"lab/go.mod": "module fakeforgecell\n\ngo 1.21\n", "lab/cmd/forgecell/main.go": devFakeMainFor(protocol)} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	devGit(t, dir, "init", "-q")
	devGit(t, dir, "add", "-A")
	devGit(t, dir, "commit", "-q", "-m", "initial")
	return dir, devGit(t, dir, "rev-parse", "--short", "HEAD")
}

func devEnv(t *testing.T) (home, bin string) {
	t.Helper()
	home, bin = t.TempDir(), filepath.Join(t.TempDir(), "bin")
	t.Setenv("FORGECELL_HOME", home)
	t.Setenv("FORGECELL_BIN", bin)
	return home, bin
}

func runDevCLI(args ...string) (code int, out, stderr string) {
	var o, e bytes.Buffer
	code = Run(context.Background(), args, strings.NewReader(""), &o, &e, "0.2.0-preview.2")
	return code, o.String(), e.String()
}

func TestDevInstallStagesAndReportsJSON(t *testing.T) {
	checkout, rev := devFakeCheckout(t, harness.AdapterProtocolVersion)
	home, bin := devEnv(t)
	code, out, stderr := runDevCLI("dev", "install", "--from", checkout)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var got map[string]string
	if err := dec.Decode(&got); err != nil || dec.More() || len(got) != 3 {
		t.Fatalf("stdout must be one JSON object with three keys: %q %v", out, err)
	}
	version := "0.2.0-dev." + rev
	realHome, _ := filepath.EvalSymlinks(home)
	path := filepath.Join(realHome, "releases", version, "forgecell")
	if got["version"] != version || got["path"] != path || got["manifestSha256"] != harness.ReadManifest(filepath.Dir(path)).Digest || got["manifestSha256"] == "" {
		t.Fatalf("%v", got)
	}
	built, err := exec.Command(path, "--version").Output()
	if err != nil || strings.TrimSpace(string(built)) != version {
		t.Fatalf("the build must report its stamped version: %q %v", built, err)
	}
	if line := strings.TrimSpace(stderr); strings.Contains(line, "\n") || !strings.Contains(line, version) || !strings.Contains(line, "staged only") {
		t.Fatalf("stderr: %q", stderr)
	}
	if _, err := os.Lstat(filepath.Join(home, "current")); !os.IsNotExist(err) {
		t.Fatal("staging activated the build")
	}
	if _, err := os.Lstat(bin); !os.IsNotExist(err) {
		t.Fatal("staging created the command directory")
	}
}

func TestDevInstallActivateMakesTheBuildCurrentAndLauncherCompatible(t *testing.T) {
	checkout, rev := devFakeCheckout(t, harness.AdapterProtocolVersion)
	home, bin := devEnv(t)
	code, out, stderr := runDevCLI("dev", "install", "--from", checkout, "--activate")
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	version := "0.2.0-dev." + rev
	if !strings.Contains(out, version) || !strings.Contains(stderr, "activated") {
		t.Fatalf("%q %q", out, stderr)
	}
	if target, err := os.Readlink(filepath.Join(home, "current")); err != nil || filepath.Base(target) != version {
		t.Fatalf("%s %v", target, err)
	}
	id := harness.ResolveLauncher(filepath.Join(bin, "forgecell"))
	if id.Class != harness.LauncherCompatible || !id.Managed || id.Manifest.Version != version {
		t.Fatalf("an installed dev build must never read as launcher-unknown: %+v", id)
	}
}

func TestDevInstallDefaultsToTheWorkingDirectoryAndRefusesDirtyTrees(t *testing.T) {
	checkout, rev := devFakeCheckout(t, harness.AdapterProtocolVersion)
	home, _ := devEnv(t)
	if err := os.WriteFile(filepath.Join(checkout, "scratch.txt"), []byte("wip"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(checkout)
	code, out, stderr := runDevCLI("dev", "install")
	if code != 1 || out != "" || !strings.HasPrefix(stderr, "forgecell:") || !strings.Contains(stderr, "--allow-dirty") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
	if _, err := os.Lstat(filepath.Join(home, "releases")); !os.IsNotExist(err) {
		t.Fatal("a refused build left a release")
	}
	code, out, stderr = runDevCLI("dev", "install", "--allow-dirty")
	if code != 0 || !strings.Contains(out, "0.2.0-dev."+rev+".dirty.") {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
}

func TestDevInstallRecordsTheCheckoutsProtocolNotTheRunningOne(t *testing.T) {
	checkout, _ := devFakeCheckout(t, devFakeProtocol)
	home, _ := devEnv(t)
	code, out, stderr := runDevCLI("dev", "install", "--from", checkout)
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(got["path"]), "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Version         string `json:"version"`
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.ProtocolVersion != devFakeProtocol || meta.ProtocolVersion == harness.AdapterProtocolVersion || meta.Version != got["version"] {
		t.Fatalf("%+v", meta)
	}
	if _, err := os.Lstat(filepath.Join(home, "current")); !os.IsNotExist(err) {
		t.Fatal("staging activated the build")
	}
}

func TestDevProtocolFlagReportsTheAdapterProtocol(t *testing.T) {
	code, out, stderr := runDevCLI("--protocol")
	if code != 0 || strings.TrimSpace(out) != harness.AdapterProtocolVersion || stderr != "" {
		t.Fatalf("%d %q %q", code, out, stderr)
	}
}

func TestDevCommandRejectsBadUsage(t *testing.T) {
	home, _ := devEnv(t)
	for _, args := range [][]string{
		{"dev"},
		{"dev", "bogus"},
		{"dev", "install", "extra"},
		{"dev", "install", "--from"},
		{"dev", "install", "--nope"},
	} {
		code, out, stderr := runDevCLI(args...)
		if code != 1 || out != "" || !strings.HasPrefix(stderr, "forgecell:") {
			t.Errorf("%v: %d %q %q", args, code, out, stderr)
		}
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatalf("rejected usage wrote to the home: %v", entries)
	}
}

func TestDevHelpListsInstall(t *testing.T) {
	_, out, _ := runDevCLI("--help")
	if !strings.Contains(out, "dev install [--from CHECKOUT] [--activate] [--allow-dirty]") || !strings.Contains(out, "the checkout is trusted code") {
		t.Fatal(out)
	}
}
