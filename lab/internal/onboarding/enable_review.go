package onboarding

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
)

// reviewAtomIndex is where formula.ExecutionRoles requires the review Atom:
// immediately after the check Atom and before the human review gate.
const reviewAtomIndex = 4

// SameReviewerWarning is shown when the resolved reviewer runs the coding command.
const SameReviewerWarning = "the same CLI reviews its own change, in read-only review mode; configure harnesses.review to use a different reviewer"

// maskReviewKeys removes the only key enabling review may change: the top-level
// atoms list.
func maskReviewKeys(doc map[string]any) {
	delete(doc, "atoms")
}

// verifyReviewDiff proves an enable-review proposal differs from the approved
// Formula only in the key maskReviewKeys names.
func verifyReviewDiff(before, after string) error {
	var a, b map[string]any
	if err := yaml.Unmarshal([]byte(before), &a); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(after), &b); err != nil {
		return err
	}
	maskReviewKeys(a)
	maskReviewKeys(b)
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("--enable-review would change more than the atoms list; no proposal saved")
	}
	return nil
}

// verifyReviewExact proves the proposal is the approved Formula with exactly one
// {id: review, type: review} Atom inserted after the check Atom.
func verifyReviewExact(before, after string) error {
	if err := verifyReviewDiff(before, after); err != nil {
		return err
	}
	var expected, proposed map[string]any
	if err := yaml.Unmarshal([]byte(before), &expected); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(after), &proposed); err != nil {
		return err
	}
	atoms, ok := expected["atoms"].([]any)
	if !ok || len(atoms) < reviewAtomIndex {
		return fmt.Errorf("the approved Formula has no atoms list with a check Atom; no proposal saved")
	}
	expected["atoms"] = slices.Insert(slices.Clone(atoms), reviewAtomIndex, any(map[string]any{"id": "review", "type": "review"}))
	if !reflect.DeepEqual(expected, proposed) {
		return fmt.Errorf("--enable-review would change the atoms list other than by inserting {id: review, type: review} after the check Atom; no proposal saved")
	}
	return nil
}

// insertReviewAtomText inserts one review Atom item into the top-level block
// atoms sequence of the approved Formula text, immediately before the item at
// reviewAtomIndex, in that item's style. Every other line is kept byte for byte.
// A list it cannot edit in place is refused.
func insertReviewAtomText(text string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode || doc.Content[0].Style&yaml.FlowStyle != 0 {
		return "", fmt.Errorf("the approved Formula is not a block mapping, so atoms cannot be edited in place; no proposal saved")
	}
	lines := strings.Split(text, "\n")
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Value != "atoms" {
			continue
		}
		if key.Column != 1 || key.Anchor != "" || val.Kind != yaml.SequenceNode || val.Anchor != "" || val.Style&yaml.FlowStyle != 0 || len(val.Content) <= reviewAtomIndex {
			return "", fmt.Errorf("atoms is an alias, an anchor, a flow sequence or not a top-level block list with a gate after the check Atom, so it cannot be edited in place; no proposal saved")
		}
		next := val.Content[reviewAtomIndex]
		if next.Kind != yaml.MappingNode || next.Anchor != "" || next.Line < 1 || next.Line > len(lines) || next.Column < 3 {
			return "", fmt.Errorf("the Atom after the check Atom is not a plain mapping item, so atoms cannot be edited in place; no proposal saved")
		}
		line := lines[next.Line-1]
		dash := next.Column - 3
		if len(line) < next.Column-1 || strings.TrimLeft(line[:dash], " ") != "" || line[dash:next.Column-1] != "- " {
			return "", fmt.Errorf("the Atom after the check Atom does not start its own list line, so atoms cannot be edited in place; no proposal saved")
		}
		indent := line[:dash]
		item := []string{indent + "- {id: review, type: review}"}
		if next.Style&yaml.FlowStyle == 0 {
			item = []string{indent + "- id: review", indent + "  type: review"}
		}
		return strings.Join(slices.Insert(lines, next.Line-1, item...), "\n"), nil
	}
	return "", fmt.Errorf("the approved Formula has no top-level atoms list; no proposal saved")
}

