package harness

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// Sandbox capabilities the probe checks, in the order it reports them (#403).
const (
	SandboxWorkspaceWrite    = "workspace-write"
	SandboxHarnessCacheWrite = "harness-cache-write"
	SandboxSpawn             = "spawn"
	SandboxPS                = "ps"
	SandboxLocalhostBind     = "localhost-bind"
)

// SandboxCapabilities lists every capability the probe reports, in its order.
var SandboxCapabilities = []string{SandboxWorkspaceWrite, SandboxHarnessCacheWrite, SandboxSpawn, SandboxPS, SandboxLocalhostBind}

// States of one probe check, and of the probe as a whole.
const (
	SandboxOK               = "ok"
	SandboxDenied           = "denied"
	SandboxUnknown          = "unknown"
	SandboxProbeRan         = "ran"
	SandboxProbeUnavailable = "unavailable"
)

// SandboxDeniesVariable names the denied capabilities, comma-separated, in a Codex
// coding run's environment. It is unset when nothing is denied.
const SandboxDeniesVariable = "FORGECELL_SANDBOX_DENIES"

// DefaultSandboxProbeTimeout bounds the probe and, separately, the version read.
const DefaultSandboxProbeTimeout = 20 * time.Second

// sandboxProbeScript is the fixed POSIX probe. It is never repository code: $1 is the
// workspace and $2 the harness GOCACHE, and every check prints one name=state line.
const sandboxProbeScript = `ws=$1
cache=$2
writable() {
  f="$1/.forgecell-sandbox-probe-$$"
  if : > "$f" 2>/dev/null && rm -f "$f" 2>/dev/null; then echo ok; else rm -f "$f" 2>/dev/null; echo denied; fi
}
echo "workspace-write=$(writable "$ws")"
echo "harness-cache-write=$(writable "$cache")"
if sh -c 'exit 0' >/dev/null 2>&1; then echo spawn=ok; else echo spawn=denied; fi
if ps -p $$ >/dev/null 2>&1; then echo ps=ok; else echo ps=denied; fi
if command -v python3 >/dev/null 2>&1; then
  if python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); s.listen(1); s.close()' >/dev/null 2>&1; then echo localhost-bind=ok; else echo localhost-bind=denied; fi
elif command -v nc >/dev/null 2>&1; then
  nc -l 127.0.0.1 47391 </dev/null >/dev/null 2>&1 &
  pid=$!
  sleep 1
  if kill -0 "$pid" 2>/dev/null; then kill "$pid" 2>/dev/null; echo localhost-bind=ok; else echo localhost-bind=denied; fi
else
  echo localhost-bind=unknown
fi
`

// SandboxCheck is one probed capability and its state (ok, denied or unknown).
type SandboxCheck struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

// SandboxProbe is the recorded result of probing a Codex coding sandbox before the
// coding run starts (#403). State is "ran" with every check, or "unavailable" with the
// reason when the probe could not run or its output could not be read.
type SandboxProbe struct {
	CodexVersion string         `json:"codexVersion,omitempty"`
	State        string         `json:"state"`
	Reason       string         `json:"reason,omitempty"`
	Checks       []SandboxCheck `json:"checks,omitempty"`
}

// Denied returns the denied capabilities in probe order; none for an unavailable probe.
func (p SandboxProbe) Denied() []string {
	var denied []string
	for _, c := range p.Checks {
		if c.State == SandboxDenied {
			denied = append(denied, c.Name)
		}
	}
	return denied
}

// Denies reports whether the probe found capability denied.
func (p SandboxProbe) Denies(capability string) bool {
	return slices.Contains(p.Denied(), capability)
}

// DeniesValue is the comma-separated FORGECELL_SANDBOX_DENIES value, empty when nothing
// is denied.
func (p SandboxProbe) DeniesValue() string {
	return strings.Join(p.Denied(), ",")
}

// Instruction is the sentence appended to the coding instruction, empty when nothing is
// denied.
func (p SandboxProbe) Instruction() string {
	denied := p.Denied()
	if len(denied) == 0 {
		return ""
	}
	return " The sandbox denies: " + strings.Join(denied, ", ") + ". Don't run checks that need them; the Lab runs the approved checks outside the sandbox."
}

