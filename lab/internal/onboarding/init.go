package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
	"strings"
	"time"
)

type InitOptions struct {
	Cwd, LabDir, Launcher, Preferred string
	Env                              map[string]string
	Candidates                       []Candidate
	Runner                           Runner
}
type Outcome struct {
	Binding    *BindingReport `json:"bindingCapabilities,omitempty"`
	Status     string         `json:"status"`
	Repository Repository     `json:"repository"`
	Candidates []Candidate    `json:"candidates"`
	Selection  Selection      `json:"selection"`
	Proposal   *Proposal      `json:"proposal,omitempty"`
	BeforeYAML string         `json:"beforeYaml,omitempty"`
	Note       string         `json:"note"`
}

func Prepare(ctx context.Context, o InitOptions) (out Outcome, err error) {
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

	if active.Formula.ID != "" {
		approved, e := formula.Load(o.LabDir, "")
		if e != nil || !approved.Approved {
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
	existing := ""
	if active.Approved {
		existing = active.Formula.Harness.Binding
	}
	out.Selection = Select(out.Candidates, o.Preferred, existing, o.Env)
	if active.Approved && o.Preferred == "" {
		checked := ProbeBinding(ctx, active.Formula.Harness, out.Repository.Root, o.Runner)
		out.Binding = &BindingReport{Command: active.Formula.Harness.Command, Candidate: checked, Workflow: checked.WorkflowCapability(), LearnBound: LearnBound(active.Formula)}
		out.Status = "unchanged"
		out.BeforeYAML = active.Formula.YAML
		out.Note = "Preserved existing approved Formula and exact command bindings. Explicitly choose --harness to propose a rebind."
		return out, nil
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
	if selected.Executable == "" || o.Launcher == "" {
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
		binding["binding"] = selected.ID
		binding["command"] = command
		if atoms, ok := recipe["atoms"].([]any); ok {
			for _, value := range atoms {
				if atom, ok := value.(map[string]any); ok && atom["type"] == "harness" {
					atom["binding"] = selected.ID
				}
			}
		}
		out.Note = "Proposes only a coding-harness rebind; existing instructions, timeouts, workflow and meta-harness configuration are preserved. Review the complete YAML before approval."
	} else {
		name := strings.ToLower(strings.Split(out.Repository.Repo, "/")[1]) + "-github-issue"
		recipe = map[string]any{"schemaVersion": "v0", "kind": "formula", "id": name, "name": out.Repository.Repo + " issue workflow", "createdAt": time.Now().UTC().Format(time.RFC3339Nano), "source": "bootstrap-assay", "intake": map[string]any{"source": "github-issues", "repo": out.Repository.Repo}, "harness": map[string]any{"binding": selected.ID, "command": command, "invoke": "byo", "timeoutMs": 900000}, "lab": map[string]any{"dir": o.LabDir, "observe": false}, "repositoryContext": out.Repository.Compact(), "atoms": []map[string]any{{"id": "intake", "type": "intake", "source": "github-issues"}, {"id": "scope", "type": "gate", "purpose": "scope", "mode": "human"}, {"id": "harness", "type": "harness", "binding": selected.ID}, {"id": "check", "type": "check", "workflows": out.Repository.Workflows}, {"id": "gate", "type": "gate", "purpose": "review", "mode": "human"}, {"id": "ship", "type": "ship", "via": "github"}, {"id": "document", "type": "document", "target": "github-issue"}}}
		out.Note = "Bootstrap Assay is read-only. Proposed checks are evidence, not executed commands. Approving binds the coding harness; runs still stop at human review."
	}
	encoded, err := yaml.Marshal(recipe)
	if err != nil {
		return out, err
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
	p, err := saveWithEvidence(o.LabDir, string(encoded), evidence, out.Binding)
	if err != nil {
		return out, err
	}
	out.Status = "pending"
	out.Proposal = &p
	return out, nil
}
