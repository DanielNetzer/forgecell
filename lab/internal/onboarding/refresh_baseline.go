package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
)

// maskBaselineKeys removes the only key a baseline refresh may change: the
// top-level requiredChecks.
func maskBaselineKeys(doc map[string]any) {
	delete(doc, "requiredChecks")
}

// verifyBaselineDiff proves a baseline refresh proposal differs from the approved
// Formula only in the key maskBaselineKeys names.
func verifyBaselineDiff(before, after string) error {
	var a, b map[string]any
	if err := yaml.Unmarshal([]byte(before), &a); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(after), &b); err != nil {
		return err
	}
	maskBaselineKeys(a)
	maskBaselineKeys(b)
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("baseline refresh would change more than requiredChecks; no proposal saved")
	}
	return nil
}

// verifyBaselineExact proves the proposal is the approved Formula with only the
// derived requiredChecks set.
func verifyBaselineExact(before, after string, checks []formula.RequiredCheck) error {
	if err := verifyBaselineDiff(before, after); err != nil {
		return err
	}
	var expected, proposed map[string]any
	if err := yaml.Unmarshal([]byte(before), &expected); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(after), &proposed); err != nil {
		return err
	}
	encoded, err := yaml.Marshal(checks)
	if err != nil {
		return err
	}
	var value any
	if err = yaml.Unmarshal(encoded, &value); err != nil {
		return err
	}
	expected["requiredChecks"] = value
	if !reflect.DeepEqual(expected, proposed) {
		return fmt.Errorf("baseline refresh would set requiredChecks other than the derived baseline; no proposal saved")
	}
	return nil
}

// editRequiredChecksText replaces the top-level requiredChecks block of the
// approved Formula text with the derived checks, or appends one when the key is
// absent. Every other line is kept byte for byte. A key it cannot replace in
// place is refused.
func editRequiredChecksText(text string, checks []formula.RequiredCheck) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode || doc.Content[0].Style&yaml.FlowStyle != 0 {
		return "", fmt.Errorf("the approved Formula is not a block mapping, so requiredChecks cannot be edited in place; no proposal saved")
	}
	encoded, err := yaml.Marshal(map[string]any{"requiredChecks": checks})
	if err != nil {
		return "", err
	}
	block := strings.Split(strings.TrimSuffix(string(encoded), "\n"), "\n")
	lines := strings.Split(text, "\n")
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, val := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode || key.Value != "requiredChecks" {
			continue
		}
		if key.Column != 1 || key.Line < 1 || key.Line > len(lines) || key.Anchor != "" || val.Kind == yaml.AliasNode || val.Anchor != "" || val.Line < key.Line {
			return "", fmt.Errorf("requiredChecks is an alias, an anchor or not a top-level block key, so it cannot be edited in place; no proposal saved")
		}
		last := lastLine(lines, key.Line-1, 0, val.Kind == yaml.SequenceNode)
		return strings.Join(slices.Replace(lines, key.Line-1, last+1, block...), "\n"), nil
	}
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	return strings.Join(slices.Insert(lines, end, block...), "\n"), nil
}

func sameRequiredCheck(a, b formula.RequiredCheck) bool {
	return a.ID == b.ID && slices.Equal(a.Argv, b.Argv) && a.Dir == b.Dir && a.TimeoutMS == b.TimeoutMS && a.Workflow == b.Workflow && a.Reason == b.Reason && a.Select == b.Select
}

func argvText(argv []string) string {
	b, _ := json.Marshal(argv)
	return string(b)
}

