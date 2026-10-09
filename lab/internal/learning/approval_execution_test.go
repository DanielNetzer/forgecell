package learning

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

func TestLearningRejectedSnapshotsNeverInvokeMetaHarness(t *testing.T) {
	for _, kind := range []string{"legacy", "edited", "missing", "historical", "stale"} {
		t.Run(kind, func(t *testing.T) {
			d, _ := fixture(t)
			yaml := recipe + "  - type: learn\n    command: [meta-harness]\n"
			approveLearning(t, d, yaml, "generation")
			ledger := molecule.Record{ID: "mol-1", SchemaVersion: "v0", Kind: "molecule", Mode: "ticket", FinishedAt: stamp(), FormulaApproved: true, FormulaID: "default", FormulaSnapshot: molecule.Snapshot{YAML: yaml, SHA256: hash(yaml)}}
			os.MkdirAll(filepath.Join(d, "ledgers"), 0700)
			b, _ := json.Marshal(ledger)
			os.WriteFile(filepath.Join(d, "ledgers/mol-1.json"), b, 0600)
			switch kind {
			case "legacy":
				os.WriteFile(filepath.Join(d, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"default"}`), 0600)
			case "edited":
				os.WriteFile(filepath.Join(d, "formulas/default.yaml"), []byte(yaml+"# modified\n"), 0600)
			case "missing":
				os.Remove(filepath.Join(d, "formulas/default.yaml"))
			case "historical":
				approveLearning(t, d, strings.Replace(yaml, "id: default", "id: next", 1), "next")
			case "stale":
				os.RemoveAll(filepath.Join(d, "formula-decisions"))
			}
			calls := 0
			_, e := create(context.Background(), Options{LabDir: d, MoleculeIDs: []string{"mol-1"}}, func(context.Context, []string, string, time.Duration, map[string]any) process.Result {
				calls++
				return process.Result{}
			})
			if e == nil || calls != 0 {
				t.Fatalf("error=%v meta calls=%d", e, calls)
			}
		})
	}
}
func TestApprovalIdentityDriftDuringGenerationSavesNothing(t *testing.T) {
	d, _ := fixture(t)
	yaml := recipe + "  - type: learn\n    command: [meta-harness]\n"
	approveLearning(t, d, yaml, "generation")
	ledger := molecule.Record{ID: "mol-1", SchemaVersion: "v0", Kind: "molecule", Mode: "ticket", FinishedAt: stamp(), FormulaApproved: true, FormulaID: "default", FormulaSnapshot: molecule.Snapshot{YAML: yaml, SHA256: hash(yaml)}}
	os.MkdirAll(filepath.Join(d, "ledgers"), 0700)
	b, _ := json.Marshal(ledger)
	os.WriteFile(filepath.Join(d, "ledgers/mol-1.json"), b, 0600)
	before, _ := os.ReadDir(filepath.Join(d, "suggestions"))
	_, e := create(context.Background(), Options{LabDir: d, MoleculeIDs: []string{"mol-1"}}, func(context.Context, []string, string, time.Duration, map[string]any) process.Result {
		approveLearning(t, d, yaml, "reactivated")
		return process.Result{OK: true, Stdout: `{"summary":"changed","rationale":"evidence","expectedImpact":"hypothesis","evaluation":"measure","yaml":"kind: formula"}`}
	})
	after, _ := os.ReadDir(filepath.Join(d, "suggestions"))
	if e == nil || !strings.Contains(e.Error(), "changed during learning") || len(after) != len(before) {
		t.Fatalf("identity drift saved suggestion: %v", e)
	}
}

func TestSuggestionApprovalNeverInvokesHarness(t *testing.T) {
	d, p := fixture(t)
	marker := filepath.Join(d, "called")
	provider := filepath.Join(d, "provider")
	if e := os.WriteFile(provider, []byte("#!/bin/sh\necho called > "+marker+"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	p.ProposedYAML = strings.Replace(recipe, "command: [echo]", "command: ["+provider+"]", 1) + "  - type: learn\n    command: [" + provider + "]\n"
	p.ProposedHash = hash(p.ProposedYAML)
	if e := save(d, p); e != nil {
		t.Fatal(e)
	}
	if _, e := Review(d, p.ID, "approve"); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatalf("harness invoked: %v", e)
	}
	for _, name := range []string{"workspaces", "ledgers"} {
		if _, e := os.Stat(filepath.Join(d, name)); !os.IsNotExist(e) {
			t.Fatalf("ticket operation %s: %v", name, e)
		}
	}
}
