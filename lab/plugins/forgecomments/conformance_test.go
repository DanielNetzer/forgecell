package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const pluginImportPath = "github.com/DanielNetzer/forgecell/lab/plugins/forgecomments"

// hangGuard bounds the handshake and every tool call. It only stops a hung
// plugin failing the test well inside go test's own timeout; a loaded machine
// never reaches it, so no outcome depends on speed.
const hangGuard = 5 * time.Minute

func fired() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

// noEnv is the plugin's environment lookup: it reads nothing.
func noEnv(string) (string, bool) { return "", false }

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
	out := filepath.Join(dir, "forgecell-forgecomments")
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

// renderManifest fills plugin.json.tmpl for a server binary.
func renderManifest(t *testing.T, server string) []byte {
	t.Helper()
	tmpl, err := os.ReadFile("plugin.json.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(server)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	quoted, _ := json.Marshal(server)
	out := strings.ReplaceAll(string(tmpl), `"{{SERVER_PATH}}"`, string(quoted))
	return []byte(strings.ReplaceAll(out, "{{SERVER_SHA256}}", hex.EncodeToString(sum[:])))
}

// TestTheForgeCommentsPluginPassesContractConformance builds the plugin, pins
// it in a manifest rendered from plugin.json.tmpl and runs the contract-mode
// kit with an empty environment.
func TestTheForgeCommentsPluginPassesContractConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the plugin with go build and launches it as a process")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run only on Linux and macOS")
	}
	server := buildPlugin(t)
	manifest := plugintest.WriteManifest(t, renderManifest(t, server.Path))
	root := plugintest.TempDir(t)
	r := conformance.Run(context.Background(), manifest, conformance.Options{Root: root, LookupEnv: noEnv, Grace: fired, Handshake: hangGuard, Timeout: hangGuard})
	if left, err := os.ReadDir(root); err != nil || len(left) != 0 {
		t.Fatalf("the run left %d entries behind (%v)", len(left), err)
	}
	if !r.Passed || r.Contract != "forgecell.reviewer/v1" || r.Kind != "reviewer" || r.Plugin != "forgecomments" {
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
}

func TestTheManifestTemplateDeclaresTheContract(t *testing.T) {
	server := filepath.Join(plugintest.TempDir(t), "forgecell-forgecomments")
	if err := os.WriteFile(server, []byte("stand-in"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := plugin.Decode(renderManifest(t, server))
	if err != nil {
		t.Fatalf("the rendered template is not a valid manifest: %v", err)
	}
	if m.Kind != "reviewer" || m.Contract != "forgecell.reviewer/v1" || m.Name != "forgecomments" || len(m.Server.Args) != 0 {
		t.Fatalf("manifest %+v", m)
	}
	if len(m.Needs.Env) != 0 || len(m.Needs.Network) != 0 || len(m.Roles) != 0 || m.Needs.MinPollSeconds != 60 {
		t.Fatalf("needs %+v, roles %v", m.Needs, m.Roles)
	}
	c, _ := contract.Lookup("reviewer")
	if len(m.Tools) != len(c.Tools) {
		t.Fatalf("tools %+v", m.Tools)
	}
	for i, tool := range c.Tools {
		if m.Tools[i].Name != tool.Name || m.Tools[i].Access != plugin.AccessFor(tool.Access) || m.Tools[i].Access != "read-only" {
			t.Fatalf("tool %+v, want %s read-only", m.Tools[i], tool.Name)
		}
	}
}
