package onboarding

import (
	"context"
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
				text := "-c --disable --color --strict-config --skip-git-repo-check --ignore-rules Codex Claude Code Cursor Agent --sandbox --output-last-message --output-schema --ephemeral --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --disallowedTools --mode --trust"
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
				text := "-c --disable --color --strict-config --skip-git-repo-check --ignore-rules Codex Claude Code Cursor Agent --sandbox --output-last-message --output-schema --ephemeral --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --disallowedTools --mode --trust --safe-mode --strict-mcp-config --no-chrome --disable-slash-commands"
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
				text := "-c --disable --color --strict-config --skip-git-repo-check --ignore-rules Codex Claude Code --sandbox --output-last-message --output-schema --ephemeral --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --disallowedTools logged in"
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
	for _, missing := range []string{"--setting-sources", "--strict-mcp-config", "--disallowedTools"} {
		t.Run(missing, func(t *testing.T) {
			help := strings.Replace("Claude Code --print --output-format --permission-mode --tools --json-schema --no-session-persistence --setting-sources --strict-mcp-config --disallowedTools", " "+missing, "", 1)
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
