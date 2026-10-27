package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"go.yaml.in/yaml/v4"
	"reflect"
	"slices"
	"strings"
	"time"
)

type InitOptions struct {
	Cwd, LabDir, Launcher, Preferred string
	Env                              map[string]string
	Candidates                       []Candidate
	Runner                           Runner
	MetaHarness                      string
	MetaCommand                      []string
	// Rebind proposes a binding-only change of the approved default harness.
	Rebind string
	// HarnessTimeoutMS and MaxTimeoutMS set the reviewed coding budget and the
	// ceiling for per-Molecule timeouts; zero selects the defaults below.
	HarnessTimeoutMS, MaxTimeoutMS int
	// ModelSet marks an explicit --model, so an empty Model proposes removing the
	// pin. Role limits the proposal to the named harnesses.<role> override.
	Model, Role string
	ModelSet    bool
	// RefreshBaseline proposes a baseline-only change: requiredChecks re-derived
	// from the current pull-request workflows of the approved Formula's repository.
	RefreshBaseline bool
	// EnableReview proposes an insert-only change: one review Atom after the check
	// Atom of the approved Formula.
	EnableReview bool
}

// Bootstrap budgets: a 15-minute default coding attempt that intake may raise,
// for larger approved scopes, up to a 60-minute reviewed ceiling.
const (
	DefaultHarnessTimeoutMS = 900000
	DefaultMaxTimeoutMS     = 3600000
)

// harnessBudget resolves and validates the bootstrap timeout and ceiling.
func harnessBudget(o InitOptions) (int, int, error) {
	timeout, ceiling := o.HarnessTimeoutMS, o.MaxTimeoutMS
	if timeout == 0 {
		timeout = DefaultHarnessTimeoutMS
	}
	if ceiling == 0 {
		ceiling = max(DefaultMaxTimeoutMS, timeout)
	}
	if timeout < 1 || timeout > DefaultMaxTimeoutMS || ceiling < timeout || ceiling > DefaultMaxTimeoutMS {
		return 0, 0, fmt.Errorf("harness timeout must be 1-%d ms and the maximum timeout must be at least the timeout and at most %d ms", DefaultMaxTimeoutMS, DefaultMaxTimeoutMS)
	}
	return timeout, ceiling, nil
}

type Outcome struct {
	Meta       *MetaReport    `json:"metaBinding"`
	Binding    *BindingReport `json:"bindingCapabilities,omitempty"`
	Status     string         `json:"status"`
	Repository Repository     `json:"repository"`
	Candidates []Candidate    `json:"candidates"`
	Selection  Selection      `json:"selection"`
	Proposal   *Proposal      `json:"proposal,omitempty"`
	BeforeYAML string         `json:"beforeYaml,omitempty"`
	Note       string         `json:"note"`
}

// nativeLauncher returns the launcher of a native [launcher, "__adapter", adapter,
// provider] command, or "" for any other command.
func nativeLauncher(command []string) string {
	if len(command) == 4 && command[1] == "__adapter" {
		return command[0]
	}
	return ""
}

// recordedLaunchers are the native launchers the approved Formula's default and
// coding bindings already name: Forgecell wrote them and a human approved them.
func recordedLaunchers(f formula.Formula) map[string]bool {
	recorded := map[string]bool{}
	for _, b := range []formula.Binding{f.Harness, f.Harnesses[string(formula.CodingHarness)]} {
		if l := nativeLauncher(b.Command); l != "" {
			recorded[l] = true
		}
	}
	return recorded
}

// establishedForgecell reports whether a saved launcher is established as a
// Forgecell executable without executing it: the running executable, a managed
// launcher, a launcher beside a release manifest that records the launcher's own
// SHA-256, or a launcher the approved Formula already records. A custom tool that
// merely has the native argv shape, or sits beside a manifest.json it did not
// install, is none of these.
func establishedForgecell(saved string, recorded map[string]bool) bool {
	if recorded[saved] {
		return true
	}
	id := harness.ResolveLauncher(saved)
	return id.Class == harness.LauncherRunning || id.Managed || id.ExecutableMatchesManifest()
}

// RebindRepoints reports whether init --rebind would repoint a native command
// saved with this launcher in f, so doctor names the command only when it works.
func RebindRepoints(f formula.Formula, launcher string) bool {
	return launcher != "" && establishedForgecell(launcher, recordedLaunchers(f))
}

