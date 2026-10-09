package harness

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnalysisNeverFallsThroughToCoding(t *testing.T) {
	request := map[string]any{"kind": "ticket-analysis", "issue": map[string]any{"title": "Fix parser"}}
	for _, id := range []string{"codex", "claude-code"} {
		t.Run(id, func(t *testing.T) {
			call, err := BuildInvocation(id, request, "/tmp/result")
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Join(call.Args, " ")
			if strings.Contains(call.Input, "Implement the supplied GitHub Issue") || !strings.Contains(call.Input, "clarification") {
				t.Fatal("analysis used coding instruction")
			}
			if id == "codex" {
				for _, want := range []string{"--sandbox read-only", "--output-schema", "--disable shell_tool", "--disable apps", "--disable plugins", "--disable hooks", "--skip-git-repo-check"} {
					if !strings.Contains(args, want) {
						t.Fatalf("missing %s: %s", want, args)
					}
				}
				if strings.Contains(args, "--ignore-user-config") {
					t.Fatal("discarded user's model settings")
				}
			} else {
				for _, want := range []string{"--safe-mode", "--tools", "--strict-mcp-config", "--no-chrome", "--json-schema"} {
					if !strings.Contains(args, want) {
						t.Fatalf("missing %s: %s", want, args)
					}
				}
			}
		})
	}
	if _, err := BuildInvocation("cursor", request, "/tmp/result"); err == nil {
		t.Fatal("Cursor analysis enabled without verified MCP/action isolation")
	}
}

func TestCustomAnalysisDoesNotInvokeCodingCommand(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	command := []string{"sh", "-c", "touch " + marker}
	r := InvokeBinding(context.Background(), command, dir, time.Second, map[string]any{"kind": "ticket-analysis"})
	if r.OK || !strings.Contains(r.Error, "Custom binding") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("custom coding command invoked as analysis")
	}
}
func TestCodexAnalysisDisablesConfiguredServers(t *testing.T) {
	dir := t.TempDir()
	provider := filepath.Join(dir, "provider")
	script := `#!/bin/sh
if [ "$1" = "--help" ]; then printf '%s' '--config -c --disable'; exit 0; fi
if [ "$1" = "exec" ] && [ "$2" = "--help" ]; then printf '%s' '--sandbox --ephemeral --color --output-last-message --output-schema --strict-config --skip-git-repo-check --ignore-rules'; exit 0; fi
if [ "$5" = "mcp" ]; then printf '[{"name":"deploy","command":"secret config must not enter evidence"},{"name":"with-dash"}]'; exit 0; fi
exit 99
`
	if err := os.WriteFile(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	call, err := BuildInvocation("codex", map[string]any{"kind": "ticket-analysis"}, "/tmp/result")
	if err != nil {
		t.Fatal(err)
	}
	args, err := constrainAnalysis(context.Background(), ExecuteOptions{ID: "codex", Executable: provider, Dir: dir}, call.Args)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, `mcp_servers.deploy.enabled=false`) || !strings.Contains(joined, `mcp_servers.with-dash.enabled=false`) || args[len(args)-1] != "-" || strings.Contains(joined, "secret config") {
		t.Fatalf("bad analysis controls: %v", args)
	}
}

func TestReportingUsesRuntimeProjectBlocker(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex/config.toml"), []byte("private model selection"), 0600); err != nil {
		t.Fatal(err)
	}
	err := ProbeAnalysisControls(context.Background(), "codex", "/never-invoked", dir, func(context.Context, process.Options) process.Result {
		t.Fatal("probed despite project blocker")
		return process.Result{}
	})
	if err == nil || !strings.Contains(err.Error(), "project-scoped codex configuration") {
		t.Fatal(err)
	}
	_, runtimeErr := constrainAnalysis(context.Background(), ExecuteOptions{ID: "codex", Executable: "/never-invoked", Dir: dir}, []string{"-"})
	if runtimeErr == nil || runtimeErr.Error() != err.Error() {
		t.Fatalf("%v != %v", runtimeErr, err)
	}
}

