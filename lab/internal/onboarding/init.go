package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
	"reflect"
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

func codingOverridden(f formula.Formula) bool {
	_, ok := f.Harnesses[string(formula.CodingHarness)]
	return ok
}

// checkRebindTarget requires a ready, discovered harness with an executable and
// a launcher, and refuses a rebind that would change nothing.
func checkRebindTarget(candidates []Candidate, id, launcher string, active formula.Formula, lab string) error {
	for _, c := range candidates {
		if c.ID != id {
			continue
		}
		if c.Readiness != "ready" || c.Executable == "" {
			return fmt.Errorf("cannot rebind to %s: %s; run forgecell doctor --lab %q to see what is missing", id, c.Detail, lab)
		}
		if launcher == "" {
			return fmt.Errorf("cannot rebind to %s: the Forgecell launcher is unknown; set FORGECELL_LAUNCHER to its path", id)
		}
		if active.Harness.Binding == id && reflect.DeepEqual(active.Harness.Command, []string{launcher, "__adapter", id, c.Executable}) {
			return fmt.Errorf("the default harness is already bound to %s with this launcher and executable; nothing to rebind", id)
		}
		return nil
	}
	return fmt.Errorf("cannot rebind to %q: no such harness was discovered; run forgecell doctor --lab %q to list the available harnesses", id, lab)
}

// verifyRebindDiff proves a rebind proposal differs from the approved Formula only
// in binding keys: harness.binding, harness.command and, unless harnesses.coding
// overrides it, the binding of the harness Atom.
func verifyRebindDiff(before, after string, codingOverride bool) error {
	var a, b map[string]any
	if err := yaml.Unmarshal([]byte(before), &a); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(after), &b); err != nil {
		return err
	}
	for _, doc := range []map[string]any{a, b} {
		if h, ok := doc["harness"].(map[string]any); ok {
			delete(h, "binding")
			delete(h, "command")
		}
		if atoms, ok := doc["atoms"].([]any); ok && !codingOverride {
			for _, v := range atoms {
				if atom, ok := v.(map[string]any); ok && atom["type"] == "harness" {
					delete(atom, "binding")
				}
			}
		}
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("rebind would change more than the binding keys; no proposal saved")
	}
	return nil
}

func Prepare(ctx context.Context, o InitOptions) (out Outcome, err error) {
	budgetRequested := o.HarnessTimeoutMS != 0 || o.MaxTimeoutMS != 0
	if budgetRequested {
		if _, _, err = harnessBudget(o); err != nil {
			return out, err
		}
	}
	if o.Rebind != "" {
		if o.Preferred != "" || o.MetaHarness != "" || o.MetaCommand != nil || budgetRequested {
			return out, fmt.Errorf("--rebind changes only the binding; do not combine it with --harness, --meta-harness, --meta-command or the timeout flags")
		}
		o.Preferred = o.Rebind
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
			return out, fmt.Errorf("a proposal is already pending; review, approve or dismiss it with init --lab %q --review/--approve/--dismiss %s before --rebind", o.LabDir, pending.ID)
		}
		if budgetRequested {
			return out, fmt.Errorf("a proposal is already pending; review, approve or dismiss it before proposing a new harness timeout budget")
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
			report := DescribeMeta(nil, "", 0, Candidate{})
			for _, atom := range f.Atoms {
				if atom.Type == "learn" {
					report = DescribeMeta(atom.Command, atom.Binding, atom.TimeoutMS, Candidate{})
					break
				}
			}
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
		return out, fmt.Errorf("--rebind needs an approved Formula; run init --lab %q and approve the bootstrap proposal first", o.LabDir)
	}
	if active.Formula.ID != "" {
		approved, e := formula.Load(o.LabDir, "")
		if e != nil || !approved.Approved {
			if o.Rebind != "" {
				return out, fmt.Errorf("--rebind needs a current exact approval of the active Formula; run init --lab %q to review it, then init --lab %q --approve PROPOSAL_ID", o.LabDir, o.LabDir)
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
	var recipe map[string]any
	if active.Approved {
		out.BeforeYAML = active.Formula.YAML
		if err = yaml.Unmarshal([]byte(active.Formula.YAML), &recipe); err != nil {
			return out, err
		}
		binding, ok := recipe["harness"].(map[string]any)
		if !ok {
			return out, fmt.Errorf("existing harness is not a mapping")
		}
		if o.Preferred != "" {
			binding["binding"] = selected.ID
			binding["command"] = command
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
		if atoms, ok := recipe["atoms"].([]any); ok && !codingOverridden(active.Formula) {
			for _, value := range atoms {
				if atom, ok := value.(map[string]any); ok && atom["type"] == "harness" && o.Preferred != "" {
					atom["binding"] = selected.ID
				}
			}
		}
		out.Note = "Proposes only a coding-harness rebind (plus any explicitly requested timeout budget); existing instructions, workflow and meta-harness configuration are preserved. Review the complete YAML before approval."
		if o.Rebind != "" {
			out.Note = "Proposes only a binding rebind to " + selected.ID + ": harness.binding, harness.command and the harness Atom binding change (the Atom binding is left alone when harnesses.coding overrides it). Atoms, checks, instructions, learned guidance and per-role bindings are preserved. Review the exact YAML before approval."
		}
	} else {
		name := strings.ToLower(strings.Split(out.Repository.Repo, "/")[1]) + "-github-issue"
		timeoutMS, maxTimeoutMS, err := harnessBudget(o)
		if err != nil {
			return out, err
		}
		recipe = map[string]any{"schemaVersion": "v0", "kind": "formula", "id": name, "name": out.Repository.Repo + " issue workflow", "createdAt": time.Now().UTC().Format(time.RFC3339Nano), "source": "bootstrap-assay", "intake": map[string]any{"source": "github-issues", "repo": out.Repository.Repo}, "harness": map[string]any{"binding": selected.ID, "command": command, "invoke": "byo", "timeoutMs": timeoutMS, "maxTimeoutMs": maxTimeoutMS}, "lab": map[string]any{"dir": o.LabDir, "observe": false}, "repositoryContext": out.Repository.Compact(), "atoms": []map[string]any{{"id": "intake", "type": "intake", "source": "github-issues"}, {"id": "scope", "type": "gate", "purpose": "scope", "mode": "human"}, {"id": "harness", "type": "harness", "binding": selected.ID}, {"id": "check", "type": "check", "workflows": out.Repository.Workflows}, {"id": "gate", "type": "gate", "purpose": "review", "mode": "human"}, {"id": "ship", "type": "ship", "via": "github"}, {"id": "document", "type": "document", "target": "github-issue"}}}
		out.Note = "Bootstrap Assay is read-only. Proposed checks are evidence, not executed commands. Approving binds the coding harness; runs still stop at human review."
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
		if err = verifyRebindDiff(active.Formula.YAML, string(encoded), codingOverridden(active.Formula)); err != nil {
			return out, err
		}
	}
	var evidence *Repository
	if !active.Approved {
		evidence = &out.Repository
	} else if active.Approval.SourceKind == "bootstrap-assay" {
		previous, e := InspectProposal(o.LabDir, active.Approval.SourceID)
		if e != nil {
			return out, e
		}
		if previous.EvidencePath != "" {
			data, e := readArtifact(o.LabDir, previous.EvidencePath)
			if e != nil {
				return out, e
			}
			var original Repository
			if e = json.Unmarshal(data, &original); e != nil {
				return out, e
			}
			evidence = &original
		}
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
	out.Status = "pending"
	out.Proposal = &p
	return out, nil
}
