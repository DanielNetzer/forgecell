// Package onboarding observes capabilities and prepares human-reviewed recipes.
package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
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
	// Failures lists the failing probe steps; Steps keeps every evaluated step in
	// probe order, in memory only, so saved proposals and init output stay unchanged.
	Failures []Step              `json:"failures,omitempty"`
	Binding  *BindingDiagnostics `json:"binding,omitempty"`
	Steps    []Step              `json:"-"`
}

// Step is one fixed read-only probe and its bounded outcome. It never carries
// provider output: only fixed argv, an error class, the expected marker or flag,
// and the authentication state.
type Step struct {
	Step     string   `json:"step"`
	Status   string   `json:"status"`
	Reason   string   `json:"reason"`
	Argv     []string `json:"argv,omitempty"`
	Result   string   `json:"result,omitempty"`
	Expected string   `json:"expected,omitempty"`
	State    string   `json:"state,omitempty"`
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

// resultClass names why a provider call did not succeed without echoing its output.
func resultClass(r process.Result) string {
	switch {
	case r.TimedOut:
		return "timeout"
	case r.Overflow:
		return "output-limit"
	case r.Interrupted:
		return "interrupted"
	case r.Code > 0:
		return fmt.Sprintf("exit-code-%d", r.Code)
	case r.Code < 0:
		return "start-failed"
	}
	return "unsuccessful"
}

// identityMarker describes the product text the version and help output must contain.
func identityMarker(id string) string {
	switch id {
	case "codex":
		return "codex"
	case "claude-code":
		return "claude code"
	case "cursor":
		return "cursor agent or cursor cli"
	}
	return ""
}

// failureDetail summarizes failing steps, grouping missing flags into one clause.
func failureDetail(failures []Step) string {
	var parts, flags []string
	for _, s := range failures {
		if strings.HasPrefix(s.Step, "required-flag:") {
			flags = append(flags, s.Expected)
			continue
		}
		parts = append(parts, s.Step+": "+s.Reason)
	}
	if len(flags) > 0 {
		parts = append(parts, "required-flag: missing from help output: "+strings.Join(flags, ", "))
	}
	label := "Probe step"
	if len(failures) > 1 {
		label = "Probe steps"
	}
	detail := label + " failed — " + strings.Join(parts, "; ")
	if len(detail) > 600 {
		detail = detail[:600] + "…"
	}
	return detail
}

func Probe(ctx context.Context, id, executable, cwd string, run Runner) (c Candidate) {
	c = Candidate{ID: id, Executable: executable, Readiness: "missing", Detail: "CLI executable not found.", Capabilities: harness.AdapterCapabilities(id), Installed: harness.Capability{State: "missing", Reason: "CLI executable not found."}, Authentication: harness.Capability{State: "unknown", Reason: "Authentication not probed."}}
	record := func(s Step) {
		c.Steps = append(c.Steps, s)
		if s.Status == "failed" {
			c.Failures = append(c.Failures, s)
		}
	}
	defer func() {
		if c.Readiness != "ready" {
			return
		}
		if c.Capabilities.Analysis.State != "supported" {
			c.Capabilities.AnalysisControls = harness.Capability{State: "unsupported", Reason: c.Capabilities.Analysis.Reason}
			record(Step{Step: "isolation", Status: "skipped", Reason: "Read-only ticket analysis is unsupported for this harness, so isolation controls are not probed."})
			return
		}
		err := harness.ProbeAnalysisControls(ctx, id, executable, cwd, run)
		c.Capabilities.AnalysisControls = harness.Capability{State: "supported", Reason: "Existing runtime isolation controls verified; model access remains unverified."}
		step := Step{Step: "isolation", Status: "passed", Reason: "Existing runtime isolation controls verified."}
		if err != nil {
			c.Capabilities.AnalysisControls = harness.Capability{State: "blocked", Reason: err.Error()}
			reason := err.Error()
			if len(reason) > 300 {
				reason = reason[:300] + "…"
			}
			step = Step{Step: "isolation", Status: "failed", Reason: reason}
		}
		record(step)
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
	argv := func(args ...string) []string { return append([]string{executable}, args...) }
	c.Installed = harness.Capability{State: "unknown", Reason: "CLI identity has not been verified."}
	version, help := call("--version"), call("--help")
	text := strings.ToLower(version.Stdout + help.Stdout + help.Stderr)
	c.Readiness = "unsupported"
	c.Detail = "Could not verify CLI identity and required capabilities."
	for _, s := range []struct {
		name string
		args string
		r    process.Result
	}{{"version", "--version", version}, {"help", "--help", help}} {
		if s.r.OK {
			record(Step{Step: s.name, Status: "passed", Reason: "`" + s.args + "` completed.", Argv: argv(s.args)})
		} else {
			record(Step{Step: s.name, Status: "failed", Reason: "`" + s.args + "` did not complete successfully.", Argv: argv(s.args), Result: resultClass(s.r)})
		}
	}
	expected := id == "codex" && strings.Contains(text, "codex") || id == "claude-code" && strings.Contains(text, "claude code") || id == "cursor" && (strings.Contains(text, "cursor agent") || strings.Contains(text, "cursor cli"))
	switch {
	case !version.OK || !help.OK:
		record(Step{Step: "identity", Status: "skipped", Reason: "Not evaluated because version or help did not complete."})
	case expected:
		record(Step{Step: "identity", Status: "passed", Reason: "Version and help output identify the product.", Expected: identityMarker(id)})
	default:
		record(Step{Step: "identity", Status: "failed", Reason: "Version and help completed but the product marker was not found.", Expected: identityMarker(id)})
	}
	if !version.OK || !help.OK || !expected {
		c.Detail = failureDetail(c.Failures)
		return c
	}
	c.Installed = harness.Capability{State: "verified", Reason: "CLI identity verified by version and help."}
	capability, capabilityArgs := help, []string{"--help"}
	var required, authArgs []string
	switch id {
	case "codex":
		capability, capabilityArgs = call("exec", "--help"), []string{"exec", "--help"}
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
	if id == "codex" {
		if !capability.OK {
			record(Step{Step: "help", Status: "failed", Reason: "`exec --help` did not complete successfully.", Argv: argv(capabilityArgs...), Result: resultClass(capability)})
			c.Detail = failureDetail(c.Failures)
			return c
		}
		record(Step{Step: "help", Status: "passed", Reason: "`exec --help` completed.", Argv: argv(capabilityArgs...)})
	}
	for _, flag := range required {
		if strings.Contains(capability.Stdout+capability.Stderr, flag) {
			record(Step{Step: "required-flag:" + flag, Status: "passed", Reason: "Flag is listed in help output.", Argv: argv(capabilityArgs...), Expected: flag})
		} else {
			record(Step{Step: "required-flag:" + flag, Status: "failed", Reason: "Flag is not listed in help output.", Argv: argv(capabilityArgs...), Expected: flag})
		}
	}
	if len(c.Failures) > 0 {
		c.Detail = failureDetail(c.Failures)
		return c
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
	c.Detail = "Probe step auth failed — authentication status could not be verified; use the harness login command."
	step := Step{Step: "auth", Status: "failed", Reason: "Authentication status could not be verified.", Argv: argv(authArgs...), State: "unverifiable"}
	if auth.OK {
		step.Result = "unrecognized-output"
	} else {
		step.Result = resultClass(auth)
	}
	if known && !authenticated {
		c.Authentication = harness.Capability{State: "login-required", Reason: "Authenticate this harness and retry."}
		c.Readiness = "login-required"
		c.Detail = "Probe step auth failed — provider reports it is not logged in; authenticate this harness and retry."
		step.Reason, step.State, step.Result = "Provider reports it is not logged in.", "login-required", ""
	}
	if auth.OK && known && authenticated {
		c.Authentication = harness.Capability{State: "verified", Reason: "CLI reports authenticated; model access remains unverified."}
		c.Readiness = "ready"
		c.Detail = "CLI capabilities and authentication verified. Model access is checked only during a run."
		step = Step{Step: "auth", Status: "passed", Reason: "Provider reports it is authenticated.", Argv: argv(authArgs...), State: "authenticated"}
	}
	record(step)
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
