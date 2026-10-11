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

func TestRebindRepointsRoleOverridesOnTheOldLauncherOnly(t *testing.T) {
	for _, c := range []struct {
		name          string
		launcher      string
		custom        bool
		wantRepointed bool
	}{
		{name: "old launcher", launcher: "/cli", wantRepointed: true},
		{name: "different launcher", launcher: "/other"},
		{name: "custom argv", launcher: "/cli", custom: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, runner, dir, _ := approvedLab(t)
			approved := approveEdited(t, dir, func(doc map[string]any) {
				learn := learnAtomOf(t, doc)
				if c.custom {
					learn["command"] = []any{"/bin/custom-learner"}
					return
				}
				learn["command"] = nativeCommand(c.launcher, "codex")
				doc["harnesses"] = map[string]any{
					"analysis": map[string]any{"binding": "claude-code", "command": nativeCommand(c.launcher, "claude-code")},
					"meta":     map[string]any{"binding": "codex", "command": nativeCommand(c.launcher, "codex")},
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
					t.Fatalf("commands that do not use the old launcher were changed:\n%s", next.Proposal.YAML)
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
