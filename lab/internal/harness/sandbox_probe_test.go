package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// fakeSandboxCodex writes a fake codex that answers --version, records the argv of its
// `sandbox` call and then runs sandbox, the shell body for that subcommand. Every other
// subcommand fails as an unknown one. No real Codex is involved.
func fakeSandboxCodex(t *testing.T, sandbox string) (provider, argv string) {
	t.Helper()
	dir := t.TempDir()
	provider = filepath.Join(dir, "codex")
	argv = filepath.Join(dir, "argv.txt")
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.167.0'; exit 0; fi\nif [ \"$1\" = sandbox ]; then\nprintf '%%s\\n' \"$@\" > %q\n%s\nexit 0\nfi\necho \"error: unrecognized subcommand '$1'\" >&2\nexit 2\n", argv, sandbox)
	// Holding ForkLock keeps the write handle out of any child forked meanwhile, so the
	// exec never meets a busy file.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	if err := os.WriteFile(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return provider, argv
}

func probeLines(lines ...string) string {
	return "printf '%s\\n' " + strings.Join(lines, " ")
}

func TestSandboxProbeParsesAndRecordsDenials(t *testing.T) {
	provider, argv := fakeSandboxCodex(t, probeLines("workspace-write=ok", "harness-cache-write=ok", "spawn=ok", "ps=denied", "localhost-bind=denied"))
	workspace, cache := t.TempDir(), t.TempDir()
	p := ProbeSandbox(context.Background(), provider, workspace, cache, 0)
	if p.State != SandboxProbeRan || p.CodexVersion != "codex-cli 0.167.0" || p.Reason != "" {
		t.Fatalf("probe %+v", p)
	}
	want := []SandboxCheck{{SandboxWorkspaceWrite, SandboxOK}, {SandboxHarnessCacheWrite, SandboxOK}, {SandboxSpawn, SandboxOK}, {SandboxPS, SandboxDenied}, {SandboxLocalhostBind, SandboxDenied}}
	if !slices.Equal(p.Checks, want) {
		t.Fatalf("checks %+v", p.Checks)
	}
	if got := p.DeniesValue(); got != "ps,localhost-bind" {
		t.Fatalf("denies value %q", got)
	}
	if got := p.Instruction(); got != " The sandbox denies: ps, localhost-bind. Don't run checks that need them; the Lab runs the approved checks outside the sandbox." {
		t.Fatalf("instruction %q", got)
	}
	if p.Denies(SandboxHarnessCacheWrite) || !p.Denies(SandboxPS) {
		t.Fatalf("Denies disagrees with the checks: %+v", p)
	}
	raw, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	overrides, err := codexHarnessCacheOverrides(cache)
	if err != nil {
		t.Fatal(err)
	}
	root, err := tomlBasicString(cache)
	if err != nil {
		t.Fatal(err)
	}
	head := slices.Concat([]string{"sandbox", "-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.writable_roots=[" + root + "]"}, overrides, []string{"--", "sh", "-c"})
	if len(args) < len(head) || !slices.Equal(args[:len(head)], head) {
		t.Fatalf("probe argv %q, want prefix %q", args, head)
	}
	if tail := args[len(args)-2:]; !slices.Equal(tail, []string{workspace, resources.HarnessCacheAt(cache).BuildCache}) {
		t.Fatalf("probe arguments %q", tail)
	}
}

// `codex sandbox` rejects --add-dir and accepts -C only with --permission-profile, so
// the probe makes the cache root writable through writable_roots and runs in the
// workspace instead.
func TestSandboxProbeArgsUseWritableRootsNotAddDirOrC(t *testing.T) {
	cache := `/tmp/odd "cache" \root`
	args, err := SandboxProbeArgs("/work/space", cache)
	if err != nil {
		t.Fatal(err)
	}
	probeOptions := args[:slices.Index(args, "--")]
	for _, flag := range []string{"--add-dir", "-C", "--cd"} {
		if slices.Contains(probeOptions, flag) {
			t.Fatalf("probe argv passes %s: %q", flag, args)
		}
	}
	want := `sandbox_workspace_write.writable_roots=["/tmp/odd \"cache\" \\root"]`
	if i := slices.Index(probeOptions, want); i < 1 || probeOptions[i-1] != "-c" {
		t.Fatalf("probe argv %q lacks -c %s", args, want)
	}
	if _, err := SandboxProbeArgs("/work/space", "/tmp/new\nline"); err == nil {
		t.Fatal("a cache root with a newline was quoted")
	}

	dir := t.TempDir()
	pwdFile := filepath.Join(dir, "pwd.txt")
	provider, _ := fakeSandboxCodex(t, fmt.Sprintf("pwd -P > %q\n%s", pwdFile, probeLines("workspace-write=ok", "harness-cache-write=ok", "spawn=ok", "ps=ok", "localhost-bind=ok")))
	workspace := t.TempDir()
	if p := ProbeSandbox(context.Background(), provider, workspace, t.TempDir(), 0); p.State != SandboxProbeRan {
		t.Fatalf("probe %+v", p)
	}
	raw, err := os.ReadFile(pwdFile)
	if err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != real {
		t.Fatalf("probe ran in %q, want the workspace %q", got, real)
	}
}

