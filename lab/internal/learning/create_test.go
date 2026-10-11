package learning

import (
	"context"
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateProposal(t *testing.T) {
	for _, scenario := range []string{"pending", "check change", "invalid gates", "cosmetic", "missing explanation", "stale during invocation", "unbound", "mixed snapshot", "damaged snapshot"} {
		t.Run(scenario, func(t *testing.T) {
			d, _ := fixture(t)
			yaml := recipe + "  - type: learn\n    command: [meta-harness]\n"
			if scenario == "unbound" {
				yaml = recipe
			}
			approveLearning(t, d, yaml, "generation")
			r := molecule.Record{ID: "mol-1", SchemaVersion: "v0", Kind: "molecule", Mode: "ticket", FinishedAt: stamp(), FormulaApproved: true, FormulaID: "default", FormulaSnapshot: molecule.Snapshot{YAML: yaml, SHA256: hash(yaml)}}
			if scenario == "damaged snapshot" {
				r.FormulaSnapshot.SHA256 = "wrong"
			}
			os.MkdirAll(filepath.Join(d, "ledgers"), 0700)
			b, _ := json.Marshal(r)
			os.WriteFile(filepath.Join(d, "ledgers/mol-1.json"), b, 0600)
			ids := []string{"mol-1"}
			if scenario == "mixed snapshot" {
				r.ID = "mol-2"
				r.FormulaSnapshot.YAML += "# older\n"
				r.FormulaSnapshot.SHA256 = hash(r.FormulaSnapshot.YAML)
				b, _ = json.Marshal(r)
				os.WriteFile(filepath.Join(d, "ledgers/mol-2.json"), b, 0600)
				ids = append(ids, "mol-2")
			}
			invoked := false
			p, e := create(context.Background(), Options{LabDir: d, MoleculeIDs: ids}, func(_ context.Context, args []string, _ string, _ time.Duration, request map[string]any) process.Result {
				invoked = true
				if args[0] != "meta-harness" || request["kind"] != "formula-improvement" {
					t.Fatal("wrong harness contract")
				}
				candidate := strings.Replace(yaml, "Check tests.", "Run the targeted check before the full suite.", 1)
				if scenario == "check change" {
					candidate = strings.Replace(yaml, "command: [echo, checks]", "command: [echo, targeted]", 1)
				}
				if scenario == "invalid gates" {
					candidate = strings.Replace(candidate, "purpose: scope", "purpose: review", 1)
				}
				if scenario == "cosmetic" {
					candidate = yaml + "# pretty\n"
				}
				summary := "Run targeted checks first."
				if scenario == "missing explanation" {
					summary = ""
				}
				if scenario == "stale during invocation" {
					os.WriteFile(filepath.Join(d, "formulas/default.yaml"), []byte(yaml+"# concurrent\n"), 0600)
				}
				b, _ := json.Marshal(map[string]string{"summary": summary, "rationale": "The ledger recorded a failing full suite; this is limited evidence.", "expectedImpact": "May shorten feedback time.", "evaluation": "Compare failure discovery time and full-suite regressions on equivalent work.", "yaml": candidate})
				return process.Result{OK: true, Stdout: string(b)}
			})
			if scenario == "pending" || scenario == "check change" || scenario == "invalid gates" {
				if e != nil || p.Status != "pending" {
					t.Fatalf("%+v %v", p, e)
				}
				if len(p.MotivatingEvidence) != 1 || p.MotivatingEvidence[0].MoleculeID != "mol-1" || p.MotivatingEvidence[0].SHA256 != hash(string(p.MotivatingEvidence[0].Ledger)) || p.Comparison.Status != "missing" {
					t.Fatal("missing motivating identities or initial comparison state")
				}
				if scenario == "check change" && !strings.HasPrefix(p.Comparison.Support, "unsupported:") {
					t.Fatal("unsupported intervention hidden")
				}
				if scenario == "pending" && !strings.Contains(p.Diff, "+  instructions:") {
					t.Fatal("missing instruction diff")
				}
				if p.ProposedReady != (scenario != "invalid gates") {
					t.Fatalf("unexpected readiness: %+v", p)
				}
				if scenario == "invalid gates" && !strings.Contains(p.ReadinessError, "expected type gate") {
					t.Fatal(p.ReadinessError)
				}
				current, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
				if string(current) != yaml {
					t.Fatal("proposal applied")
				}
				if _, e = Read(d, p.ID); e != nil {
					t.Fatal(e)
				}
				if scenario != "invalid gates" {
					if _, e = Review(d, p.ID, "approve"); e != nil {
						t.Fatal(e)
					}
				}
			} else {
				if e == nil {
					t.Fatal("invalid proposal accepted")
				}
				if (scenario == "unbound" || scenario == "mixed snapshot" || scenario == "damaged snapshot") && invoked {
					t.Fatal("invoked on invalid evidence")
				}
			}
		})
	}
}

func TestExtensionDeclarationsAreProtectedFromLearning(t *testing.T) {
	digest, other := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	notes := "  notes: {manifest: extensions/notes.json, sha256: " + digest + ", required: true}\n"
	base := strings.Replace(recipe, "harness:\n", "extensions:\n  graphify: {manifest: extensions/graphify.json, sha256: "+digest+"}\n"+notes+"harness:\n", 1)
	base = strings.Replace(base, "  instructions: Check tests.\n", "  instructions: Check tests.\n  extensions: [graphify]\n", 1)
	yaml := base + "  - type: learn\n    command: [meta-harness]\n"
	replace := func(from, to string) func(string) string {
		return func(y string) string { return strings.Replace(y, from, to, 1) }
	}
	for _, tc := range []struct {
		name    string
		edit    func(string) string
		refused string
	}{
		{"unchanged declarations", func(y string) string { return y }, ""},
		{"role attachment may change", replace("  extensions: [graphify]\n", "  extensions: [graphify, notes]\n"), ""},
		{"required may change", replace("required: true", "required: false"), ""},
		{"added declaration", replace("harness:\n  command", "  extra: {manifest: extensions/extra.json, sha256: "+digest+"}\nharness:\n  command"), "extra"},
		{"removed declaration", replace(notes, ""), "notes"},
		{"changed manifest", replace("extensions/graphify.json", "extensions/other.json"), "graphify"},
		{"changed sha256", replace("graphify.json, sha256: "+digest, "graphify.json, sha256: "+other), "graphify"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every proposal also carries a genuine workflow change.
			candidate := tc.edit(strings.Replace(yaml, "Check tests.", "Run the targeted check before the full suite.", 1))
			saved, e := proposeFormula(t, yaml, candidate)
			if tc.refused == "" {
				if e != nil || saved != 1 {
					t.Fatalf("%v, %d new suggestions", e, saved)
				}
				return
			}
			if e == nil || !strings.Contains(e.Error(), `extension "`+tc.refused+`"`) {
				t.Fatalf("want a refusal naming %q, got %v", tc.refused, e)
			}
			if saved != 0 {
				t.Fatalf("a refused proposal was saved: %d new suggestions", saved)
			}
		})
	}
}

