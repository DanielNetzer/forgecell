package onboarding

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// pinRoles are the roles --role accepts: the three default-resolving roles and review.
var pinRoles = append(slices.Clone(formula.HarnessRoles), formula.ReviewHarness)

// checkModelPinOptions refuses --model and --role combinations before anything is
// read or written.
func checkModelPinOptions(o InitOptions, budgetRequested bool) error {
	if !o.ModelSet {
		if o.Role != "" || o.Model != "" {
			return fmt.Errorf("--role needs --model")
		}
		return nil
	}
	if o.Preferred != "" || o.Rebind != "" || o.MetaHarness != "" || o.MetaCommand != nil || budgetRequested {
		return fmt.Errorf("--model changes only the model pin; do not combine it with --harness, --rebind, --meta-harness, --meta-command or the timeout flags")
	}
	if o.Model != "" {
		if err := formula.ValidateModel(o.Model); err != nil {
			return fmt.Errorf("--model: %w", err)
		}
	}
	if o.Role != "" && !slices.Contains(pinRoles, formula.HarnessRole(o.Role)) {
		return fmt.Errorf("--role %q is unknown; use analysis, coding, meta or review", o.Role)
	}
	return nil
}

// pinTarget is a binding a model proposal may change. Path is its Formula key.
type pinTarget struct {
	Path    string
	Binding formula.Binding
	// Undeclared marks a default harness the Formula does not declare.
	Undeclared bool
}

// modelChange is one recorded rewrite of a binding's model key. Old is empty for
// an absent key; Remove deletes the key.
type modelChange struct {
	Path, Old, New string
	Remove         bool
}

func pinTargets(f formula.Formula, root *yaml.Node, role string) ([]pinTarget, error) {
	if role != "" {
		b, declared := f.Harnesses[role]
		if !declared {
			return nil, fmt.Errorf("harnesses.%s is not declared, so there is no %s binding to pin; run without --role to pin the default harness and every declared override, or declare harnesses.%s through a reviewed Formula change first; no proposal saved", role, role, role)
		}
		return []pinTarget{{Path: "harnesses." + role, Binding: b}}, nil
	}
	targets := []pinTarget{{Path: "harness", Binding: f.Harness, Undeclared: childNode(root, "harness") == nil}}
	for _, r := range pinRoles {
		if b, declared := f.Harnesses[string(r)]; declared {
			targets = append(targets, pinTarget{Path: "harnesses." + string(r), Binding: b})
		}
	}
	return targets, nil
}

// pinRefusal is why a binding cannot carry a pinned model, or "" when it can.
func pinRefusal(b formula.Binding) string {
	if !nativeAdapter(b) {
		return "is a custom command; Forgecell cannot enforce a pin on a custom command"
	}
	if adapter := b.Command[2]; adapter != "claude-code" {
		return fmt.Sprintf("is bound to %s; a pinned model is not supported for %s in v1", adapter, adapter)
	}
	return ""
}

// pinRefusals names every targeted binding that cannot carry a pinned model.
func pinRefusals(targets []pinTarget) []string {
	var refused []string
	for _, t := range targets {
		if t.Undeclared {
			refused = append(refused, "harness is not declared in the approved Formula, so there is no default binding to pin")
		} else if reason := pinRefusal(t.Binding); reason != "" {
			refused = append(refused, fmt.Sprintf("%s (binding %q) %s", t.Path, t.Binding.Binding, reason))
		}
	}
	return refused
}

func modelText(model string) string {
	if model == "" {
		return "unpinned"
	}
	b, _ := json.Marshal(model)
	return string(b)
}

