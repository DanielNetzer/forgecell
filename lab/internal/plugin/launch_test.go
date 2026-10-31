package plugin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

func TestMain(m *testing.M) { os.Exit(plugintest.Main(m)) }

// fired is a grace channel that has already fired.
func fired() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

func startFake(t *testing.T, kind, mode string, env map[string]string, needsEnv ...string) (*Process, string) {
	t.Helper()
	server := plugintest.Server(t)
	m, err := Decode(plugintest.Manifest(t, server, kind, mode, needsEnv...))
	if err != nil {
		t.Fatal(err)
	}
	root := plugintest.TempDir(t)
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }
	p, err := Start(context.Background(), m, Options{Root: root, LookupEnv: lookup})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Shutdown(fired()) })
	return p, root
}

func entries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestStartScrubsTheEnvironmentAndRemovesItsHome(t *testing.T) {
	env := map[string]string{"PATH": os.Getenv("PATH"), "LANG": "C.UTF-8", "FAKE_TOKEN": "token-value-123", "UNDECLARED": "x"}
	p, root := startFake(t, "tracker", "good", env, "FAKE_TOKEN", "UNSET_TOKEN")
	if len(entries(t, root)) != 1 {
		t.Fatal("the start did not create one private directory")
	}
	if _, err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(nil); err != nil {
		t.Fatal(err)
	}
	if p.cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("a good server did not exit on its own at stdin EOF: %v", p.cmd.ProcessState)
	}
	diag := p.Diagnostics()
	for _, want := range []string{plugintest.ReadyMarker, "fake-env: FAKE_TOKEN,HOME,LANG,PATH\n", "fake-home: dir=true cwd=true", "fake: stdin closed", "[redacted]"} {
		if !strings.Contains(diag, want) {
			t.Fatalf("diagnostics lack %q:\n%s", want, diag)
		}
	}
	if strings.Contains(diag, plugintest.PlantedSecret) || strings.Contains(diag, root) {
		t.Fatalf("diagnostics leak a secret or a local path:\n%s", diag)
	}
	if left := entries(t, root); len(left) != 0 {
		t.Fatalf("the private directory was left behind: %v", left)
	}
}

