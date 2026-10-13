package onboarding

import (
	"context"
	"errors"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// BindingState is one precise fact about the saved native binding: launcher-missing,
// launcher-incompatible or launcher-unknown (the adapter protocol blocks the run),
// launcher-compatible (informational: a different build with the same adapter
// protocol serves the binding), adapter-mismatch or provider-missing.
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
	LauncherProtocol      string         `json:"launcherProtocol,omitempty"`
	ResolvedLauncher      string         `json:"resolvedLauncher,omitempty"`
	RunningLauncher       string         `json:"runningLauncher,omitempty"`
	RunningVersion        string         `json:"runningVersion,omitempty"`
	RunningProtocol       string         `json:"runningProtocol,omitempty"`
	Provider              string         `json:"provider"`
	ResolvedProvider      string         `json:"resolvedProvider,omitempty"`
	States                []BindingState `json:"states"`
}

// WithRunningVersion records the running release. When the saved launcher is the
// running executable, its version is the running version.
func (d *BindingDiagnostics) WithRunningVersion(version string) {
	d.RunningVersion = version
	for _, s := range d.States {
		if strings.HasPrefix(s.State, "launcher-") {
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
		if b.AnalysisWrapper != nil {
			// Inspection never reads, hashes or runs the wrapper; intake revalidates it.
			c.Capabilities.Analysis = harness.Capability{State: "unknown", Reason: "An analysis wrapper is declared (" + b.AnalysisWrapper.Manifest + ") and unverified until intake revalidates its pinned manifest, identities and controls. Declared controls are wrapper claims; it was not executed."}
			c.Capabilities.AnalysisControls = c.Capabilities.Analysis
			c.Readiness = "unknown"
			c.Detail = "Custom command preserved and not executed. An analysis wrapper is declared but unverified until intake revalidation; doctor cannot verify it or its containment."
			return c
		}
		c.Capabilities.Analysis = harness.Capability{State: "unsupported", Reason: "Custom binding has no verified read-only ticket-analysis protocol; it was preserved and not invoked."}
		c.Capabilities.AnalysisControls = c.Capabilities.Analysis
		c.Readiness = "unknown"
		c.Detail = "Custom command preserved and not executed. Readiness requires a reviewed run; doctor cannot safely probe arbitrary commands."
		return c
	}
	d := &BindingDiagnostics{FormulaBinding: b.Binding, Adapter: b.Command[2], Launcher: b.Command[0], LauncherVersion: "unknown", LauncherVersionSource: "unavailable", LauncherProtocol: "unknown", RunningProtocol: harness.AdapterProtocolVersion, Provider: b.Command[3], States: []BindingState{}}
	c.Binding = d
	id := harness.ResolveLauncher(b.Command[0])
	d.RunningLauncher = id.Running
	if v := harness.RunningVersion(); v != "unknown" {
		d.RunningVersion = v
	}
	if id.Managed {
		d.ResolvedLauncher = id.Executable
	}
	if id.Manifest.Found {
		d.LauncherVersion, d.LauncherVersionSource = id.Manifest.Version, "manifest.json"
	}
	d.LauncherProtocol = id.Manifest.Protocol
	if id.Class == harness.LauncherRunning {
		d.LauncherProtocol = harness.AdapterProtocolVersion
	} else {
		statePath := id.Path
		if statePath == "" {
			statePath = b.Command[0]
		}
		d.States = append(d.States, BindingState{State: "launcher-" + id.Class, Path: statePath, Detail: id.Describe(b.Command[2])})
	}
	launcherOK := id.Runnable()
	mismatch := b.Command[2] != b.Binding
	if mismatch {
		d.States = append(d.States, BindingState{State: "adapter-mismatch", Detail: fmt.Sprintf("Saved adapter %q differs from the Formula binding %q (launcher %s, provider %s).", b.Command[2], b.Binding, b.Command[0], b.Command[3])})
	}
	var provider string
	var providerErr error
	if !filepath.IsAbs(b.Command[3]) {
		// A bare or relative provider would resolve against the working directory or PATH.
		detail := fmt.Sprintf("Bound provider %q must be an absolute path; it was not resolved or run.", b.Command[3])
		providerErr = errors.New(detail)
		d.States = append(d.States, BindingState{State: "provider-not-absolute", Path: b.Command[3], Detail: detail})
		c.Installed = harness.Capability{State: "blocked", Reason: detail}
	} else if provider, providerErr = exec.LookPath(b.Command[3]); providerErr != nil {
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
// The default `harness` binding is not judged for its operation: custom defaults
// stay valid. A role that binds extensions is judged on whichever binding it
// resolves to, including the default.
func ValidateRoleBindings(f formula.Formula) error {
	if err := validateAbsoluteNative(f); err != nil {
		return err
	}
	for _, role := range formula.HarnessRoles {
		b, declared := f.Harnesses[string(role)]
		if !declared {
			continue
		}
		if c := roleCapability(role, b); c.State == "unsupported" {
			return fmt.Errorf("harnesses.%s: binding %q cannot be used for %s: %s", role, b.Binding, roleOperation(role), c.Reason)
		}
	}
	for _, role := range formula.HarnessRoles {
		names := f.RoleExtensions(role)
		if len(names) == 0 {
			continue
		}
		b, declared := f.RoleBinding(role)
		if c := harness.BindingExtensionCapability(b.Command); c.State == "unsupported" {
			where := "harness (default binding, " + string(role) + " role)"
			if declared {
				where = "harnesses." + string(role)
			}
			return fmt.Errorf("%s: binding %q cannot be used with extensions %s: %s", where, b.Binding, strings.Join(names, ", "), c.Reason)
		}
	}
	if err := validateReviewBinding(f); err != nil {
		return err
	}
	return f.ValidateHarnessRoles()
}

// requireAbsoluteNative refuses a native [launcher, "__adapter", adapter, provider]
// command whose launcher or provider is not an absolute path: either would resolve
// against the working directory or PATH. key is the command's Formula YAML key
// path, so the error names the exact element to change, with the absolute path it
// currently resolves to when it does. Any other command is not judged.
func requireAbsoluteNative(key string, command []string) error {
	if len(command) != 4 || command[1] != "__adapter" {
		return nil
	}
	for _, part := range []struct {
		index int
		name  string
	}{{0, "launcher"}, {3, "provider"}} {
		if path := command[part.index]; !filepath.IsAbs(path) {
			return fmt.Errorf("%s[%d]: native %s %q must be an absolute path%s", key, part.index, part.name, path, absoluteHint(path))
		}
	}
	return nil
}

// absoluteHint names the absolute path a non-absolute command element resolves to
// here. It looks the path up and runs nothing; the Formula is never rewritten from it.
func absoluteHint(path string) string {
	if found, err := exec.LookPath(path); err == nil {
		if abs, err := filepath.Abs(found); err == nil {
			return fmt.Sprintf("; set it to %q", abs)
		}
	}
	return "; it does not resolve to an executable here, so set the absolute path of the installed executable"
}

// validateAbsoluteNative applies requireAbsoluteNative to the default harness, every
// declared role and every learn Atom command.
func validateAbsoluteNative(f formula.Formula) error {
	if err := requireAbsoluteNative("harness.command", f.Harness.Command); err != nil {
		return err
	}
	for _, role := range append(slices.Clone(formula.HarnessRoles), formula.ReviewHarness) {
		if b, declared := f.Harnesses[string(role)]; declared {
			if err := requireAbsoluteNative("harnesses."+string(role)+".command", b.Command); err != nil {
				return err
			}
		}
	}
	for _, a := range f.Atoms {
		if a.Type == "learn" {
			if err := requireAbsoluteNative("atoms["+a.ID+"].command", a.Command); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReviewResolution is the binding a review Atom runs under and where it came from.
// SameCommandAsCoding is recorded and shown, never a refusal by itself.
type ReviewResolution struct {
	Binding             formula.Binding
	Source              string
	SameCommandAsCoding bool
}

// ReviewMissingBindingInstruction tells the operator how to bind a review.
const ReviewMissingBindingInstruction = "configure harnesses.review, or a meta binding (harnesses.meta with a learn Atom, or a learn Atom command); the default coding harness is never used to review its own change"

// ResolveReviewBinding resolves the binding of a Formula's review Atom: the
// declared harnesses.review role first, else the meta binding as MetaBinding
// resolves it (harnesses.meta with a learn Atom, else the learn Atom's command),
// else an error. The default `harness` binding is never used. A Formula with no
// review Atom and no review role is not configured: configured is false and the
// error is nil. Source is "review-role", "meta-override" or "learn-atom".
func ResolveReviewBinding(f formula.Formula) (res ReviewResolution, configured bool, err error) {
	_, present, err := f.ReviewAtom()
	if err != nil {
		return ReviewResolution{}, false, err
	}
	role, declared := f.RoleBinding(formula.ReviewHarness)
	if !present && !declared {
		return ReviewResolution{}, false, nil
	}
	switch {
	case declared:
		res = ReviewResolution{Binding: role, Source: "review-role"}
	default:
		meta, source, bound := MetaBinding(f)
		if !bound {
			return ReviewResolution{}, true, fmt.Errorf("the review Atom has no usable binding: %s", ReviewMissingBindingInstruction)
		}
		res = ReviewResolution{Binding: meta, Source: "learn-atom"}
		if source == "override" {
			res.Source = "meta-override"
		}
	}
	if len(res.Binding.Command) == 0 {
		return ReviewResolution{}, true, fmt.Errorf("the resolved review binding %q has no command: %s", res.Binding.Binding, ReviewMissingBindingInstruction)
	}
	coding, _ := f.RoleBinding(formula.CodingHarness)
	res.SameCommandAsCoding = slices.Equal(res.Binding.Command, coding.Command)
	return res, true, nil
}

// reviewRoleCapability is the adapter's static support for read-only review. It
// executes nothing.
func reviewRoleCapability(b formula.Binding) harness.Capability {
	if !nativeAdapter(b) {
		return harness.Capability{State: "unsupported", Reason: "Custom binding has no verified read-only review protocol; it was preserved and not invoked."}
	}
	return harness.ReviewCapability(b.Command[2])
}

// validateReviewBinding fails closed at Formula proposal: a review Atom needs a
// resolvable binding whose adapter can run the read-only review.
func validateReviewBinding(f formula.Formula) error {
	if err := f.ValidateReviewAtom(); err != nil {
		return err
	}
	res, configured, err := ResolveReviewBinding(f)
	if err != nil || !configured {
		return err
	}
	if c := reviewRoleCapability(res.Binding); c.State != "supported" {
		return fmt.Errorf("review binding %q (%s) cannot be used for read-only review (%s): %s; %s", res.Binding.Binding, res.Source, c.State, c.Reason, ReviewMissingBindingInstruction)
	}
	return nil
}

// CheckAnalysisFallback refuses a new default binding that would silently become
// the analysis binding when harnesses.analysis is absent but cannot perform
// read-only ticket analysis. It executes nothing.
func CheckAnalysisFallback(f formula.Formula, target formula.Binding) error {
	if _, declared := f.Harnesses[string(formula.AnalysisHarness)]; declared {
		return nil
	}
	if c := roleCapability(formula.AnalysisHarness, target); c.State == "unsupported" {
		return fmt.Errorf("cannot rebind to %s: harnesses.analysis is not declared, so read-only ticket analysis would fall back to the default binding, which cannot be used for it: %s; declare harnesses.analysis with a supported harness first", target.Binding, c.Reason)
	}
	return nil
}

// RoleCapability is the adapter's static support for one role, computed from the
// proposed Formula alone. It is a saved observation, never a fresh probe.
type RoleCapability struct {
	Role       string             `json:"role"`
	Binding    string             `json:"binding"`
	Source     string             `json:"source"`
	Capability harness.Capability `json:"capability"`
}

// RoleCapabilities lists the roles InspectRoles would probe, without probing.
func RoleCapabilities(f formula.Formula) []RoleCapability {
	var roles []RoleCapability
	for _, role := range formula.HarnessRoles {
		b, override := f.RoleBinding(role)
		source := "default"
		if override {
			source = "override"
		} else if role == formula.MetaHarness {
			var bound bool
			if b, source, bound = MetaBinding(f); !bound {
				continue
			}
		}
		roles = append(roles, RoleCapability{Role: string(role), Binding: b.Binding, Source: source, Capability: roleCapability(role, b)})
	}
	// Review appears only for a Formula that has a review Atom or role, so the saved
	// evidence and proposal identity of every other Formula stay unchanged.
	if res, configured, err := ResolveReviewBinding(f); configured && err == nil {
		roles = append(roles, RoleCapability{Role: string(formula.ReviewHarness), Binding: res.Binding.Binding, Source: res.Source, Capability: reviewRoleCapability(res.Binding)})
	}
	return roles
}

// validateRoleEvidence checks that saved role evidence names the same roles,
// bindings and sources as the proposed Formula. Capability states are
// observations and are covered by the proposal identity instead.
func validateRoleEvidence(f formula.Formula, saved []RoleCapability) error {
	want := RoleCapabilities(f)
	if len(saved) != len(want) {
		return fmt.Errorf("saved role capability evidence does not match the proposed Formula")
	}
	for i, r := range saved {
		if r.Role != want[i].Role || r.Binding != want[i].Binding || r.Source != want[i].Source {
			return fmt.Errorf("saved role capability evidence does not match the proposed Formula")
		}
	}
	return nil
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
	// Model is the pinned model of the binding the role resolves to; empty when
	// unpinned.
	Model string `json:"model,omitempty"`
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
			var bound bool
			if b, source, bound = MetaBinding(f); !bound {
				continue
			}
		}
		c := probe(b)
		item := RoleReadiness{Role: string(role), Binding: b.Binding, Source: source, Readiness: c.Readiness, Detail: c.Detail, Model: b.Model, Candidate: c}
		switch role {
		case formula.AnalysisHarness:
			item.Capability = c.WorkflowCapability()
			item.Ready = item.Capability.State == "conditional"
		case formula.MetaHarness:
			item.Capability = c.Capabilities.Meta
			item.Ready = c.Readiness == "ready" && item.Capability.State == "supported"
			if learnID, conflict := MetaConflict(f); conflict {
				reason := metaConflictReason(learnID)
				item.Readiness, item.Detail, item.Ready = "blocked", reason, false
				item.Capability = harness.Capability{State: "blocked", Reason: reason}
			}
		default:
			item.Capability = c.Capabilities.Coding
			item.Ready = c.Readiness == "ready" && item.Capability.State == "supported"
		}
		roles = append(roles, item)
	}
	if res, configured, err := ResolveReviewBinding(f); configured && err == nil {
		c := probe(res.Binding)
		item := RoleReadiness{Role: string(formula.ReviewHarness), Binding: res.Binding.Binding, Source: res.Source, Readiness: c.Readiness, Detail: c.Detail, Model: res.Binding.Model, Candidate: c}
		if static := reviewRoleCapability(res.Binding); static.State != "supported" {
			item.Capability = static
		} else {
			item.Capability = c.WorkflowCapability()
			item.Ready = item.Capability.State == "conditional"
		}
		roles = append(roles, item)
	}
	return roles
}

// metaLearnAtom is the learn Atom learning is bound through: the first one,
// preferring one that carries a command.
func metaLearnAtom(f formula.Formula) *formula.Atom {
	var learn *formula.Atom
	for i := range f.Atoms {
		a := &f.Atoms[i]
		if a.Type == "learn" && (learn == nil || len(learn.Command) == 0 && len(a.Command) > 0) {
			learn = a
		}
	}
	return learn
}

// MetaConflict reports a declared harnesses.meta command that differs from the
// command of the learn Atom, and that Atom's id. Learning refuses to run in that
// state, so it is never a usable meta binding.
func MetaConflict(f formula.Formula) (learnID string, conflict bool) {
	learn := metaLearnAtom(f)
	if learn == nil {
		return "", false
	}
	role, declared := f.RoleBinding(formula.MetaHarness)
	if !declared || len(learn.Command) == 0 || slices.Equal(learn.Command, role.Command) {
		return "", false
	}
	return learn.ID, true
}

func metaConflictReason(learnID string) string {
	return fmt.Sprintf("harnesses.meta command conflicts with the command of learn Atom %s; learn invokes no learner until they are equal or one is removed through a reviewed Formula change.", learnID)
}

// MetaBinding resolves the separate meta binding learning runs. A declared
// harnesses.meta role supplies it, and a commandless learn Atom is enough; the
// Atom's own timeoutMs still wins, as learning runs it. Without a role the
// command of a learn Atom is the binding. Source is "override" or "learn-atom". A
// role whose command conflicts with the learn Atom's is not bound (MetaConflict).
func MetaBinding(f formula.Formula) (b formula.Binding, source string, bound bool) {
	learn := metaLearnAtom(f)
	if learn == nil {
		return formula.Binding{}, "", false
	}
	if _, conflict := MetaConflict(f); conflict {
		return formula.Binding{}, "", false
	}
	if role, declared := f.RoleBinding(formula.MetaHarness); declared {
		if learn.TimeoutMS > 0 {
			role.TimeoutMS = learn.TimeoutMS
		}
		return role, "override", true
	}
	if len(learn.Command) == 0 {
		return formula.Binding{}, "", false
	}
	return formula.Binding{Binding: learn.Binding, Command: learn.Command, TimeoutMS: learn.TimeoutMS}, "learn-atom", true
}

func LearnBound(f formula.Formula) bool {
	_, _, bound := MetaBinding(f)
	return bound
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

// metaSubject names what a Formula's learn Atom and meta role bind, without
// judging whether it is a usable command.
func metaSubject(f formula.Formula) (command []string, binding string, timeout int, found bool) {
	if b, _, bound := MetaBinding(f); bound {
		return b.Command, b.Binding, b.TimeoutMS, true
	}
	for _, a := range f.Atoms {
		if a.Type == "learn" {
			return a.Command, a.Binding, a.TimeoutMS, true
		}
	}
	return nil, "", 0, false
}

// withMetaConflict reports a harnesses.meta/learn Atom command conflict instead of
// the capability of whichever command was named, so it never reads as healthy.
func withMetaConflict(f formula.Formula, r MetaReport) MetaReport {
	if learnID, conflict := MetaConflict(f); conflict {
		reason := metaConflictReason(learnID)
		r.Capability = harness.Capability{State: "blocked", Reason: reason}
		r.NextAction = reason
	}
	return r
}

// DescribeFormulaMeta reports the declared meta binding without probing anything.
func DescribeFormulaMeta(f formula.Formula) MetaReport {
	command, binding, timeout, _ := metaSubject(f)
	return withMetaConflict(f, DescribeMeta(command, binding, timeout, Candidate{}))
}

func InspectMeta(ctx context.Context, f formula.Formula, cwd string, run Runner) MetaReport {
	command, binding, timeout, found := metaSubject(f)
	if !found {
		return DescribeMeta(nil, "", 0, Candidate{})
	}
	return withMetaConflict(f, DescribeMeta(command, binding, timeout, ProbeBinding(ctx, formula.Binding{Binding: binding, Command: command}, cwd, run)))
}
