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
