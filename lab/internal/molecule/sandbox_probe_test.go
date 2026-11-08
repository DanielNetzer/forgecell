package molecule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

// codexSandboxProbeFixture is codexCacheFixture with a fake codex that also answers
// --version and runs sandbox, the shell body of its `sandbox` subcommand. Its exec call
// records argv, stdin (the prompt) and FORGECELL_SANDBOX_DENIES; its sandbox call leaves
// a marker. No real Codex, model or network is involved.
type sandboxProbeFiles struct{ argv, stdin, env, probed string }

func codexSandboxProbeFixture(t *testing.T, sandbox string) (Options, sandboxProbeFiles) {
	t.Helper()
	o, argv, _ := codexCacheFixture(t)
	root := filepath.Dir(argv)
	files := sandboxProbeFiles{argv: argv, stdin: filepath.Join(root, "stdin.txt"), env: filepath.Join(root, "env.txt"), probed: filepath.Join(root, "probed")}
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'codex-cli 0.167.0'; exit 0; fi\nif [ \"$1\" = sandbox ]; then\ntouch %q\n%s\nexit 0\nfi\nprintf '%%s\\n' \"$@\" > %q\nenv | grep '^FORGECELL_SANDBOX_DENIES=' > %q\nout=\"\"\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--output-last-message\" ]; then shift; out=\"$1\"; fi\n  shift\ndone\ncat > %q\nprintf changed > code.txt\nprintf '%%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"implemented\",\"paths\":[]}' > \"$out\"\n", files.probed, sandbox, argv, files.env, files.stdin)
	if err := writeStub(filepath.Join(root, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return o, files
}

func probeOutput(states ...string) string {
	lines := make([]string, len(harness.SandboxCapabilities))
	for i, name := range harness.SandboxCapabilities {
		lines[i] = name + "=" + states[i]
	}
	return "printf '%s\\n' " + strings.Join(lines, " ")
}

func TestSandboxProbeRecordedAndPassedToCodexCoding(t *testing.T) {
	t.Parallel()
	o, files := codexSandboxProbeFixture(t, probeOutput("ok", "ok", "ok", "denied", "denied"))
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].SandboxProbe == nil {
		t.Fatalf("attempt did not record the sandbox probe: %+v", r.HarnessAttempts)
	}
	probe := *r.HarnessAttempts[0].SandboxProbe
	if probe.State != harness.SandboxProbeRan || probe.CodexVersion != "codex-cli 0.167.0" || !slices.Equal(probe.Denied(), []string{"ps", "localhost-bind"}) {
		t.Fatalf("probe %+v", probe)
	}
	data, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Record
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if got := saved.HarnessAttempts[0].SandboxProbe; got == nil || !slices.Equal(got.Checks, probe.Checks) || got.CodexVersion != probe.CodexVersion {
		t.Fatalf("persisted ledger lost the sandbox probe: %+v", got)
	}
	if !strings.Contains(string(data), `"sandboxProbe"`) {
		t.Fatal("ledger has no sandboxProbe field")
	}
	stdin, err := os.ReadFile(files.stdin)
	if err != nil {
		t.Fatal(err)
	}
	sentence := "The sandbox denies: ps, localhost-bind. Don't run checks that need them; the Lab runs the approved checks outside the sandbox."
	if strings.Count(string(stdin), sentence) != 1 {
		t.Fatalf("coding instruction lacks the denied-capabilities sentence: %s", stdin)
	}
	rawEnv, _ := os.ReadFile(files.env)
	if got := strings.TrimSpace(string(rawEnv)); got != "FORGECELL_SANDBOX_DENIES=ps,localhost-bind" {
		t.Fatalf("provider environment %q", got)
	}
	rawArgv, _ := os.ReadFile(files.argv)
	if !slices.Contains(strings.Split(string(rawArgv), "\n"), `shell_environment_policy.set.FORGECELL_SANDBOX_DENIES="ps,localhost-bind"`) {
		t.Fatalf("provider argv lacks the denies override: %s", rawArgv)
	}
}

func TestSandboxProbeNothingDeniedChangesNothing(t *testing.T) {
	t.Parallel()
	o, files := codexSandboxProbeFixture(t, probeOutput("ok", "ok", "ok", "ok", "unknown"))
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].SandboxProbe == nil || r.HarnessAttempts[0].SandboxProbe.State != harness.SandboxProbeRan {
		t.Fatalf("attempts %+v", r.HarnessAttempts)
	}
	stdin, _ := os.ReadFile(files.stdin)
	if strings.Contains(string(stdin), "The sandbox denies") {
		t.Fatal("an undenied sandbox changed the coding instruction")
	}
	rawEnv, _ := os.ReadFile(files.env)
	rawArgv, _ := os.ReadFile(files.argv)
	if strings.TrimSpace(string(rawEnv)) != "" || strings.Contains(string(rawArgv), "FORGECELL_SANDBOX_DENIES") {
		t.Fatalf("FORGECELL_SANDBOX_DENIES set without a denial: %q %q", rawEnv, rawArgv)
	}
}