func TestSandboxProbeNothingDeniedLeavesInstructionUnchanged(t *testing.T) {
	provider, _ := fakeSandboxCodex(t, probeLines("workspace-write=ok", "harness-cache-write=ok", "spawn=ok", "ps=ok", "localhost-bind=unknown"))
	p := ProbeSandbox(context.Background(), provider, t.TempDir(), t.TempDir(), 0)
	if p.State != SandboxProbeRan || len(p.Denied()) != 0 || p.DeniesValue() != "" || p.Instruction() != "" {
		t.Fatalf("probe %+v", p)
	}
}

// The real fixed script runs here under plain sh, outside any sandbox: every check
// must report a known state, and the writable directories must be left clean.
func TestSandboxProbeScriptReportsEveryCheck(t *testing.T) {
	provider, _ := fakeSandboxCodex(t, `while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done; shift; exec "$@"`)
	workspace, cache := t.TempDir(), t.TempDir()
	if err := os.Mkdir(resources.HarnessCacheAt(cache).BuildCache, 0700); err != nil {
		t.Fatal(err)
	}
	p := ProbeSandbox(context.Background(), provider, workspace, cache, 0)
	if p.State != SandboxProbeRan || len(p.Checks) != len(SandboxCapabilities) {
		t.Fatalf("probe %+v", p)
	}
	for _, c := range p.Checks[:2] {
		if c.State != SandboxOK {
			t.Fatalf("%s is %s outside a sandbox", c.Name, c.State)
		}
	}
	for _, dir := range []string{workspace, resources.HarnessCacheAt(cache).BuildCache} {
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Fatalf("probe left files in %s: %v %v", dir, entries, err)
		}
	}
}

func TestSandboxProbeUnavailable(t *testing.T) {
	for _, c := range []struct {
		name, sandbox, reason string
	}{
		{"unknown subcommand", "", ""},
		{"unknown name", probeLines("workspace-write=ok", "harness-cache-write=ok", "spawn=ok", "ps=ok", "localhost-bind=ok", "network=ok"), "unexpected line"},
		{"duplicate", probeLines("workspace-write=ok", "workspace-write=ok", "harness-cache-write=ok", "spawn=ok", "ps=ok", "localhost-bind=ok"), "duplicate workspace-write"},
		{"missing", probeLines("workspace-write=ok", "harness-cache-write=ok", "spawn=ok", "ps=ok"), "missing localhost-bind"},
		{"invalid state", probeLines("workspace-write=ok", "harness-cache-write=maybe", "spawn=ok", "ps=ok", "localhost-bind=ok"), "invalid state for harness-cache-write"},
		{"noise", "echo hello", "unexpected line"},
	} {
		t.Run(c.name, func(t *testing.T) {
			provider, _ := fakeSandboxCodex(t, c.sandbox)
			if c.sandbox == "" {
				// A Codex without the subcommand: rewrite the fake so `sandbox` is unknown.
				syscall.ForkLock.RLock()
				err := os.WriteFile(provider, []byte("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.100.0'; exit 0; fi\necho \"error: unrecognized subcommand '$1'\" >&2\nexit 2\n"), 0700)
				syscall.ForkLock.RUnlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			p := ProbeSandbox(context.Background(), provider, t.TempDir(), t.TempDir(), 0)
			if p.State != SandboxProbeUnavailable || p.Reason == "" || len(p.Checks) != 0 || p.DeniesValue() != "" || p.Instruction() != "" {
				t.Fatalf("probe %+v", p)
			}
			if c.sandbox == "" && (!strings.HasPrefix(p.Reason, "codex sandbox failed") || p.CodexVersion != "codex-cli 0.100.0") {
				t.Fatalf("missing subcommand recorded as %+v", p)
			}
			if !strings.Contains(p.Reason, c.reason) {
				t.Fatalf("reason %q lacks %q", p.Reason, c.reason)
			}
		})
	}
}

func TestSandboxProbeTimeoutIsUnavailable(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "started")
	provider, _ := fakeSandboxCodex(t, fmt.Sprintf("touch %q\nsleep 60", marker))
	// The probe's own timeout is reported the same way as a context deadline; the
	// context expires once the fake has provably started, so no wall-clock budget is
	// raced.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(marker); err == nil {
				cancel(context.DeadlineExceeded)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	p := ProbeSandbox(ctx, provider, t.TempDir(), t.TempDir(), time.Minute)
	if p.State != SandboxProbeUnavailable || !strings.Contains(p.Reason, "timed out") || len(p.Checks) != 0 {
		t.Fatalf("probe %+v", p)
	}
}

