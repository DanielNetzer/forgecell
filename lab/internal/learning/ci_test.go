package learning

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

func TestExactCILearningEvidence(t *testing.T) {
	for _, status := range []string{"passed", "failed", "unknown", "stale", "missing", "pending", "tampered", "budget"} {
		t.Run(status, func(t *testing.T) {
			d, _ := fixture(t)
			yaml := recipe + "  - type: learn\n    command: [meta-harness]\n"
			approveLearning(t, d, yaml, "generation")
			r := verifiedCIRecord(t)
			r.SchemaVersion = "v0"
			r.Kind = "molecule"
			r.Mode = "ticket"
			r.FormulaApproved = true
			r.FormulaID = "default"
			r.FormulaSnapshot = molecule.Snapshot{YAML: yaml, SHA256: hash(yaml)}
			os.MkdirAll(filepath.Join(d, "ledgers"), 0700)
			ledger, _ := json.Marshal(r)
			os.WriteFile(filepath.Join(d, "ledgers", r.ID+".json"), ledger, 0600)
			receipt := delivery.Receipt{State: "published", DraftAtPublication: true, Commit: strings.Repeat("1", 40), URL: "https://github.com/owner/repo/pull/9", Preview: delivery.PreviewResult{MoleculeID: r.ID, Repo: r.Issue.Repo, BaseCommit: r.Workspace.BaseCommit, Branch: r.Workspace.Branch, Tree: r.Verification.SourceTree}}
			raw, _ := json.Marshal(receipt.Preview)
			receipt.Preview.Digest = fmt.Sprintf("%x", sha256.Sum256(raw))
			raw, _ = json.Marshal(receipt)
			os.MkdirAll(filepath.Join(d, "deliveries"), 0700)
			os.WriteFile(filepath.Join(d, "deliveries", r.ID+".json"), raw, 0600)
			o := delivery.Options{LabDir: d, MoleculeID: r.ID}
			s := checks.Snapshot{Repo: r.Issue.Repo, PR: 9, Commit: receipt.Commit, ObservedHead: receipt.Commit, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Required: []string{"test"}, Status: "passed", Checks: []checks.Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "https://github.com/owner/repo/actions/runs/1"}}}
			if _, err := delivery.AppendCI(o, s); err != nil {
				t.Fatal(err)
			}
			if status == "budget" {
				large := s
				large.Status = "unknown"
				large.Checks = nil
				for i := 0; i < 100; i++ {
					large.Checks = append(large.Checks, checks.Check{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS", URL: strings.Repeat("x", 1900)})
				}
				for i := 0; i < 12; i++ {
					if _, err := delivery.AppendCI(o, large); err != nil {
						t.Fatal(err)
					}
				}
			}
			if status != "passed" && status != "tampered" && status != "budget" {
				s.Status = status
				switch status {
				case "failed":
					s.Checks[0].Conclusion = "FAILURE"
				case "pending":
					s.Checks[0].Status = "IN_PROGRESS"
				case "stale":
					s.ObservedHead = strings.Repeat("2", 40)
				case "missing", "unknown":
					s.Checks = nil
				}
				if _, err := delivery.AppendCI(o, s); err != nil {
					t.Fatal(err)
				}
			}
			expected, err := delivery.ReadCI(o)
			if err != nil {
				t.Fatal(err)
			}
			if status == "tampered" {
				files, _ := filepath.Glob(filepath.Join(d, "deliveries", r.ID, "ci", "*.json"))
				os.WriteFile(files[0], []byte(`{}`), 0600)
			}
			invoked := false
			ids := []string{r.ID}
			if status == "budget" {
				ids = append(ids, r.ID)
			}
			_, err = create(context.Background(), Options{LabDir: d, MoleculeIDs: ids}, func(_ context.Context, _ []string, _ string, _ time.Duration, req map[string]any) process.Result {
				invoked = true
				outcomes := req["evidenceOutcomes"].([]EvidenceOutcome)
				e := outcomes[0]
				if e.Outcome != "draft-published" || e.IndependentAcceptance || e.CIClassification != wantCIClassification(status) || !reflect.DeepEqual(e.CIObservations, expected) {
					t.Fatalf("incorrect CI payload: %+v", e)
				}
				return process.Result{Error: "synthetic stop after payload inspection"}
			})
			if err == nil || invoked == (status == "tampered" || status == "budget") {
				t.Fatalf("invoked=%t error=%v", invoked, err)
			}
			if status == "budget" && !strings.Contains(err.Error(), "CI learning evidence exceeds budget") {
				t.Fatalf("wrong budget error: %v", err)
			}
			after, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
			if string(after) != yaml {
				t.Fatal("Formula mutated")
			}
			after, _ = os.ReadFile(filepath.Join(d, "ledgers", r.ID+".json"))
			if string(after) != string(ledger) {
				t.Fatal("ledger mutated")
			}
		})
	}
}

func wantCIClassification(status string) string {
	switch status {
	case "passed":
		return "passed"
	case "failed":
		return "failure"
	default:
		return "unknown"
	}
}
