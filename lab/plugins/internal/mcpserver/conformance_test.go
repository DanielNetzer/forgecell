package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/pinned"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

// The test binary doubles as the stub forge plugin: a copy of it, launched by
// the conformance kit with the manifest's --kind forge --mode MODE arguments and
// stubEnv set through needs.env, serves the stub instead of running tests.
const (
	stubEnv   = "MCPSERVER_CONFORMANCE_STUB"
	stubValue = "serve"
)

func TestMain(m *testing.M) {
	if mode, ok := stubMode(os.Args[1:]); ok {
		os.Exit(serveStub(mode))
	}
	os.Exit(m.Run())
}

func stubMode(args []string) (string, bool) {
	if value, _ := Env(stubEnv); value != stubValue {
		return "", false
	}
	if len(args) != 4 || args[0] != "--kind" || args[1] != "forge" || args[2] != "--mode" {
		return "", false
	}
	return args[3], true
}

const commit = "0123456789abcdef0123456789abcdef01234567"

// stubOutputs are canned outputs that are valid under forge.v1.json.
var stubOutputs = map[string]map[string]any{
	"forge_describe":      {"identity": "forge.example/example/project", "host": "forge.example", "displayName": "Stub forge", "changeNoun": "change request", "defaultBranch": "main", "ciKind": "none", "webURL": "https://forge.example/example/project"},
	"forge_policy":        {"requiredChecks": []string{}, "requiredReviews": 0, "protected": false},
	"forge_find_change":   {"changes": []any{}},
	"forge_publish_draft": {"ref": "!1", "url": "https://forge.example/1", "draft": true, "headCommit": commit},
	"forge_change":        {"state": "open", "draft": true, "headCommit": commit, "url": "https://forge.example/1"},
	"forge_checks":        {"checks": []any{}, "complete": true},
	"forge_comments":      {"comments": []any{}},
}

// serveStub registers every forge contract tool with its contract input schema
// and output bound (the missing-tool mode leaves forge_comments out) and serves
// stdio.
func serveStub(mode string) int {
	c, _ := contract.Lookup("forge")
	s := New("stub-forge", "1", c.ID)
	for i := range c.Tools {
		t := &c.Tools[i]
		if mode == "missing-tool" && t.Name == "forge_comments" {
			continue
		}
		s.Tool(t.Name, t.InputSchema(), func(_ context.Context, args json.RawMessage) (any, error) {
			if err := t.ValidateInputJSON(args); err != nil {
				return nil, Fail(Failed, "the arguments do not match the input schema")
			}
			return stubOutputs[t.Name], nil
		}, MaxOutputBytes(t.MaxOutputBytes))
	}
	s.Log("stub forge serving")
	if err := s.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
		return 1
	}
	return 0
}

func fired() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

// hangGuard bounds the handshake and every tool call. It only stops a hung
// server failing the test well inside go test's own timeout; a loaded machine
// never reaches it, so no outcome depends on speed.
const hangGuard = 5 * time.Minute

// runStub copies the test binary into the test's temporary directory, pins it
// in a forge manifest for mode and runs the conformance kit against it.
func runStub(t *testing.T, mode string) conformance.Report {
	t.Helper()
	if testing.Short() {
		t.Skip("launches the stub plugin as a process")
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("plugins run only on Linux and macOS")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(plugintest.TempDir(t), "bin")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stub")
	if err := os.WriteFile(path, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	server := pinned.File{Path: path, SHA256: hex.EncodeToString(sum[:])}
	manifest := plugintest.WriteManifest(t, plugintest.Manifest(t, server, "forge", mode, stubEnv))
	root := plugintest.TempDir(t)
	lookup := func(name string) (string, bool) {
		if name == stubEnv {
			return stubValue, true
		}
		return "", false
	}
	r := conformance.Run(context.Background(), manifest, conformance.Options{Root: root, LookupEnv: lookup, Grace: fired, Handshake: hangGuard, Timeout: hangGuard})
	if left, err := os.ReadDir(root); err != nil || len(left) != 0 {
		t.Fatalf("the run left %d entries behind (%v)", len(left), err)
	}
	return r
}

func TestTheStubForgePassesContractConformance(t *testing.T) {
	r := runStub(t, "good")
	if !r.Passed || r.Contract != "forgecell.forge/v1" || r.Kind != "forge" {
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

func TestAStubMissingAToolFailsToolsList(t *testing.T) {
	r := runStub(t, "missing-tool")
	failed, ok := r.Failed()
	if r.Passed || !ok || failed.ID != conformance.CheckToolsList || failed.Status != conformance.Fail || failed.Reason != string(mcp.ContractMismatch) {
		t.Fatalf("first failure %+v, want %s %s", failed, conformance.CheckToolsList, mcp.ContractMismatch)
	}
}