// SandboxProbeArgs is the argv after the provider executable: the same workspace-write
// sandbox and shell_environment_policy.set overrides as the coding run, and the fixed
// probe script run with the workspace and the harness GOCACHE as arguments. `codex
// sandbox` has no --add-dir and accepts -C only with --permission-profile, so the cache
// root is made writable with the writable_roots override and ProbeSandbox runs the
// command with the workspace as its working directory.
func SandboxProbeArgs(workspace, cacheRoot string) ([]string, error) {
	if cacheRoot == "" || workspace == "" {
		return nil, fmt.Errorf("the sandbox probe needs a workspace and a harness Go cache")
	}
	root, err := tomlBasicString(cacheRoot)
	if err != nil {
		return nil, err
	}
	overrides, err := codexHarnessCacheOverrides(cacheRoot)
	if err != nil {
		return nil, err
	}
	args := []string{"sandbox", "-c", `sandbox_mode="workspace-write"`, "-c", "sandbox_workspace_write.writable_roots=[" + root + "]"}
	args = append(args, overrides...)
	return append(args, "--", "sh", "-c", sandboxProbeScript, "forgecell-sandbox-probe", workspace, resources.HarnessCacheAt(cacheRoot).BuildCache), nil
}

// ProbeSandbox runs the fixed probe through the Codex provider executable, with no model
// call. Any failure to run it (an older Codex without `sandbox`, a timeout, unreadable
// output) is returned as unavailable with the reason, never as an error, so coding still
// starts. A timeout of zero uses DefaultSandboxProbeTimeout.
func ProbeSandbox(ctx context.Context, provider, workspace, cacheRoot string, timeout time.Duration) SandboxProbe {
	if timeout <= 0 {
		timeout = DefaultSandboxProbeTimeout
	}
	probe := SandboxProbe{CodexVersion: codexVersion(ctx, provider, workspace, timeout)}
	args, err := SandboxProbeArgs(workspace, cacheRoot)
	if err != nil {
		return unavailable(probe, err.Error())
	}
	env := HarnessCacheEnvironment(os.Environ(), resources.HarnessCacheAt(cacheRoot))
	result := process.Run(ctx, process.Options{Argv: append([]string{provider}, args...), Dir: workspace, Env: env, Stdin: []byte{}, Timeout: timeout, MaxOutputBytes: 64_000})
	switch {
	case result.TimedOut || result.ParentDeadline:
		return unavailable(probe, fmt.Sprintf("codex sandbox timed out after %s", timeout))
	case !result.OK:
		reason := "codex sandbox failed"
		if result.Error != "" {
			reason += ": " + boundedText(result.Error, 200)
		}
		return unavailable(probe, reason)
	}
	checks, err := parseSandboxProbe(result.Stdout)
	if err != nil {
		return unavailable(probe, err.Error())
	}
	probe.State, probe.Checks = SandboxProbeRan, checks
	return probe
}

func unavailable(p SandboxProbe, reason string) SandboxProbe {
	p.State, p.Reason, p.Checks = SandboxProbeUnavailable, reason, nil
	return p
}

// codexVersion reads `<provider> --version`, bounded to one printable line; it is empty
// when unreadable.
func codexVersion(ctx context.Context, provider, dir string, timeout time.Duration) string {
	r := process.Run(ctx, process.Options{Argv: []string{provider, "--version"}, Dir: dir, Stdin: []byte{}, Timeout: timeout, MaxOutputBytes: 4096})
	if !r.OK {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(r.Stdout), "\n")
	return ReviewDisplayText(strings.TrimSpace(line), modelFieldRunes)
}

// parseSandboxProbe reads the name=state lines. Blank lines are skipped; any other line,
// an unknown name or state, a duplicate or a missing check makes the output unreadable.
func parseSandboxProbe(stdout string) ([]SandboxCheck, error) {
	states := map[string]string{}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, state, ok := strings.Cut(line, "=")
		if !ok || !slices.Contains(SandboxCapabilities, name) {
			return nil, fmt.Errorf("unreadable sandbox probe output: unexpected line %q", boundedText(line, 80))
		}
		if state != SandboxOK && state != SandboxDenied && state != SandboxUnknown {
			return nil, fmt.Errorf("unreadable sandbox probe output: invalid state for %s", name)
		}
		if _, seen := states[name]; seen {
			return nil, fmt.Errorf("unreadable sandbox probe output: duplicate %s", name)
		}
		states[name] = state
	}
	checks := make([]SandboxCheck, 0, len(SandboxCapabilities))
	for _, name := range SandboxCapabilities {
		state, ok := states[name]
		if !ok {
			return nil, fmt.Errorf("unreadable sandbox probe output: missing %s", name)
		}
		checks = append(checks, SandboxCheck{Name: name, State: state})
	}
	return checks, nil
}
