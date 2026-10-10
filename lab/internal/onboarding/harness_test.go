package onboarding

import (
	"context"
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"reflect"
	"strings"
	"testing"
)

func TestSelectionPrecedence(t *testing.T) {
	candidates := []Candidate{{ID: "codex", Readiness: "ready"}, {ID: "claude-code", Readiness: "ready"}}
	cases := []struct {
		preferred, existing string
		env                 map[string]string
		want, status        string
	}{
		{"claude-code", "codex", nil, "claude-code", "ready"},
		{"", "codex", map[string]string{"CLAUDECODE": "1"}, "codex", "ready"},
		{"", "", map[string]string{"CODEX_THREAD_ID": "test"}, "codex", "ready"},
		{"", "", nil, "", "ambiguous"},
		{"cursor", "", nil, "cursor", "blocked"},
	}
	for _, c := range cases {
		got := Select(candidates, c.preferred, c.existing, c.env)
		if got.ID != c.want || got.Status != c.status {
			t.Fatalf("%+v", got)
		}
	}
}
func TestProbeOnlyUsesStatusAndHelp(t *testing.T) {
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		t.Run(id, func(t *testing.T) {
			var calls [][]string
			run := func(_ context.Context, o process.Options) process.Result {
				args := o.Argv[1:]
				calls = append(calls, args)
				text := "-c --disable --color --strict-config --skip-git-repo-check --ignore-rules Codex Claude Code Cursor Agent --sandbox --output-last-message --output-schema --ephemeral --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --mcp-config --allowedTools --disallowedTools --mode --trust"
				if reflect.DeepEqual(args, []string{"login", "status"}) {
					text = "Logged in using private-account"
				}
				if reflect.DeepEqual(args, []string{"auth", "status", "--json"}) {
					text = `{"loggedIn":true,"email":"private-account"}`
				}
				if reflect.DeepEqual(args, []string{"status", "--format", "json"}) {
					text = `{"isAuthenticated":true,"email":"private-account"}`
				}
				return process.Result{OK: true, Stdout: text}
			}
			c := Probe(context.Background(), id, "/fake/provider", "/repo", run)
			if c.Readiness != "ready" || strings.Contains(c.Detail, "private-account") {
				t.Fatalf("%+v", c)
			}
			for _, args := range calls {
				if len(args) == 0 || args[0] == "--print" || args[0] == "-p" {
					t.Fatal("model invocation during detection")
				}
			}
		})
	}
}
func TestCurrentHarnessDoesNotSilentlyFallback(t *testing.T) {
	s := Select([]Candidate{{ID: "codex", Readiness: "login-required"}, {ID: "cursor", Readiness: "ready"}}, "", "", map[string]string{"CODEX_THREAD_ID": "test"})
	if s.ID != "codex" || s.Status != "blocked" {
		t.Fatalf("%+v", s)
	}
}

func TestCapabilitiesSeparateProbeFromWorkflow(t *testing.T) {
	for _, id := range []string{"cursor", "claude-code", "codex"} {
		t.Run(id, func(t *testing.T) {
			run := func(_ context.Context, o process.Options) process.Result {
				text := "-c --disable --color --strict-config --skip-git-repo-check --ignore-rules Codex Claude Code Cursor Agent --sandbox --output-last-message --output-schema --ephemeral --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --mcp-config --allowedTools --disallowedTools --mode --trust --safe-mode --strict-mcp-config --no-chrome --disable-slash-commands"
				args := strings.Join(o.Argv[1:], " ")
				if args == "login status" {
					text = "Logged in"
				}
				if args == "auth status --json" {
					text = `{"loggedIn":true}`
				}
				if args == "status --format json" {
					text = `{"isAuthenticated":true}`
				}
				if strings.Contains(args, "mcp list") {
					text = `[]`
				}
				return process.Result{OK: true, Stdout: text}
			}
			c := Probe(context.Background(), id, "/fixture/provider", t.TempDir(), run)
			if c.Readiness != "ready" || c.Capabilities.ModelAccess.State != "unknown" || c.Capabilities.Coding.State != "supported" {
				t.Fatalf("%+v", c)
			}
			want := "supported"
			if id == "cursor" {
				want = "unsupported"
			}
			if c.Capabilities.Analysis.State != want || c.Capabilities.AnalysisControls.State != want || c.Capabilities.Analysis.Reason == "" {
				t.Fatalf("%+v", c.Capabilities)
			}
		})
	}
	c := Probe(context.Background(), "codex", "", t.TempDir(), nil)
	if c.Capabilities.AnalysisControls.State != "unknown" || c.Capabilities.ModelAccess.State != "unknown" {
		t.Fatal(c)
	}
}

