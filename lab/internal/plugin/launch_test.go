package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

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

func TestShutdownKillsWhatAPluginLeftInItsGroup(t *testing.T) {
	// The grandchild mode leaves a background child that holds stderr open and
	// never exits on its own; the server reports it before serving.
	p, root := startFake(t, "tracker", "grandchild", nil)
	if _, err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Shutdown(nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Minute):
		// Only a regression gets here: free the pipe so the test can end.
		if pid := grandchildPID(p.Diagnostics()); pid > 0 {
			if grandchild, err := os.FindProcess(pid); err == nil {
				grandchild.Kill()
			}
		}
		<-done
		t.Fatal("Shutdown waited on a stderr pipe the grandchild held")
	}
	// With no grace, Shutdown returns only once stderr reached EOF, which the
	// grandchild allows only by exiting.
	if p.cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("the server did not exit on its own: %v", p.cmd.ProcessState)
	}
	diag := p.Diagnostics()
	if grandchildPID(diag) <= 0 || !strings.Contains(diag, "fake: stdin closed") {
		t.Fatalf("diagnostics lack the grandchild marker or the clean exit:\n%s", diag)
	}
	if left := entries(t, root); len(left) != 0 {
		t.Fatalf("the private directory was left behind: %v", left)
	}
}

// grandchildPID reads the pid the grandchild mode reports, or 0.
func grandchildPID(diag string) int {
	_, rest, ok := strings.Cut(diag, "fake-grandchild-ready pid=")
	if !ok {
		return 0
	}
	line, _, _ := strings.Cut(rest, "\n")
	pid, err := strconv.Atoi(line)
	if err != nil {
		return 0
	}
	return pid
}

func TestShutdownRemovesAHomeThePluginMadeReadOnly(t *testing.T) {
	outside := filepath.Join(plugintest.TempDir(t), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "kept"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(outside, 0o700) })
	// locked-home leaves HOME/locked (0500) holding sub (0000), and a symlink
	// HOME/outside to a read-only directory outside the run directory.
	p, root := startFake(t, "forge", "locked-home", map[string]string{"FAKE_OUTSIDE": outside}, "FAKE_OUTSIDE")
	if _, err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(nil); err != nil {
		t.Fatal(err)
	}
	if left := entries(t, root); len(left) != 0 {
		t.Fatalf("the private directory was left behind: %v", left)
	}
	info, err := os.Lstat(outside)
	if err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("the symlinked directory outside the run was changed: %v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "kept")); err != nil {
		t.Fatalf("a file outside the run was removed: %v", err)
	}
}

func TestCleanupFailuresAreFixedErrors(t *testing.T) {
	// The unmark plugin removes its run marker, so the directory is refused.
	p, root := startFake(t, "reviewer", "unmark", nil)
	if _, err := p.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	err := p.Shutdown(nil)
	if err == nil || strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "/") {
		t.Fatalf("Shutdown = %v, want a fixed error", err)
	}
	if len(entries(t, root)) != 1 {
		t.Fatal("an unmarked directory was removed")
	}

	base := plugintest.TempDir(t)
	dir, err := makeRunDir(base)
	if err != nil {
		t.Fatal(err)
	}
	removeAll = func(path string) error {
		return &os.PathError{Op: "unlinkat", Path: filepath.Join(path, "home", "x"), Err: os.ErrPermission}
	}
	err = removeRunDir(dir)
	removeAll = os.RemoveAll
	if err == nil || strings.Contains(err.Error(), base) || strings.Contains(err.Error(), "/") {
		t.Fatalf("removeRunDir = %v, want a fixed error", err)
	}
	if err := removeRunDir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestStartVerifiesTheInterpreterJustBeforeExec(t *testing.T) {
	server := plugintest.Server(t)
	m, err := Decode(plugintest.Manifest(t, server, "forge", "good"))
	if err != nil {
		t.Fatal(err)
	}
	// Any pinned file stands in for the interpreter: it is never run.
	interpreter := plugintest.Server(t)
	m.Server.Interpreter = &interpreter
	beforeExec = func() {
		if err := os.WriteFile(interpreter.Path, []byte("#!/bin/sh\necho swapped\n"), 0o700); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeExec = nil })
	root := plugintest.TempDir(t)
	_, err = Start(context.Background(), m, Options{Root: root})
	if mcp.ReasonOf(err) != mcp.ManifestChanged || err.Error() != "plugin: manifest-changed" {
		t.Fatalf("Start = %v, want manifest-changed", err)
	}
	if len(entries(t, root)) != 0 {
		t.Fatal("the private directory was left behind")
	}
}

func TestHandshakeHasADefaultTimeout(t *testing.T) {
	// The silent servers never answer, so any timeout gives the same outcome.
	handshakeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = 30 * time.Second })
	p, _ := startFake(t, "tracker", "silent", nil)
	if _, err := p.Initialize(context.Background()); mcp.ReasonOf(err) != mcp.Timeout {
		t.Fatalf("Initialize = %v, want timeout", err)
	}
	p, _ = startFake(t, "tracker", "silent-list", nil)
	// The server answers initialize; a caller deadline keeps the lowered
	// default from bounding its start-up.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if _, err := p.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListTools(context.Background()); mcp.ReasonOf(err) != mcp.Timeout {
		t.Fatalf("ListTools = %v, want timeout", err)
	}
}

func TestHandshakeKeepsTheCallerDeadline(t *testing.T) {
	handshakeTimeout = 0
	t.Cleanup(func() { handshakeTimeout = 30 * time.Second })
	p, _ := startFake(t, "forge", "good", nil)
	// A zero default would expire at once; the caller's deadline is used.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	if _, err := p.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListTools(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWaitTerminatedAcceptsOnlyATerminatedLeader(t *testing.T) {
	const (
		exited, killed, dumped, trapped, stopped, continued = 1, 2, 3, 4, 5, 6
	)
	for _, want := range []int32{exited, killed, dumped} {
		// A stop, a continue, a trap and an interrupted call come first; each must
		// wait again rather than count as the leader's exit.
		script := []struct {
			code  int32
			retry bool
		}{{stopped, false}, {continued, false}, {trapped, false}, {exited, true}, {want, false}}
		calls := 0
		err := waitTerminated(func(info *siginfo) (bool, error) {
			s := script[calls]
			calls++
			*(*int32)(unsafe.Pointer(&info[siCodeOffset])) = s.code
			return s.retry, nil
		})
		if err != nil || calls != len(script) {
			t.Fatalf("si_code %d: err %v after %d calls, want %d", want, err, calls, len(script))
		}
	}
	for _, code := range []int32{0, stopped, continued, trapped} {
		if terminated(code) {
			t.Fatalf("si_code %d counted as terminated", code)
		}
	}
}

func TestWaitTerminatedReportsAWaitFailure(t *testing.T) {
	failure := errors.New("waitid failed")
	calls := 0
	err := waitTerminated(func(info *siginfo) (bool, error) {
		calls++
		return false, failure
	})
	if err != failure || calls != 1 {
		t.Fatalf("err %v after %d calls", err, calls)
	}
}

func TestInitializeRefusesAZeroWidthContract(t *testing.T) {
	p, _ := startFake(t, "tracker", "zero-width-contract", nil)
	info, err := p.Initialize(context.Background())
	if mcp.ReasonOf(err) != mcp.ContractMismatch || info.Contract != "forgecell.tracker/v1" {
		t.Fatalf("Initialize = %+v, %v", info, err)
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
