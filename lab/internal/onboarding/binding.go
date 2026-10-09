package onboarding

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"os"
	"os/exec"
)

// ProbeBinding checks the saved native adapter and provider, never an arbitrary command.
// Custom bindings remain valid execution choices, but readiness cannot be inferred safely.
func ProbeBinding(ctx context.Context, b formula.Binding, cwd string, run Runner) Candidate {
	c := Candidate{ID: b.Binding, Capabilities: harness.AdapterCapabilities(""), Installed: harness.Capability{State: "unknown", Reason: "Saved command has not been probed."}, Authentication: harness.Capability{State: "unknown", Reason: "Saved command authentication is unknown."}, Readiness: "blocked", Detail: "No coding command is bound. Propose a binding with init --harness."}
	if len(b.Command) == 0 {
		return c
	}
	if len(b.Command) != 4 || b.Command[1] != "__adapter" {
		c.Capabilities.Analysis = harness.Capability{State: "unsupported", Reason: "Custom binding has no verified read-only ticket-analysis protocol; it was preserved and not invoked."}
		c.Capabilities.AnalysisControls = c.Capabilities.Analysis
		c.Readiness = "unknown"
		c.Detail = "Custom command preserved and not executed. Readiness requires a reviewed run; doctor cannot safely probe arbitrary commands."
		return c
	}
	if b.Command[2] != b.Binding {
		c.Detail = "Saved adapter identity differs from the Formula binding. Review a rebind through init."
		return c
	}
	current, e := os.Executable()
	bound, be := exec.LookPath(b.Command[0])
	if e == nil && be == nil {
		a, ae := os.Stat(current)
		other, oe := os.Stat(bound)
		if ae == nil && oe == nil && os.SameFile(a, other) {
			return Probe(ctx, b.Binding, b.Command[3], cwd, run)
		}
	}
	c.Detail = "Saved adapter is missing or belongs to a different Forgecell executable. Propose a rebind through init --harness before running."
	return c
}

// WorkflowCapability describes whether intake can start, never end-to-end success.
func (c Candidate) WorkflowCapability() harness.Capability {
	if c.Capabilities.Analysis.State == "unsupported" {
		return c.Capabilities.Analysis
	}
	if c.Readiness == "unknown" || c.Readiness == "" {
		return harness.Capability{State: "unknown", Reason: c.Detail}
	}
	if c.Readiness != "ready" {
		return harness.Capability{State: "blocked", Reason: c.Detail}
	}
	if c.Capabilities.AnalysisControls.State == "blocked" || c.Capabilities.AnalysisControls.State == "unsupported" {
		return c.Capabilities.AnalysisControls
	}
	if c.Capabilities.Analysis.State != "supported" || c.Capabilities.AnalysisControls.State != "supported" {
		return harness.Capability{State: "unknown", Reason: "Ticket-analysis support or installed isolation controls are unknown."}
	}
	return harness.Capability{State: "conditional", Reason: "Ticket analysis can be attempted with verified controls; model access and end-to-end workflow remain unverified."}
}

func LearnBound(f formula.Formula) bool {
	for _, atom := range f.Atoms {
		if atom.Type == "learn" && len(atom.Command) > 0 {
			return true
		}
	}
	return false
}