// Fixed help fixtures keep candidate regression assertions separate from the builder.
const codexRootHelp = "Options: --config <KEY=VALUE> -c <KEY=VALUE> --disable <FEATURE>"
const codexExecHelp = "Options: --sandbox <MODE> --ephemeral --color <WHEN> --output-last-message <FILE> --output-schema <FILE> --strict-config --skip-git-repo-check --ignore-rules"

func TestCodexAnalysisRequiresInstalledFlags(t *testing.T) {
	for _, missing := range strings.Fields("-c --disable --sandbox --ephemeral --color --output-last-message --output-schema --strict-config --skip-git-repo-check --ignore-rules") {
		t.Run(missing, func(t *testing.T) {
			run := func(_ context.Context, o process.Options) process.Result {
				args := strings.Join(o.Argv[1:], " ")
				switch args {
				case "--help":
					return process.Result{OK: true, Stdout: strings.ReplaceAll(codexRootHelp+" ", missing+" ", "")}
				case "exec --help":
					return process.Result{OK: true, Stdout: strings.ReplaceAll(codexExecHelp+" ", missing+" ", "")}
				default:
					t.Fatal("enumeration or model execution before flag verification: " + args)
					return process.Result{}
				}
			}
			err := ProbeAnalysisControls(context.Background(), "codex", "/fixture/provider", t.TempDir(), run)
			if err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("missing %s reported supported: %v", missing, err)
			}
		})
	}
}

func TestCodexAnalysisRejectsWrongHelpEvidence(t *testing.T) {
	for _, mode := range []string{"wrong surface", "longer option"} {
		t.Run(mode, func(t *testing.T) {
			run := func(_ context.Context, o process.Options) process.Result {
				switch strings.Join(o.Argv[1:], " ") {
				case "--help":
					return process.Result{OK: true, Stdout: codexRootHelp + " --strict-config"}
				case "exec --help":
					replacement := ""
					if mode == "longer option" {
						replacement = "--strict-config-example"
					}
					return process.Result{OK: true, Stdout: strings.ReplaceAll(codexExecHelp, "--strict-config", replacement)}
				default:
					t.Fatal("enumerated configuration before verifying controls")
					return process.Result{}
				}
			}
			err := ProbeAnalysisControls(context.Background(), "codex", "/fixture/provider", t.TempDir(), run)
			if err == nil || !strings.Contains(err.Error(), "--strict-config in exec --help") {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexAnalysisUsesBoundedHelpSurfaces(t *testing.T) {
	for _, failed := range []string{"", "--help", "exec --help"} {
		t.Run("failed="+failed, func(t *testing.T) {
			var calls []string
			run := func(_ context.Context, o process.Options) process.Result {
				args := strings.Join(o.Argv[1:], " ")
				calls = append(calls, args)
				if o.Timeout <= 0 || o.Timeout > 10*time.Second || o.MaxOutputBytes <= 0 || o.MaxOutputBytes > 1_000_000 || len(o.Stdin) != 0 {
					t.Fatalf("unbounded probe: %+v", o)
				}
				if args == failed {
					return process.Result{OK: false, Stdout: "private detail"}
				}
				switch args {
				case "--help":
					return process.Result{OK: true, Stdout: codexRootHelp}
				case "exec --help":
					return process.Result{OK: true, Stdout: codexExecHelp}
				case "--disable plugins --disable apps mcp list --json":
					return process.Result{OK: true, Stdout: "[]"}
				default:
					t.Fatal("unexpected probe: " + args)
					return process.Result{}
				}
			}
			err := ProbeAnalysisControls(context.Background(), "codex", "/fixture/provider", t.TempDir(), run)
			if failed == "" {
				if err != nil || len(calls) != 3 {
					t.Fatalf("%v: %v", calls, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private detail") {
				t.Fatalf("%v: %v", calls, err)
			}
		})
	}
}
