package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// Probe capabilities without making a model request. Preserve provider auth and
// model selection; never clone secrets into the temporary evidence directory.
func constrainAnalysis(ctx context.Context, o ExecuteOptions, args []string) ([]string, error) {
	// Moving analysis outside the repository must not silently lose a project-
	// scoped model/provider selection. Until that selection is explicitly bound,
	// retain the binding and stop instead of falling back to another model.
	return analysisControls(ctx, o, args, process.Run)
}

// ProbeAnalysisControls reuses the runtime isolation checks without invoking a model.
func ProbeAnalysisControls(ctx context.Context, id, executable, dir string, run func(context.Context, process.Options) process.Result) error {
	if run == nil {
		run = process.Run
	}
	invocation, err := BuildInvocation(id, map[string]any{"kind": "ticket-analysis"}, "capability-output")
	if err != nil {
		return err
	}
	_, err = analysisControls(ctx, ExecuteOptions{ID: id, Executable: executable, Dir: dir}, invocation.Args, run)
	return err
}

func analysisControls(ctx context.Context, o ExecuteOptions, args []string, run func(context.Context, process.Options) process.Result) ([]string, error) {
	home, _ := os.UserHomeDir()
	for dir := o.Dir; dir != "" && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if dir == home {
			break
		}
		for _, relative := range []string{".codex/config.toml", ".claude/settings.json", ".claude/settings.local.json"} {
			if o.ID == "codex" && !strings.HasPrefix(relative, ".codex/") || o.ID == "claude-code" && !strings.HasPrefix(relative, ".claude/") {
				continue
			}
			if _, err := os.Lstat(filepath.Join(dir, relative)); err == nil {
				return nil, fmt.Errorf("project-scoped %s configuration needs an explicitly preserved analysis binding; no fallback model was invoked", o.ID)
			} else if !os.IsNotExist(err) {
				return nil, err
			}
		}
	}
	help := run(ctx, process.Options{Argv: []string{o.Executable, "--help"}, Dir: o.Dir, Env: o.Env, Stdin: []byte{}, Timeout: 10 * time.Second, MaxOutputBytes: 64000})
	if !help.OK {
		return nil, fmt.Errorf("cannot verify %s analysis controls", o.ID)
	}
	if o.ID == "claude-code" {
		for _, flag := range []string{"--safe-mode", "--tools", "--strict-mcp-config", "--json-schema", "--no-chrome", "--disable-slash-commands"} {
			if !strings.Contains(help.Stdout, flag) {
				return nil, fmt.Errorf("Claude Code lacks required analysis control %s; update the installed harness", flag)
			}
		}
		return args, nil
	}
	if o.ID != "codex" {
		return nil, fmt.Errorf("unsupported ticket-analysis provider")
	}
	// Root options and exec options have different help surfaces. Derive the
	// requirements from the same invocation used at runtime, so reporting cannot
	// omit a newly added analysis flag. Help confirms syntax, not model access.
	invocation, err := BuildInvocation("codex", map[string]any{"kind": "ticket-analysis"}, "capability-output")
	if err != nil {
		return nil, err
	}
	execHelp := run(ctx, process.Options{Argv: []string{o.Executable, "exec", "--help"}, Dir: o.Dir, Env: o.Env, Stdin: []byte{}, Timeout: 10 * time.Second, MaxOutputBytes: 64000})
	if !execHelp.OK {
		return nil, fmt.Errorf("cannot verify Codex analysis controls from exec --help; analysis not started")
	}
	seen := map[string]bool{}
	for _, flag := range invocation.Args {
		if flag == "-" || !strings.HasPrefix(flag, "-") || seen[flag] {
			continue
		}
		seen[flag] = true
		surface, text := "exec --help", execHelp.Stdout
		if flag == "-c" || flag == "--disable" {
			surface, text = "--help", help.Stdout
		}
		// A longer similarly named option does not establish support.
		pattern := `(^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(flag) + `($|[^A-Za-z0-9_-])`
		if !regexp.MustCompile(pattern).MatchString(text) {
			return nil, fmt.Errorf("Codex lacks required analysis control %s in %s; analysis not started", flag, surface)
		}
	}
	// Listing configured servers reads configuration only. Output may contain
	// configuration values, so only server names are parsed and never logged.
	listed := run(ctx, process.Options{Argv: []string{o.Executable, "--disable", "plugins", "--disable", "apps", "mcp", "list", "--json"}, Dir: o.Dir, Env: o.Env, Stdin: []byte{}, Timeout: 10 * time.Second, MaxOutputBytes: 1_000_000})
	if !listed.OK {
		return nil, fmt.Errorf("cannot enumerate Codex MCP configuration; analysis not started")
	}
	var servers []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(listed.Stdout), &servers); err != nil || servers == nil {
		return nil, fmt.Errorf("invalid Codex MCP configuration listing; analysis not started")
	}
	extra := []string{}
	for _, s := range servers {
		if !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(s.Name) {
			return nil, fmt.Errorf("invalid MCP server name")
		}
		extra = append(extra, "-c", "mcp_servers."+s.Name+".enabled=false")
	}
	// Codex's stdin marker stays last so every server override is an option.
	out := append([]string{}, args[:len(args)-1]...)
	out = append(out, extra...)
	out = append(out, "-")
	return out, nil
}
