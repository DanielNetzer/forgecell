package onboarding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"go.yaml.in/yaml/v4"
)

func nativeCommand(launcher, adapter string) []any {
	return []any{launcher, "__adapter", adapter, "/" + adapter}
}

func learnAtomOf(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	for _, v := range doc["atoms"].([]any) {
		if a := v.(map[string]any); a["type"] == "learn" {
			return a
		}
	}
	t.Fatal("no learn Atom")
	return nil
}

// approveEdited edits the active Formula, saves it as a proposal and approves it.
func approveEdited(t *testing.T, dir string, mutate func(doc map[string]any)) formula.Loaded {
	t.Helper()
	active, err := formula.Inspect(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeYAML(t, active.Formula.YAML)
	mutate(doc)
	edited, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p, err := SaveProposal(dir, string(edited))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, p.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	loaded, err := formula.Load(dir, "")
	if err != nil || !loaded.Approved {
		t.Fatalf("edited Formula not approved: %v", err)
	}
	return loaded
}

func rebindToNewLauncher(o *InitOptions) { o.Launcher = "/new-cli" }

// forgecellBuild lays out an older Forgecell build: an executable beside its
// release manifest, which is what establishes it as Forgecell without running it.
func forgecellBuild(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	manifest := `{"version":"0.2.0","protocolVersion":"` + harness.AdapterProtocolVersion + `"}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "forgecell")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// lookalikeTool is an executable that only shares the native argv shape.
func lookalikeTool(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "custom-tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func quoted(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRebindRepointsStaleNativeLearnAtom(t *testing.T) {
	root, runner, dir, boot := approvedLab(t)
	active, _ := formula.Inspect(dir, "")
	next, err := rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher)
	if err != nil || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	parsed, err := formula.Parse([]byte(next.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/new-cli", "__adapter", "claude-code", "/claude-code"}; !reflect.DeepEqual(parsed.Harness.Command, want) {
		t.Fatalf("harness command = %v", parsed.Harness.Command)
	}
	var learn formula.Atom
	for _, a := range parsed.Atoms {
		if a.Type == "learn" {
			learn = a
		}
	}
	if want := []string{"/new-cli", "__adapter", "codex", "/codex"}; !reflect.DeepEqual(learn.Command, want) || learn.Binding != "codex" || learn.TimeoutMS != 900000 {
		t.Fatalf("learn Atom was not moved to the new launcher on its own adapter: %+v", learn)
	}
	if err = parsed.ValidateHarnessRoles(); err != nil {
		t.Fatal(err)
	}
	saved, err := InspectProposal(dir, next.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CapabilityEvidence.Meta == nil || !reflect.DeepEqual(saved.CapabilityEvidence.Meta.Command, learn.Command) {
		t.Fatalf("saved meta evidence still names the old learn command: %+v", saved.CapabilityEvidence.Meta)
	}
	if saved.EvidenceHash != boot.Proposal.EvidenceHash || saved.EvidencePath != boot.Proposal.EvidencePath {
		t.Fatal("detached repository evidence changed")
	}
	if err = verifyRebindDiff(active.Formula.YAML, next.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`harness.command: ["/cli","__adapter","codex","/codex"] -> ["/new-cli","__adapter","claude-code","/claude-code"]`, `atoms[learn].command[0]: "/cli" -> "/new-cli"`, `harness.binding: "codex" -> "claude-code"`, `atoms[harness].binding: "codex" -> "claude-code"`} {
		if !strings.Contains(next.Note, want) {
			t.Fatalf("the note omits %q:\n%s", want, next.Note)
		}
	}
	if _, err = Review(dir, next.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	now, err := formula.Load(dir, "")
	if err != nil || !now.Approved || nativeLauncher(now.Formula.Harness.Command) != "/new-cli" || learnCommand(now.Formula)[0] != "/new-cli" {
		t.Fatalf("approval did not activate the moved learn Atom: %v", err)
	}
}

func TestRebindRepointsNativeRoleOverridesOnAnyOtherLauncher(t *testing.T) {
	for _, c := range []struct {
		name          string
		launcher      func(t *testing.T) string
		custom        bool
		wantRepointed bool
	}{
		{name: "old launcher", launcher: func(*testing.T) string { return "/cli" }, wantRepointed: true},
		{name: "different Forgecell build", launcher: forgecellBuild, wantRepointed: true},
		{name: "custom argv", launcher: func(*testing.T) string { return "/cli" }, custom: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, runner, dir, _ := approvedLab(t)
			launcher := c.launcher(t)
			approved := approveEdited(t, dir, func(doc map[string]any) {
				learn := learnAtomOf(t, doc)
				if c.custom {
					learn["command"] = []any{"/bin/custom-learner"}
					return
				}
				learn["command"] = nativeCommand(launcher, "codex")
				doc["harnesses"] = map[string]any{
					"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand(launcher, "claude-code")},
					"meta":     map[string]any{"binding": "codex", "command": nativeCommand(launcher, "codex")},
				}
			})
			next, err := rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher)
			if err != nil {
				t.Fatal(err)
			}
			before, after := decodeYAML(t, approved.Formula.YAML), decodeYAML(t, next.Proposal.YAML)
			if c.wantRepointed {
				for role, adapter := range map[string]string{"analysis": "claude-code", "meta": "codex"} {
					got := after["harnesses"].(map[string]any)[role].(map[string]any)["command"]
					if !reflect.DeepEqual(got, nativeCommand("/new-cli", adapter)) {
						t.Fatalf("%s role command = %v", role, got)
					}
				}
				if got := learnAtomOf(t, after)["command"]; !reflect.DeepEqual(got, nativeCommand("/new-cli", "codex")) {
					t.Fatalf("learn command = %v", got)
				}
				if !strings.Contains(next.Note, "harnesses.analysis.command[0]") || !strings.Contains(next.Note, "harnesses.meta.command[0]") {
					t.Fatalf("each rewritten role command must be listed: %s", next.Note)
				}
			} else {
				if !reflect.DeepEqual(before["harnesses"], after["harnesses"]) || !reflect.DeepEqual(learnAtomOf(t, before), learnAtomOf(t, after)) {
					t.Fatalf("custom commands were changed:\n%s", next.Proposal.YAML)
				}
				if strings.Contains(next.Note, "command[0]") {
					t.Fatalf("the note lists a launcher rewrite that did not happen: %s", next.Note)
				}
			}
			if err = verifyRebindDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
				t.Fatal(err)
			}
			if _, err = InspectProposal(dir, next.Proposal.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRebindMovesLearnAtomStrandedOnAnOlderBuild(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	older := forgecellBuild(t)
	approved := approveEdited(t, dir, func(doc map[string]any) {
		learnAtomOf(t, doc)["command"] = nativeCommand(older, "codex")
		doc["harnesses"] = map[string]any{"coding": map[string]any{"binding": "codex", "command": nativeCommand("/cli", "codex")}}
	})
	next, err := rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher)
	if err != nil || next.Status != "pending" || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	parsed, err := formula.Parse([]byte(next.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/new-cli", "__adapter", "claude-code", "/claude-code"}; !reflect.DeepEqual(parsed.Harness.Command, want) {
		t.Fatalf("harness command = %v", parsed.Harness.Command)
	}
	if coding, _ := parsed.RoleBinding(formula.CodingHarness); !reflect.DeepEqual(coding.Command, []string{"/new-cli", "__adapter", "claude-code", "/claude-code"}) {
		t.Fatalf("coding command = %v", coding.Command)
	}
	var learn formula.Atom
	for _, a := range parsed.Atoms {
		if a.Type == "learn" {
			learn = a
		}
	}
	if want := []string{"/new-cli", "__adapter", "codex", "/codex"}; learn.ID != "learn" || learn.Binding != "codex" || learn.TimeoutMS != 900000 || !reflect.DeepEqual(learn.Command, want) {
		t.Fatalf("the learn Atom on the older build was not moved onto the new launcher: %+v", learn)
	}
	if want := `atoms[learn].command[0]: ` + quoted(t, older) + ` -> "/new-cli"`; !strings.Contains(next.Note, want) {
		t.Fatalf("the note omits %q:\n%s", want, next.Note)
	}
	if err = verifyRebindDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
	tampered := decodeYAML(t, next.Proposal.YAML)
	learnAtomOf(t, tampered)["timeoutMs"] = 1
	encoded, err := yaml.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyRebindDiff(approved.Formula.YAML, string(encoded)); err == nil {
		t.Fatal("a changed learn timeout was accepted beside the launcher rewrite")
	}
	saved, err := InspectProposal(dir, next.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CapabilityEvidence.Meta == nil || !reflect.DeepEqual(saved.CapabilityEvidence.Meta.Command, learn.Command) {
		t.Fatalf("saved meta evidence does not carry the moved learn command: %+v", saved.CapabilityEvidence.Meta)
	}
}

func TestRebindProposesStaleLearnAtomWhenCodingAlreadyMatches(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	older := forgecellBuild(t)
	approved := approveEdited(t, dir, func(doc map[string]any) {
		learnAtomOf(t, doc)["command"] = nativeCommand(older, "codex")
	})
	next, err := rebind(t, root, runner, dir, "codex")
	if err != nil || next.Status != "pending" || next.Proposal == nil {
		t.Fatalf("a stale learn Atom must make the rebind a real change: %+v %v", next, err)
	}
	before, after := decodeYAML(t, approved.Formula.YAML), decodeYAML(t, next.Proposal.YAML)
	if !reflect.DeepEqual(before["harness"], after["harness"]) || !reflect.DeepEqual(harnessAtom(before), harnessAtom(after)) {
		t.Fatalf("the coding binding changed:\n%s", next.Proposal.YAML)
	}
	if got := learnAtomOf(t, after)["command"]; !reflect.DeepEqual(got, nativeCommand("/cli", "codex")) {
		t.Fatalf("learn command = %v", got)
	}
	if !strings.Contains(next.Note, `atoms[learn].command[0]: `+quoted(t, older)+` -> "/cli"`) || strings.Contains(next.Note, "harness.binding") || strings.Contains(next.Note, "harness.command") {
		t.Fatalf("the note must list only the learn launcher: %s", next.Note)
	}
	if err = verifyRebindDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, next.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err = rebind(t, root, runner, dir, "codex"); err == nil || !strings.Contains(err.Error(), "nothing to rebind") {
		t.Fatalf("every native command is on the target launcher now: %v", err)
	}
}

func TestRebindRepointsStaleRoleOverridesWhenDefaultHarnessIsCustom(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	older := forgecellBuild(t)
	approved := approveEdited(t, dir, func(doc map[string]any) {
		doc["harness"].(map[string]any)["command"] = []any{"/bin/custom"}
		doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand(older, "claude-code")}}
		learnAtomOf(t, doc)["command"] = nativeCommand(older, "codex")
	})
	next, err := rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher)
	if err != nil || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	after := decodeYAML(t, next.Proposal.YAML)
	if got := after["harnesses"].(map[string]any)["analysis"].(map[string]any)["command"]; !reflect.DeepEqual(got, nativeCommand("/new-cli", "claude-code")) {
		t.Fatalf("analysis command = %v", got)
	}
	if got := learnAtomOf(t, after)["command"]; !reflect.DeepEqual(got, nativeCommand("/new-cli", "codex")) {
		t.Fatalf("learn command = %v", got)
	}
	if !strings.Contains(next.Note, "harnesses.analysis.command[0]") || !strings.Contains(next.Note, "atoms[learn].command[0]") {
		t.Fatalf("each rewritten command must be listed: %s", next.Note)
	}
	if err = verifyRebindDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
}

func TestRebindLeavesLearnAtomWithCustomCommandUntouchedBesideStaleRoles(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	approved := approveEdited(t, dir, func(doc map[string]any) {
		learnAtomOf(t, doc)["command"] = []any{"/bin/custom-learner"}
		doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand(forgecellBuild(t), "claude-code")}}
	})
	next, err := rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher)
	if err != nil || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	before, after := decodeYAML(t, approved.Formula.YAML), decodeYAML(t, next.Proposal.YAML)
	if !reflect.DeepEqual(learnAtomOf(t, before), learnAtomOf(t, after)) {
		t.Fatalf("the custom learn command changed:\n%s", next.Proposal.YAML)
	}
	if strings.Contains(next.Note, "atoms[learn]") || !strings.Contains(next.Note, "harnesses.analysis.command[0]") {
		t.Fatalf("the note must list only the native rewrite: %s", next.Note)
	}
	if err = verifyRebindDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
}

func TestRebindLeavesNativeShapedToolsThatAreNotForgecellUntouched(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	tool := lookalikeTool(t)
	approved := approveEdited(t, dir, func(doc map[string]any) {
		learnAtomOf(t, doc)["command"] = nativeCommand(tool, "codex")
		doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand("/not/installed", "claude-code")}}
	})
	next, err := rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher)
	if err != nil || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	before, after := decodeYAML(t, approved.Formula.YAML), decodeYAML(t, next.Proposal.YAML)
	if !reflect.DeepEqual(learnAtomOf(t, before), learnAtomOf(t, after)) || !reflect.DeepEqual(before["harnesses"], after["harnesses"]) {
		t.Fatalf("a tool that is not an established Forgecell executable was rewritten:\n%s", next.Proposal.YAML)
	}
	for _, want := range []string{"Left unchanged because the launcher is not established as a Forgecell executable", `atoms[learn].command[0]: ` + quoted(t, tool), `harnesses.analysis.command[0]: "/not/installed"`} {
		if !strings.Contains(next.Note, want) {
			t.Fatalf("the note omits %q:\n%s", want, next.Note)
		}
	}
	if strings.Contains(next.Note, quoted(t, tool)+" ->") || strings.Contains(next.Note, `"/not/installed" ->`) {
		t.Fatalf("the note lists a rewrite that did not happen: %s", next.Note)
	}
	if err = verifyRebindExact(approved.Formula.YAML, next.Proposal.YAML, "claude-code", []string{"/new-cli", "__adapter", "claude-code", "/claude-code"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, next.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	if _, err = rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher); err == nil || !strings.Contains(err.Error(), "nothing to rebind") {
		t.Fatalf("a launcher that is not established as Forgecell must not make a rebind a change: %v", err)
	}
}

func TestVerifyRebindExactRefusesAnythingButTheRecordedRewrites(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	older := forgecellBuild(t)
	approved := approveEdited(t, dir, func(doc map[string]any) {
		learnAtomOf(t, doc)["command"] = nativeCommand(older, "codex")
		doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand(older, "claude-code")}}
	})
	next, err := rebind(t, root, runner, dir, "claude-code", rebindToNewLauncher)
	if err != nil || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	command := []string{"/new-cli", "__adapter", "claude-code", "/claude-code"}
	exact := []launcherRewrite{{"harnesses.analysis", older, "/new-cli"}, {"atoms[learn]", older, "/new-cli"}}
	if err = verifyRebindExact(approved.Formula.YAML, next.Proposal.YAML, "claude-code", command, exact); err != nil {
		t.Fatalf("the exact proposal was refused: %v", err)
	}
	for _, c := range []struct {
		name     string
		tamper   func(doc map[string]any)
		rewrites []launcherRewrite
		launcher bool
	}{
		{name: "learn rewritten to an unexpected launcher", tamper: func(doc map[string]any) { learnAtomOf(t, doc)["command"] = nativeCommand("/evil", "codex") }, rewrites: exact, launcher: true},
		{name: "analysis rewritten to an unexpected launcher", tamper: func(doc map[string]any) {
			doc["harnesses"].(map[string]any)["analysis"].(map[string]any)["command"] = nativeCommand("/evil", "claude-code")
		}, rewrites: exact, launcher: true},
		{name: "recorded rewrite not applied", tamper: func(doc map[string]any) { learnAtomOf(t, doc)["command"] = nativeCommand(older, "codex") }, rewrites: exact, launcher: true},
		{name: "rewrite not recorded", rewrites: exact[:1], launcher: true},
		{name: "record names a launcher the Formula does not have", rewrites: []launcherRewrite{{"harnesses.analysis", "/other", "/new-cli"}, exact[1]}, launcher: true},
		{name: "record names a command that is not native", rewrites: append([]launcherRewrite{{"harnesses.meta", older, "/new-cli"}}, exact...), launcher: true},
		{name: "default harness points elsewhere", tamper: func(doc map[string]any) {
			doc["harness"].(map[string]any)["command"] = nativeCommand("/evil", "claude-code")
		}, rewrites: exact},
		{name: "harness Atom binding differs", tamper: func(doc map[string]any) { harnessAtom(doc)["binding"] = "codex" }, rewrites: exact},
	} {
		t.Run(c.name, func(t *testing.T) {
			proposed := next.Proposal.YAML
			if c.tamper != nil {
				doc := decodeYAML(t, proposed)
				c.tamper(doc)
				encoded, err := yaml.Marshal(doc)
				if err != nil {
					t.Fatal(err)
				}
				proposed = string(encoded)
			}
			if err := verifyRebindExact(approved.Formula.YAML, proposed, "claude-code", command, c.rewrites); err == nil {
				t.Fatal("a proposal that is not exactly the recorded rewrites was accepted")
			}
			if c.launcher && c.tamper != nil {
				if err := verifyRebindDiff(approved.Formula.YAML, proposed); err != nil {
					t.Fatalf("the structural check was expected to accept a launcher-only change: %v", err)
				}
			}
		})
	}
}

func TestRebindRepointsOnlyEstablishedForgecellLaunchers(t *testing.T) {
	running, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifestOnly := t.TempDir()
	if err = os.WriteFile(filepath.Join(manifestOnly, "manifest.json"), []byte(`{"version":"0.1.0","protocolVersion":"1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	if err = os.MkdirAll(filepath.Join(home, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(home, "bin", "forgecell")
	if err = os.WriteFile(managed, []byte("#!/bin/sh\n# Forgecell managed native launcher v1\nexec \"$0\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := formula.Parse([]byte(roleFormula("")))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, launcher string
		want           bool
	}{
		{"release build beside its manifest", forgecellBuild(t), true},
		{"manifest of a build whose executable was removed", filepath.Join(manifestOnly, "forgecell"), true},
		{"managed launcher without an active release", managed, true},
		{"running executable", running, true},
		{"launcher the approved Formula records", "/cli", true},
		{"tool with the native argv shape", lookalikeTool(t), false},
		{"launcher that does not exist", "/not/installed", false},
		{"empty launcher", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := RebindRepoints(f, c.launcher); got != c.want {
				t.Fatalf("RebindRepoints = %t, want %t", got, c.want)
			}
			stale := f
			stale.Atoms = append([]formula.Atom(nil), f.Atoms...)
			stale.Atoms = append(stale.Atoms, formula.Atom{ID: "learn", Type: "learn", Binding: "codex", Command: []string{c.launcher, "__adapter", "codex", "/codex"}})
			if got := staleNativeCommand(stale, "/new-cli"); got != (c.want && c.launcher != "") {
				t.Fatalf("staleNativeCommand = %t, want %t", got, c.want)
			}
		})
	}
}

func TestRebindRewritesCodingOverride(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	approved := approveEdited(t, dir, func(doc map[string]any) {
		doc["harnesses"] = map[string]any{"coding": map[string]any{"binding": "codex", "command": nativeCommand("/cli", "codex"), "instructions": "coding guidance", "timeoutMs": 1234, "maxTimeoutMs": 5678}}
	})
	next, err := rebind(t, root, runner, dir, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := formula.Parse([]byte(next.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	coding, override := parsed.RoleBinding(formula.CodingHarness)
	if !override || coding.Binding != "claude-code" || !reflect.DeepEqual(coding.Command, []string{"/cli", "__adapter", "claude-code", "/claude-code"}) {
		t.Fatalf("coding role did not move: %+v", coding)
	}
	if coding.Instructions != "coding guidance" || coding.TimeoutMS != 1234 || coding.MaxTimeoutMS != 5678 {
		t.Fatalf("coding override lost its own fields: %+v", coding)
	}
	if got := harnessAtom(decodeYAML(t, next.Proposal.YAML))["binding"]; got != "claude-code" {
		t.Fatalf("harness Atom binding = %v", got)
	}
	if err = parsed.ValidateHarnessRoles(); err != nil {
		t.Fatal(err)
	}
	if err = verifyRebindDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next.Note, `harnesses.coding.binding: "codex" -> "claude-code"`) {
		t.Fatalf("the note omits the coding override: %s", next.Note)
	}
	if _, err = InspectProposal(dir, next.Proposal.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRebindIsNeverASilentNoOpForTheCodingRole(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	approveEdited(t, dir, func(doc map[string]any) {
		doc["harness"].(map[string]any)["binding"] = "claude-code"
		doc["harness"].(map[string]any)["command"] = nativeCommand("/cli", "claude-code")
		doc["harnesses"] = map[string]any{"coding": map[string]any{"binding": "codex", "command": nativeCommand("/cli", "codex")}}
	})
	next, err := rebind(t, root, runner, dir, "claude-code")
	if err != nil || next.Proposal == nil {
		t.Fatalf("a coding override that differs from the target must be rebound: %+v %v", next, err)
	}
	if _, err = Review(dir, next.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	now, _ := formula.Load(dir, "")
	if coding, _ := now.Formula.RoleBinding(formula.CodingHarness); coding.Binding != "claude-code" {
		t.Fatalf("coding role = %+v", coding)
	}
	if _, err = rebind(t, root, runner, dir, "claude-code"); err == nil || !strings.Contains(err.Error(), "nothing to rebind") {
		t.Fatalf("both bindings already match: %v", err)
	}
}

func TestRebindRejectsAnalysisFallbackToUnsupportedAdapter(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	count := assayCount(dir)
	_, err := rebind(t, root, runner, dir, "cursor")
	reason := harness.AdapterCapabilities("cursor").Analysis.Reason
	if err == nil || reason == "" || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "harnesses.analysis") {
		t.Fatalf("error %v, want the adapter reason %q and the remedy", err, reason)
	}
	if assayCount(dir) != count {
		t.Fatal("a refused rebind saved a proposal")
	}
	approveEdited(t, dir, func(doc map[string]any) {
		doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand("/cli", "claude-code")}}
	})
	next, err := rebind(t, root, runner, dir, "cursor")
	if err != nil || next.Proposal == nil {
		t.Fatalf("a declared analysis role makes the rebind safe: %+v %v", next, err)
	}
	parsed, _ := formula.Parse([]byte(next.Proposal.YAML))
	if analysis, override := parsed.RoleBinding(formula.AnalysisHarness); !override || analysis.Binding != "claude-code" || parsed.Harness.Binding != "cursor" {
		t.Fatalf("analysis %+v default %s", analysis, parsed.Harness.Binding)
	}
	if _, err = InspectProposal(dir, next.Proposal.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAnalysisFallbackExecutesNothingAndJudgesOnlyTheFallback(t *testing.T) {
	declared, _ := formula.Parse([]byte(roleFormula("harnesses:\n  analysis: {binding: claude-code, command: [/cli, __adapter, claude-code, /claude]}\n")))
	absent, _ := formula.Parse([]byte(roleFormula("")))
	for _, c := range []struct {
		name    string
		f       formula.Formula
		target  formula.Binding
		wantErr bool
	}{
		{"cursor without analysis", absent, formula.Binding{Binding: "cursor", Command: []string{"/cli", "__adapter", "cursor", "/cursor"}}, true},
		{"custom without analysis", absent, formula.Binding{Binding: "custom", Command: []string{"/bin/custom"}}, true},
		{"codex without analysis", absent, formula.Binding{Binding: "codex", Command: []string{"/cli", "__adapter", "codex", "/codex"}}, false},
		{"cursor with analysis", declared, formula.Binding{Binding: "cursor", Command: []string{"/cli", "__adapter", "cursor", "/cursor"}}, false},
		{"claude-code without analysis", absent, formula.Binding{Binding: "claude-code", Command: []string{"/cli", "__adapter", "claude-code", "/claude"}}, false},
		{"unknown adapter is allowed", absent, formula.Binding{Binding: "future", Command: []string{"/cli", "__adapter", "future", "/future"}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := CheckAnalysisFallback(c.f, c.target); (err != nil) != c.wantErr {
				t.Fatalf("error = %v, want error %t", err, c.wantErr)
			}
		})
	}
}

const diffBase = `kind: formula
id: diff
intake: {repo: acme/widgets}
harness: {binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 1000}
harnesses:
  analysis: {binding: claude-code, command: [/cli, __adapter, claude-code, /claude]}
  coding: {binding: codex, command: [/cli, __adapter, codex, /codex], instructions: keep, timeoutMs: 500}
  meta: {binding: codex, command: [/cli, __adapter, codex, /codex]}
atoms:
  - {id: intake, type: intake}
  - {id: harness, type: harness, binding: codex}
  - {id: check, type: check, command: [go, test]}
  - {id: learn, type: learn, binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 900000}
`

func TestVerifyRebindDiffAcceptsOnlyBindingKeys(t *testing.T) {
	const (
		harnessLine  = "harness: {binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 1000}"
		analysisLine = "  analysis: {binding: claude-code, command: [/cli, __adapter, claude-code, /claude]}"
		codingLine   = "  coding: {binding: codex, command: [/cli, __adapter, codex, /codex], instructions: keep, timeoutMs: 500}"
		metaLine     = "  meta: {binding: codex, command: [/cli, __adapter, codex, /codex]}"
		atomLine     = "  - {id: harness, type: harness, binding: codex}"
		checkLine    = "  - {id: check, type: check, command: [go, test]}"
		learnLine    = "  - {id: learn, type: learn, binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 900000}"
	)
	for _, c := range []struct {
		name, old, new string
		ok             bool
	}{
		{"harness binding and command", harnessLine, "harness: {binding: cursor, command: [/n, __adapter, cursor, /cursor], timeoutMs: 1000}", true},
		{"harness Atom binding", atomLine, "  - {id: harness, type: harness, binding: cursor}", true},
		{"coding binding and command", codingLine, "  coding: {binding: cursor, command: [/n, __adapter, cursor, /cursor], instructions: keep, timeoutMs: 500}", true},
		{"learn launcher", learnLine, "  - {id: learn, type: learn, binding: codex, command: [/n, __adapter, codex, /codex], timeoutMs: 900000}", true},
		{"analysis launcher", analysisLine, "  analysis: {binding: claude-code, command: [/n, __adapter, claude-code, /claude]}", true},
		{"meta launcher", metaLine, "  meta: {binding: codex, command: [/n, __adapter, codex, /codex]}", true},

		{"harness timeout", harnessLine, "harness: {binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 2000}", false},
		{"harness instructions added", harnessLine, "harness: {binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 1000, instructions: x}", false},
		{"coding instructions", codingLine, "  coding: {binding: codex, command: [/cli, __adapter, codex, /codex], instructions: changed, timeoutMs: 500}", false},
		{"coding timeout", codingLine, "  coding: {binding: codex, command: [/cli, __adapter, codex, /codex], instructions: keep, timeoutMs: 900}", false},
		{"analysis adapter", analysisLine, "  analysis: {binding: claude-code, command: [/cli, __adapter, codex, /claude]}", false},
		{"analysis provider", analysisLine, "  analysis: {binding: claude-code, command: [/cli, __adapter, claude-code, /other]}", false},
		{"analysis binding", analysisLine, "  analysis: {binding: other, command: [/cli, __adapter, claude-code, /claude]}", false},
		{"analysis timeout", analysisLine, "  analysis: {binding: claude-code, command: [/cli, __adapter, claude-code, /claude], timeoutMs: 5}", false},
		{"analysis made custom", analysisLine, "  analysis: {binding: claude-code, command: [/bin/custom]}", false},
		{"meta provider", metaLine, "  meta: {binding: codex, command: [/cli, __adapter, codex, /other]}", false},
		{"meta binding", metaLine, "  meta: {binding: other, command: [/cli, __adapter, codex, /codex]}", false},
		{"learn timeout", learnLine, "  - {id: learn, type: learn, binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 1}", false},
		{"learn id", learnLine, "  - {id: study, type: learn, binding: codex, command: [/cli, __adapter, codex, /codex], timeoutMs: 900000}", false},
		{"learn binding", learnLine, "  - {id: learn, type: learn, binding: cursor, command: [/cli, __adapter, codex, /codex], timeoutMs: 900000}", false},
		{"learn adapter", learnLine, "  - {id: learn, type: learn, binding: codex, command: [/cli, __adapter, claude-code, /codex], timeoutMs: 900000}", false},
		{"learn provider", learnLine, "  - {id: learn, type: learn, binding: codex, command: [/cli, __adapter, codex, /other], timeoutMs: 900000}", false},
		{"learn made custom", learnLine, "  - {id: learn, type: learn, binding: codex, command: [/bin/custom], timeoutMs: 900000}", false},
		{"check command", checkLine, "  - {id: check, type: check, command: [go, vet]}", false},
		{"atom removed", learnLine + "\n", "", false},
		{"atom added", learnLine, learnLine + "\n  - {id: ship, type: ship}", false},
		{"check atom gains a binding", checkLine, "  - {id: check, type: check, command: [go, test], binding: codex}", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			after := strings.Replace(diffBase, c.old, c.new, 1)
			if after == diffBase {
				t.Fatal("the table did not change the Formula")
			}
			if err := verifyRebindDiff(diffBase, after); (err == nil) != c.ok {
				t.Fatalf("error = %v, want accepted %t", err, c.ok)
			}
		})
	}
	if err := verifyRebindDiff(diffBase, strings.Replace(diffBase, "  coding:", "  zcoding:", 1)); err == nil {
		t.Fatal("a renamed role was accepted")
	}
	if err := verifyRebindDiff("not: [valid", diffBase); err == nil {
		t.Fatal("unparseable YAML was accepted")
	}
}

func TestProposalSavesPerRoleCapabilityEvidence(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	next, err := rebind(t, root, runner, dir, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := InspectProposal(dir, next.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	roles := saved.CapabilityEvidence.Roles
	want := []struct{ role, binding, source, state string }{
		{"analysis", "claude-code", "default", harness.AdapterCapabilities("claude-code").Analysis.State},
		{"coding", "claude-code", "default", harness.AdapterCapabilities("claude-code").Coding.State},
		{"meta", "codex", "learn-atom", harness.AdapterCapabilities("codex").Meta.State},
	}
	if len(roles) != len(want) {
		t.Fatalf("roles = %+v", roles)
	}
	for i, w := range want {
		if roles[i].Role != w.role || roles[i].Binding != w.binding || roles[i].Source != w.source || roles[i].Capability.State != w.state {
			t.Fatalf("role %d = %+v, want %+v", i, roles[i], w)
		}
	}

	tampered := saved
	tampered.CapabilityEvidence = &BindingReport{}
	*tampered.CapabilityEvidence = *saved.CapabilityEvidence
	tampered.CapabilityEvidence.Roles = append([]RoleCapability(nil), roles...)
	tampered.CapabilityEvidence.Roles[0].Binding = "codex"
	tampered.ID = proposalID(tampered)
	if err = saveProposal(dir, tampered); err != nil {
		t.Fatal(err)
	}
	if _, err = InspectProposal(dir, tampered.ID); err == nil || !strings.Contains(err.Error(), "role capability evidence") {
		t.Fatalf("role evidence that disagrees with the YAML was accepted: %v", err)
	}
	os.Remove(filepath.Join(dir, "assays", tampered.ID+".json"))
}

func TestProposalWithoutRoleEvidenceStaysInspectable(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	next, err := rebind(t, root, runner, dir, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	historical := *next.Proposal
	report := *historical.CapabilityEvidence
	report.Roles = nil
	historical.CapabilityEvidence = &report
	if encoded, _ := json.Marshal(report); strings.Contains(string(encoded), `"roles"`) {
		t.Fatalf("an absent role list changes the evidence bytes, and with them historical proposal IDs: %s", encoded)
	}
	os.Remove(filepath.Join(dir, "assays", historical.ID+".json"))
	historical.ID = proposalID(historical)
	if err = saveProposal(dir, historical); err != nil {
		t.Fatal(err)
	}
	if _, err = InspectProposal(dir, historical.ID); err != nil {
		t.Fatalf("a proposal saved before role evidence existed no longer inspects: %v", err)
	}
}

// switchHarness proposes init --harness ID, which is not a rebind.
func switchHarness(t *testing.T, root string, runner Runner, dir, id string) (Outcome, error) {
	t.Helper()
	return rebind(t, root, runner, dir, id, func(o *InitOptions) { o.Rebind, o.Preferred = "", id })
}

func TestInitHarnessRejectsAnalysisFallbackToUnsupportedAdapter(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	count := assayCount(dir)
	_, err := switchHarness(t, root, runner, dir, "cursor")
	reason := harness.AdapterCapabilities("cursor").Analysis.Reason
	if err == nil || reason == "" || !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), "harnesses.analysis") {
		t.Fatalf("error %v, want the adapter reason %q and the remedy", err, reason)
	}
	if assayCount(dir) != count {
		t.Fatal("a refused init --harness saved a proposal")
	}
	if _, err = switchHarness(t, root, runner, dir, "claude-code"); err != nil {
		t.Fatalf("an adapter that supports analysis is a safe default: %v", err)
	}
	if _, err = Review(dir, mustPending(t, dir), "dismiss"); err != nil {
		t.Fatal(err)
	}
	approveEdited(t, dir, func(doc map[string]any) {
		doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand("/cli", "claude-code")}}
	})
	next, err := switchHarness(t, root, runner, dir, "cursor")
	if err != nil || next.Proposal == nil {
		t.Fatalf("a declared analysis role makes init --harness safe: %+v %v", next, err)
	}
	parsed, _ := formula.Parse([]byte(next.Proposal.YAML))
	if analysis, override := parsed.RoleBinding(formula.AnalysisHarness); !override || analysis.Binding != "claude-code" || parsed.Harness.Binding != "cursor" {
		t.Fatalf("analysis %+v default %s", analysis, parsed.Harness.Binding)
	}
}

func mustPending(t *testing.T, dir string) string {
	t.Helper()
	pending, err := pendingProposal(dir)
	if err != nil || pending == nil {
		t.Fatalf("no pending proposal: %v", err)
	}
	return pending.ID
}

func TestRebindNamesAnOverwrittenCodingOverrideSeparately(t *testing.T) {
	for _, c := range []struct {
		name   string
		coding map[string]any
		want   []string
	}{
		{"custom command", map[string]any{"binding": "custom", "command": []any{"/bin/custom-coder"}}, []string{"harnesses.coding override was overwritten", "custom command", "/bin/custom-coder"}},
		{"another adapter than the old default", map[string]any{"binding": "cursor", "command": nativeCommand("/cli", "cursor")}, []string{"harnesses.coding override was overwritten", "bound to cursor", "old default codex"}},
		{"the old default adapter", map[string]any{"binding": "codex", "command": nativeCommand("/cli", "codex")}, nil},
		{"no override", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, runner, dir, _ := approvedLab(t)
			approved, err := formula.Inspect(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			if c.coding != nil {
				approved = approveEdited(t, dir, func(doc map[string]any) {
					doc["harnesses"] = map[string]any{"coding": c.coding}
					harnessAtom(doc)["binding"] = c.coding["binding"]
				})
			}
			next, err := rebind(t, root, runner, dir, "claude-code")
			if err != nil || next.Proposal == nil {
				t.Fatalf("an overwritten override is a note, never a refusal: %+v %v", next, err)
			}
			if got := strings.Contains(next.Note, "harnesses.coding override was overwritten"); got != (len(c.want) > 0) {
				t.Fatalf("overwrite note present = %t:\n%s", got, next.Note)
			}
			for _, want := range c.want {
				if !strings.Contains(next.Note, want) {
					t.Fatalf("the note omits %q:\n%s", want, next.Note)
				}
			}
			if err = verifyRebindDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
				t.Fatal(err)
			}
			parsed, _ := formula.Parse([]byte(next.Proposal.YAML))
			if coding, _ := parsed.RoleBinding(formula.CodingHarness); coding.Binding != "claude-code" {
				t.Fatalf("coding role = %+v", coding)
			}
		})
	}
}

// init --harness repoints only commands on the old default launcher, and a custom
// default has none; --rebind is the way to move established Forgecell launchers.
func TestInitHarnessWithACustomDefaultRepointsNoNativeCommand(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	older := forgecellBuild(t)
	approved := approveEdited(t, dir, func(doc map[string]any) {
		doc["harness"].(map[string]any)["command"] = []any{"/bin/custom"}
		doc["harnesses"] = map[string]any{"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand(older, "claude-code")}}
		learnAtomOf(t, doc)["command"] = nativeCommand(older, "codex")
	})
	next, err := switchHarness(t, root, runner, dir, "claude-code")
	if err != nil || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	before, after := decodeYAML(t, approved.Formula.YAML), decodeYAML(t, next.Proposal.YAML)
	if !reflect.DeepEqual(before["harnesses"], after["harnesses"]) || !reflect.DeepEqual(learnAtomOf(t, before), learnAtomOf(t, after)) {
		t.Fatalf("init --harness repointed a native command although the default harness is custom:\n%s", next.Proposal.YAML)
	}
	if strings.Contains(next.Note, "command[0]") {
		t.Fatalf("the note lists a launcher rewrite that did not happen: %s", next.Note)
	}
	if got := after["harness"].(map[string]any)["command"]; !reflect.DeepEqual(got, nativeCommand("/cli", "claude-code")) {
		t.Fatalf("default command = %v", got)
	}
}