// staleNativeCommand reports whether a native analysis or meta role override or
// learn Atom command names an established Forgecell launcher other than the
// target one.
func staleNativeCommand(f formula.Formula, launcher string) bool {
	recorded := recordedLaunchers(f)
	stale := func(command []string) bool {
		current := nativeLauncher(command)
		return current != "" && current != launcher && establishedForgecell(current, recorded)
	}
	for _, role := range []formula.HarnessRole{formula.AnalysisHarness, formula.MetaHarness} {
		if b, ok := f.Harnesses[string(role)]; ok && stale(b.Command) {
			return true
		}
	}
	for _, a := range f.Atoms {
		if a.Type == "learn" && stale(a.Command) {
			return true
		}
	}
	return false
}

// checkRebindTarget requires a ready, discovered harness with an executable and
// a launcher, refuses a rebind that would change nothing, and refuses a target
// that would become the analysis binding by fallback without supporting it.
func checkRebindTarget(candidates []Candidate, id, launcher string, active formula.Formula, lab string) error {
	for _, c := range candidates {
		if c.ID != id {
			continue
		}
		if c.Readiness != "ready" || c.Executable == "" {
			return fmt.Errorf("cannot rebind to %s: %s; run forgecell doctor --lab %s to see what is missing", id, c.Detail, formula.ShellWord(lab))
		}
		if launcher == "" {
			return fmt.Errorf("cannot rebind to %s: the Forgecell launcher is unknown; set FORGECELL_LAUNCHER to its path", id)
		}
		command := []string{launcher, "__adapter", id, c.Executable}
		coding, overridden := active.Harnesses[string(formula.CodingHarness)]
		if active.Harness.Binding == id && reflect.DeepEqual(active.Harness.Command, command) && (!overridden || coding.Binding == id && reflect.DeepEqual(coding.Command, command)) && !staleNativeCommand(active, launcher) {
			return fmt.Errorf("the default harness and any coding role binding are already bound to %s with this launcher and executable, and no native analysis, meta or learn command is on another Forgecell launcher; nothing to rebind", id)
		}
		return CheckAnalysisFallback(active, formula.Binding{Binding: id, Command: command})
	}
	return fmt.Errorf("cannot rebind to %q: no such harness was discovered; run forgecell doctor --lab %s to list the available harnesses", id, formula.ShellWord(lab))
}

// maskLauncher blanks the launcher of a native command held under m["command"].
func maskLauncher(m map[string]any) {
	if args, ok := m["command"].([]any); ok && len(args) == 4 && args[1] == "__adapter" {
		m["command"] = append([]any{"<launcher>"}, args[1:]...)
	}
}

// maskRebindKeys removes the only keys a rebind may change: harness.binding and
// harness.command, harnesses.coding.binding and harnesses.coding.command, the
// binding of the harness Atom, and the launcher element of native commands in the
// learn Atom and in the analysis and meta overrides.
func maskRebindKeys(doc map[string]any) {
	if h, ok := doc["harness"].(map[string]any); ok {
		delete(h, "binding")
		delete(h, "command")
	}
	if roles, ok := doc["harnesses"].(map[string]any); ok {
		if coding, ok := roles[string(formula.CodingHarness)].(map[string]any); ok {
			delete(coding, "binding")
			delete(coding, "command")
		}
		for _, role := range []formula.HarnessRole{formula.AnalysisHarness, formula.MetaHarness} {
			if m, ok := roles[string(role)].(map[string]any); ok {
				maskLauncher(m)
			}
		}
	}
	if atoms, ok := doc["atoms"].([]any); ok {
		for _, v := range atoms {
			atom, ok := v.(map[string]any)
			if !ok {
				continue
			}
			switch atom["type"] {
			case "harness":
				delete(atom, "binding")
			case "learn":
				maskLauncher(atom)
			}
		}
	}
}

// verifyRebindDiff proves a rebind proposal differs from the approved Formula only
// in the keys maskRebindKeys names.
func verifyRebindDiff(before, after string) error {
	var a, b map[string]any
	if err := yaml.Unmarshal([]byte(before), &a); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(after), &b); err != nil {
		return err
	}
	maskRebindKeys(a)
	maskRebindKeys(b)
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("rebind would change more than the binding keys; no proposal saved")
	}
	return nil
}

// launcherRewrite is one recorded repoint of a native command's launcher. Path
// names the command's holder, such as harnesses.meta or atoms[learn].
type launcherRewrite struct{ Path, Old, New string }

