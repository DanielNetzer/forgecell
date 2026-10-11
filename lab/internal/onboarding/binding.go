package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// BindingState is one precise reason the saved native binding cannot run as saved:
// launcher-missing, launcher-different, adapter-mismatch or provider-missing.
type BindingState struct {
	State  string `json:"state"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail"`
}

// BindingDiagnostics reports the saved native command by path. The launcher is
// inspected but never executed.
type BindingDiagnostics struct {
	FormulaBinding        string         `json:"formulaBinding"`
	Adapter               string         `json:"adapter"`
	Launcher              string         `json:"launcher"`
	LauncherVersion       string         `json:"launcherVersion"`
	LauncherVersionSource string         `json:"launcherVersionSource"`
	RunningLauncher       string         `json:"runningLauncher,omitempty"`
	RunningVersion        string         `json:"runningVersion,omitempty"`
	Provider              string         `json:"provider"`
	ResolvedProvider      string         `json:"resolvedProvider,omitempty"`
	States                []BindingState `json:"states"`
}

// WithRunningVersion records the running release. When the saved launcher is the
// running executable, its version is the running version.
func (d *BindingDiagnostics) WithRunningVersion(version string) {
	d.RunningVersion = version
	for _, s := range d.States {
		if s.State == "launcher-missing" || s.State == "launcher-different" {
			return
		}
	}
	if version != "" {
		d.LauncherVersion, d.LauncherVersionSource = version, "running executable"
	}
}

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
	d := &BindingDiagnostics{FormulaBinding: b.Binding, Adapter: b.Command[2], Launcher: b.Command[0], LauncherVersion: "unknown", LauncherVersionSource: "unavailable", Provider: b.Command[3], States: []BindingState{}}
	c.Binding = d
	current, currentErr := os.Executable()
	if currentErr == nil {
		d.RunningLauncher = current
	}
	launcher, launcherErr := exec.LookPath(b.Command[0])
	manifestDir := ""
	if launcherErr == nil {
		manifestDir = filepath.Dir(launcher)
	} else if strings.ContainsRune(b.Command[0], filepath.Separator) {
		manifestDir = filepath.Dir(b.Command[0])
	}
	if manifestDir != "" {
		d.LauncherVersion, d.LauncherVersionSource = launcherVersion(manifestDir)
	}
	launcherOK := false
	switch {
	case launcherErr != nil:
		d.States = append(d.States, BindingState{State: "launcher-missing", Path: b.Command[0], Detail: fmt.Sprintf("Saved launcher %s was not found or is not executable.", b.Command[0])})
	case currentErr != nil:
		d.States = append(d.States, BindingState{State: "launcher-different", Path: launcher, Detail: fmt.Sprintf("Saved launcher %s cannot be compared because the running executable could not be determined; it was not executed.", launcher)})
	default:
		a, ae := os.Stat(current)
		other, oe := os.Stat(launcher)
		if ae == nil && oe == nil && os.SameFile(a, other) {
			launcherOK = true
		} else {
			d.States = append(d.States, BindingState{State: "launcher-different", Path: launcher, Detail: fmt.Sprintf("Saved launcher %s is a different Forgecell executable than the running %s; it was not executed.", launcher, current)})
		}
	}
	mismatch := b.Command[2] != b.Binding
	if mismatch {
		d.States = append(d.States, BindingState{State: "adapter-mismatch", Detail: fmt.Sprintf("Saved adapter %q differs from the Formula binding %q (launcher %s, provider %s).", b.Command[2], b.Binding, b.Command[0], b.Command[3])})
	}
	provider, providerErr := exec.LookPath(b.Command[3])
	if providerErr != nil {
		d.States = append(d.States, BindingState{State: "provider-missing", Path: b.Command[3], Detail: fmt.Sprintf("Bound provider %s was not found or is not an executable file.", b.Command[3])})
		c.Installed = harness.Capability{State: "missing", Reason: fmt.Sprintf("Bound provider %s was not found or is not an executable file.", b.Command[3])}
	} else {
		d.ResolvedProvider = provider
	}
	if !mismatch && providerErr == nil {
		probed := Probe(ctx, b.Binding, provider, cwd, run)
		if launcherOK {
			probed.Binding = d
			return probed
		}
		c.Executable, c.Installed, c.Authentication, c.Capabilities = probed.Executable, probed.Installed, probed.Authentication, probed.Capabilities
		c.Failures, c.Steps = probed.Failures, probed.Steps
		c.Detail = bindingDetail(d.States) + fmt.Sprintf(" The bound provider was probed at its own path: %s — %s", probed.Readiness, probed.Detail)
		return c
	}
	c.Detail = bindingDetail(d.States)
	return c
}

func bindingDetail(states []BindingState) string {
	var parts []string
	for _, s := range states {
		parts = append(parts, s.State+": "+s.Detail)
	}
	return strings.Join(parts, " ")
}

const maxManifestBytes = 4096

