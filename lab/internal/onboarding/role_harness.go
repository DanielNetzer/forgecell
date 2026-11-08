package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// checkRoleHarnessOptions refuses --harness ID --role ROLE combinations before
// anything is read or written.
func checkRoleHarnessOptions(o InitOptions, budgetRequested bool) error {
	if o.Preferred == "" {
		return fmt.Errorf("--role needs --harness ID to propose a role harness override")
	}
	if o.Rebind != "" || o.ModelSet || o.Model != "" || o.MetaHarness != "" || o.MetaCommand != nil || budgetRequested {
		return fmt.Errorf("--harness ID --role ROLE changes only harnesses.ROLE; do not combine it with --rebind, --model, --meta-harness, --meta-command or the timeout flags")
	}
	if !slices.Contains(formula.HarnessRoles, formula.HarnessRole(o.Role)) {
		return fmt.Errorf("--role %q cannot take a harness override; use analysis, coding or meta", o.Role)
	}
	return nil
}

// roleHarnessTarget requires a ready, discovered harness with an executable and a
// launcher whose adapter can perform the role's operation, and returns the native
// command the role override would run.
func roleHarnessTarget(candidates []Candidate, id, launcher string, role formula.HarnessRole, lab string) ([]string, error) {
	path := "harnesses." + string(role)
	for _, c := range candidates {
		if c.ID != id {
			continue
		}
		if c.Readiness != "ready" || c.Executable == "" {
			return nil, fmt.Errorf("cannot bind %s to %s: %s; run forgecell doctor --lab %s to see what is missing; no proposal saved", path, id, c.Detail, formula.ShellWord(lab))
		}
		if launcher == "" {
			return nil, fmt.Errorf("cannot bind %s to %s: the Forgecell launcher is unknown; set FORGECELL_LAUNCHER to its path; no proposal saved", path, id)
		}
		command := []string{launcher, "__adapter", id, c.Executable}
		if err := requireAbsoluteNative(path+".command", command); err != nil {
			return nil, err
		}
		if capability := roleCapability(role, formula.Binding{Binding: id, Command: command}); capability.State == "unsupported" {
			return nil, fmt.Errorf("cannot bind %s to %s: it cannot be used for %s: %s; no proposal saved", path, id, roleOperation(role), capability.Reason)
		}
		return command, nil
	}
	return nil, fmt.Errorf("cannot bind %s to %q: no such harness was discovered; run forgecell doctor --lab %s to list the available harnesses; no proposal saved", path, id, formula.ShellWord(lab))
}

// effectiveRoleBinding is the binding a role runs before the change: its own
// override, else the learn Atom binding for meta, else the default harness.
func effectiveRoleBinding(f formula.Formula, role formula.HarnessRole) (b formula.Binding, source string) {
	if b, declared := f.Harnesses[string(role)]; declared {
		return b, "its own override"
	}
	if role == formula.MetaHarness {
		if b, _, bound := MetaBinding(f); bound {
			return b, "the learn Atom"
		}
	}
	return f.Harness, "the default binding"
}

func anyArgv(command []string) []any {
	items := make([]any, len(command))
	for i, item := range command {
		items[i] = item
	}
	return items
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// roleHolders returns the harnesses mapping of a decoded Formula and the role's
// binding mapping in it; either is nil when absent.
func roleHolders(doc map[string]any, role formula.HarnessRole) (roles, holder map[string]any, err error) {
	value, present := doc["harnesses"]
	if !present {
		return nil, nil, nil
	}
	if roles, _ = value.(map[string]any); roles == nil {
		return nil, nil, fmt.Errorf("harnesses is not a mapping; no proposal saved")
	}
	value, present = roles[string(role)]
	if !present {
		return roles, nil, nil
	}
	if holder, _ = value.(map[string]any); holder == nil {
		return nil, nil, fmt.Errorf("harnesses.%s is not a mapping; no proposal saved", role)
	}
	return roles, holder, nil
}

// roleHarnessRecipe sets the binding and command of harnesses.ROLE, declaring the
// role when it is absent, and returns each change as `path: old -> new`.
func roleHarnessRecipe(recipe map[string]any, role formula.HarnessRole, target string, command []string) ([]string, error) {
	roles, holder, err := roleHolders(recipe, role)
	if err != nil {
		return nil, err
	}
	path := "harnesses." + string(role)
	if holder == nil {
		if roles == nil {
			roles = map[string]any{}
			recipe["harnesses"] = roles
		}
		declared := map[string]any{"binding": target, "command": anyArgv(command)}
		roles[string(role)] = declared
		return []string{path + ": (none) -> " + jsonText(declared)}, nil
	}
	var changes []string
	for key, value := range map[string]any{"binding": target, "command": anyArgv(command)} {
		if old, ok := holder[key]; !ok {
			changes = append(changes, path+"."+key+": (none) -> "+jsonText(value))
		} else if !reflect.DeepEqual(old, value) {
			changes = append(changes, path+"."+key+": "+jsonText(old)+" -> "+jsonText(value))
		}
		holder[key] = value
	}
	slices.Sort(changes)
	return changes, nil
}

// takeRole removes harnesses.ROLE from a decoded Formula, and harnesses itself
// when nothing else is left in it, and returns the removed binding mapping.
func takeRole(doc map[string]any, role formula.HarnessRole) (map[string]any, error) {
	roles, holder, err := roleHolders(doc, role)
	if err != nil || roles == nil {
		return nil, err
	}
	delete(roles, string(role))
	if len(roles) == 0 {
		delete(doc, "harnesses")
	}
	return holder, nil
}

// verifyRoleHarnessExact proves the proposal is the approved Formula with only the
// binding and command of harnesses.ROLE set or added: everything outside the role,
// and every other key of the role, is unchanged.
func verifyRoleHarnessExact(before, after string, role formula.HarnessRole, target string, command []string) error {
	var expected, proposed map[string]any
	if err := yaml.Unmarshal([]byte(before), &expected); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(after), &proposed); err != nil {
		return err
	}
	was, err := takeRole(expected, role)
	if err != nil {
		return err
	}
	now, err := takeRole(proposed, role)
	if err != nil {
		return err
	}
	if now == nil || !reflect.DeepEqual(expected, proposed) {
		return fmt.Errorf("the role harness change would change more than harnesses.%s; no proposal saved", role)
	}
	want := maps.Clone(was)
	if want == nil {
		want = map[string]any{}
	}
	want["binding"], want["command"] = target, anyArgv(command)
	if !reflect.DeepEqual(want, now) {
		return fmt.Errorf("the role harness change would change more than the binding and command of harnesses.%s; no proposal saved", role)
	}
	return nil
}