func TestAnalysisControlBlockersAndFailedProbe(t *testing.T) {
	for _, tc := range []struct {
		id, blocker string
		failed      bool
	}{{"claude-code", "--safe-mode", false}, {"codex", "enumerate", false}, {"cursor", "identity", true}} {
		t.Run(tc.id, func(t *testing.T) {
			run := func(_ context.Context, o process.Options) process.Result {
				args := strings.Join(o.Argv[1:], " ")
				if tc.failed {
					return process.Result{OK: false}
				}
				if strings.Contains(args, "mcp list") {
					return process.Result{OK: false, Stdout: "private configuration"}
				}
				text := "-c --disable --color --strict-config --skip-git-repo-check --ignore-rules Codex Claude Code --sandbox --output-last-message --output-schema --ephemeral --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --mcp-config --allowedTools --disallowedTools logged in"
				if args == "auth status --json" {
					text = `{"loggedIn":true}`
				}
				return process.Result{OK: true, Stdout: text}
			}
			c := Probe(context.Background(), tc.id, "/fixture/provider", t.TempDir(), run)
			if tc.failed {
				if c.Readiness == "ready" || c.Authentication.State != "unknown" || c.Capabilities.AnalysisControls.State != "unknown" {
					t.Fatal(c)
				}
				return
			}
			if c.Readiness != "ready" || c.WorkflowCapability().State != "blocked" || !strings.Contains(c.Capabilities.AnalysisControls.Reason, tc.blocker) || strings.Contains(c.Capabilities.AnalysisControls.Reason, "private configuration") {
				t.Fatalf("%+v", c)
			}
		})
	}
}

func TestCodexAnalysisFlagsBlockWorkflowWithoutChangingSelection(t *testing.T) {
	for _, missing := range []string{"--strict-config", "--ignore-rules", "--skip-git-repo-check", "--color", "--disable", "-c"} {
		t.Run(missing, func(t *testing.T) {
			run := func(_ context.Context, o process.Options) process.Result {
				args := strings.Join(o.Argv[1:], " ")
				switch args {
				case "--version":
					return process.Result{OK: true, Stdout: "Codex fixture"}
				case "--help", "exec --help":
					return process.Result{OK: true, Stdout: strings.ReplaceAll("Codex -c --disable --sandbox --ephemeral --color --output-last-message --output-schema --strict-config --skip-git-repo-check --ignore-rules ", missing+" ", "")}
				case "login status":
					return process.Result{OK: true, Stdout: "Logged in"}
				default:
					t.Fatal("unexpected probe before control verification: " + args)
					return process.Result{}
				}
			}
			c := Probe(context.Background(), "codex", "/fixture/codex", t.TempDir(), run)
			if c.Readiness != "ready" || c.Authentication.State != "verified" || c.Capabilities.AnalysisControls.State != "blocked" || c.WorkflowCapability().State != "blocked" || !strings.Contains(c.WorkflowCapability().Reason, missing) || c.Capabilities.ModelAccess.State != "unknown" {
				t.Fatalf("%+v", c)
			}
			selection := Select([]Candidate{c, {ID: "claude-code", Readiness: "ready"}}, "codex", "", nil)
			if selection.ID != "codex" {
				t.Fatalf("silently substituted provider: %+v", selection)
			}
		})
	}
}

// A Claude Code CLI without the coding isolation flags cannot run the hardened
// coding invocation, so readiness must not report it as ready.
func TestClaudeProbeRequiresCodingIsolationFlags(t *testing.T) {
	for _, missing := range []string{"--setting-sources", "--strict-mcp-config", "--mcp-config", "--allowedTools", "--disallowedTools"} {
		t.Run(missing, func(t *testing.T) {
			help := strings.Replace("Claude Code --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --mcp-config --allowedTools --disallowedTools", " "+missing, "", 1)
			run := func(_ context.Context, o process.Options) process.Result {
				if reflect.DeepEqual(o.Argv[1:], []string{"auth", "status", "--json"}) {
					return process.Result{OK: true, Stdout: `{"loggedIn":true}`}
				}
				return process.Result{OK: true, Stdout: help}
			}
			if c := Probe(context.Background(), "claude-code", "/fake/claude", "/repo", run); c.Readiness == "ready" {
				t.Fatalf("CLI without %s reported ready: %+v", missing, c)
			}
		})
	}
}

