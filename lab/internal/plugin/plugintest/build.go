// Package plugintest builds the in-repo fake MCP server for tests and writes
// plugin manifests that point at it. Only _test.go files import it.
package plugintest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

const fakeImportPath = "github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest/fakeserver"

// ReadyMarker is the line the fake server writes to stderr once it serves.
const ReadyMarker = "fake-mcp-ready"

// PlantedSecret is the credential the fake server writes to stderr.
const PlantedSecret = "fakefakefakefake0123456789"

var (
	managed  bool
	once     sync.Once
	shared   string
	built    string
	buildErr error
)

// Main runs the tests of a package so that the fake server is built once for
// all of them, then removes the directory that build used. Call it from
// TestMain. Without it, each Server call builds into its own test's
// temporary directory.
func Main(m *testing.M) int {
	managed = true
	code := m.Run()
	if shared != "" {
		os.RemoveAll(shared)
	}
	return code
}

// build compiles the fake server into dir. GOCACHE comes from the environment
// when set; otherwise the build uses a cache inside dir, so nothing is written
// to the user's own cache. The network and toolchain downloads are off.
func build(dir string) (string, error) {
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(goTool); err != nil {
		if goTool, err = exec.LookPath("go"); err != nil {
			return "", errors.New("no go toolchain")
		}
	}
	out := filepath.Join(dir, "fakeserver")
	cmd := exec.Command(goTool, "build", "-trimpath", "-o", out, fakeImportPath)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=")
	if os.Getenv("GOCACHE") == "" {
		cmd.Env = append(cmd.Env, "GOCACHE="+filepath.Join(dir, "gocache"))
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s", err, output)
	}
	return out, nil
}

// Server returns a private copy of the fake server under the test's temporary
// directory, pinned by its digest, so a test may tamper with its own copy. It
// skips under -short, because it needs go build.
func Server(t testing.TB) pinned.File {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the fake MCP server with go build")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run only on Linux and macOS")
	}
	binary := ""
	if managed {
		once.Do(func() {
			if shared, buildErr = os.MkdirTemp("", "forgecell-fake-mcp-"); buildErr == nil {
				built, buildErr = build(shared)
			}
		})
		binary = built
	} else {
		var err error
		if binary, err = build(TempDir(t)); err != nil {
			buildErr = err
		}
	}
	if binary == "" {
		t.Fatalf("building the fake MCP server failed: %v", buildErr)
	}
	raw, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(TempDir(t), "bin")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fake")
	if err := os.WriteFile(path, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return pinned.File{Path: path, SHA256: hex.EncodeToString(sum[:])}
}

// TempDir returns the test's temporary directory with symlinks resolved, as
// pinned placement checks require.
func TempDir(t testing.TB) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Manifest returns a plugin manifest for the fake server serving kind in mode,
// listing every contract tool with its access and the given needs.env names.
func Manifest(t testing.TB, server pinned.File, kind, mode string, env ...string) []byte {
	t.Helper()
	c, ok := contract.Lookup(kind)
	if !ok {
		t.Fatalf("no contract for %s", kind)
	}
	tools := []map[string]string{}
	for _, tool := range c.Tools {
		access := "read-only"
		if tool.Access == contract.Write {
			access = "read-write"
		}
		tools = append(tools, map[string]string{"name": tool.Name, "access": access})
	}
	if env == nil {
		env = []string{}
	}
	raw, err := json.MarshalIndent(map[string]any{
		"schemaVersion": "v1",
		"kind":          kind,
		"name":          "fake-" + kind,
		"contract":      c.ID,
		"server":        map[string]any{"executable": server, "args": []string{"--kind", kind, "--mode", mode}},
		"tools":         tools,
		"roles":         []string{},
		"needs":         map[string]any{"env": env, "network": []string{}, "minPollSeconds": 60},
		"dependencies":  []any{},
		"limitations":   "Test fake; serves canned outputs.",
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// WriteManifest writes a manifest into a fresh temporary directory and returns
// its absolute path.
func WriteManifest(t testing.TB, raw []byte) string {
	t.Helper()
	path := filepath.Join(TempDir(t), "plugin.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