// learnMetaReport describes the one learn Atom that carries a command, which is
// what saved meta evidence must name; nil when there is no such single Atom.
func learnMetaReport(f formula.Formula) *MetaReport {
	learns := 0
	for _, a := range f.Atoms {
		if a.Type == "learn" {
			learns++
		}
	}
	learn := metaLearnAtom(f)
	if learns != 1 || learn == nil || len(learn.Command) == 0 {
		return nil
	}
	r := DescribeMeta(learn.Command, learn.Binding, learn.TimeoutMS, Candidate{})
	r.BeforeYAML = f.YAML
	return &r
}

// proposeRoleHarness saves a reviewed proposal that declares harnesses.ROLE, or
// rebinds it when declared, to the native command of a discovered harness on the
// running launcher. Nothing else in the approved Formula changes.
func proposeRoleHarness(ctx context.Context, o InitOptions, out Outcome, active formula.Loaded) (Outcome, error) {
	f := active.Formula
	role := formula.HarnessRole(o.Role)
	path := "harnesses." + o.Role
	out.Candidates = o.Candidates
	if out.Candidates == nil {
		out.Candidates = Discover(ctx, out.Repository.Root, o.Runner)
	}
	for i := range out.Candidates {
		out.Candidates[i] = out.Candidates[i].CompleteUnknowns()
	}
	command, err := roleHarnessTarget(out.Candidates, o.Preferred, o.Launcher, role, o.LabDir)
	if err != nil {
		return out, err
	}
	if current, source := effectiveRoleBinding(f, role); current.Binding == o.Preferred && slices.Equal(current.Command, command) {
		return out, fmt.Errorf("%s already runs %s with this launcher and executable through %s; nothing to change; no proposal saved", path, o.Preferred, source)
	}

	var recipe map[string]any
	if err = yaml.Unmarshal([]byte(f.YAML), &recipe); err != nil {
		return out, err
	}
	changes, err := roleHarnessRecipe(recipe, role, o.Preferred, command)
	if err != nil {
		return out, err
	}
	encoded, err := yaml.Marshal(recipe)
	if err != nil {
		return out, err
	}
	if err = verifyRoleHarnessExact(f.YAML, string(encoded), role, o.Preferred, command); err != nil {
		return out, err
	}
	proposed, err := formula.Parse(encoded)
	if err != nil {
		return out, err
	}
	if err = ValidateRoleBindings(proposed); err != nil {
		return out, fmt.Errorf("binding %s to %s would not be a valid Formula: %w; no proposal saved", path, o.Preferred, err)
	}

	probed := ProbeBinding(ctx, f.Harness, out.Repository.Root, o.Runner)
	evidence, err := proposalEvidence(o.LabDir, active, nil)
	if err != nil {
		return out, err
	}
	out.BeforeYAML = f.YAML
	out.Binding = &BindingReport{Command: f.Harness.Command, Candidate: probed, Workflow: probed.WorkflowCapability(), LearnBound: LearnBound(f), BeforeYAML: f.YAML}
	if r := learnMetaReport(f); r != nil {
		out.Meta = r
		out.Binding.Meta = r
	}
	p, err := saveWithEvidence(o.LabDir, string(encoded), evidence, out.Binding)
	if err != nil {
		return out, err
	}
	out.Status = "pending"
	out.Proposal = &p
	out.Selection = Selection{ID: o.Preferred, Status: "ready", Reason: "Explicit role harness override for " + o.Role + "."}
	out.Note = "Proposes only a " + o.Role + " role harness change to " + o.Preferred + ". Rewritten keys (old -> new): " + strings.Join(changes, "; ") + ". The default harness, atoms, checks, instructions, timeouts, learned guidance and every other binding are preserved. Changing the binding changes the binding fingerprint (#36) of the " + o.Role + " role; in-flight plans for that role need a fresh intake. Review the exact YAML before approval."
	return out, nil
}