const probeHelp = "-c --disable --color --strict-config --skip-git-repo-check --ignore-rules Codex Claude Code Cursor Agent --sandbox --output-last-message --output-schema --ephemeral --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --mcp-config --allowedTools --disallowedTools --mode --trust --safe-mode --strict-mcp-config --no-chrome --disable-slash-commands"

var requiredFlags = map[string][]string{
	"codex":       {"--sandbox", "--output-last-message", "--output-schema", "--ephemeral"},
	"claude-code": {"--print", "--output-format", "--permission-mode", "--tools", "--json-schema", "--no-session-persistence", "--setting-sources", "--strict-mcp-config", "--mcp-config", "--allowedTools", "--disallowedTools"},
	"cursor":      {"--print", "--output-format", "--sandbox", "--mode", "--trust"},
}

// healthyRunner answers every fixed read-only probe successfully unless override replies.
func healthyRunner(override func(args string) *process.Result) Runner {
	return func(_ context.Context, o process.Options) process.Result {
		args := strings.Join(o.Argv[1:], " ")
		if override != nil {
			if r := override(args); r != nil {
				return *r
			}
		}
		text := probeHelp
		switch args {
		case "login status":
			text = "Logged in"
		case "auth status --json":
			text = `{"loggedIn":true}`
		case "status --format json":
			text = `{"isAuthenticated":true}`
		}
		if strings.Contains(args, "mcp list") {
			text = "[]"
		}
		return process.Result{OK: true, Stdout: text}
	}
}

func authArgs(id string) string {
	return map[string]string{"codex": "login status", "claude-code": "auth status --json", "cursor": "status --format json"}[id]
}

func failedSteps(c Candidate) []string {
	var names []string
	for _, s := range c.Failures {
		names = append(names, s.Step)
	}
	return names
}

func stepNamed(t *testing.T, c Candidate, name string) Step {
	t.Helper()
	for _, s := range c.Steps {
		if s.Step == name {
			return s
		}
	}
	t.Fatalf("no step %q in %+v", name, c.Steps)
	return Step{}
}

func TestProbeNamesFailingVersionAndHelpSteps(t *testing.T) {
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		for _, tc := range []struct {
			name   string
			fail   map[string]process.Result
			failed []string
			result map[string]string
		}{
			{"version", map[string]process.Result{"--version": {Code: 3}}, []string{"version"}, map[string]string{"version": "exit-code-3"}},
			{"help", map[string]process.Result{"--help": {TimedOut: true}}, []string{"help"}, map[string]string{"help": "timeout"}},
			{"both", map[string]process.Result{"--version": {Code: -1, Error: "start"}, "--help": {Overflow: true}}, []string{"version", "help"}, map[string]string{"version": "start-failed", "help": "output-limit"}},
		} {
			t.Run(id+"/"+tc.name, func(t *testing.T) {
				run := healthyRunner(func(args string) *process.Result {
					if r, ok := tc.fail[args]; ok {
						r.Stdout = "private-account private configuration"
						return &r
					}
					return nil
				})
				c := Probe(context.Background(), id, "/fixture/provider", t.TempDir(), run)
				if c.Readiness != "unsupported" || !reflect.DeepEqual(failedSteps(c), tc.failed) {
					t.Fatalf("%+v", c)
				}
				for name, want := range tc.result {
					s := stepNamed(t, c, name)
					if s.Result != want || s.Status != "failed" || s.Reason == "" || s.Argv[0] != "/fixture/provider" || !strings.Contains(c.Detail, name) {
						t.Fatalf("%s: %+v detail=%q", name, s, c.Detail)
					}
				}
				if s := stepNamed(t, c, "identity"); s.Status != "skipped" {
					t.Fatalf("identity must not be judged without version and help: %+v", s)
				}
				raw, _ := json.Marshal(c)
				if strings.Contains(string(raw), "private") {
					t.Fatalf("provider output leaked: %s", raw)
				}
			})
		}
	}
}