// childNode is the value of key in a mapping node, or nil.
func childNode(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Kind == yaml.ScalarNode && m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func pinHolder(doc map[string]any, path string) map[string]any {
	if path == "harness" {
		m, _ := doc["harness"].(map[string]any)
		return m
	}
	roles, _ := doc["harnesses"].(map[string]any)
	m, _ := roles[strings.TrimPrefix(path, "harnesses.")].(map[string]any)
	return m
}

func applyModelChanges(doc map[string]any, changes []modelChange) error {
	for _, c := range changes {
		holder := pinHolder(doc, c.Path)
		if holder == nil {
			return fmt.Errorf("model pin rewrote %s, which is not a binding in the approved Formula; no proposal saved", c.Path)
		}
		if c.Remove {
			delete(holder, "model")
		} else {
			holder["model"] = c.New
		}
	}
	return nil
}

// lineEdit replaces del lines starting at line index at with ins.
type lineEdit struct {
	at, del int
	ins     []string
}

// editModelText applies the recorded model changes to the approved Formula text.
// It locates each targeted binding mapping and its model scalar through the
// yaml.v4 Node tree and rewrites only those lines of the original text, so
// comments, blank lines, anchors, key order, indentation and quoting everywhere
// else are preserved byte for byte. A node it cannot edit in place is refused.
func editModelText(text string, changes []modelChange) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		return "", err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("Formula must be a mapping")
	}
	lines := strings.Split(text, "\n")
	var edits []lineEdit
	for _, c := range changes {
		holder, err := bindingNode(doc.Content[0], c.Path)
		if err != nil {
			return "", err
		}
		edit, err := modelLineEdit(lines, holder, c)
		if err != nil {
			return "", fmt.Errorf("%s: %w; no proposal saved", c.Path, err)
		}
		edits = append(edits, edit)
	}
	slices.SortFunc(edits, func(a, b lineEdit) int { return b.at - a.at })
	for _, e := range edits {
		lines = slices.Replace(lines, e.at, e.at+e.del, e.ins...)
	}
	return strings.Join(lines, "\n"), nil
}

func bindingNode(root *yaml.Node, path string) (*yaml.Node, error) {
	var n *yaml.Node
	if path == "harness" {
		n = childNode(root, "harness")
	} else {
		n = childNode(childNode(root, "harnesses"), strings.TrimPrefix(path, "harnesses."))
	}
	switch {
	case n == nil:
		return nil, fmt.Errorf("model pin rewrote %s, which is not a binding in the approved Formula; no proposal saved", path)
	case n.Kind != yaml.MappingNode || n.Style&yaml.FlowStyle != 0:
		return nil, fmt.Errorf("%s is an alias or a flow-style mapping, which cannot be edited in place; rewrite it as a block mapping through a reviewed Formula change first; no proposal saved", path)
	}
	return n, nil
}

var plainModel = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._/-]*$`)

var yamlWords = []string{"true", "false", "yes", "no", "on", "off", "null", "y", "n"}

// renderModel writes a model scalar, keeping the quoting an existing value used.
func renderModel(model string, style yaml.Style) string {
	switch {
	case style&yaml.SingleQuotedStyle != 0:
		return "'" + strings.ReplaceAll(model, "'", "''") + "'"
	case style&yaml.DoubleQuotedStyle != 0 || !plainModel.MatchString(model) || slices.Contains(yamlWords, strings.ToLower(model)):
		return strconv.Quote(model)
	}
	return model
}

func byteOffset(line string, runes int) int {
	for i := range line {
		if runes == 0 {
			return i
		}
		runes--
	}
	return len(line)
}

// scalarEnd is the byte offset where the scalar that starts at start ends.
func scalarEnd(line string, start int, val *yaml.Node) (int, error) {
	rest := line[start:]
	switch {
	case val.Style&yaml.DoubleQuotedStyle != 0:
		if strings.HasPrefix(rest, `"`) {
			for i := 1; i < len(rest); i++ {
				switch rest[i] {
				case '\\':
					i++
				case '"':
					return start + i + 1, nil
				}
			}
		}
	case val.Style&yaml.SingleQuotedStyle != 0:
		if strings.HasPrefix(rest, "'") {
			for i := 1; i < len(rest); i++ {
				if rest[i] == '\'' {
					if i+1 < len(rest) && rest[i+1] == '\'' {
						i++
						continue
					}
					return start + i + 1, nil
				}
			}
		}
	case val.Style == 0 && strings.HasPrefix(rest, val.Value):
		return start + len(val.Value), nil
	}
	return 0, fmt.Errorf("the model value cannot be located in place")
}

// lastLine is the index of the last line of the block that starts at line index
// keyIdx and is indented deeper than indent. A block sequence may sit at indent.
func lastLine(lines []string, keyIdx, indent int, sequence bool) int {
	last := keyIdx
	for i := keyIdx + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		depth := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
		if depth > indent || sequence && depth == indent && strings.HasPrefix(trimmed, "-") {
			last = i
			continue
		}
		break
	}
	return last
}

