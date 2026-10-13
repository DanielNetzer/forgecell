package onboarding

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"go.yaml.in/yaml/v4"
)

const pinnedModel = "claude-opus-4-8"

// claudeLab approves a bootstrap Formula whose default harness is native Claude
// Code, then approves mutate's edit of it when one is given.
func claudeLab(t *testing.T, mutate func(doc map[string]any)) (root string, runner Runner, dir string) {
	t.Helper()
	root, runner = repoFixture(t)
	dir = t.TempDir()
	boot, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Preferred: "claude-code", Runner: runner})
	if err != nil || boot.Proposal == nil {
		t.Fatalf("%+v %v", boot, err)
	}
	if _, err = Review(dir, boot.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		approveEdited(t, dir, mutate)
	}
	return root, runner, dir
}

func pinModel(root string, runner Runner, dir, model, role string, mutate ...func(*InitOptions)) (Outcome, error) {
	o := InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Runner: runner, Model: model, ModelSet: true, Role: role}
	for _, m := range mutate {
		m(&o)
	}
	return Prepare(context.Background(), o)
}

func claudeOverride() map[string]any {
	return map[string]any{"binding": "claude-code", "command": nativeCommand("/cli", "claude-code")}
}

func addAtoms(doc map[string]any, atoms ...map[string]any) {
	list := doc["atoms"].([]any)
	for _, a := range atoms {
		list = append(list, a)
	}
	doc["atoms"] = list
}

// allClaudeRoles declares every override as native Claude Code, with the learn
// and review Atoms their roles need.
func allClaudeRoles(doc map[string]any) {
	doc["harnesses"] = map[string]any{"analysis": claudeOverride(), "coding": claudeOverride(), "meta": claudeOverride(), "review": claudeOverride()}
	addAtoms(doc, map[string]any{"id": "learn", "type": "learn"})
	addReviewAtom(doc)
}

// addReviewAtom places the review Atom between the check and the review gate.
func addReviewAtom(doc map[string]any) {
	atoms := doc["atoms"].([]any)
	doc["atoms"] = append(atoms[:4:4], append([]any{map[string]any{"id": "review", "type": "review"}}, atoms[4:]...)...)
}

func modelOf(t *testing.T, doc map[string]any, path ...string) (string, bool) {
	t.Helper()
	m := doc
	for _, key := range path[:len(path)-1] {
		m = m[key].(map[string]any)
	}
	v, ok := m[path[len(path)-1]]
	if !ok {
		return "", false
	}
	return v.(string), true
}

func withoutModelKeys(doc map[string]any) map[string]any {
	delete(doc["harness"].(map[string]any), "model")
	if roles, ok := doc["harnesses"].(map[string]any); ok {
		for _, v := range roles {
			delete(v.(map[string]any), "model")
		}
	}
	return doc
}