func TestStartRefusesATamperedServer(t *testing.T) {
	server := plugintest.Server(t)
	m, err := Decode(plugintest.Manifest(t, server, "forge", "good"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server.Path, []byte("#!/bin/sh\necho tampered\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root := plugintest.TempDir(t)
	_, err = Start(context.Background(), m, Options{Root: root})
	if mcp.ReasonOf(err) != mcp.ManifestChanged || err.Error() != "plugin: manifest-changed" {
		t.Fatalf("Start = %v, want manifest-changed", err)
	}
	if len(entries(t, root)) != 0 {
		t.Fatal("a directory was created for a refused start")
	}
	os.Remove(server.Path)
	if _, err = Start(context.Background(), m, Options{Root: root}); mcp.ReasonOf(err) != mcp.NotInstalled {
		t.Fatalf("Start of a missing server = %v", err)
	}
}

func TestStartRefusesAnUnusableRoot(t *testing.T) {
	server := plugintest.Server(t)
	m, err := Decode(plugintest.Manifest(t, server, "forge", "good"))
	if err != nil {
		t.Fatal(err)
	}
	dir := plugintest.TempDir(t)
	for _, root := range []string{"", "relative", filepath.Join(dir, "missing"), dir + "/./x"} {
		if _, err := Start(context.Background(), m, Options{Root: root}); mcp.ReasonOf(err) != mcp.Failed {
			t.Errorf("root %q: %v", root, err)
		}
	}
}

func TestShutdownKillsAServerThatIgnoresEOF(t *testing.T) {
	p, root := startFake(t, "reviewer", "slow", nil)
	if _, err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, _ := contract.Lookup("reviewer")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Call(ctx, c.Describe(), map[string]any{"config": map[string]any{}}, time.Hour); mcp.ReasonOf(err) != mcp.Timeout {
		t.Fatalf("Call on an expired context = %v", err)
	}
	if err := p.Shutdown(fired()); err != nil {
		t.Fatal(err)
	}
	if !p.Exited() || p.cmd.ProcessState.ExitCode() != -1 {
		t.Fatalf("the server was not killed: %v", p.cmd.ProcessState)
	}
	if len(entries(t, root)) != 0 {
		t.Fatal("the private directory was left behind")
	}
}

func TestInitializeRefusesAnotherContract(t *testing.T) {
	p, _ := startFake(t, "forge", "wrong-contract", nil)
	info, err := p.Initialize(context.Background())
	if mcp.ReasonOf(err) != mcp.ContractMismatch || info.Contract != "forgecell.forge/v2" {
		t.Fatalf("Initialize = %+v, %v", info, err)
	}
}

func TestCallDecodesContractOutput(t *testing.T) {
	p, _ := startFake(t, "forge", "good", nil)
	if _, err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, _ := contract.Lookup("forge")
	got, err := p.Call(context.Background(), c.Describe(), map[string]any{"project": "example/project"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := got.(contract.ForgeDescription); !ok || d.ChangeNoun != "change request" {
		t.Fatalf("Call = %#v", got)
	}
}

func TestRemoveRunDirRemovesOnlyMarkedDirectories(t *testing.T) {
	base := plugintest.TempDir(t)
	unmarked := filepath.Join(base, runPrefix+"unmarked")
	if err := os.Mkdir(unmarked, 0o700); err != nil {
		t.Fatal(err)
	}
	if removeRunDir(unmarked) == nil {
		t.Fatal("an unmarked directory was removed")
	}
	target, err := makeRunDir(base)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, runPrefix+"link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if removeRunDir(link) == nil {
		t.Fatal("a symlink was followed")
	}
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, runMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if removeRunDir(other) == nil {
		t.Fatal("a directory without the run prefix was removed")
	}
	linkedMarker := filepath.Join(base, runPrefix+"linked-marker")
	if err := os.Mkdir(linkedMarker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(target, runMarker), filepath.Join(linkedMarker, runMarker)); err != nil {
		t.Fatal(err)
	}
	if removeRunDir(linkedMarker) == nil {
		t.Fatal("a symlinked marker was accepted")
	}
	for _, kept := range []string{unmarked, target, other, linkedMarker} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("%s was removed", filepath.Base(kept))
		}
	}
	if err := removeRunDir(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("a marked directory was kept")
	}
}

func TestCaptureIsBoundedAndDiagnosticsRedact(t *testing.T) {
	c := &capture{done: make(chan struct{})}
	c.run(strings.NewReader(strings.Repeat("x", maxStderrBytes) + " GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz" + strings.Repeat("y", 100<<10)))
	raw, dropped := c.snapshot()
	if len(raw) != maxStderrBytes+stderrLookahead || dropped == 0 {
		t.Fatalf("kept %d bytes, dropped %d", len(raw), dropped)
	}
	p := &Process{stderr: c, dir: "/nonexistent/run", secrets: []string{"declared-value"}}
	diag := p.Diagnostics()
	if strings.Contains(diag, "ghp_") || !strings.HasSuffix(diag, "[stderr truncated after 65536 bytes]") {
		t.Fatalf("diagnostics tail: %q", diag[len(diag)-120:])
	}
	c = &capture{done: make(chan struct{})}
	c.run(strings.NewReader("at /nonexistent/run/home: declared-value\x1b[31m\n"))
	p = &Process{stderr: c, dir: "/nonexistent/run", secrets: []string{"declared-value"}}
	if diag := p.Diagnostics(); diag != "at <plugin-dir>/home: [redacted] [31m" {
		t.Fatalf("diagnostics = %q", diag)
	}
}

func TestEnvironmentIsExact(t *testing.T) {
	lookup := func(name string) (string, bool) {
		v, ok := map[string]string{"A_TOKEN": "abcd", "SHORT": "x", "OTHER": "o"}[name]
		return v, ok
	}
	env, secrets := environment([]string{"A_TOKEN", "SHORT", "MISSING"}, "/h", lookup)
	if strings.Join(env, " ") != "PATH="+defaultPath+" HOME=/h LANG=C A_TOKEN=abcd SHORT=x" {
		t.Fatalf("environment = %v", env)
	}
	if len(secrets) != 1 || secrets[0] != "abcd" {
		t.Fatalf("secrets = %v", secrets)
	}
}
