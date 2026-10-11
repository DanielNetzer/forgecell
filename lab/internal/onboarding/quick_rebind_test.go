package onboarding

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"go.yaml.in/yaml/v4"
)

func rebindCandidates() []Candidate {
	var all []Candidate
	for _, id := range []string{"codex", "claude-code", "cursor"} {
		all = append(all, Candidate{ID: id, Executable: "/" + id, Readiness: "ready", Capabilities: harness.AdapterCapabilities(id)})
	}
	return all
}

// approvedLab bootstraps and approves a codex Formula that also binds learning.
func approvedLab(t *testing.T) (root string, runner Runner, dir string, boot Outcome) {
	t.Helper()
	root, runner = repoFixture(t)
	dir = t.TempDir()
	boot, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Preferred: "codex", MetaHarness: "codex", Runner: runner})
	if err != nil || boot.Proposal == nil {
		t.Fatalf("%+v %v", boot, err)
	}
	if _, err = Review(dir, boot.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	return root, runner, dir, boot
}

func rebind(t *testing.T, root string, runner Runner, dir, id string, mutate ...func(*InitOptions)) (Outcome, error) {
	t.Helper()
	o := InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: rebindCandidates(), Rebind: id, Runner: runner}
	for _, m := range mutate {
		m(&o)
	}
	return Prepare(context.Background(), o)
}

func decodeYAML(t *testing.T, text string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func harnessAtom(doc map[string]any) map[string]any {
	for _, v := range doc["atoms"].([]any) {
		if a := v.(map[string]any); a["type"] == "harness" {
			return a
		}
	}
	return nil
}

func assayCount(dir string) int {
	entries, _ := os.ReadDir(filepath.Join(dir, "assays"))
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			count++
		}
	}
	return count
}

