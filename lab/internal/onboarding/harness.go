// Package onboarding observes capabilities and prepares human-reviewed recipes.
package onboarding

import (
	"context"
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Candidate struct {
	ID             string               `json:"id"`
	Executable     string               `json:"executable,omitempty"`
	Readiness      string               `json:"readiness"`
	Detail         string               `json:"detail"`
	Project        bool                 `json:"project"`
	Capabilities   harness.Capabilities `json:"capabilities"`
	Installed      harness.Capability   `json:"installed"`
	Authentication harness.Capability   `json:"authentication"`
}
type Selection struct {
	ID     string `json:"id,omitempty"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}
type Runner func(context.Context, process.Options) process.Result

func Select(candidates []Candidate, preferred, existing string, env map[string]string) Selection {
	choose := func(id, reason string) Selection {
		s := Selection{ID: id, Status: "blocked", Reason: reason}
		for _, c := range candidates {
			if c.ID == id && c.Readiness == "ready" {
				s.Status = "ready"
			}
		}
		return s
	}
	if preferred != "" {
		return choose(preferred, "Explicit harness choice.")
	}
	if existing != "" {
		return choose(existing, "Preserving the approved binding.")
	}
	var current []string
	if env["CODEX_THREAD_ID"] != "" {
		current = append(current, "codex")
	}
	if env["CLAUDECODE"] != "" {
		current = append(current, "claude-code")
	}
	if env["CURSOR_AGENT"] != "" || env["TERM_PROGRAM"] == "cursor" {
		current = append(current, "cursor")
	}
	if len(current) == 1 {
		return choose(current[0], "Detected current harness environment.")
	}
	if len(current) > 1 {
		return Selection{Status: "ambiguous", Reason: "Conflicting current harness signals; choose a harness."}
	}
	var project, ready []string
	for _, c := range candidates {
		if c.Project {
			project = append(project, c.ID)
		}
		if c.Readiness == "ready" {
			ready = append(ready, c.ID)
		}
	}
	if len(project) == 1 {
		return choose(project[0], "Repository configuration identifies a harness.")
	}
	if len(project) > 1 {
		return Selection{Status: "ambiguous", Reason: "Multiple repository harness configurations; choose a harness."}
	}
	if len(ready) == 1 {
		return choose(ready[0], "Only one installed harness passed readiness checks.")
	}
	if len(ready) > 1 {
		return Selection{Status: "ambiguous", Reason: "Multiple ready harnesses; choose a default."}
	}
	return Selection{Status: "missing", Reason: "Install or authenticate a supported harness, then retry."}
}
func Probe(ctx context.Context, id, executable, cwd string, run Runner) (c Candidate) {
	c = Candidate{ID: id, Executable: executable, Readiness: "missing", Detail: "CLI executable not found.", Capabilities: harness.AdapterCapabilities(id), Installed: harness.Capability{State: "missing", Reason: "CLI executable not found."}, Authentication: harness.Capability{State: "unknown", Reason: "Authentication not probed."}}
	defer func() {
		if c.Readiness != "ready" {
			return
		}
		if c.Capabilities.Analysis.State != "supported" {
			c.Capabilities.AnalysisControls = harness.Capability{State: "unsupported", Reason: c.Capabilities.Analysis.Reason}
			return
		}
		err := harness.ProbeAnalysisControls(ctx, id, executable, cwd, run)
		c.Capabilities.AnalysisControls = harness.Capability{State: "supported", Reason: "Existing runtime isolation controls verified; model access remains unverified."}
		if err != nil {
			c.Capabilities.AnalysisControls = harness.Capability{State: "blocked", Reason: err.Error()}
		}
	}()
	if executable == "" {
		return c
	}
	if run == nil {
		run = process.Run
	}
	call := func(args ...string) process.Result {
		return run(ctx, process.Options{Argv: append([]string{executable}, args...), Dir: cwd, Stdin: []byte{}, Timeout: 5 * time.Second, MaxOutputBytes: 200000})
	}
	c.Installed = harness.Capability{State: "unknown", Reason: "CLI identity has not been verified."}
	version, help := call("--version"), call("--help")
	text := strings.ToLower(version.Stdout + help.Stdout + help.Stderr)
	c.Readiness = "unsupported"
	c.Detail = "Could not verify CLI identity and required capabilities."
	expected := id == "codex" && strings.Contains(text, "codex") || id == "claude-code" && strings.Contains(text, "claude code") || id == "cursor" && (strings.Contains(text, "cursor agent") || strings.Contains(text, "cursor cli"))
	if !version.OK || !help.OK || !expected {
		return c
	}
	c.Installed = harness.Capability{State: "verified", Reason: "CLI identity verified by version and help."}
	capability := help
	var required, authArgs []string
	switch id {
	case "codex":
		capability = call("exec", "--help")
		required = []string{"--sandbox", "--output-last-message", "--output-schema", "--ephemeral"}
		authArgs = []string{"login", "status"}
	case "claude-code":
		required = []string{"--print", "--output-format", "--permission-mode", "--tools", "--json-schema", "--no-session-persistence", "--setting-sources", "--strict-mcp-config", "--mcp-config", "--allowedTools", "--disallowedTools"}
		authArgs = []string{"auth", "status", "--json"}
	case "cursor":
		required = []string{"--print", "--output-format", "--sandbox", "--mode", "--trust"}
		authArgs = []string{"status", "--format", "json"}
	default:
		return c
	}
	if !capability.OK {
		return c
	}
	for _, flag := range required {
		if !strings.Contains(capability.Stdout+capability.Stderr, flag) {
			return c
		}
	}
	auth := call(authArgs...)
	known, authenticated := false, false
	if id == "codex" {
		status := strings.ToLower(auth.Stdout + auth.Stderr)
		known = strings.Contains(status, "logged in")
		authenticated = known && !strings.Contains(status, "not logged in")
	} else {
		var value map[string]any
		if json.Unmarshal([]byte(auth.Stdout), &value) == nil {
			key := "loggedIn"
			if id == "cursor" {
				key = "isAuthenticated"
				if _, ok := value[key]; !ok {
					key = "authenticated"
				}
			}
			authenticated, known = value[key].(bool)
		}
	}
	c.Authentication = harness.Capability{State: "unknown", Reason: "Authentication could not be verified."}
	c.Readiness = "unknown"
	c.Detail = "Authentication could not be verified; use the harness login command."
	if known && !authenticated {
		c.Authentication = harness.Capability{State: "login-required", Reason: "Authenticate this harness and retry."}
		c.Readiness = "login-required"
		c.Detail = "Authenticate this harness and retry."
	}
	if auth.OK && known && authenticated {
		c.Authentication = harness.Capability{State: "verified", Reason: "CLI reports authenticated; model access remains unverified."}
		c.Readiness = "ready"
		c.Detail = "CLI capabilities and authentication verified. Model access is checked only during a run."
	}
	return c
}
func Discover(ctx context.Context, cwd string, run Runner) []Candidate {
	specs := []struct {
		id          string
		bins, paths []string
	}{{"codex", []string{"codex"}, []string{".codex"}}, {"claude-code", []string{"claude"}, []string{".claude", "CLAUDE.md"}}, {"cursor", []string{"cursor-agent", "agent"}, []string{".cursor", ".cursorrules"}}}
	var candidates []Candidate
	for _, s := range specs {
		var executable string
		for _, name := range s.bins {
			if p, err := exec.LookPath(name); err == nil {
				executable = p
				break
			}
		}
		c := Probe(ctx, s.id, executable, cwd, run)
		for _, p := range s.paths {
			if _, err := os.Stat(filepath.Join(cwd, p)); err == nil {
				c.Project = true
			}
		}
		candidates = append(candidates, c)
	}
	return candidates
}

// CompleteUnknowns retains absent evidence as unknown rather than inferring support.
func (c Candidate) CompleteUnknowns() Candidate {
	unknown := harness.Capability{State: "unknown", Reason: "Capability evidence was not recorded."}
	for _, value := range []*harness.Capability{&c.Capabilities.Coding, &c.Capabilities.Analysis, &c.Capabilities.Meta, &c.Capabilities.AnalysisControls, &c.Capabilities.ModelAccess, &c.Installed, &c.Authentication} {
		if value.State == "" {
			*value = unknown
		}
	}
	return c
}