func TestProbeNamesIdentityStepSeparatelyFromVersionAndHelp(t *testing.T) {
	for id, marker := range map[string]string{"codex": "codex", "claude-code": "claude code", "cursor": "cursor agent or cursor cli"} {
		t.Run(id, func(t *testing.T) {
			run := healthyRunner(func(args string) *process.Result {
				if args == "--version" || args == "--help" {
					return &process.Result{OK: true, Stdout: "some other tool private-account"}
				}
				return nil
			})
			c := Probe(context.Background(), id, "/fixture/provider", t.TempDir(), run)
			s := stepNamed(t, c, "identity")
			if c.Readiness != "unsupported" || !reflect.DeepEqual(failedSteps(c), []string{"identity"}) || s.Expected != marker || s.Reason == "" || !strings.Contains(c.Detail, "identity") {
				t.Fatalf("%+v", c)
			}
			if stepNamed(t, c, "version").Status != "passed" || stepNamed(t, c, "help").Status != "passed" {
				t.Fatalf("version and help ran and must be reported as passed: %+v", c.Steps)
			}
			if raw, _ := json.Marshal(c); strings.Contains(string(raw), "private-account") {
				t.Fatalf("provider output leaked: %s", raw)
			}
		})
	}
}

func TestProbeReportsCodexExecHelpFailureAsHelpStep(t *testing.T) {
	run := healthyRunner(func(args string) *process.Result {
		if args == "exec --help" {
			return &process.Result{Code: 2}
		}
		return nil
	})
	c := Probe(context.Background(), "codex", "/fixture/codex", t.TempDir(), run)
	if c.Readiness != "unsupported" || !reflect.DeepEqual(failedSteps(c), []string{"help"}) {
		t.Fatalf("%+v", c)
	}
	s := c.Failures[0]
	if !reflect.DeepEqual(s.Argv, []string{"/fixture/codex", "exec", "--help"}) || s.Result != "exit-code-2" || c.Installed.State != "verified" {
		t.Fatalf("%+v installed=%+v", s, c.Installed)
	}
}

func TestProbeReportsEveryMissingRequiredFlag(t *testing.T) {
	for id, flags := range requiredFlags {
		t.Run(id, func(t *testing.T) {
			dropped := []string{flags[0], flags[len(flags)-1]}
			text := probeHelp
			for _, flag := range dropped {
				text = strings.ReplaceAll(text, flag, "")
			}
			helpArgs := "--help"
			if id == "codex" {
				helpArgs = "exec --help"
			}
			run := healthyRunner(func(args string) *process.Result {
				if args == helpArgs {
					return &process.Result{OK: true, Stdout: text}
				}
				return nil
			})
			c := Probe(context.Background(), id, "/fixture/provider", t.TempDir(), run)
			want := []string{"required-flag:" + dropped[0], "required-flag:" + dropped[1]}
			if c.Readiness != "unsupported" || !reflect.DeepEqual(failedSteps(c), want) {
				t.Fatalf("want %v: %+v", want, c)
			}
			for i, s := range c.Failures {
				if s.Expected != dropped[i] || s.Reason == "" || s.Argv[len(s.Argv)-1] != "--help" {
					t.Fatalf("%+v", s)
				}
				if !strings.Contains(c.Detail, dropped[i]) {
					t.Fatalf("detail omits %s: %q", dropped[i], c.Detail)
				}
			}
			for _, flag := range flags[1 : len(flags)-1] {
				if stepNamed(t, c, "required-flag:"+flag).Status != "passed" {
					t.Fatalf("%s is listed and must pass: %+v", flag, c.Steps)
				}
			}
		})
	}
}