// proposeFormula approves yaml, lets a stub learner return candidate, and reports
// create's error with how many new suggestions were saved.
func proposeFormula(t *testing.T, yaml, candidate string) (int, error) {
	t.Helper()
	d, _ := fixture(t)
	approveLearning(t, d, yaml, "generation")
	r := molecule.Record{ID: "mol-1", SchemaVersion: "v0", Kind: "molecule", Mode: "ticket", FinishedAt: stamp(), FormulaApproved: true, FormulaID: "default", FormulaSnapshot: molecule.Snapshot{YAML: yaml, SHA256: hash(yaml)}}
	os.MkdirAll(filepath.Join(d, "ledgers"), 0700)
	b, _ := json.Marshal(r)
	os.WriteFile(filepath.Join(d, "ledgers/mol-1.json"), b, 0600)
	_, e := create(context.Background(), Options{LabDir: d, MoleculeIDs: []string{"mol-1"}}, func(context.Context, []string, string, time.Duration, map[string]any) process.Result {
		b, _ := json.Marshal(map[string]string{"summary": "Run targeted checks first.", "rationale": "The ledger recorded a failing full suite; this is limited evidence.", "expectedImpact": "May shorten feedback time.", "evaluation": "Compare failure discovery time and full-suite regressions on equivalent work.", "yaml": candidate})
		return process.Result{OK: true, Stdout: string(b)}
	})
	saved, _ := os.ReadDir(filepath.Join(d, "suggestions"))
	return len(saved) - 1, e
}

func extensionNativeCommand(adapter string) string {
	return "[/cli, __adapter, " + adapter + ", /" + adapter + "]"
}

// extensionBase is the default recipe plus declarations g and h, the given default
// harness lines, a learn Atom and a trailing harnesses block.
func extensionBase(harnessLines, roles string) string {
	digest := strings.Repeat("ab", 32)
	declarations := "extensions:\n  g: {manifest: extensions/g.json, sha256: " + digest + "}\n  h: {manifest: extensions/h.json, sha256: " + digest + ", required: true}\n"
	y := strings.Replace(recipe, "harness:\n  command: [echo]\n", declarations+"harness:\n"+harnessLines, 1)
	return y + "  - type: learn\n    command: [meta-harness]\n" + roles
}