func modelLineEdit(lines []string, holder *yaml.Node, c modelChange) (lineEdit, error) {
	pairs := holder.Content
	var key, val *yaml.Node
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i].Kind == yaml.ScalarNode && pairs[i].Value == "model" {
			key, val = pairs[i], pairs[i+1]
		}
	}
	if key != nil && (val.Kind != yaml.ScalarNode || val.Anchor != "" || val.Line != key.Line || val.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 || key.Line > len(lines)) {
		return lineEdit{}, fmt.Errorf("model is not a single-line scalar that can be edited in place")
	}
	switch {
	case c.Remove && key == nil:
		return lineEdit{}, fmt.Errorf("the binding has no model key of its own to remove")
	case c.Remove:
		return lineEdit{at: key.Line - 1, del: 1}, nil
	case key != nil:
		line := lines[key.Line-1]
		start := byteOffset(line, val.Column-1)
		end, err := scalarEnd(line, start, val)
		if err != nil {
			return lineEdit{}, err
		}
		return lineEdit{at: key.Line - 1, del: 1, ins: []string{line[:start] + renderModel(c.New, val.Style) + line[end:]}}, nil
	}
	count := len(pairs) / 2
	if count == 0 {
		return lineEdit{}, fmt.Errorf("the binding mapping is empty")
	}
	leading := 0
	for leading < count && pairs[2*leading].Value < "model" {
		leading++
	}
	after := count - 1
	if leading > 0 && leading < count {
		after = leading - 1
	}
	anchor := pairs[2*after]
	if anchor.Line > len(lines) {
		return lineEdit{}, fmt.Errorf("the binding mapping cannot be located in place")
	}
	last := lastLine(lines, anchor.Line-1, anchor.Column-1, pairs[2*after+1].Kind == yaml.SequenceNode)
	return lineEdit{at: last + 1, ins: []string{strings.Repeat(" ", pairs[0].Column-1) + "model: " + renderModel(c.New, 0)}}, nil
}

func withoutModelLines(text string) []string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimLeft(line, " "), "model:") {
			kept = append(kept, line)
		}
	}
	return kept
}

// verifyModelPinExact proves the proposal is the approved Formula with only the
// recorded model keys set or removed. The decoded documents must match once the
// recorded changes are applied, and the texts must match line for line once every
// `model:` line is set aside, so no comment, key order or formatting moved.
func verifyModelPinExact(before, after string, changes []modelChange) error {
	var expected, proposed map[string]any
	if err := yaml.Unmarshal([]byte(before), &expected); err != nil {
		return err
	}
	if err := yaml.Unmarshal([]byte(after), &proposed); err != nil {
		return err
	}
	if err := applyModelChanges(expected, changes); err != nil {
		return err
	}
	if !reflect.DeepEqual(expected, proposed) {
		return fmt.Errorf("model pin would change more than the recorded model keys; no proposal saved")
	}
	if !slices.Equal(withoutModelLines(before), withoutModelLines(after)) {
		return fmt.Errorf("model pin would change lines other than `model:` lines; no proposal saved")
	}
	return nil
}

// roleModelNotes states, from the resolution code itself, which model each role
// without its own override runs under in the proposed Formula.
func roleModelNotes(f formula.Formula) []string {
	var notes []string
	for _, role := range formula.HarnessRoles {
		if b, override := f.RoleBinding(role); !override && role != formula.MetaHarness {
			notes = append(notes, fmt.Sprintf("%s has no override and resolves to the default harness, so it inherits the default's model (%s)", role, modelText(b.Model)))
		}
	}
	if b, source, bound := MetaBinding(f); bound && source == "learn-atom" {
		notes = append(notes, fmt.Sprintf("meta has no override and runs the learn Atom's command, which carries no model, so it does not inherit the default's pin (%s)", modelText(b.Model)))
	}
	if res, configured, err := ResolveReviewBinding(f); configured && err == nil && res.Source != "review-role" {
		notes = append(notes, fmt.Sprintf("review has no override and resolves to the %s binding (%s)", res.Source, modelText(res.Binding.Model)))
	}
	return notes
}

// effectiveBindings is the binding each role runs, as the resolution code picks it.
func effectiveBindings(f formula.Formula) map[formula.HarnessRole]formula.Binding {
	bindings := map[formula.HarnessRole]formula.Binding{}
	for _, role := range []formula.HarnessRole{formula.AnalysisHarness, formula.CodingHarness} {
		bindings[role], _ = f.RoleBinding(role)
	}
	if b, _, bound := MetaBinding(f); bound {
		bindings[formula.MetaHarness] = b
	}
	if res, configured, err := ResolveReviewBinding(f); configured && err == nil {
		bindings[formula.ReviewHarness] = res.Binding
	}
	return bindings
}