var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`)

// launcherVersion reads the release manifest beside a saved launcher. It never
// executes the launcher and reports "unknown" unless the manifest is a small
// regular file with a release version.
func launcherVersion(dir string) (version, source string) {
	file := filepath.Join(dir, "manifest.json")
	st, err := os.Lstat(file)
	if err != nil || !st.Mode().IsRegular() || st.Size() > maxManifestBytes {
		return "unknown", "unavailable"
	}
	f, err := os.Open(file)
	if err != nil {
		return "unknown", "unavailable"
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	var m struct {
		Version string `json:"version"`
	}
	if err != nil || len(raw) > maxManifestBytes || json.Unmarshal(raw, &m) != nil || !releaseVersion.MatchString(m.Version) {
		return "unknown", "unavailable"
	}
	return m.Version, "manifest.json"
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

func nativeAdapter(b formula.Binding) bool {
	return len(b.Command) == 4 && b.Command[1] == "__adapter"
}

func roleOperation(role formula.HarnessRole) string {
	switch role {
	case formula.AnalysisHarness:
		return "read-only ticket analysis"
	case formula.MetaHarness:
		return "meta learning"
	}
	return "coding"
}

// roleCapability is the adapter's static support for a role's operation. It
// executes nothing; probing is separate (InspectRoles).
func roleCapability(role formula.HarnessRole, b formula.Binding) harness.Capability {
	caps := harness.AdapterCapabilities("")
	if nativeAdapter(b) {
		caps = harness.AdapterCapabilities(b.Command[2])
	} else {
		caps.Analysis = harness.Capability{State: "unsupported", Reason: "Custom binding has no verified read-only ticket-analysis protocol; it was preserved and not invoked."}
	}
	switch role {
	case formula.AnalysisHarness:
		return caps.Analysis
	case formula.MetaHarness:
		return caps.Meta
	}
	return caps.Coding
}

// ValidateRoleBindings rejects a declared role whose adapter cannot perform the
// role's operation, quoting the adapter's own reason, and any role/Atom conflict.
// The default `harness` binding is not judged here: custom defaults stay valid.
func ValidateRoleBindings(f formula.Formula) error {
	for _, role := range formula.HarnessRoles {
		b, declared := f.Harnesses[string(role)]
		if !declared {
			continue
		}
		if c := roleCapability(role, b); c.State == "unsupported" {
			return fmt.Errorf("harnesses.%s: binding %q cannot be used for %s: %s", role, b.Binding, roleOperation(role), c.Reason)
		}
	}
	return f.ValidateHarnessRoles()
}

// RoleReadiness is one role's current readiness, probed without invoking a model.
type RoleReadiness struct {
	Role       string             `json:"role"`
	Binding    string             `json:"binding"`
	Source     string             `json:"source"`
	Ready      bool               `json:"ready"`
	Readiness  string             `json:"readiness"`
	Capability harness.Capability `json:"capability"`
	Detail     string             `json:"detail"`
	// Candidate is the probe result, for callers that derive next commands.
	Candidate   Candidate `json:"-"`
	NextCommand string    `json:"nextCommand,omitempty"`
}

// InspectRoles probes each configured role's binding. Analysis and coding always
// appear; meta appears when a role override or a bound learn Atom configures it.
func InspectRoles(ctx context.Context, f formula.Formula, cwd string, run Runner) []RoleReadiness {
	probed := map[string]Candidate{}
	probe := func(b formula.Binding) Candidate {
		key := b.Binding + "\x00" + strings.Join(b.Command, "\x00")
		if c, ok := probed[key]; ok {
			return c
		}
		c := ProbeBinding(ctx, b, cwd, run).CompleteUnknowns()
		probed[key] = c
		return c
	}
	var roles []RoleReadiness
	for _, role := range formula.HarnessRoles {
		b, override := f.RoleBinding(role)
		source := "default"
		if override {
			source = "override"
		} else if role == formula.MetaHarness {
			bound := false
			for _, a := range f.Atoms {
				if a.Type == "learn" && len(a.Command) > 0 {
					b, bound = formula.Binding{Binding: a.Binding, Command: a.Command, TimeoutMS: a.TimeoutMS}, true
					break
				}
			}
			if !bound {
				continue
			}
			source = "learn-atom"
		}
		c := probe(b)
		item := RoleReadiness{Role: string(role), Binding: b.Binding, Source: source, Readiness: c.Readiness, Detail: c.Detail, Candidate: c}
		switch role {
		case formula.AnalysisHarness:
			item.Capability = c.WorkflowCapability()
			item.Ready = item.Capability.State == "conditional"
		case formula.MetaHarness:
			item.Capability = c.Capabilities.Meta
			item.Ready = c.Readiness == "ready" && item.Capability.State == "supported"
		default:
			item.Capability = c.Capabilities.Coding
			item.Ready = c.Readiness == "ready" && item.Capability.State == "supported"
		}
		roles = append(roles, item)
	}
	return roles
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
