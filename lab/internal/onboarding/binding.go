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

// MetaReport records declarations and observations, never verified containment.
type MetaReport struct {
	Command        []string           `json:"command"`
	Binding        string             `json:"binding"`
	TimeoutMS      int                `json:"timeoutMs"`
	Candidate      Candidate          `json:"candidate"`
	Capability     harness.Capability `json:"capability"`
	Containment    harness.Capability `json:"containment"`
	Restrictions   string             `json:"restrictions"`
	EvidenceInputs string             `json:"evidenceInputs"`
	NextAction     string             `json:"nextAction"`
	BeforeYAML     string             `json:"beforeYaml,omitempty"`
}

func DescribeMeta(command []string, binding string, timeout int, candidate Candidate) MetaReport {
	c := candidate.CompleteUnknowns()
	r := MetaReport{Command: command, Binding: binding, TimeoutMS: timeout, Candidate: c, Capability: c.Capabilities.Meta,
		Containment:    harness.Capability{State: "unknown", Reason: "Installed containment and integrations are unverified; coding permissions do not establish meta permissions."},
		Restrictions:   "BYO commands retain local process permissions. No read-only containment is enforced by Forgecell. Prompt restrictions are declarations, not a sandbox.",
		EvidenceInputs: "A separate learn invocation selects eligible finished ticket Molecule ledgers from the same intact approved Formula snapshot: Atom outcomes, verification evidence and limitations. Pending attempts, mixed snapshots, damaged evidence and uncertain publication are rejected.",
		NextAction:     "Opt in with init --meta-harness ID or init --meta-command '[\"/absolute/command\",\"arg\"]'. Review exact before/after YAML and approve separately. Activation invokes no model. Later learn MOLECULE_ID saves only a pending suggestion; Formula mutation requires separate human suggestion approval."}
	if len(command) == 0 {
		r.Capability = harness.Capability{State: "unavailable", Reason: "No separate learn command is bound."}
		if binding != "" {
			r.Capability = c.Capabilities.Meta
			if r.Capability.State == "supported" {
				r.Capability = harness.Capability{State: "blocked", Reason: "Installation/authentication readiness is insufficient. " + c.Detail}
			}
		}
		return r
	}
	if len(command) == 4 && command[1] == "__adapter" && command[2] == binding {
		switch binding {
		case "codex":
			r.Restrictions = "Adapter requests read-only sandbox and approval_policy=never; meta does not receive ticket-analysis tool/MCP/plugin isolation or its temporary working directory. Installed enforcement is unverified. No-edit and supplied-evidence-only prompts are declarations."
		case "claude-code":
			r.Restrictions = "Adapter requests dontAsk, disables built-in tools and MCP servers with empty tools and strict empty MCP configuration. Meta retains supplied working directory and lacks analysis safe-mode controls. Installed enforcement is unverified; prompt restrictions are declarations."
		case "cursor":
			r.Restrictions = "Adapter requests sandbox enabled and ask mode. MCP/plugin isolation is unverified; ask mode is not verified read-only containment. Meta retains supplied working directory; prompt restrictions are declarations."
		}
	} else {
		r.Capability = harness.Capability{State: "unknown", Reason: "Custom meta protocol and containment are unknown; command was not probed or invoked."}
	}
	return r
}

func InspectMeta(ctx context.Context, f formula.Formula, cwd string, run Runner) MetaReport {
	for _, a := range f.Atoms {
		if a.Type == "learn" {
			return DescribeMeta(a.Command, a.Binding, a.TimeoutMS, ProbeBinding(ctx, formula.Binding{Binding: a.Binding, Command: a.Command}, cwd, run))
		}
	}
	return DescribeMeta(nil, "", 0, Candidate{})
}