// fingerprintRoles lists the roles whose native Claude binding fingerprint, which
// covers the pinned model, differs between the approved and proposed Formula.
func fingerprintRoles(before, after formula.Formula) []string {
	claude := func(b formula.Binding) bool { return nativeAdapter(b) && b.Command[2] == "claude-code" }
	was, now := effectiveBindings(before), effectiveBindings(after)
	var roles []string
	for _, role := range pinRoles {
		b0, ok0 := was[role]
		b1, ok1 := now[role]
		if (ok0 && claude(b0) || ok1 && claude(b1)) && b0.Model != b1.Model {
			roles = append(roles, string(role))
		}
	}
	return roles
}

// proposeModelPin saves a reviewed proposal that sets or removes `model` on the
// default harness and declared role overrides of the approved Formula.
func proposeModelPin(ctx context.Context, o InitOptions, out Outcome, active formula.Loaded) (Outcome, error) {
	f := active.Formula
	var tree yaml.Node
	if err := yaml.Unmarshal([]byte(f.YAML), &tree); err != nil {
		return out, err
	}
	var root *yaml.Node
	if tree.Kind == yaml.DocumentNode && len(tree.Content) == 1 {
		root = tree.Content[0]
	}
	targets, err := pinTargets(f, root, o.Role)
	if err != nil {
		return out, err
	}
	remove := o.Model == ""
	var changes []modelChange
	if remove {
		for _, t := range targets {
			if t.Binding.Model != "" {
				changes = append(changes, modelChange{Path: t.Path, Old: t.Binding.Model, Remove: true})
			}
		}
		if len(changes) == 0 {
			return out, fmt.Errorf("no targeted binding (%s) carries a model; nothing to remove; no proposal saved", targetPaths(targets))
		}
	} else {
		refused := pinRefusals(targets)
		for _, t := range targets {
			if t.Binding.Model != o.Model {
				changes = append(changes, modelChange{Path: t.Path, Old: t.Binding.Model, New: o.Model})
			}
		}
		if len(refused) > 0 {
			var native []string
			for _, r := range pinRoles {
				if b, declared := f.Harnesses[string(r)]; declared && pinRefusal(b) == "" {
					native = append(native, string(r))
				}
			}
			suggestion := "run with --role on a native Claude override"
			if len(native) > 0 {
				suggestion += " (declared: " + strings.Join(native, ", ") + ")"
			} else {
				suggestion += ", declaring one first through a reviewed Formula change"
			}
			return out, fmt.Errorf("cannot pin a model: %s; %s; no proposal saved", strings.Join(refused, "; "), suggestion)
		}
		if len(changes) == 0 {
			return out, fmt.Errorf("every targeted binding (%s) is already pinned to %s; nothing to change", targetPaths(targets), modelText(o.Model))
		}
	}

	proposedText, err := editModelText(f.YAML, changes)
	if err != nil {
		return out, err
	}
	if err = verifyModelPinExact(f.YAML, proposedText, changes); err != nil {
		return out, err
	}
	proposed, err := formula.Parse([]byte(proposedText))
	if err != nil {
		return out, err
	}

	rewritten := make([]string, len(changes))
	paths := make([]string, len(changes))
	for i, c := range changes {
		next := modelText(c.New)
		if c.Remove {
			next = "unpinned"
		}
		rewritten[i] = fmt.Sprintf("%s.model: %s -> %s", c.Path, modelText(c.Old), next)
		paths[i] = c.Path
	}
	note := "Proposes only a model pin change"
	if remove {
		note = "Proposes removing the model pin from " + strings.Join(paths, ", ")
	}
	note += ". Rewritten keys (old -> new): " + strings.Join(rewritten, "; ") + ". The rewritten keys are only `model`; atoms, checks, instructions, timeouts and every binding and command are preserved."
	if notes := roleModelNotes(proposed); len(notes) > 0 {
		note += " Roles without their own override: " + strings.Join(notes, "; ") + "."
	}
	if roles := fingerprintRoles(f, proposed); len(roles) > 0 {
		note += " Changing the pin changes the binding fingerprint (#36) of the native Claude binding these roles run: " + strings.Join(roles, ", ") + "; in-flight plans for those roles need a fresh intake."
	} else {
		note += " No role's native Claude binding changes, so no binding fingerprint changes (#36) and no in-flight plan needs a fresh intake."
	}
	note += " Review the exact YAML before approval."

	probed := ProbeBinding(ctx, f.Harness, out.Repository.Root, o.Runner)
	evidence, err := proposalEvidence(o.LabDir, active, nil)
	if err != nil {
		return out, err
	}
	out.BeforeYAML = f.YAML
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

func targetPaths(targets []pinTarget) string {
	paths := make([]string, len(targets))
	for i, t := range targets {
		paths[i] = t.Path
	}
	return strings.Join(paths, ", ")
}