func TestProbeSeparatesLoginRequiredFromUnverifiableAuth(t *testing.T) {
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		login := map[string]string{"codex": "Not logged in private-account", "claude-code": `{"loggedIn":false,"email":"private-account"}`, "cursor": `{"isAuthenticated":false,"email":"private-account"}`}[id]
		for _, tc := range []struct {
			name      string
			reply     process.Result
			readiness string
			state     string
			result    string
		}{
			{"login-required", process.Result{Code: 1, Stdout: login}, "login-required", "login-required", ""},
			{"unrecognized", process.Result{OK: true, Stdout: "private-account garbage"}, "unknown", "unverifiable", "unrecognized-output"},
			{"failed", process.Result{Code: 7, Stdout: "private-account"}, "unknown", "unverifiable", "exit-code-7"},
		} {
			t.Run(id+"/"+tc.name, func(t *testing.T) {
				run := healthyRunner(func(args string) *process.Result {
					if args == authArgs(id) {
						return &tc.reply
					}
					return nil
				})
				c := Probe(context.Background(), id, "/fixture/provider", t.TempDir(), run)
				s := stepNamed(t, c, "auth")
				if c.Readiness != tc.readiness || !reflect.DeepEqual(failedSteps(c), []string{"auth"}) || s.State != tc.state || s.Result != tc.result || s.Reason == "" || s.Argv[1] != strings.Fields(authArgs(id))[0] {
					t.Fatalf("%+v step=%+v", c, s)
				}
				if c.Installed.State != "verified" || c.Authentication.State == "verified" {
					t.Fatalf("%+v", c)
				}
				if raw, _ := json.Marshal(c); strings.Contains(string(raw), "private-account") {
					t.Fatalf("provider output leaked: %s", raw)
				}
			})
		}
	}
}

func TestProbeReportsIsolationStepWithoutChangingReadiness(t *testing.T) {
	for _, tc := range []struct {
		id, blocker string
		override    func(args string) *process.Result
	}{
		{"claude-code", "--safe-mode", func(args string) *process.Result {
			if args == "--help" {
				return &process.Result{OK: true, Stdout: strings.ReplaceAll(probeHelp, "--safe-mode", "")}
			}
			return nil
		}},
		{"codex", "enumerate", func(args string) *process.Result {
			if strings.Contains(args, "mcp list") {
				return &process.Result{Stdout: "private configuration"}
			}
			return nil
		}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			c := Probe(context.Background(), tc.id, "/fixture/provider", t.TempDir(), healthyRunner(tc.override))
			s := stepNamed(t, c, "isolation")
			if c.Readiness != "ready" || s.Status != "failed" || !strings.Contains(s.Reason, tc.blocker) || !reflect.DeepEqual(failedSteps(c), []string{"isolation"}) || c.WorkflowCapability().State != "blocked" {
				t.Fatalf("%+v step=%+v", c, s)
			}
			if raw, _ := json.Marshal(c); strings.Contains(string(raw), "private configuration") {
				t.Fatalf("provider output leaked: %s", raw)
			}
		})
	}
}

func TestProbeRecordsPassedStepsInProbeOrder(t *testing.T) {
	for id, flags := range requiredFlags {
		t.Run(id, func(t *testing.T) {
			c := Probe(context.Background(), id, "/fixture/provider", t.TempDir(), healthyRunner(nil))
			want := []string{"version", "help", "identity"}
			if id == "codex" {
				want = append(want, "help")
			}
			for _, flag := range flags {
				want = append(want, "required-flag:"+flag)
			}
			want = append(want, "auth", "isolation")
			var got []string
			for _, s := range c.Steps {
				got = append(got, s.Step)
			}
			if c.Readiness != "ready" || !reflect.DeepEqual(got, want) || len(c.Failures) != 0 {
				t.Fatalf("got %v want %v: %+v", got, want, c)
			}
			isolation := stepNamed(t, c, "isolation").Status
			if (id == "cursor") != (isolation == "skipped") || (id != "cursor" && isolation != "passed") {
				t.Fatalf("isolation %s", isolation)
			}
			raw, _ := json.Marshal(c)
			if strings.Contains(string(raw), `"steps"`) {
				t.Fatalf("ordered steps are in-memory evidence, not saved output: %s", raw)
			}
		})
	}
}

func TestProbeWithoutExecutableRecordsNoSteps(t *testing.T) {
	c := Probe(context.Background(), "codex", "", t.TempDir(), nil)
	if c.Readiness != "missing" || len(c.Steps) != 0 || len(c.Failures) != 0 || c.Detail != "CLI executable not found." {
		t.Fatalf("%+v", c)
	}
}