func quotedText(s string) string {
	if s == "" {
		return "absent"
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// workflowSource names the repository-relative workflow file a check came from and
// its SHA-256 in the current repository snapshot.
func workflowSource(r Repository, workflow string) string {
	for _, e := range r.Evidence {
		if e.Path == workflow {
			return fmt.Sprintf("%s sha256 %s", workflow, e.SHA256)
		}
	}
	return workflow + " (not in the current repository snapshot)"
}

// baselineChanges lists each check added, removed or changed between the approved
// requiredChecks and the derived baseline, keyed by id. Both lists are bounded by
// formula.MaxRequiredChecks.
func baselineChanges(current, derived []formula.RequiredCheck, r Repository) []string {
	var changes []string
	for _, d := range derived {
		i := slices.IndexFunc(current, func(c formula.RequiredCheck) bool { return c.ID == d.ID })
		if i < 0 {
			changes = append(changes, fmt.Sprintf("added %s: argv %s, dir %s, timeoutMs %d (from %s)", d.ID, argvText(d.Argv), quotedText(d.Dir), d.TimeoutMS, workflowSource(r, d.Workflow)))
			continue
		}
		c := current[i]
		if sameRequiredCheck(c, d) {
			continue
		}
		var fields []string
		if !slices.Equal(c.Argv, d.Argv) {
			fields = append(fields, fmt.Sprintf("argv %s -> %s", argvText(c.Argv), argvText(d.Argv)))
		} else {
			fields = append(fields, "argv "+argvText(d.Argv)+" (unchanged)")
		}
		if c.TimeoutMS != d.TimeoutMS {
			fields = append(fields, fmt.Sprintf("timeoutMs %d -> %d", c.TimeoutMS, d.TimeoutMS))
		} else {
			fields = append(fields, fmt.Sprintf("timeoutMs %d (unchanged)", d.TimeoutMS))
		}
		if c.Dir != d.Dir {
			fields = append(fields, fmt.Sprintf("dir %s -> %s", quotedText(c.Dir), quotedText(d.Dir)))
		}
		if c.Workflow != d.Workflow {
			fields = append(fields, fmt.Sprintf("workflow %s -> %s", quotedText(c.Workflow), quotedText(d.Workflow)))
		}
		if c.Select != d.Select {
			fields = append(fields, fmt.Sprintf("select %s -> %s", quotedText(c.Select), quotedText(d.Select)))
		}
		if c.Reason != d.Reason {
			fields = append(fields, "reason rewritten from the current workflow")
		}
		changes = append(changes, fmt.Sprintf("changed %s: %s (from %s)", d.ID, strings.Join(fields, "; "), workflowSource(r, d.Workflow)))
	}
	for _, c := range current {
		if !slices.ContainsFunc(derived, func(d formula.RequiredCheck) bool { return d.ID == c.ID }) {
			changes = append(changes, fmt.Sprintf("removed %s: argv %s, dir %s, timeoutMs %d (was from %s)", c.ID, argvText(c.Argv), quotedText(c.Dir), c.TimeoutMS, workflowSource(r, c.Workflow)))
		}
	}
	if len(changes) == 0 {
		ids := func(checks []formula.RequiredCheck) string {
			out := make([]string, len(checks))
			for i, c := range checks {
				out[i] = c.ID
			}
			return strings.Join(out, ", ")
		}
		changes = append(changes, fmt.Sprintf("order changed: %s -> %s", ids(current), ids(derived)))
	}
	return changes
}

// proposeBaselineRefresh saves a reviewed proposal that sets requiredChecks of the
// approved Formula to the baseline derived from the current repository snapshot.
// An unchanged baseline saves nothing; an empty one never replaces checks.
func proposeBaselineRefresh(ctx context.Context, o InitOptions, out Outcome, active formula.Loaded) (Outcome, error) {
	f := active.Formula
	// A plugin forge's CI kind selects the derivation; GitHub keeps every word.
	source := "pull-request workflows"
	if o.ciKind == gitlabCIKind {
		source = gitlabCIFile
	}
	derived, err := bootstrapBaselineFor(ctx, out.Repository, o.ciKind)
	if err != nil {
		return out, err
	}
	current := f.RequiredChecks
	if o.ciKind == ciKindNone {
		if len(current) > 0 {
			return out, fmt.Errorf("the forge plugin reports no CI (CI kind %s), so there is no baseline to derive, and the approved Formula has %d requiredChecks; refusing to propose dropping every protected check; no proposal saved", ciKindNone, len(current))
		}
		out.BeforeYAML = f.YAML
		out.Status = "unchanged"
		out.Note = fmt.Sprintf("Baseline unchanged: the forge plugin reports no CI (CI kind %s), so there is no baseline to derive; no proposal saved.", ciKindNone)
		return out, nil
	}
	if len(derived) == 0 && len(current) > 0 {
		if o.ciKind == gitlabCIKind {
			return out, fmt.Errorf("the current %s yields no recognized baseline check, and the approved Formula has %d requiredChecks; refusing to propose dropping every protected check; no proposal saved", source, len(current))
		}
		return out, fmt.Errorf("the current %s yield no recognized baseline check, and the approved Formula has %d requiredChecks; refusing to propose dropping every protected check; no proposal saved", source, len(current))
	}
	out.BeforeYAML = f.YAML
	if slices.EqualFunc(current, derived, sameRequiredCheck) {
		out.Status = "unchanged"
		out.Note = fmt.Sprintf("Baseline unchanged: the CI baseline derived from the current %s equals the approved requiredChecks (%d check(s)); no proposal saved.", source, len(current))
		return out, nil
	}

	proposedText, err := editRequiredChecksText(f.YAML, derived)
	if err != nil {
		return out, err
	}
	if err = verifyBaselineExact(f.YAML, proposedText, derived); err != nil {
		return out, err
	}

	note := fmt.Sprintf("Proposes only a requiredChecks baseline refresh, derived read-only from the %s at revision %s. Changes: %s.", source, out.Repository.Revision, strings.Join(baselineChanges(current, derived, out.Repository), "; "))
	note += " Only the top-level requiredChecks key changes; atoms, instructions, harness bindings, model pins, timeouts and learned guidance are preserved. Every later plan must include the new baseline. Review the exact YAML before approval."

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
		return out, err
	}
	out.Status = "pending"
	out.Proposal = &p
	out.Note = note
	return out, nil
}