// verifyRebindExact proves the proposal is the approved Formula with the target
// binding keys set and only the recorded launcher rewrites applied: any other
// launcher value, or any unrecorded change, is refused.
func verifyRebindExact(before, after, target string, command []string, rewrites []launcherRewrite) error {
	if err := verifyRebindDiff(before, after); err != nil {
		return err
	}
	var expected, proposed map[string]any
	if err := yaml.Unmarshal([]byte(before), &expected); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(after), &proposed); err != nil {
		return err
	}
	setBindingKeys(expected, target, command, func(_ string, holder map[string]any, key string, value any) {
		if list, ok := value.([]string); ok {
			items := make([]any, len(list))
			for i, item := range list {
				items[i] = item
			}
			value = items
		}
		holder[key] = value
	})
	holders := nativeHolders(expected)
	for _, w := range rewrites {
		index := slices.IndexFunc(holders, func(h nativeHolder) bool { return h.path == w.Path })
		if index < 0 {
			return fmt.Errorf("rebind rewrote %s, which is not a native command in the approved Formula; no proposal saved", w.Path)
		}
		args := holders[index].holder["command"].([]any)
		if args[0] != w.Old {
			return fmt.Errorf("rebind rewrote %s from a launcher the approved Formula does not name; no proposal saved", w.Path)
		}
		holders[index].holder["command"] = append([]any{w.New}, args[1:]...)
	}
	if !reflect.DeepEqual(expected, proposed) {
		return fmt.Errorf("rebind would change more than the recorded launcher rewrites; no proposal saved")
	}
	return nil
}

func atomPath(i int, atom map[string]any) string {
	if id, ok := atom["id"].(string); ok && id != "" {
		return "atoms[" + id + "]"
	}
	return fmt.Sprintf("atoms[%d]", i)
}

// setBindingKeys visits every key a rebind sets to the target binding and
// command: the default harness, the coding override and each harness Atom.
func setBindingKeys(doc map[string]any, target string, command []string, set func(path string, holder map[string]any, key string, value any)) {
	if h, ok := doc["harness"].(map[string]any); ok {
		set("harness.binding", h, "binding", target)
		set("harness.command", h, "command", command)
	}
	roles, _ := doc["harnesses"].(map[string]any)
	if coding, ok := roles[string(formula.CodingHarness)].(map[string]any); ok {
		set("harnesses.coding.binding", coding, "binding", target)
		set("harnesses.coding.command", coding, "command", command)
	}
	atoms, _ := doc["atoms"].([]any)
	for i, v := range atoms {
		if atom, ok := v.(map[string]any); ok && atom["type"] == "harness" {
			set(atomPath(i, atom)+".binding", atom, "binding", target)
		}
	}
}

// nativeHolder is a native [launcher, "__adapter", adapter, provider] command held
// by an analysis or meta role override or by a learn Atom.
type nativeHolder struct {
	path   string
	holder map[string]any
}

func nativeHolders(doc map[string]any) []nativeHolder {
	var found []nativeHolder
	add := func(path string, holder map[string]any) {
		if args, ok := holder["command"].([]any); ok && len(args) == 4 && args[1] == "__adapter" {
			found = append(found, nativeHolder{path, holder})
		}
	}
	roles, _ := doc["harnesses"].(map[string]any)
	for _, role := range []formula.HarnessRole{formula.AnalysisHarness, formula.MetaHarness} {
		if m, ok := roles[string(role)].(map[string]any); ok {
			add("harnesses."+string(role), m)
		}
	}
	atoms, _ := doc["atoms"].([]any)
	for i, v := range atoms {
		if atom, ok := v.(map[string]any); ok && atom["type"] == "learn" {
			add(atomPath(i, atom), atom)
		}
	}
	return found
}