func TestExtensionsCannotBindToUnsupportedAdaptersThroughLearning(t *testing.T) {
	const reason = "extensions are not wired for this adapter in v1"
	attach := func(on bool) string {
		if on {
			return "  extensions: [g]\n"
		}
		return ""
	}
	defaultLines := func(adapter string, on bool) string {
		return "  binding: " + adapter + "\n  command: " + extensionNativeCommand(adapter) + "\n" + attach(on)
	}
	role := func(name, adapter string, on bool) string {
		list := ""
		if on {
			list = ", extensions: [g]"
		}
		return "harnesses:\n  " + name + ": {binding: " + adapter + ", command: " + extensionNativeCommand(adapter) + list + "}\n"
	}
	echo := "  command: [echo]\n"
	for _, tc := range []struct {
		name    string
		build   func(attached bool) string
		role    string
		refused bool
	}{
		{"cursor default supplies coding", func(on bool) string { return extensionBase(defaultLines("cursor", on), "") }, "coding", true},
		{"codex default supplies coding", func(on bool) string { return extensionBase(defaultLines("codex", on), "") }, "coding", true},
		{"codex coding override", func(on bool) string { return extensionBase(echo, role("coding", "codex", on)) }, "coding", true},
		{"cursor coding override", func(on bool) string { return extensionBase(echo, role("coding", "cursor", on)) }, "coding", true},
		{"codex analysis override", func(on bool) string { return extensionBase(echo, role("analysis", "codex", on)) }, "analysis", true},
		{"claude-code default", func(on bool) string { return extensionBase(defaultLines("claude-code", on), "") }, "coding", false},
		{"claude-code coding override", func(on bool) string { return extensionBase(echo, role("coding", "claude-code", on)) }, "coding", false},
		{"custom default is not judged", func(on bool) string { return extensionBase(echo+attach(on), "") }, "coding", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved, e := proposeFormula(t, tc.build(false), tc.build(true))
			if !tc.refused {
				if e != nil || saved != 1 {
					t.Fatalf("%v, %d new suggestions", e, saved)
				}
				return
			}
			if e == nil || !strings.Contains(e.Error(), reason) || !strings.Contains(e.Error(), "the "+tc.role+" role") {
				t.Fatalf("want a refusal quoting the adapter reason for the %s role, got %v", tc.role, e)
			}
			if saved != 0 {
				t.Fatalf("a refused proposal was saved: %d new suggestions", saved)
			}
		})
	}
}

func TestExtensionAttachmentIsWorkflowBehavior(t *testing.T) {
	claude := "  binding: claude-code\n  command: " + extensionNativeCommand("claude-code") + "\n"
	detached := extensionBase(claude, "")
	attached := extensionBase(claude+"  extensions: [g]\n", "")
	roleBase := func(list string) string {
		return extensionBase("  command: [echo]\n", "harnesses:\n  coding: {binding: claude-code, command: "+extensionNativeCommand("claude-code")+list+"}\n")
	}
	for _, tc := range []struct {
		name     string
		from, to string
		changed  bool
	}{
		{"attach only", detached, attached, true},
		{"detach only", attached, detached, true},
		{"role attach only", roleBase(""), roleBase(", extensions: [h]"), true},
		{"role detach only", roleBase(", extensions: [h]"), roleBase(""), true},
		{"reorder only", extensionBase(claude+"  extensions: [g, h]\n", ""), extensionBase(claude+"  extensions: [h, g]\n", ""), true},
		{"required flip only", detached, strings.Replace(detached, "required: true", "required: false", 1), true},
		{"empty list equals absent", detached, strings.Replace(detached, "  instructions: Check tests.\n", "  instructions: Check tests.\n  extensions: []\n", 1), false},
		{"comment only", attached, attached + "# pretty\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved, e := proposeFormula(t, tc.from, tc.to)
			if tc.changed {
				if e != nil || saved != 1 {
					t.Fatalf("a behavior change was refused: %v, %d new suggestions", e, saved)
				}
				return
			}
			if e == nil || !strings.Contains(e.Error(), "no workflow behavior change") || saved != 0 {
				t.Fatalf("a cosmetic edit was accepted: %v, %d new suggestions", e, saved)
			}
		})
	}
}

func TestAggregateMotivatingEvidenceBoundBeforeInvocation(t *testing.T) {
	d, _ := fixture(t)
	r := molecule.Record{ID: "mol-large", SchemaVersion: "v0", Kind: "molecule", Mode: "ticket", FinishedAt: stamp(), Status: "failed", FormulaApproved: true, FormulaID: "default", FormulaSnapshot: molecule.Snapshot{YAML: recipe, SHA256: hash(recipe)}, Issue: molecule.Issue{Body: strings.Repeat("x", 2_100_000)}}
	raw, _ := json.Marshal(r)
	os.MkdirAll(filepath.Join(d, "ledgers"), 0700)
	os.WriteFile(filepath.Join(d, "ledgers", r.ID+".json"), raw, 0600)
	_, err := create(context.Background(), Options{LabDir: d, MoleculeIDs: []string{r.ID, r.ID}}, func(context.Context, []string, string, time.Duration, map[string]any) process.Result {
		t.Fatal("invoked oversized evidence")
		return process.Result{}
	})
	if err == nil || !strings.Contains(err.Error(), "aggregate motivating") {
		t.Fatalf("%v", err)
	}
}
