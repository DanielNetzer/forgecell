package learning

import (
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const recipe = `kind: formula
id: default
harness:
  command: [echo]
  instructions: Check tests.
atoms:
  - type: intake
  - type: gate
    purpose: scope
  - type: harness
  - type: check
    command: [echo, checks]
  - type: gate
    purpose: review
  - type: ship
  - type: document
`

func fixture(t *testing.T) (string, Suggestion) {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
	if err := os.WriteFile(filepath.Join(dir, "formulas/default.yaml"), []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	approval := approveLearning(t, dir, recipe, "initial")
	p := Suggestion{OriginApproval: approval, ID: "suggestion-test", FormulaID: "default", Status: "pending", OriginalYAML: recipe, ProposedYAML: recipe + "# reviewed\n"}
	p.OriginalHash = hash(p.OriginalYAML)
	p.ProposedHash = hash(p.ProposedYAML)
	if err := save(dir, p); err != nil {
		t.Fatal(err)
	}
	return dir, p
}
func TestReviewTrust(t *testing.T) {
	t.Run("approve exact bytes", func(t *testing.T) {
		d, p := fixture(t)
		r, e := Review(d, p.ID, "approve")
		if e != nil || r.Status != "approved" {
			t.Fatalf("%+v %v", r, e)
		}
		b, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
		if string(b) != p.ProposedYAML {
			t.Fatal("wrong recipe")
		}
		if _, e = Review(d, p.ID, "approve"); e == nil {
			t.Fatal("duplicate approval")
		}
	})
	t.Run("stale refuses", func(t *testing.T) {
		d, p := fixture(t)
		os.WriteFile(filepath.Join(d, "formulas/default.yaml"), []byte(recipe+"# changed\n"), 0600)
		if _, e := Review(d, p.ID, "approve"); e == nil {
			t.Fatal("stale applied")
		}
		if _, e := Review(d, p.ID, "dismiss"); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("tampered refuses", func(t *testing.T) {
		d, p := fixture(t)
		p.ProposedYAML += "# tampered\n"
		save(d, p)
		if _, e := Review(d, p.ID, "approve"); e == nil {
			t.Fatal("tampered applied")
		}
	})
	t.Run("recover intent", func(t *testing.T) {
		d, p := fixture(t)
		p.ConfigBefore, _ = formula.FileHash(filepath.Join(d, "lab.json"))
		a := formula.Approval{FormulaID: p.FormulaID, SHA256: p.ProposedHash, DecisionID: p.ID, SourceKind: "learning-suggestion", SourceID: p.ID}
		if e := formula.BeginActivation(d, p.ProposedYAML, a, p.ConfigBefore, p.OriginalHash); e != nil {
			t.Fatal(e)
		}
		p.ApplyingAt = "2026-09-29T00:00:00Z"
		save(d, p)
		os.WriteFile(filepath.Join(d, "formulas/default.yaml"), []byte(p.ProposedYAML), 0600)
		if _, e := Review(d, p.ID, "dismiss"); e == nil {
			t.Fatal("dismissed applied approval")
		}
		if _, e := Review(d, p.ID, "approve"); e != nil {
			t.Fatal(e)
		}
	})
	t.Run("shared lock", func(t *testing.T) {
		d, p := fixture(t)
		os.Mkdir(filepath.Join(d, ".formula-write.lock"), 0700)
		if _, e := Review(d, p.ID, "approve"); e == nil {
			t.Fatal("ignored lock")
		}
	})
	t.Run("dismiss preserves", func(t *testing.T) {
		d, p := fixture(t)
		if _, e := Review(d, p.ID, "dismiss"); e != nil {
			t.Fatal(e)
		}
		b, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
		if string(b) != recipe {
			t.Fatal("dismiss mutated recipe")
		}
	})
}

func TestReviewRecomputesDiff(t *testing.T) {
	d, p := fixture(t)
	p.Diff = "Misleading model-provided review"
	save(d, p)
	r, e := Read(d, p.ID)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(r.Diff, "Misleading") || !strings.Contains(r.Diff, "+# reviewed") {
		t.Fatal("did not derive review from exact YAML")
	}
}
func TestDottedFormulaID(t *testing.T) {
	d, p := fixture(t)
	p.FormulaID = "forge.cell"
	p.OriginalYAML = strings.Replace(p.OriginalYAML, "id: default", "id: forge.cell", 1)
	p.ProposedYAML = strings.Replace(p.ProposedYAML, "id: default", "id: forge.cell", 1)
	p.OriginalHash = hash(p.OriginalYAML)
	p.ProposedHash = hash(p.ProposedYAML)
	p.OriginApproval = approveLearning(t, d, p.OriginalYAML, "dotted")
	save(d, p)
	if _, e := Review(d, p.ID, "approve"); e != nil {
		t.Fatal(e)
	}
}

func approveLearning(t *testing.T, dir, yaml, id string) formula.Approval {
	t.Helper()
	f, e := formula.Parse([]byte(yaml))
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := formula.FileHash(filepath.Join(dir, "lab.json"))
	if e != nil {
		t.Fatal(e)
	}
	target, e := formula.Inspect(dir, f.ID)
	before := "absent"
	if e == nil {
		before = target.Formula.SHA256
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: id, SourceKind: "test-review", SourceID: id}
	if e = formula.BeginActivation(dir, yaml, a, cfg, before); e != nil {
		t.Fatal(e)
	}
	if e = formula.ApplyActivation(dir, a); e != nil {
		t.Fatal(e)
	}
	if e = formula.CompleteActivation(dir, a); e != nil {
		t.Fatal(e)
	}
	return a
}

func TestSameBytesNewApprovalInvalidatesSuggestion(t *testing.T) {
	d, p := fixture(t)
	approveLearning(t, d, recipe, "reactivated")
	if _, e := Review(d, p.ID, "approve"); e == nil {
		t.Fatal("old originating approval authorized new decision")
	}
}
func TestInterruptedLearningApprovalBoundaries(t *testing.T) {
	for _, boundary := range []string{"intent", "yaml", "record", "decision"} {
		t.Run(boundary, func(t *testing.T) {
			d, p := fixture(t)
			p.ConfigBefore, _ = formula.FileHash(filepath.Join(d, "lab.json"))
			a := formula.Approval{FormulaID: p.FormulaID, SHA256: p.ProposedHash, DecisionID: p.ID, SourceKind: "learning-suggestion", SourceID: p.ID}
			if e := formula.BeginActivation(d, p.ProposedYAML, a, p.ConfigBefore, p.OriginalHash); e != nil {
				t.Fatal(e)
			}
			p.ApplyingAt = stamp()
			if e := save(d, p); e != nil {
				t.Fatal(e)
			}
			if boundary == "yaml" {
				os.WriteFile(filepath.Join(d, "formulas/default.yaml"), []byte(p.ProposedYAML), 0600)
			}
			if boundary == "record" || boundary == "decision" {
				if e := formula.ApplyActivation(d, a); e != nil {
					t.Fatal(e)
				}
			}
			if boundary == "decision" {
				p.Status = "approved"
				p.ReviewedAt = stamp()
				save(d, p)
			}
			{
				if _, e := formula.Load(d, ""); e == nil {
					t.Fatal("incomplete approval executed")
				}
			}
			if p.Status == "pending" {
				if _, e := Review(d, p.ID, "dismiss"); e == nil {
					t.Fatal("applying approval dismissed")
				}
			}
			if _, e := Review(d, p.ID, "approve"); e != nil {
				t.Fatal(e)
			}
			loaded, e := formula.Load(d, "")
			if e != nil || loaded.Approval != a || loaded.Formula.YAML != p.ProposedYAML {
				t.Fatalf("%+v %v", loaded, e)
			}
		})
	}
}

func TestPersistedLearningIntentCannotBeDismissedBeforeApplyingFlag(t *testing.T) {
	d, p := fixture(t)
	cfg, _ := formula.FileHash(filepath.Join(d, "lab.json"))
	a := formula.Approval{FormulaID: p.FormulaID, SHA256: p.ProposedHash, DecisionID: p.ID, SourceKind: "learning-suggestion", SourceID: p.ID}
	if e := formula.BeginActivation(d, p.ProposedYAML, a, cfg, p.OriginalHash); e != nil {
		t.Fatal(e)
	}
	if _, e := Review(d, p.ID, "dismiss"); e == nil {
		t.Fatal("saved approval intent dismissed")
	}
	if _, e := Review(d, p.ID, "approve"); e != nil {
		t.Fatal(e)
	}
}

func TestInvalidSuggestionApprovalBoundaries(t *testing.T) {
	for _, boundary := range []string{"pending", "intent", "yaml", "record", "approved"} {
		t.Run(boundary, func(t *testing.T) {
			d, p := fixture(t)
			p.ProposedYAML = strings.Replace(recipe, "purpose: scope", "purpose: review", 1)
			p.ProposedHash = hash(p.ProposedYAML)
			a := formula.Approval{FormulaID: p.FormulaID, SHA256: p.ProposedHash, DecisionID: p.ID, SourceKind: "learning-suggestion", SourceID: p.ID}
			if boundary != "pending" {
				p.ConfigBefore, _ = formula.FileHash(filepath.Join(d, "lab.json"))
				if e := formula.BeginActivation(d, p.ProposedYAML, a, p.ConfigBefore, p.OriginalHash); e != nil {
					t.Fatal(e)
				}
				if boundary != "intent" {
					p.ApplyingAt = stamp()
				}
				if boundary == "yaml" {
					os.WriteFile(filepath.Join(d, "formulas/default.yaml"), []byte(p.ProposedYAML), 0600)
				}
				if boundary == "record" || boundary == "approved" {
					if e := formula.ApplyActivation(d, a); e != nil {
						t.Fatal(e)
					}
				}
				if boundary == "approved" {
					p.Status = "approved"
				}
			}
			if e := save(d, p); e != nil {
				t.Fatal(e)
			}
			before := map[string]string{}
			filepath.WalkDir(d, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !entry.IsDir() {
					b, e := os.ReadFile(path)
					if e != nil {
						return e
					}
					before[path] = string(b)
				}
				return nil
			})
			r, e := Read(d, p.ID)
			if e != nil || r.ProposedReady || !strings.Contains(r.ReadinessError, "expected type gate") {
				t.Fatalf("inspection: %+v %v", r, e)
			}
			if _, e = Review(d, p.ID, "approve"); e == nil || !strings.Contains(e.Error(), r.ReadinessError) {
				t.Fatalf("approval: %v", e)
			}
			for path, want := range before {
				b, e := os.ReadFile(path)
				if e != nil || string(b) != want {
					t.Fatalf("changed %s: %v", path, e)
				}
			}
			after := 0
			filepath.WalkDir(d, func(_ string, entry os.DirEntry, err error) error {
				if err == nil && !entry.IsDir() {
					after++
				}
				return err
			})
			if after != len(before) {
				t.Fatal("approval created files")
			}
			if boundary == "pending" {
				if _, e = Review(d, p.ID, "dismiss"); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}
func TestSuggestionReadinessVariants(t *testing.T) {
	for _, yaml := range []string{
		strings.Replace(recipe, "  - type: gate\n    purpose: scope\n", "", 1),
		strings.Replace(recipe, "purpose: scope", "purpose: unknown", 1),
		strings.Replace(recipe, "  - type: harness\n  - type: check", "  - type: check\n  - type: harness", 1),
		strings.Replace(recipe, "  - type: document\n", "", 1),
		recipe + "  - type: unsupported\n",
	} {
		d, p := fixture(t)
		p.ProposedYAML = yaml
		p.ProposedHash = hash(yaml)
		save(d, p)
		f, e := formula.Parse([]byte(yaml))
		if e != nil {
			t.Fatal(e)
		}
		reason := f.ValidateReadiness()
		r, e := Read(d, p.ID)
		if e != nil || r.ProposedReady || r.ReadinessError != reason.Error() {
			t.Fatalf("%+v %v", r, e)
		}
		if _, e = Review(d, p.ID, "approve"); e == nil {
			t.Fatal("invalid activated")
		}
	}
}

func TestComparisonViewDoesNotChangeApprovalOrPersistDerivedEvidence(t *testing.T) {
	d, p := manualFixture(t)
	if _, err := LinkComparison(d, p.ID, LinkOptions{Baseline: "mol-before", Candidate: "mol-after", Basis: "Related stopped tasks; no causality claimed"}); err != nil {
		t.Fatal(err)
	}
	got, err := Review(d, p.ID, "approve")
	if err != nil || got.Status != "approved" {
		t.Fatalf("approval: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(d, "suggestions", p.ID+".json"))
	var saved Suggestion
	if json.Unmarshal(raw, &saved) != nil || len(saved.Comparison.Links) != 0 {
		t.Fatal("derived comparisons copied into mutable review record")
	}
	got, err = Read(d, p.ID)
	if err != nil || got.Comparison.Status != "manual" {
		t.Fatalf("approval invalidated historical link: %s %v", got.Comparison.Status, err)
	}
	current, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
	if string(current) != p.ProposedYAML {
		t.Fatal("human approval did not preserve exact bytes")
	}
}

func TestNearLimitReviewRefusesBeforeActivation(t *testing.T) {
	d, p := fixture(t)
	p.Diff = ""
	p.Rationale = ""
	raw, _ := json.MarshalIndent(p, "", "  ")
	p.Rationale = strings.Repeat("x", 8_000_000-len(raw)-2)
	raw, _ = json.MarshalIndent(p, "", "  ")
	if len(raw)+1 > 8_000_000 {
		t.Fatal("fixture exceeds read limit")
	}
	if err := os.WriteFile(filepath.Join(d, "suggestions", p.ID+".json"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
	if _, err := Review(d, p.ID, "approve"); err == nil {
		t.Fatal("near-limit approval accepted")
	}
	a := formula.Approval{FormulaID: p.FormulaID, SHA256: p.ProposedHash, DecisionID: p.ID, SourceKind: "learning-suggestion", SourceID: p.ID}
	started, err := formula.ActivationStarted(d, a)
	if err != nil || started {
		t.Fatalf("activation stranded: %v %v", started, err)
	}
	after, _ := os.ReadFile(filepath.Join(d, "formulas/default.yaml"))
	if string(before) != string(after) {
		t.Fatal("Formula changed")
	}
}

func TestReservedNearLimitRecordRecoversActivation(t *testing.T) {
	d, p := fixture(t)
	p.Rationale = strings.Repeat("x", 7_990_000)
	if err := save(d, p); err != nil {
		t.Fatal(err)
	}
	p.ConfigBefore, _ = formula.FileHash(filepath.Join(d, "lab.json"))
	a := formula.Approval{FormulaID: p.FormulaID, SHA256: p.ProposedHash, DecisionID: p.ID, SourceKind: "learning-suggestion", SourceID: p.ID}
	if err := formula.BeginActivation(d, p.ProposedYAML, a, p.ConfigBefore, p.OriginalHash); err != nil {
		t.Fatal(err)
	}
	p.ApplyingAt = stamp()
	if err := save(d, p); err != nil {
		t.Fatal(err)
	}
	got, err := Review(d, p.ID, "approve")
	if err != nil || got.Status != "approved" {
		t.Fatalf("recovery: %v", err)
	}
	if _, err := Read(d, p.ID); err != nil {
		t.Fatal(err)
	}
}