// rebindRecipe points the coding binding at the target and re-points native
// commands onto the target launcher: the learn Atom and the analysis and meta
// overrides keep their own adapter and provider. With repointStale a native
// command on another launcher moves only when that launcher is established as a
// Forgecell executable; the rest are returned as left. Otherwise only commands on
// the old default launcher move. It returns each rewritten key path with its old
// and new value, the structured launcher rewrites, and the commands left alone.
func rebindRecipe(recipe map[string]any, active formula.Formula, target string, command []string, repointStale bool) (changes []string, rewrites []launcherRewrite, left []string) {
	record := func(path string, before, after any) {
		b, _ := json.Marshal(before)
		a, _ := json.Marshal(after)
		if string(b) != string(a) {
			changes = append(changes, fmt.Sprintf("%s: %s -> %s", path, b, a))
		}
	}
	setBindingKeys(recipe, target, command, func(path string, holder map[string]any, key string, value any) {
		record(path, holder[key], value)
		holder[key] = value
	})
	old := nativeLauncher(active.Harness.Command)
	recorded := recordedLaunchers(active)
	for _, h := range nativeHolders(recipe) {
		args := h.holder["command"].([]any)
		launcher, _ := args[0].(string)
		if launcher == command[0] {
			continue
		}
		if repointStale {
			if !establishedForgecell(launcher, recorded) {
				quoted, _ := json.Marshal(args[0])
				left = append(left, fmt.Sprintf("%s.command[0]: %s", h.path, quoted))
				continue
			}
		} else if old == "" || launcher != old {
			continue
		}
		record(h.path+".command[0]", args[0], command[0])
		rewrites = append(rewrites, launcherRewrite{h.path, launcher, command[0]})
		h.holder["command"] = append([]any{command[0]}, args[1:]...)
	}
	return changes, rewrites, left
}

// codingOverrideNote names a harnesses.coding override that a rebind overwrites
// although it is a custom command or is bound to another adapter than the old
// default. It is a note for the reviewer, never a refusal.
func codingOverrideNote(active formula.Formula, target string, command []string) string {
	coding, ok := active.Harnesses[string(formula.CodingHarness)]
	if !ok || coding.Binding == target && reflect.DeepEqual(coding.Command, command) {
		return ""
	}
	var what string
	switch {
	case nativeLauncher(coding.Command) == "":
		argv, _ := json.Marshal(coding.Command)
		what = "a custom command " + string(argv)
	case coding.Binding != active.Harness.Binding:
		what = fmt.Sprintf("bound to %s, not the old default %s", coding.Binding, active.Harness.Binding)
	default:
		return ""
	}
	return fmt.Sprintf(" The harnesses.coding override was overwritten although it was %s; confirm the new default binding should replace it.", what)
}

// proposalEvidence is the detached repository evidence a proposal carries: the
// observed repository for a bootstrap, else the evidence the approved bootstrap
// proposal saved.
func proposalEvidence(dir string, active formula.Loaded, observed *Repository) (*Repository, error) {
	if !active.Approved {
		return observed, nil
	}
	if active.Approval.SourceKind != "bootstrap-assay" {
		return nil, nil
	}
	previous, err := InspectProposal(dir, active.Approval.SourceID)
	if err != nil {
		return nil, err
	}
	if previous.EvidencePath == "" {
		return nil, nil
	}
	data, err := readArtifact(dir, previous.EvidencePath)
	if err != nil {
		return nil, err
	}
	var original Repository
	if err = json.Unmarshal(data, &original); err != nil {
		return nil, err
	}
	return &original, nil
}

// learnCommand is the command learning runs, from harnesses.meta or the learn Atom.
func learnCommand(f formula.Formula) []string {
	b, _, _ := MetaBinding(f)
	return b.Command
}