func TestQuickRebindDiffsOnlyBindingKeys(t *testing.T) {
	root, runner, dir, boot := approvedLab(t)
	active, err := formula.Inspect(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	next, err := rebind(t, root, runner, dir, "claude-code")
	if err != nil || next.Status != "pending" || next.Proposal == nil {
		t.Fatalf("%+v %v", next, err)
	}
	before, after := decodeYAML(t, active.Formula.YAML), decodeYAML(t, next.Proposal.YAML)
	if after["harness"].(map[string]any)["binding"] != "claude-code" || harnessAtom(after)["binding"] != "claude-code" {
		t.Fatalf("binding was not rebound: %v", after["harness"])
	}
	if got := after["harness"].(map[string]any)["command"].([]any); got[0] != "/cli" || got[1] != "__adapter" || got[2] != "claude-code" || got[3] != "/claude-code" {
		t.Fatalf("command = %v", got)
	}
	for _, doc := range []map[string]any{before, after} {
		delete(doc["harness"].(map[string]any), "binding")
		delete(doc["harness"].(map[string]any), "command")
		delete(harnessAtom(doc), "binding")
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rebind changed more than binding keys:\nbefore %v\nafter  %v", before, after)
	}
	parsed, err := formula.Parse([]byte(next.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	learn := 0
	for _, a := range parsed.Atoms {
		if a.Type == "learn" {
			learn++
			if a.Binding != "codex" || len(a.Command) != 4 || a.Command[2] != "codex" {
				t.Fatalf("learn Atom changed: %+v", a)
			}
		}
	}
	if learn != 1 || next.Proposal.EvidenceHash != boot.Proposal.EvidenceHash || next.Proposal.EvidencePath != boot.Proposal.EvidencePath {
		t.Fatalf("learn Atoms=%d evidence %q vs %q", learn, next.Proposal.EvidenceHash, boot.Proposal.EvidenceHash)
	}
	saved, err := InspectProposal(dir, next.Proposal.ID)
	if err != nil || saved.CapabilityEvidence == nil || saved.CapabilityEvidence.Candidate.ID != "claude-code" || !reflect.DeepEqual(saved.CapabilityEvidence.Command, parsed.Harness.Command) {
		t.Fatalf("capability evidence did not follow the binding: %+v %v", saved.CapabilityEvidence, err)
	}
	if saved.CapabilityEvidence.Meta == nil || saved.CapabilityEvidence.Meta.Binding != "codex" {
		t.Fatalf("meta evidence lost: %+v", saved.CapabilityEvidence)
	}
}

func TestQuickRebindApproveAndDismiss(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	config, _ := os.ReadFile(filepath.Join(dir, "lab.json"))
	active, _ := formula.Inspect(dir, "")
	dismissed, err := rebind(t, root, runner, dir, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, dismissed.Proposal.ID, "dismiss"); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(filepath.Join(dir, "lab.json")); string(now) != string(config) {
		t.Fatal("dismissal changed lab.json")
	}
	if now, _ := formula.Inspect(dir, ""); now.Formula.YAML != active.Formula.YAML {
		t.Fatal("dismissal changed the active Formula")
	}
	approved, err := rebind(t, root, runner, dir, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, approved.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	now, err := formula.Load(dir, "")
	if err != nil || !now.Approved || now.Formula.YAML != approved.Proposal.YAML || now.Formula.Harness.Binding != "claude-code" {
		t.Fatalf("approval did not activate the exact proposed bytes: %v", err)
	}
}

func TestQuickRebindPreservesInstructionsAndRoleBindings(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	active, _ := formula.Inspect(dir, "")
	doc := decodeYAML(t, active.Formula.YAML)
	doc["harness"].(map[string]any)["instructions"] = "learned guidance: run the narrowest test first"
	doc["harnesses"] = map[string]any{
		"analysis": map[string]any{"binding": "claude-code", "command": []string{"/cli", "__adapter", "claude-code", "/claude-code"}},
		"coding":   map[string]any{"binding": "codex", "command": []string{"/cli", "__adapter", "codex", "/codex"}},
	}
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
	approved, _ := formula.Inspect(dir, "")
	next, err := rebind(t, root, runner, dir, "cursor")
	if err != nil {
		t.Fatal(err)
	}
	before, after := decodeYAML(t, approved.Formula.YAML), decodeYAML(t, next.Proposal.YAML)
	if harnessAtom(after)["binding"] != "cursor" {
		t.Fatalf("the harness Atom binding must follow the rebound coding override: %v", harnessAtom(after))
	}
	roles := after["harnesses"].(map[string]any)
	coding := roles["coding"].(map[string]any)
	if coding["binding"] != "cursor" || !reflect.DeepEqual(coding["command"], []any{"/cli", "__adapter", "cursor", "/cursor"}) {
		t.Fatalf("the coding override was not rebound: %v", coding)
	}
	if after["harness"].(map[string]any)["instructions"] != "learned guidance: run the narrowest test first" || !reflect.DeepEqual(before["harnesses"].(map[string]any)["analysis"], roles["analysis"]) {
		t.Fatal("instructions or the analysis role binding were not preserved")
	}
	if err = verifyRebindDiff(approved.Formula.YAML, next.Proposal.YAML); err != nil {
		t.Fatalf("rebind changed more than binding keys: %v", err)
	}
	parsed, err := formula.Parse([]byte(next.Proposal.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if b, override := parsed.RoleBinding(formula.CodingHarness); !override || b.Binding != "cursor" {
		t.Fatalf("the coding role does not resolve to the rebound harness: %+v", b)
	}
}

func TestQuickRebindGuards(t *testing.T) {
	root, runner, dir, _ := approvedLab(t)
	count := assayCount(dir)
	cases := []struct {
		name   string
		id     string
		want   string
		mutate func(*InitOptions)
	}{
		{"unknown harness", "nonesuch", "no such harness", nil},
		{"not ready", "cursor", "cannot rebind to cursor", func(o *InitOptions) {
			o.Candidates = []Candidate{{ID: "cursor", Executable: "/cursor", Readiness: "blocked", Detail: "not logged in"}}
		}},
		{"no executable", "cursor", "cannot rebind to cursor", func(o *InitOptions) {
			o.Candidates = []Candidate{{ID: "cursor", Readiness: "ready"}}
		}},
		{"no launcher", "cursor", "launcher", func(o *InitOptions) { o.Launcher = "" }},
		{"already bound", "codex", "nothing to rebind", nil},
		{"with harness", "cursor", "only the binding", func(o *InitOptions) { o.Preferred = "codex" }},
		{"with meta", "cursor", "only the binding", func(o *InitOptions) { o.MetaHarness = "codex" }},
		{"with timeouts", "cursor", "only the binding", func(o *InitOptions) { o.HarnessTimeoutMS = 1000 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var mutate []func(*InitOptions)
			if c.mutate != nil {
				mutate = append(mutate, c.mutate)
			}
			if _, err := rebind(t, root, runner, dir, c.id, mutate...); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want %q", err, c.want)
			}
			if assayCount(dir) != count {
				t.Fatal("a refused rebind saved a proposal")
			}
		})
	}
	if _, err := rebind(t, root, runner, dir, "claude-code"); err != nil {
		t.Fatal(err)
	}
	if _, err := rebind(t, root, runner, dir, "cursor"); err == nil || !strings.Contains(err.Error(), "already pending") {
		t.Fatalf("second rebind beside a pending proposal: %v", err)
	}
}

func TestQuickRebindRequiresCurrentExactApproval(t *testing.T) {
	root, runner := repoFixture(t)
	empty := t.TempDir()
	if _, err := rebind(t, root, runner, empty, "codex"); err == nil || !strings.Contains(err.Error(), "approved Formula") {
		t.Fatalf("no Formula: %v", err)
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
	os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"sample"}`), 0600)
	os.WriteFile(filepath.Join(dir, "formulas", "sample.yaml"), []byte("kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex]}\natoms: [{id: harness, type: harness, binding: codex}]\n"), 0600)
	if _, err := rebind(t, root, runner, dir, "cursor"); err == nil || !strings.Contains(err.Error(), "exact approval") {
		t.Fatalf("unapproved Formula: %v", err)
	}
	if assayCount(dir) != 0 {
		t.Fatal("a refused rebind saved a proposal")
	}
}

func roleFormula(harnesses string) string {
	return "kind: formula\nid: roles\nintake: {repo: acme/widgets}\nharness: {binding: codex, command: [/cli, __adapter, codex, /codex]}\n" + harnesses + "atoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness, binding: codex}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n"
}

func TestQuickRebindRejectsUnsupportedRoleWithAdapterReason(t *testing.T) {
	for name, c := range map[string]struct{ harnesses, reason string }{
		"cursor analysis": {"harnesses:\n  analysis: {binding: cursor, command: [/cli, __adapter, cursor, /cursor]}\n", harness.AdapterCapabilities("cursor").Analysis.Reason},
		"custom analysis": {"harnesses:\n  analysis: {binding: custom, command: [/bin/custom]}\n", "no verified read-only ticket-analysis protocol"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := SaveProposal(dir, roleFormula(c.harnesses))
			if err == nil || c.reason == "" || !strings.Contains(err.Error(), c.reason) || !strings.Contains(err.Error(), "harnesses.analysis") {
				t.Fatalf("error %v, want the adapter reason %q", err, c.reason)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("a rejected assignment wrote files: %v", entries)
			}
		})
	}
	dir := t.TempDir()
	supported := "harnesses:\n  analysis: {binding: claude-code, command: [/cli, __adapter, claude-code, /claude]}\n  coding: {binding: codex, command: [/bin/custom]}\n"
	p, err := SaveProposal(dir, roleFormula(supported))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = InspectProposal(dir, p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestQuickRebindRoleConflictIsRejectedAtProposalTime(t *testing.T) {
	harnesses := "harnesses:\n  coding: {binding: claude-code, command: [/cli, __adapter, claude-code, /claude]}\n"
	if _, err := SaveProposal(t.TempDir(), roleFormula(harnesses)); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("a coding role that contradicts the harness Atom binding was accepted: %v", err)
	}
}

func TestQuickRebindRoleReadinessNamesEachRole(t *testing.T) {
	f, err := formula.Parse([]byte(roleFormula("harnesses:\n  analysis: {binding: cursor, command: [/bin/custom]}\n")))
	if err != nil {
		t.Fatal(err)
	}
	roles := InspectRoles(context.Background(), f, t.TempDir(), nil)
	if len(roles) != 2 || roles[0].Role != "analysis" || roles[0].Source != "override" || roles[1].Role != "coding" || roles[1].Source != "default" {
		t.Fatalf("%+v", roles)
	}
	if roles[0].Ready || roles[0].Capability.State != "unsupported" {
		t.Fatalf("analysis must not be ready: %+v", roles[0])
	}
}