func TestModelPinWithoutRolePinsDefaultAndEveryOverride(t *testing.T) {
	root, runner, dir := claudeLab(t, allClaudeRoles)
	active, err := formula.Inspect(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pinModel(root, runner, dir, pinnedModel, "")
	if err != nil || out.Status != "pending" || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	if out.BeforeYAML != active.Formula.YAML {
		t.Fatal("the before YAML is not the approved YAML")
	}
	after := decodeYAML(t, out.Proposal.YAML)
	for _, path := range [][]string{{"harness", "model"}, {"harnesses", "analysis", "model"}, {"harnesses", "coding", "model"}, {"harnesses", "meta", "model"}, {"harnesses", "review", "model"}} {
		if got, ok := modelOf(t, after, path...); !ok || got != pinnedModel {
			t.Fatalf("%v = %q (%v)", path, got, ok)
		}
	}
	if !reflect.DeepEqual(withoutModelKeys(decodeYAML(t, active.Formula.YAML)), withoutModelKeys(after)) {
		t.Fatal("the proposal changed more than model keys")
	}
	for _, want := range []string{"only `model`", "atoms, checks, instructions, timeouts", "harness.model: unpinned -> \"" + pinnedModel + "\"", "harnesses.review.model: unpinned", "fresh intake", "(#36)", "these roles run: analysis, coding, meta, review;"} {
		if !strings.Contains(out.Note, want) {
			t.Fatalf("the summary omits %q:\n%s", want, out.Note)
		}
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	loaded, err := formula.Load(dir, "")
	if err != nil || !loaded.Approved || loaded.Formula.Harness.Model != pinnedModel || loaded.Formula.Harnesses["coding"].Model != pinnedModel {
		t.Fatalf("approval did not activate the pin: %v %+v", err, loaded.Formula.Harness)
	}
}

func TestModelPinSummaryStatesInheritanceFromResolution(t *testing.T) {
	root, runner, dir := claudeLab(t, func(doc map[string]any) {
		addAtoms(doc, map[string]any{"id": "learn", "type": "learn", "binding": "claude-code", "command": nativeCommand("/cli", "claude-code"), "timeoutMs": 900000})
		addReviewAtom(doc)
	})
	out, err := pinModel(root, runner, dir, pinnedModel, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"analysis has no override and resolves to the default harness, so it inherits the default's model (\"" + pinnedModel + "\")",
		"coding has no override and resolves to the default harness, so it inherits the default's model (\"" + pinnedModel + "\")",
		"meta has no override and runs the learn Atom's command, which carries no model, so it does not inherit the default's pin (unpinned)",
		"review has no override and resolves to the learn-atom binding (unpinned)",
		"of the native Claude binding these roles run: analysis, coding;",
	} {
		if !strings.Contains(out.Note, want) {
			t.Fatalf("the summary omits %q:\n%s", want, out.Note)
		}
	}
}

func TestModelPinRoleChangesOnlyThatOverride(t *testing.T) {
	root, runner, dir := claudeLab(t, allClaudeRoles)
	active, _ := formula.Inspect(dir, "")
	out, err := pinModel(root, runner, dir, pinnedModel, "coding")
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	after := decodeYAML(t, out.Proposal.YAML)
	if got, _ := modelOf(t, after, "harnesses", "coding", "model"); got != pinnedModel {
		t.Fatalf("coding = %q", got)
	}
	for _, path := range [][]string{{"harness", "model"}, {"harnesses", "analysis", "model"}, {"harnesses", "meta", "model"}, {"harnesses", "review", "model"}} {
		if _, ok := modelOf(t, after, path...); ok {
			t.Fatalf("%v was pinned", path)
		}
	}
	if !reflect.DeepEqual(withoutModelKeys(decodeYAML(t, active.Formula.YAML)), withoutModelKeys(after)) {
		t.Fatal("the proposal changed more than model keys")
	}
}

func TestModelPinRoleWithoutOverrideIsRefused(t *testing.T) {
	root, runner, dir := claudeLab(t, nil)
	for _, model := range []string{pinnedModel, ""} {
		before := assayCount(dir)
		_, err := pinModel(root, runner, dir, model, "coding")
		if err == nil || !strings.Contains(err.Error(), "harnesses.coding") || !strings.Contains(err.Error(), "run without --role") {
			t.Fatalf("model %q: %v", model, err)
		}
		if assayCount(dir) != before {
			t.Fatalf("model %q: a refused proposal was saved", model)
		}
		active, _ := formula.Inspect(dir, "")
		if _, declared := active.Formula.Harnesses["coding"]; declared {
			t.Fatal("an override was created")
		}
	}
}

func TestModelPinValidatesTheModel(t *testing.T) {
	root, runner, dir := claudeLab(t, nil)
	for name, model := range map[string]string{
		"blank":        "   ",
		"leading dash": "--model",
		"space":        "a b",
		"tab":          "a\tb",
		"newline":      "a\nb",
		"NUL":          "a\x00b",
		"too long":     strings.Repeat("m", formula.MaxModelLength+1),
	} {
		t.Run(name, func(t *testing.T) {
			before := assayCount(dir)
			if _, err := pinModel(root, runner, dir, model, ""); err == nil || assayCount(dir) != before {
				t.Fatalf("%q accepted: %v", model, err)
			}
		})
	}
}

func TestModelPinOptionConflicts(t *testing.T) {
	root, runner, dir := claudeLab(t, nil)
	for name, mutate := range map[string]func(*InitOptions){
		"harness":      func(o *InitOptions) { o.Preferred = "codex" },
		"rebind":       func(o *InitOptions) { o.Rebind = "codex" },
		"meta harness": func(o *InitOptions) { o.MetaHarness = "codex" },
		"meta command": func(o *InitOptions) { o.MetaCommand = []string{"/bin/x"} },
		"timeout":      func(o *InitOptions) { o.HarnessTimeoutMS = 1000 },
		"max timeout":  func(o *InitOptions) { o.MaxTimeoutMS = 1000 },
		"unknown role": func(o *InitOptions) { o.Role = "ship" },
	} {
		t.Run(name, func(t *testing.T) {
			before := assayCount(dir)
			if _, err := pinModel(root, runner, dir, pinnedModel, "", mutate); err == nil || assayCount(dir) != before {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	t.Run("role without model", func(t *testing.T) {
		_, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Runner: runner, Role: "coding"})
		if err == nil || !strings.Contains(err.Error(), "--role needs --model") {
			t.Fatal(err)
		}
	})
}

func TestModelPinRefusedWithoutCurrentApprovalOrWhilePending(t *testing.T) {
	root, runner := repoFixture(t)
	empty := t.TempDir()
	if _, err := pinModel(root, runner, empty, pinnedModel, ""); err == nil || !strings.Contains(err.Error(), "needs an approved Formula") {
		t.Fatalf("no Formula: %v", err)
	}
	if assayCount(empty) != 0 {
		t.Fatal("a refused proposal was saved")
	}

	root, runner, dir := claudeLab(t, nil)
	first, err := pinModel(root, runner, dir, pinnedModel, "")
	if err != nil || first.Proposal == nil {
		t.Fatal(err)
	}
	before := assayCount(dir)
	if _, err = pinModel(root, runner, dir, "other-model", ""); err == nil || !strings.Contains(err.Error(), "already pending") || assayCount(dir) != before {
		t.Fatalf("pending proposal: %v", err)
	}
}

func TestModelPinSameModelIsNothingToChange(t *testing.T) {
	root, runner, dir := claudeLab(t, nil)
	out, err := pinModel(root, runner, dir, pinnedModel, "")
	if err != nil || out.Proposal == nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	before := assayCount(dir)
	if _, err = pinModel(root, runner, dir, pinnedModel, ""); err == nil || !strings.Contains(err.Error(), "nothing to change") || assayCount(dir) != before {
		t.Fatalf("%v", err)
	}
	if _, err = pinModel(root, runner, dir, "", ""); err != nil {
		t.Fatalf("removing an existing pin: %v", err)
	}
}

// mixedLab approves a codex default with a custom coding override and a native
// Claude analysis override.
func mixedLab(t *testing.T, pinned bool) (root string, runner Runner, dir string) {
	t.Helper()
	root, runner, dir, _ = approvedLab(t)
	approveEdited(t, dir, func(doc map[string]any) {
		custom := map[string]any{"binding": "codex", "command": []any{"/bin/custom"}}
		analysis := claudeOverride()
		if pinned {
			doc["harness"].(map[string]any)["model"] = "codex-model"
			custom["model"] = "custom-model"
			analysis["model"] = "claude-model"
		}
		doc["harnesses"] = map[string]any{"coding": custom, "analysis": analysis}
	})
	return root, runner, dir
}

func TestModelPinRefusesNonClaudeBindings(t *testing.T) {
	root, runner, dir := mixedLab(t, false)
	before := assayCount(dir)
	_, err := pinModel(root, runner, dir, pinnedModel, "")
	if err == nil {
		t.Fatal("a mixed-adapter proposal was accepted")
	}
	for _, want := range []string{"harness (binding \"codex\")", "not supported for codex", "harnesses.coding", "custom command; Forgecell cannot enforce a pin on a custom command", "--role"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal omits %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "harnesses.analysis (binding") {
		t.Fatalf("the native override was named as refused: %v", err)
	}
	if !strings.Contains(err.Error(), "(declared: analysis)") {
		t.Fatalf("the refusal does not suggest the native override: %v", err)
	}
	if _, err = pinModel(root, runner, dir, pinnedModel, "coding"); err == nil || !strings.Contains(err.Error(), "custom command") {
		t.Fatalf("--role on a custom binding: %v", err)
	}
	if assayCount(dir) != before {
		t.Fatal("a refused proposal was saved")
	}
	out, err := pinModel(root, runner, dir, pinnedModel, "analysis")
	if err != nil || out.Proposal == nil {
		t.Fatalf("--role on the native override: %v", err)
	}
	after := decodeYAML(t, out.Proposal.YAML)
	if got, _ := modelOf(t, after, "harnesses", "analysis", "model"); got != pinnedModel {
		t.Fatalf("analysis = %q", got)
	}
	if _, ok := modelOf(t, after, "harness", "model"); ok {
		t.Fatal("the codex default was pinned")
	}
}

func TestModelPinRemovalIsAllowedOnAnyBinding(t *testing.T) {
	root, runner, dir := mixedLab(t, true)
	out, err := pinModel(root, runner, dir, "", "")
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	after := decodeYAML(t, out.Proposal.YAML)
	for _, path := range [][]string{{"harness", "model"}, {"harnesses", "coding", "model"}, {"harnesses", "analysis", "model"}} {
		if _, ok := modelOf(t, after, path...); ok {
			t.Fatalf("%v still pinned", path)
		}
	}
	for _, want := range []string{"removing the model pin from harness, harnesses.analysis, harnesses.coding", "harness.model: \"codex-model\" -> unpinned", "harnesses.coding.model: \"custom-model\" -> unpinned", "harnesses.analysis.model: \"claude-model\" -> unpinned", "only `model`", "fresh intake"} {
		if !strings.Contains(out.Note, want) {
			t.Fatalf("the summary omits %q:\n%s", want, out.Note)
		}
	}
	if _, err = formula.Parse([]byte(out.Proposal.YAML)); err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err = pinModel(root, runner, dir, "", ""); err == nil || !strings.Contains(err.Error(), "nothing to remove") {
		t.Fatalf("removing nothing: %v", err)
	}
}

func TestModelPinRemovalWithRoleOnlyTouchesThatOverride(t *testing.T) {
	root, runner, dir := mixedLab(t, true)
	out, err := pinModel(root, runner, dir, "", "coding")
	if err != nil || out.Proposal == nil {
		t.Fatalf("%v", err)
	}
	after := decodeYAML(t, out.Proposal.YAML)
	if _, ok := modelOf(t, after, "harnesses", "coding", "model"); ok {
		t.Fatal("coding still pinned")
	}
	if got, _ := modelOf(t, after, "harness", "model"); got != "codex-model" {
		t.Fatalf("default = %q", got)
	}
	if got, _ := modelOf(t, after, "harnesses", "analysis", "model"); got != "claude-model" {
		t.Fatalf("analysis = %q", got)
	}
	if !strings.Contains(out.Note, "No role's native Claude binding changes") || strings.Contains(out.Note, "these roles run") {
		t.Fatalf("a removal on a custom binding claims a fingerprint change:\n%s", out.Note)
	}
}

func TestModelPinVerifierRefusesTampering(t *testing.T) {
	_, _, dir := claudeLab(t, func(doc map[string]any) {
		allClaudeRoles(doc)
		doc["harness"].(map[string]any)["instructions"] = "guide"
	})
	active, _ := formula.Inspect(dir, "")
	before := active.Formula.YAML
	changes := []modelChange{{Path: "harness", New: pinnedModel}, {Path: "harnesses.coding", New: pinnedModel}}
	build := func(tamper func(doc map[string]any)) string {
		doc := decodeYAML(t, before)
		if err := applyModelChanges(doc, changes); err != nil {
			t.Fatal(err)
		}
		if tamper != nil {
			tamper(doc)
		}
		b, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if err := verifyModelPinExact(before, build(nil), changes); err != nil {
		t.Fatalf("an exact proposal was refused: %v", err)
	}
	for name, tamper := range map[string]func(map[string]any){
		"timeout":      func(d map[string]any) { d["harness"].(map[string]any)["timeoutMs"] = 1 },
		"instructions": func(d map[string]any) { d["harness"].(map[string]any)["instructions"] = "other" },
		"atom":         func(d map[string]any) { addAtoms(d, map[string]any{"id": "extra", "type": "document"}) },
		"check":        func(d map[string]any) { d["atoms"].([]any)[3].(map[string]any)["workflows"] = []any{"x"} },
		"binding":      func(d map[string]any) { d["harness"].(map[string]any)["binding"] = "codex" },
		"command":      func(d map[string]any) { d["harness"].(map[string]any)["command"] = []any{"/bin/x"} },
		"override": func(d map[string]any) {
			d["harnesses"].(map[string]any)["coding"].(map[string]any)["binding"] = "codex"
		},
		"untargeted model": func(d map[string]any) {
			d["harnesses"].(map[string]any)["analysis"].(map[string]any)["model"] = pinnedModel
		},
		"different model": func(d map[string]any) { d["harness"].(map[string]any)["model"] = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			if err := verifyModelPinExact(before, build(tamper), changes); err == nil {
				t.Fatal("a tampered proposal was accepted")
			}
		})
	}
	if err := verifyModelPinExact(before, build(nil), []modelChange{{Path: "harnesses.absent", New: pinnedModel}}); err == nil {
		t.Fatal("a change to an undeclared binding was accepted")
	}
}

func TestModelPinRolesReportTheirModel(t *testing.T) {
	_, runner, dir := claudeLab(t, func(doc map[string]any) {
		allClaudeRoles(doc)
		doc["harnesses"].(map[string]any)["coding"].(map[string]any)["model"] = "coding-model"
	})
	active, err := formula.Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	models := map[string]string{}
	for _, r := range InspectRoles(context.Background(), active.Formula, t.TempDir(), runner) {
		models[r.Role] = r.Model
	}
	want := map[string]string{"analysis": "", "coding": "coding-model", "meta": "", "review": ""}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("%v, want %v", models, want)
	}
}

const handAuthored = `# Reviewed by hand; keep this header.
kind: formula
id: hand-made

harness:
  timeoutMs: 600000 # ten minutes
  command: ["/cli", "__adapter", "claude-code", "/claude"]
  binding: claude-code

  # note about the roles below
harnesses:
  coding:
    command: ["/cli", "__adapter", "claude-code", "/claude"]
    binding: claude-code
    model: 'old-model' # keep this comment
atoms:
  - &gate
    id: gate
    type: approval
`

func TestModelPinEditsOnlyModelLines(t *testing.T) {
	changes := []modelChange{{Path: "harness", New: pinnedModel}, {Path: "harnesses.coding", Old: "old-model", New: pinnedModel}}
	after, err := editModelText(handAuthored, changes)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(handAuthored, "  binding: claude-code\n\n  # note", "  binding: claude-code\n  model: "+pinnedModel+"\n\n  # note", 1)
	want = strings.Replace(want, "model: 'old-model' # keep", "model: '"+pinnedModel+"' # keep", 1)
	if after != want {
		t.Fatalf("the edit changed more than model lines:\n%s\nwant\n%s", after, want)
	}
	if err = verifyModelPinExact(handAuthored, after, changes); err != nil {
		t.Fatal(err)
	}

	removal := []modelChange{{Path: "harnesses.coding", Old: "old-model", Remove: true}}
	removed, err := editModelText(handAuthored, removal)
	if err != nil || removed != strings.Replace(handAuthored, "    model: 'old-model' # keep this comment\n", "", 1) {
		t.Fatalf("%v\n%s", err, removed)
	}
	if err = verifyModelPinExact(handAuthored, removed, removal); err != nil {
		t.Fatal(err)
	}

	if err = verifyModelPinExact(handAuthored, strings.Replace(after, "# Reviewed by hand", "# Edited", 1), changes); err == nil || !strings.Contains(err.Error(), "lines other than") {
		t.Fatalf("a changed comment was accepted: %v", err)
	}
	if err = verifyModelPinExact(handAuthored, strings.Replace(after, "\n\n  # note", "\n  # note", 1), changes); err == nil {
		t.Fatal("a removed blank line was accepted")
	}
}

func TestModelPinInsertsBeforeLaterSortedKeys(t *testing.T) {
	text := "kind: formula\nid: x\nharness:\n  binding: claude-code\n  command:\n  - /cli\n  - __adapter\n  - claude-code\n  - /claude\n  timeoutMs: 5\natoms: []\n"
	after, err := editModelText(text, []modelChange{{Path: "harness", New: pinnedModel}})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(text, "  timeoutMs: 5\n", "  model: "+pinnedModel+"\n  timeoutMs: 5\n", 1)
	if after != want {
		t.Fatalf("%s", after)
	}
}

func TestModelPinRefusesWhatItCannotEditInPlace(t *testing.T) {
	flow := "kind: formula\nid: x\nharness: {binding: claude-code}\natoms: []\n"
	if _, err := editModelText(flow, []modelChange{{Path: "harness", New: pinnedModel}}); err == nil || !strings.Contains(err.Error(), "flow-style") {
		t.Fatalf("a flow-style mapping was edited: %v", err)
	}
	if _, err := editModelText(handAuthored, []modelChange{{Path: "harnesses.review", New: pinnedModel}}); err == nil || !strings.Contains(err.Error(), "not a binding") {
		t.Fatalf("an undeclared override was edited: %v", err)
	}
	if _, err := editModelText(handAuthored, []modelChange{{Path: "harness", Remove: true}}); err == nil {
		t.Fatal("removed a model key that is not there")
	}
}

func TestModelPinRefusesAnUndeclaredDefaultHarness(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("kind: formula\nid: x\n"), &doc); err != nil {
		t.Fatal(err)
	}
	targets, err := pinTargets(formula.Formula{}, doc.Content[0], "")
	if err != nil || len(targets) != 1 || !targets[0].Undeclared {
		t.Fatalf("%+v %v", targets, err)
	}
	refused := strings.Join(pinRefusals(targets), "; ")
	if !strings.Contains(refused, "harness is not declared") || strings.Contains(refused, "custom command") {
		t.Fatalf("%s", refused)
	}
}