// reviewerNote names the resolved reviewer, and says so prominently when it runs
// the coding command. It names bindings and sources, never command paths.
func reviewerNote(res ReviewResolution) string {
	note := fmt.Sprintf(" Reviewer: binding %s (from %s).", res.Binding.Binding, res.Source)
	if res.SameCommandAsCoding {
		note += " WARNING: " + SameReviewerWarning + "."
	}
	return note
}

// proposeEnableReview saves a reviewed proposal that inserts exactly one review
// Atom after the check Atom of the approved Formula. Nothing activates until the
// proposal is approved.
func proposeEnableReview(ctx context.Context, o InitOptions, out Outcome, active formula.Loaded) (Outcome, error) {
	f := active.Formula
	if _, present, err := f.ReviewAtom(); err != nil {
		return out, err
	} else if present {
		return out, fmt.Errorf("--enable-review: the approved Formula already has a review Atom; no proposal saved")
	}
	if slices.ContainsFunc(f.Atoms, func(a formula.Atom) bool { return a.ID == "review" }) {
		return out, fmt.Errorf("--enable-review: the approved Formula already has an Atom with id review; rename it through a reviewed Formula change first; no proposal saved")
	}
	if _, err := f.ExecutionRoles(); err != nil {
		return out, fmt.Errorf("--enable-review needs an approved Formula in the ticket readiness shape: %v; no proposal saved", err)
	}
	proposedText, err := insertReviewAtomText(f.YAML)
	if err != nil {
		return out, err
	}
	if err = verifyReviewExact(f.YAML, proposedText); err != nil {
		return out, err
	}
	proposed, err := formula.Parse([]byte(proposedText))
	if err != nil {
		return out, err
	}
	roles, err := proposed.ExecutionRoles()
	if err != nil {
		return out, fmt.Errorf("--enable-review: the proposed Formula fails ticket readiness: %v; no proposal saved", err)
	}
	if roles[formula.Review].ID != "review" {
		return out, fmt.Errorf("--enable-review: the inserted Atom does not take the review role; no proposal saved")
	}
	res, _, err := ResolveReviewBinding(proposed)
	if err != nil {
		return out, fmt.Errorf("--enable-review: %v; no proposal saved", err)
	}
	out.BeforeYAML = f.YAML
	note := "Proposes only enabling the adversarial review: one Atom {id: review, type: review} inserted after the check Atom and before the human review gate."
	note += reviewerNote(res)
	note += " Every other key and line is preserved byte for byte. Runs still stop at the human review gate. Review the exact YAML before approval."

	probed := ProbeBinding(ctx, f.Harness, out.Repository.Root, o.Runner)
	evidence, err := proposalEvidence(o.LabDir, active, nil)
	if err != nil {
		return out, err
	}
	out.Binding = &BindingReport{Command: f.Harness.Command, Candidate: probed, Workflow: probed.WorkflowCapability(), LearnBound: LearnBound(f), BeforeYAML: f.YAML}
	// Saved meta evidence must describe the one learn Atom that carries a command.
	learns := 0
	for _, a := range f.Atoms {
		if a.Type == "learn" {
			learns++
		}
	}
	if learn := metaLearnAtom(f); learns == 1 && learn != nil && len(learn.Command) > 0 {
		r := DescribeMeta(learn.Command, learn.Binding, learn.TimeoutMS, Candidate{})
		r.BeforeYAML = f.YAML
		out.Meta = &r
		out.Binding.Meta = &r
	}
	p, err := saveWithEvidence(o.LabDir, proposedText, evidence, out.Binding)
	if err != nil {
		return out, fmt.Errorf("--enable-review: %v; no proposal saved", err)
	}
	out.Status = "pending"
	out.Proposal = &p
	out.Note = note
	return out, nil
}
