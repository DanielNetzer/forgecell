package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const pluginImportPath = "github.com/DanielNetzer/forgecell/lab/plugins/gitlab"

// hangGuard bounds the handshake and every tool call. It only stops a hung
// plugin failing the test well inside go test's own timeout; a loaded machine
// never reaches it, so no outcome depends on speed.
const hangGuard = 5 * time.Minute

func fired() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

// buildPlugin compiles the plugin into a private bin directory under the test's
// temporary directory and pins it by its digest. GOCACHE comes from the
// environment when set; otherwise the build uses a cache inside the test's
// temporary directory, so nothing is written to the user's own cache. The
// network and toolchain downloads are off.
func buildPlugin(t *testing.T) pinned.File {
	t.Helper()
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goTool); err != nil {
		if goTool, err = exec.LookPath("go"); err != nil {
			t.Fatal(errors.New("no go toolchain"))
		}
	}
	dir := filepath.Join(plugintest.TempDir(t), "bin")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "forgecell-gitlab")
	cmd := exec.Command(goTool, "build", "-trimpath", "-o", out, pluginImportPath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=")
	if os.Getenv("GOCACHE") == "" {
		cmd.Env = append(cmd.Env, "GOCACHE="+filepath.Join(plugintest.TempDir(t), "gocache"))
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the plugin failed: %v: %s", err, output)
	}
	if err := os.Chmod(out, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return pinned.File{Path: out, SHA256: hex.EncodeToString(sum[:])}
}

// TestTheGitLabPluginPassesContractConformance builds the plugin, pins it in a
// manifest rendered from plugin.json.tmpl and runs the contract-mode kit with
// GITLAB_URL pointed at the fake and GITLAB_CA_FILE trusting its certificate.
func TestTheGitLabPluginPassesContractConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin with go build and launches it as a process")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run only on Linux and macOS")
	}
	fake := newFakeGitLab(t)
	env := fake.env(t)
	server := buildPlugin(t)
	manifest := plugintest.WriteManifest(t, renderManifest(t, server.Path, "gitlab.com", "127.0.0.1"))
	root := plugintest.TempDir(t)
	r := conformance.Run(context.Background(), manifest, conformance.Options{Root: root, LookupEnv: lookupFrom(env), Grace: fired, Handshake: hangGuard, Timeout: hangGuard})
	if left, err := os.ReadDir(root); err != nil || len(left) != 0 {
		t.Fatalf("the run left %d entries behind (%v)", len(left), err)
	}
	if !r.Passed || r.Contract != "forgecell.forge/v1" || r.Kind != "forge" || r.Plugin != "gitlab" {
		t.Fatalf("report: %+v", r)
	}
	want := []string{conformance.CheckStart, conformance.CheckInitialize, conformance.CheckToolsList, conformance.CheckDescribe, conformance.CheckDeterministic, conformance.CheckMalformed, conformance.CheckOutputBound, conformance.CheckTimeoutBound}
	if len(r.Checks) != len(want) {
		t.Fatalf("checks: %+v", r.Checks)
	}
	for i, c := range r.Checks {
		if c.ID != want[i] || c.Status != conformance.Pass {
			t.Fatalf("check %d: %+v, want %s to pass", i, c, want[i])
		}
	}
	// Describe ran against the fake, with the token as PRIVATE-TOKEN.
	described := 0
	for _, s := range fake.seen() {
		if s.Path == "/api/v4/projects/"+projectKey {
			described++
		}
		if s.Token != fakeToken {
			t.Fatalf("request %+v did not carry the token", s)
		}
	}
	if described < 2 {
		t.Fatalf("%d describe requests reached the fake", described)
	}
}