func Prepare(ctx context.Context, o InitOptions) (out Outcome, err error) {
	budgetRequested := o.HarnessTimeoutMS != 0 || o.MaxTimeoutMS != 0
	if budgetRequested {
		if _, _, err = harnessBudget(o); err != nil {
			return out, err
		}
	}
	if o.EnableReview && (o.Preferred != "" || o.Rebind != "" || o.MetaHarness != "" || o.MetaCommand != nil || o.ModelSet || o.Role != "" || budgetRequested || o.RefreshBaseline) {
		return out, fmt.Errorf("--enable-review only inserts the review Atom; do not combine it with --harness, --rebind, --meta-harness, --meta-command, --model, --role, --refresh-baseline or the timeout flags")
	}
	if o.RefreshBaseline && (o.Preferred != "" || o.Rebind != "" || o.MetaHarness != "" || o.MetaCommand != nil || o.ModelSet || o.Role != "" || budgetRequested) {
		return out, fmt.Errorf("--refresh-baseline changes only requiredChecks; do not combine it with --harness, --rebind, --meta-harness, --meta-command, --model or the timeout flags")
	}
	if o.Rebind != "" {
		if o.Preferred != "" || o.MetaHarness != "" || o.MetaCommand != nil || budgetRequested {
			return out, fmt.Errorf("--rebind changes only the binding; do not combine it with --harness, --meta-harness, --meta-command or the timeout flags")
		}
		o.Preferred = o.Rebind
	}
	if err = checkModelPinOptions(o, budgetRequested); err != nil {
		return out, err
	}
	if o.MetaHarness != "" && o.MetaCommand != nil {
		return out, fmt.Errorf("choose meta-harness or meta-command")
	}
	if o.MetaCommand != nil {
		if len(o.MetaCommand) == 0 {
			return out, fmt.Errorf("meta-command must be non-empty")
		}
		for _, arg := range o.MetaCommand {
			if strings.TrimSpace(arg) == "" || strings.ContainsRune(arg, 0) {
				return out, fmt.Errorf("invalid meta-command argument")
			}
		}
	}
	out.Repository, err = ReadRepository(ctx, o.Cwd, o.Runner)
	if err != nil {
		return out, err
	}
	active, err := formula.Inspect(o.LabDir, "")
	if err != nil {
		return out, err
	}
	if active.Formula.ID != "" && !strings.EqualFold(active.Formula.Intake.Repo, out.Repository.Repo) {
		return out, fmt.Errorf("approved Formula belongs to another repository; use a separate Lab")
	}

	meta := InspectMeta(ctx, active.Formula, out.Repository.Root, o.Runner)
	out.Meta = &meta
	if pending, e := pendingProposal(o.LabDir); e != nil {
		return out, e
	} else if pending != nil {
		if o.Rebind != "" {
			return out, fmt.Errorf("a proposal is already pending; review, approve or dismiss it with init --lab %s --review/--approve/--dismiss %s before --rebind", formula.ShellWord(o.LabDir), pending.ID)
		}
		if o.RefreshBaseline {
			return out, fmt.Errorf("a proposal is already pending; review, approve or dismiss it with init --lab %s --review/--approve/--dismiss %s before --refresh-baseline", formula.ShellWord(o.LabDir), pending.ID)
		}
		if o.EnableReview {
			return out, fmt.Errorf("a proposal is already pending; review, approve or dismiss it with init --lab %s --review/--approve/--dismiss %s before --enable-review", formula.ShellWord(o.LabDir), pending.ID)
		}
		if budgetRequested {
			return out, fmt.Errorf("a proposal is already pending; review, approve or dismiss it before proposing a new harness timeout budget")
		}
		if o.ModelSet {
			return out, fmt.Errorf("a proposal is already pending; review, approve or dismiss it with init --lab %s --review/--approve/--dismiss %s before --model", formula.ShellWord(o.LabDir), pending.ID)
		}
		f, e := formula.Parse([]byte(pending.YAML))
		if e != nil {
			return out, e
		}
		if f.Intake.Repo != "" && !strings.EqualFold(f.Intake.Repo, out.Repository.Repo) {
			return out, fmt.Errorf("pending Formula belongs to another repository; use a separate Lab")
		}
		out.Status = "pending"
		out.Proposal = pending
		out.Binding = pending.CapabilityEvidence
		if out.Binding != nil && out.Binding.Meta != nil {
			out.Meta = out.Binding.Meta
			out.BeforeYAML = out.Meta.BeforeYAML
		} else {
			// Historical proposals carry no current capability observations.
			report := DescribeFormulaMeta(f)
			out.Meta = &report
		}
		out.Note = "Preserved pending proposal. Review or dismiss before proposing another binding; no model invoked. Stale approval remains refused."
		if incomplete, e := ProposalActivationIncomplete(o.LabDir, *pending); e != nil {
			return out, e
		} else if incomplete {
			out.Note = "Preserved interrupted activation; resume its exact saved approval; no model invoked."
		}
		return out, nil
	}
	if o.Rebind != "" && active.Formula.ID == "" {
		return out, fmt.Errorf("--rebind needs an approved Formula; run init --lab %s and approve the bootstrap proposal first", formula.ShellWord(o.LabDir))
	}
	if o.RefreshBaseline && active.Formula.ID == "" {
		return out, fmt.Errorf("--refresh-baseline needs an approved Formula; run init --lab %s and approve the bootstrap proposal first", formula.ShellWord(o.LabDir))
	}
	if o.EnableReview && active.Formula.ID == "" {
		return out, fmt.Errorf("--enable-review needs an approved Formula; run init --lab %s and approve the bootstrap proposal first", formula.ShellWord(o.LabDir))
	}
	if o.ModelSet && active.Formula.ID == "" {
		return out, fmt.Errorf("--model needs an approved Formula; run init --lab %s and approve the bootstrap proposal first", formula.ShellWord(o.LabDir))
	}
	if active.Formula.ID != "" {
		approved, e := formula.Load(o.LabDir, "")
		if e != nil || !approved.Approved {
			if o.Rebind != "" {
				return out, fmt.Errorf("--rebind needs a current exact approval of the active Formula; run init --lab %s to review it, then init --lab %s --approve PROPOSAL_ID", formula.ShellWord(o.LabDir), formula.ShellWord(o.LabDir))
			}
			if o.RefreshBaseline {
				return out, fmt.Errorf("--refresh-baseline needs a current exact approval of the active Formula; run init --lab %s to review it, then init --lab %s --approve PROPOSAL_ID", formula.ShellWord(o.LabDir), formula.ShellWord(o.LabDir))
			}
			if o.EnableReview {
				return out, fmt.Errorf("--enable-review needs a current exact approval of the active Formula; run init --lab %s to review it, then init --lab %s --approve PROPOSAL_ID", formula.ShellWord(o.LabDir), formula.ShellWord(o.LabDir))
			}
			if o.ModelSet {
				return out, fmt.Errorf("--model needs a current exact approval of the active Formula; run init --lab %s to review it, then init --lab %s --approve PROPOSAL_ID", formula.ShellWord(o.LabDir), formula.ShellWord(o.LabDir))
			}
			checked := ProbeBinding(ctx, active.Formula.Harness, out.Repository.Root, o.Runner)
			out.Binding = &BindingReport{Command: active.Formula.Harness.Command, Candidate: checked, Workflow: checked.WorkflowCapability(), LearnBound: LearnBound(active.Formula)}
			p, e := saveWithEvidence(o.LabDir, active.Formula.YAML, nil, out.Binding)
			if e != nil {
				return out, e
			}
			out.Status = "pending"
			out.Proposal = &p
			out.BeforeYAML = active.Formula.YAML
			out.Note = "Current YAML has no current exact approval. Review these exact bytes and approve the saved proposal with init --lab DIR --approve PROPOSAL_ID. Historical decisions are inspection evidence only."
			return out, nil
		}
		active = approved
	}
	if o.RefreshBaseline {
		return proposeBaselineRefresh(ctx, o, out, active)
	}
	if o.EnableReview {
		return proposeEnableReview(ctx, o, out, active)
	}
	if o.ModelSet {
		return proposeModelPin(ctx, o, out, active)
	}
	out.Candidates = o.Candidates
	if out.Candidates == nil {
		out.Candidates = Discover(ctx, out.Repository.Root, o.Runner)
	}
	for i := range out.Candidates {
		out.Candidates[i] = out.Candidates[i].CompleteUnknowns()
	}
	if o.Rebind != "" {
		if err = checkRebindTarget(out.Candidates, o.Rebind, o.Launcher, active.Formula, o.LabDir); err != nil {
			return out, err
		}
	}
	existing := ""
	if active.Approved {
		existing = active.Formula.Harness.Binding
	}
	out.Selection = Select(out.Candidates, o.Preferred, existing, o.Env)
	if active.Approved && o.Preferred == "" && budgetRequested {
		return out, fmt.Errorf("timeout budget flags change an approved Formula only with an explicit --harness rebind proposal")
	}
	if active.Approved && o.Preferred == "" && (o.MetaHarness == "" && o.MetaCommand == nil || LearnBound(active.Formula)) {
		checked := ProbeBinding(ctx, active.Formula.Harness, out.Repository.Root, o.Runner)
		out.Binding = &BindingReport{Command: active.Formula.Harness.Command, Candidate: checked, Workflow: checked.WorkflowCapability(), LearnBound: LearnBound(active.Formula)}
		out.Status = "unchanged"
		out.BeforeYAML = active.Formula.YAML
		out.Note = "Preserved existing approved Formula and exact command bindings. Explicitly choose --harness to propose a rebind."
		return out, nil
	}
	if active.Approved && o.Preferred == "" {
		out.Selection = Selection{ID: existing, Status: "ready", Reason: "Preserving coding binding; explicit meta opt-in."}
	}
	out.Status = out.Selection.Status
	if out.Status != "ready" {
		out.Note = "No Formula proposed; resolve harness selection first."
		return out, nil
	}
	var selected Candidate
	for _, c := range out.Candidates {
		if c.ID == out.Selection.ID {
			selected = c
		}
	}
	if (!active.Approved || o.Preferred != "") && (selected.Executable == "" || o.Launcher == "") {
		return out, fmt.Errorf("verified harness executable and Forgecell launcher are required")
	}
	command := []string{o.Launcher, "__adapter", selected.ID, selected.Executable}
	if !active.Approved || o.Preferred != "" {
		if err = requireAbsoluteNative("harness.command", command); err != nil {
			return out, err
		}
	}
	if active.Approved && o.Preferred != "" && o.Rebind == "" {
		if err = CheckAnalysisFallback(active.Formula, formula.Binding{Binding: selected.ID, Command: command}); err != nil {
			return out, err
		}
	}
	var recipe map[string]any
	var rewrites []launcherRewrite
	if active.Approved {
		out.BeforeYAML = active.Formula.YAML
		if err = yaml.Unmarshal([]byte(active.Formula.YAML), &recipe); err != nil {
			return out, err
		}
		binding, ok := recipe["harness"].(map[string]any)
		if !ok {
			return out, fmt.Errorf("existing harness is not a mapping")
		}
		var rewritten, left []string
		if o.Preferred != "" {
			rewritten, rewrites, left = rebindRecipe(recipe, active.Formula, selected.ID, command, o.Rebind != "")
			if budgetRequested {
				current := o
				if current.HarnessTimeoutMS == 0 {
					current.HarnessTimeoutMS = active.Formula.Harness.TimeoutMS
				}
				timeoutMS, maxTimeoutMS, e := harnessBudget(current)
				if e != nil {
					return out, e
				}
				binding["timeoutMs"], binding["maxTimeoutMs"] = timeoutMS, maxTimeoutMS
			}
		} else {
			command = active.Formula.Harness.Command
			selected = ProbeBinding(ctx, active.Formula.Harness, out.Repository.Root, o.Runner)
		}
		out.Note = "Proposes only a coding-harness rebind (plus any explicitly requested timeout budget); existing instructions, workflow and meta-harness configuration are preserved. Review the complete YAML before approval."
		overwrite := ""
		if o.Preferred != "" {
			overwrite = codingOverrideNote(active.Formula, selected.ID, command)
		}
		if len(rewritten) > 0 {
			out.Note += " Rewritten keys (old -> new): " + strings.Join(rewritten, "; ") + "."
		}
		out.Note += overwrite
		if o.Rebind != "" {
			out.Note = "Proposes only a binding rebind to " + selected.ID + ". Rewritten keys (old -> new): " + strings.Join(rewritten, "; ") + "."
			if len(left) > 0 {
				out.Note += " Left unchanged because the launcher is not established as a Forgecell executable: " + strings.Join(left, "; ") + "."
			}
			out.Note += overwrite + " Atoms, checks, instructions, timeouts, learned guidance and the adapter and provider of every other binding are preserved. Review the exact YAML before approval."
		}
	} else {
		name := strings.ToLower(strings.Split(out.Repository.Repo, "/")[1]) + "-github-issue"
		timeoutMS, maxTimeoutMS, err := harnessBudget(o)
		if err != nil {
			return out, err
		}
		recipe = map[string]any{"schemaVersion": "v0", "kind": "formula", "id": name, "name": out.Repository.Repo + " issue workflow", "createdAt": time.Now().UTC().Format(time.RFC3339Nano), "source": "bootstrap-assay", "intake": map[string]any{"source": "github-issues", "repo": out.Repository.Repo}, "harness": map[string]any{"binding": selected.ID, "command": command, "invoke": "byo", "timeoutMs": timeoutMS, "maxTimeoutMs": maxTimeoutMS}, "lab": map[string]any{"dir": o.LabDir, "observe": false}, "repositoryContext": out.Repository.Compact(), "atoms": []map[string]any{{"id": "intake", "type": "intake", "source": "github-issues"}, {"id": "scope", "type": "gate", "purpose": "scope", "mode": "human"}, {"id": "harness", "type": "harness", "binding": selected.ID}, {"id": "check", "type": "check", "workflows": out.Repository.Workflows}, {"id": "gate", "type": "gate", "purpose": "review", "mode": "human"}, {"id": "ship", "type": "ship", "via": "github"}, {"id": "document", "type": "document", "target": "github-issue"}}}
		baseline, err := bootstrapBaseline(ctx, out.Repository)
		if err != nil {
			return out, err
		}
		out.Note = "Bootstrap Assay is read-only. Proposed checks are evidence, not executed commands. Approving binds the coding harness; runs still stop at human review."
		if len(baseline) > 0 {
			recipe["requiredChecks"] = baseline
			ids := make([]string, len(baseline))
			for i, c := range baseline {
				ids[i] = c.ID
			}
			out.Note += fmt.Sprintf(" Derived %d CI baseline required check(s) from pull-request workflows and recorded them as reviewed requiredChecks (%s). Every plan must include them; review the exact YAML before approval.", len(baseline), strings.Join(ids, ", "))
		}
	}
	if o.MetaHarness != "" || o.MetaCommand != nil {
		if LearnBound(active.Formula) {
			return out, fmt.Errorf("existing learn binding preserved; review an explicit Formula change")
		}
		before := out.BeforeYAML
		if before == "" {
			b, e := yaml.Marshal(recipe)
			if e != nil {
				return out, e
			}
			before = string(b)
		}
		command := o.MetaCommand
		binding := "byo"
		candidate := Candidate{}
		if o.MetaHarness != "" {
			binding = o.MetaHarness
			for _, c := range out.Candidates {
				if c.ID == binding {
					candidate = c
				}
			}
			if candidate.Readiness != "ready" || candidate.Capabilities.Meta.State != "supported" || candidate.Executable == "" || o.Launcher == "" {
				r := DescribeMeta(nil, binding, 900000, candidate)
				out.Meta = &r
				out.Status = "meta-unavailable"
				out.Note = "Meta capability unavailable or unknown. Resolve observations or explicitly configure --meta-command JSON_ARGV; BYO containment remains unknown. No proposal saved."
				return out, nil
			}
			command = []string{o.Launcher, "__adapter", binding, candidate.Executable}
			if err = requireAbsoluteNative("atoms[learn].command", command); err != nil {
				return out, err
			}
		}
		if role, declared := active.Formula.RoleBinding(formula.MetaHarness); declared && (binding != role.Binding || !slices.Equal(command, role.Command)) {
			return out, fmt.Errorf("harnesses.meta is declared (binding %q); a learn Atom with binding %q and command %v would conflict with it and learning would refuse to run; request the declared binding and command, or change harnesses.meta through a reviewed Formula change; no proposal saved", role.Binding, binding, command)
		}
		r := DescribeMeta(command, binding, 900000, candidate)
		r.BeforeYAML = before
		out.Meta = &r
		b, e := yaml.Marshal(recipe["atoms"])
		if e != nil {
			return out, e
		}
		var atoms []any
		if e = yaml.Unmarshal(b, &atoms); e != nil {
			return out, e
		}
		for _, v := range atoms {
			if a, ok := v.(map[string]any); ok && a["type"] == "learn" {
				return out, fmt.Errorf("existing learn Atom preserved; review its configuration explicitly")
			}
		}
		recipe["atoms"] = append(atoms, map[string]any{"id": "learn", "type": "learn", "binding": binding, "command": command, "timeoutMs": 900000})
		out.Note = "Explicit opt-in proposes a separate meta binding. Activation invokes no model. Later learn saves a pending suggestion; Formula mutation requires separate human approval."
	}
	encoded, err := yaml.Marshal(recipe)
	if err != nil {
		return out, err
	}
	if o.Rebind != "" {
		if err = verifyRebindExact(active.Formula.YAML, string(encoded), selected.ID, command, rewrites); err != nil {
			return out, err
		}
	}
	if active.Approved && o.Preferred != "" && o.MetaHarness == "" && o.MetaCommand == nil && LearnBound(active.Formula) {
		proposed, e := formula.Parse(encoded)
		if e != nil {
			return out, e
		}
		if !slices.Equal(learnCommand(active.Formula), learnCommand(proposed)) {
			refreshed := InspectMeta(ctx, proposed, out.Repository.Root, o.Runner)
			out.Meta = &refreshed
		}
	}
	evidence, err := proposalEvidence(o.LabDir, active, &out.Repository)
	if err != nil {
		return out, err
	}
	out.Binding = &BindingReport{Command: command, Candidate: selected, Workflow: selected.WorkflowCapability(), LearnBound: LearnBound(active.Formula)}
	if o.MetaHarness != "" || o.MetaCommand != nil || LearnBound(active.Formula) {
		out.Binding.Meta = out.Meta
		out.Binding.LearnBound = true
		if out.BeforeYAML != "" {
			out.Meta.BeforeYAML = out.BeforeYAML
		}
	}
	p, err := saveWithEvidence(o.LabDir, string(encoded), evidence, out.Binding)
	if err != nil {
		return out, err
	}
	// A first-time init (no approved Formula yet) gives the new Lab the recommended
	// verification slot limit; an existing resources.json is never touched.
	if active.Formula.ID == "" {
		if err = resources.WriteInitialConfig(o.LabDir); err != nil {
			return out, err
		}
	}
	out.Status = "pending"
	out.Proposal = &p
	return out, nil
}