func TestSandboxDeniesOnlyOnCodexCodingRequests(t *testing.T) {
	coding := map[string]any{"kind": "molecule", "codingAllowlist": []string{"Read"}}
	call, err := BuildInvocation("codex", WithSandboxDenies(coding, "ps,localhost-bind"), "/out")
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(call.Args, `shell_environment_policy.set.FORGECELL_SANDBOX_DENIES="ps,localhost-bind"`); i < 1 || call.Args[i-1] != "-c" || call.Args[len(call.Args)-1] != "-" {
		t.Fatalf("Codex argv %q", call.Args)
	}
	if strings.Contains(call.Input, "harnessSandboxDenies") || strings.Contains(call.Input, "localhost-bind") {
		t.Fatal("the denies list reached the model-visible request")
	}
	if _, present := coding[sandboxDeniesRequestKey]; present {
		t.Fatal("WithSandboxDenies modified the caller's map")
	}
	plain, err := BuildInvocation("codex", WithSandboxDenies(coding, ""), "/out")
	if err != nil || slices.ContainsFunc(plain.Args, func(a string) bool { return strings.Contains(a, "FORGECELL_SANDBOX_DENIES") }) {
		t.Fatalf("an empty denies list changed the argv: %q %v", plain.Args, err)
	}
	for _, c := range []struct {
		id   string
		kind string
	}{{"claude-code", "molecule"}, {"cursor", "molecule"}, {"codex", "ticket-analysis"}, {"codex", ReviewKind}, {"codex", "formula-improvement"}} {
		request := WithSandboxDenies(map[string]any{"kind": c.kind, "codingAllowlist": []string{"Read"}}, "ps")
		if _, err := BuildInvocation(c.id, request, "/out"); err == nil || !strings.Contains(err.Error(), "only for Codex coding requests") {
			t.Fatalf("%s/%s accepted sandbox denies: %v", c.id, c.kind, err)
		}
	}
	for _, bad := range []any{"", "ps,ps", "ps,network", "ps,", 3} {
		request := map[string]any{"kind": "molecule", sandboxDeniesRequestKey: bad}
		if _, err := BuildInvocation("codex", request, "/out"); err == nil {
			t.Fatalf("invalid denies list %v accepted", bad)
		}
	}
}

func TestExecuteSetsSandboxDeniesEnvironment(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "codex")
	envFile := filepath.Join(dir, "env.txt")
	script := fmt.Sprintf("#!/bin/sh\nenv | grep '^FORGECELL_SANDBOX_DENIES=' > %q\nout=\"\"\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--output-last-message\" ]; then shift; out=\"$1\"; fi\n  shift\ndone\ncat >/dev/null\nprintf '%%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"done\",\"paths\":[]}' > \"$out\"\n", envFile)
	syscall.ForkLock.RLock()
	err := os.WriteFile(provider, []byte(script), 0700)
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	coding := map[string]any{"kind": "molecule", "codingAllowlist": []string{"Read"}}
	for _, c := range []struct {
		denies string
		want   string
	}{{"ps,localhost-bind", "FORGECELL_SANDBOX_DENIES=ps,localhost-bind"}, {"", ""}} {
		_ = os.Remove(envFile)
		env := []string{"PATH=" + os.Getenv("PATH"), "FORGECELL_SANDBOX_DENIES=stale"}
		if c.denies == "" {
			env = env[:1]
		}
		result := Execute(context.Background(), ExecuteOptions{ID: "codex", Executable: provider, Request: WithSandboxDenies(coding, c.denies), Dir: dir, Env: env, Timeout: time.Minute})
		if result.Coding == nil || result.Coding.Outcome != "completed" {
			t.Fatalf("%q: execution %+v", c.denies, result)
		}
		raw, _ := os.ReadFile(envFile)
		if got := strings.TrimSpace(string(raw)); got != c.want {
			t.Fatalf("%q: provider environment %q, want %q", c.denies, got, c.want)
		}
	}
}