func TestSandboxProbeDeniedHarnessCacheStartsNoCoding(t *testing.T) {
	t.Parallel()
	o, files := codexSandboxProbeFixture(t, probeOutput("ok", "denied", "ok", "ok", "ok"))
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "failed" || len(r.HarnessAttempts) != 0 {
		t.Fatalf("a denied harness cache reached coding: %s %+v", r.Status, r.HarnessAttempts)
	}
	if detail := r.Readiness.Events[len(r.Readiness.Events)-1].Detail; !strings.Contains(detail, "Harness Go cache is not writable inside the Codex sandbox") {
		t.Fatalf("refusal detail %q", detail)
	}
	if _, err = os.Stat(files.probed); err != nil {
		t.Fatalf("the probe did not run: %v", err)
	}
	if _, err = os.Stat(files.argv); !os.IsNotExist(err) {
		t.Fatal("the coding process started despite a denied harness cache")
	}
}

func TestSandboxProbeUnavailableStillCodes(t *testing.T) {
	t.Parallel()
	o, files := codexSandboxProbeFixture(t, "echo \"error: unrecognized subcommand 'sandbox'\" >&2\nexit 2")
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].Coding == nil || r.HarnessAttempts[0].Coding.Outcome != "completed" {
		t.Fatalf("coding did not proceed: %+v", r.HarnessAttempts)
	}
	probe := r.HarnessAttempts[0].SandboxProbe
	if probe == nil || probe.State != harness.SandboxProbeUnavailable || probe.Reason == "" || len(probe.Checks) != 0 {
		t.Fatalf("probe %+v", probe)
	}
	stdin, _ := os.ReadFile(files.stdin)
	rawEnv, _ := os.ReadFile(files.env)
	if strings.Contains(string(stdin), "The sandbox denies") || strings.TrimSpace(string(rawEnv)) != "" {
		t.Fatalf("an unavailable probe changed the coding run: %q", rawEnv)
	}
}

func TestSandboxProbeAbsentForOtherBindingsAndEarlierLedgers(t *testing.T) {
	t.Parallel()
	o, argv := claudeAllowlistFixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].SandboxProbe != nil {
		t.Fatalf("a Claude Code attempt recorded a sandbox probe: %+v", r.HarnessAttempts)
	}
	rawArgv, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawArgv), "FORGECELL_SANDBOX_DENIES") || strings.HasPrefix(string(rawArgv), "sandbox\n") {
		t.Fatalf("a Claude Code run got the probe or its variable: %s", rawArgv)
	}
	var legacy HarnessAttempt
	if err = json.Unmarshal([]byte(`{"id":"legacy","planDigest":"d","result":{}}`), &legacy); err != nil || legacy.SandboxProbe != nil {
		t.Fatalf("an earlier attempt gained a sandbox probe: %+v %v", legacy, err)
	}
}
