package onboarding

import (
	"context"
	"encoding/json"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"go.yaml.in/yaml/v4"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const recipe = `schemaVersion: v0
kind: formula
id: sample
harness: {binding: codex, command: [/cli, __adapter, codex, /codex]}
atoms: [{id: intake, type: intake}, {id: work, type: harness}, {id: gate, type: gate}]
`

func TestExactProposalApproval(t *testing.T) {
	dir := t.TempDir()
	p, err := SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "lab.json")); !os.IsNotExist(err) {
		t.Fatal("proposal activated Formula")
	}
	got, err := Review(dir, p.ID, "approve")
	if err != nil || got.Status != "approved" {
		t.Fatalf("%+v %v", got, err)
	}
	f, err := formula.Load(dir, "")
	if err != nil || !f.Approved || f.Formula.YAML != recipe {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err = Review(dir, p.ID, "approve"); err != nil {
		t.Fatal("approval retry should be idempotent", err)
	}
}
func TestStaleOrTamperedProposalRefused(t *testing.T) {
	dir := t.TempDir()
	p, err := SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "assays", p.ID+".json")
	raw, _ := os.ReadFile(file)
	os.WriteFile(file, []byte(strings.Replace(string(raw), "binding: codex", "binding: cursor", 1)), 0600)
	if _, err = Review(dir, p.ID, "approve"); err == nil {
		t.Fatal("tampering accepted")
	}
	os.WriteFile(file, raw, 0600)
	os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"newer"}`), 0600)
	if _, err = Review(dir, p.ID, "approve"); err == nil {
		t.Fatal("stale proposal accepted")
	}
}
func TestDismissAndSharedWriteLock(t *testing.T) {
	dir := t.TempDir()
	p, err := SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(dir, ".formula-write.lock"), 0700)
	if _, err = Review(dir, p.ID, "approve"); err == nil {
		t.Fatal("ignored Formula lock")
	}
	os.Remove(filepath.Join(dir, ".formula-write.lock"))
	p, err = Review(dir, p.ID, "dismiss")
	if err != nil || p.Status != "dismissed" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "lab.json")); !os.IsNotExist(err) {
		t.Fatal("dismiss activated Formula")
	}
}

func TestInterruptedReplacementResumesExactApprovedRecipe(t *testing.T) {
	dir := t.TempDir()
	first, err := SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, first.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	replacement := strings.Replace(recipe, "binding: codex", "binding: cursor", 1)
	p, err := SaveProposal(dir, replacement)
	if err != nil {
		t.Fatal(err)
	}
	a := formula.Approval{FormulaID: "sample", SHA256: p.YAMLHash, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
	if err = formula.BeginActivation(dir, p.YAML, a, p.ConfigBefore, p.TargetBefore); err != nil {
		t.Fatal(err)
	}
	p.Applying = true
	if err = saveProposal(dir, p); err != nil {
		t.Fatal(err)
	}
	// Simulate interruption after recipe replacement, before active config/decision.
	os.WriteFile(filepath.Join(dir, "formulas", "sample.yaml"), []byte(replacement), 0600)
	p, err = Review(dir, p.ID, "approve")
	if err != nil || p.Status != "approved" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestBootstrapInterruptedActivationBoundaries(t *testing.T) {
	for _, boundary := range []string{"intent", "yaml", "record", "decision"} {
		t.Run(boundary, func(t *testing.T) {
			dir := t.TempDir()
			frozen, e := formula.Parse([]byte(recipe))
			if e != nil {
				t.Fatal(e)
			}
			report := BindingReport{Command: frozen.Harness.Command, Candidate: Candidate{ID: frozen.Harness.Binding, Capabilities: harness.AdapterCapabilities("codex")}}
			p, e := saveWithEvidence(dir, recipe, nil, &report)
			if e != nil {
				t.Fatal(e)
			}
			a := formula.Approval{FormulaID: "sample", SHA256: p.YAMLHash, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
			if e = formula.BeginActivation(dir, p.YAML, a, p.ConfigBefore, p.TargetBefore); e != nil {
				t.Fatal(e)
			}
			p.Applying = true
			saveProposal(dir, p)
			if boundary == "yaml" {
				os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
				os.WriteFile(filepath.Join(dir, "formulas/sample.yaml"), []byte(p.YAML), 0600)
			}
			if boundary == "record" || boundary == "decision" {
				if e = formula.ApplyActivation(dir, a); e != nil {
					t.Fatal(e)
				}
			}
			if boundary == "decision" {
				p.Status = "approved"
				saveProposal(dir, p)
			}
			f, e := formula.Load(dir, "")
			if e == nil && f.Approved {
				t.Fatal("incomplete approval authorized execution")
			}
			if p.Status == "pending" {
				if _, e = Review(dir, p.ID, "dismiss"); e == nil {
					t.Fatal("applying proposal dismissed")
				}
			}
			original := p
			cfgBefore, _ := os.ReadFile(filepath.Join(dir, "lab.json"))
			yamlBefore, _ := os.ReadFile(filepath.Join(dir, "formulas/sample.yaml"))
			for _, mutation := range []string{"remove", "substitute", "downgrade"} {
				p = original
				switch mutation {
				case "remove":
					p.CapabilityEvidence = nil
				case "substitute":
					changed := *original.CapabilityEvidence
					changed.Command = []string{"different"}
					p.CapabilityEvidence = &changed
				case "downgrade":
					p.CapabilityVersion = ""
				}
				if e = saveProposal(dir, p); e != nil {
					t.Fatal(e)
				}
				if _, e = Review(dir, p.ID, "approve"); e == nil {
					t.Fatalf("accepted %s at %s", mutation, boundary)
				}
				cfgAfter, _ := os.ReadFile(filepath.Join(dir, "lab.json"))
				yamlAfter, _ := os.ReadFile(filepath.Join(dir, "formulas/sample.yaml"))
				if string(cfgBefore) != string(cfgAfter) || string(yamlBefore) != string(yamlAfter) {
					t.Fatal("failed metadata check changed user bytes")
				}
			}
			p = original
			if e = saveProposal(dir, p); e != nil {
				t.Fatal(e)
			}
			if _, e = Review(dir, p.ID, "approve"); e != nil {
				t.Fatal(e)
			}
			f, e = formula.Load(dir, "")
			if e != nil || !f.Approved || f.Approval != a {
				t.Fatalf("%+v %v", f, e)
			}
		})
	}
}
func TestCompletedBootstrapCannotOverwriteNewerActivation(t *testing.T) {
	d := t.TempDir()
	p, e := SaveProposal(d, recipe)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Review(d, p.ID, "approve"); e != nil {
		t.Fatal(e)
	}
	q, e := SaveProposal(d, recipe)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Review(d, q.ID, "approve"); e != nil {
		t.Fatal(e)
	}
	if _, e = Review(d, p.ID, "approve"); e == nil {
		t.Fatal("old decision replaced newer identical bytes")
	}
}

func TestPersistedIntentCannotBeDismissedBeforeProposalApplyingFlag(t *testing.T) {
	d := t.TempDir()
	p, e := SaveProposal(d, recipe)
	if e != nil {
		t.Fatal(e)
	}
	a := formula.Approval{FormulaID: "sample", SHA256: p.YAMLHash, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
	if e = formula.BeginActivation(d, p.YAML, a, p.ConfigBefore, p.TargetBefore); e != nil {
		t.Fatal(e)
	}
	if _, e = Review(d, p.ID, "dismiss"); e == nil {
		t.Fatal("saved approval intent dismissed")
	}
	if _, e = Review(d, p.ID, "approve"); e != nil {
		t.Fatal(e)
	}
}

func TestChangedIDRecoveryRejectsChangedOriginalRecipe(t *testing.T) {
	for _, boundary := range []string{"applying", "legacy-applying", "intent", "yaml", "record", "decision"} {
		for _, mutation := range []string{"edit", "delete", "unchanged"} {
			t.Run(boundary+"/"+mutation, func(t *testing.T) {
				dir := t.TempDir()
				first, err := SaveProposal(dir, recipe)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = Review(dir, first.ID, "approve"); err != nil {
					t.Fatal(err)
				}
				p, err := SaveProposal(dir, strings.Replace(recipe, "id: sample", "id: next", 1))
				if err != nil {
					t.Fatal(err)
				}
				if boundary == "legacy-applying" {
					// Older saved proposals have only the combined baseline fingerprint.
					p.ActiveIDBefore, p.ActiveHashBefore = "", ""
					p.ID = proposalID(p)
				}
				a := formula.Approval{FormulaID: "next", SHA256: p.YAMLHash, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
				if boundary != "applying" && boundary != "legacy-applying" {
					if err = formula.BeginActivation(dir, p.YAML, a, p.ConfigBefore, p.TargetBefore); err != nil {
						t.Fatal(err)
					}
				}
				p.Applying = true
				if boundary == "yaml" {
					if err = os.WriteFile(filepath.Join(dir, "formulas/next.yaml"), []byte(p.YAML), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if boundary == "record" || boundary == "decision" {
					if err = formula.ApplyActivation(dir, a); err != nil {
						t.Fatal(err)
					}
				}
				if boundary == "decision" {
					p.Status = "approved"
				}
				if err = saveProposal(dir, p); err != nil {
					t.Fatal(err)
				}
				cfg, err := os.ReadFile(filepath.Join(dir, "lab.json"))
				if err != nil {
					t.Fatal(err)
				}
				old := filepath.Join(dir, "formulas/sample.yaml")
				switch mutation {
				case "edit":
					err = os.WriteFile(old, []byte(recipe+"# changed\n"), 0600)
				case "delete":
					err = os.Remove(old)
				}
				if err != nil {
					t.Fatal(err)
				}
				_, err = Review(dir, p.ID, "approve")
				if mutation == "unchanged" {
					if err != nil {
						t.Fatal("valid interrupted activation rejected", err)
					}
					if _, err = formula.Load(dir, ""); err != nil {
						t.Fatal(err)
					}
				} else {
					if err == nil {
						t.Error("changed original snapshot accepted")
					}
					after, e := os.ReadFile(filepath.Join(dir, "lab.json"))
					if e != nil || string(after) != string(cfg) {
						t.Fatal("rejected recovery changed configuration", e)
					}
				}
			})
		}
	}
}

func TestDetachedEvidenceIntegrityBeforeEveryApproval(t *testing.T) {
	for _, state := range []string{"pending", "applying", "approved"} {
		for _, mutation := range []string{"missing", "tampered", "symlink", "substituted", "downgrade", "yaml"} {
			t.Run(state+"/"+mutation, func(t *testing.T) {
				root, runner := repoFixture(t)
				dir := t.TempDir()
				out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner})
				if err != nil {
					t.Fatal(err)
				}
				p := *out.Proposal
				if state == "approved" {
					if _, err = Review(dir, p.ID, "approve"); err != nil {
						t.Fatal(err)
					}
				}
				if state == "applying" {
					f, e := formula.Parse([]byte(p.YAML))
					if e != nil {
						t.Fatal(e)
					}
					a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
					if err = formula.BeginActivation(dir, p.YAML, a, p.ConfigBefore, p.TargetBefore); err != nil {
						t.Fatal(err)
					}
					p.Applying = true
					if err = saveProposal(dir, p); err != nil {
						t.Fatal(err)
					}
				}
				cfg, _ := os.ReadFile(filepath.Join(dir, "lab.json"))
				evidence := filepath.Join(dir, p.EvidencePath)
				switch mutation {
				case "missing":
					err = os.Remove(evidence)
				case "tampered":
					err = os.WriteFile(evidence, []byte("{}"), 0600)
				case "symlink":
					raw, e := os.ReadFile(evidence)
					if e != nil {
						t.Fatal(e)
					}
					other := filepath.Join(t.TempDir(), "evidence")
					if e = os.WriteFile(other, raw, 0600); e != nil {
						t.Fatal(e)
					}
					if e = os.Remove(evidence); e != nil {
						t.Fatal(e)
					}
					err = os.Symlink(other, evidence)
				case "substituted":
					raw, e := json.Marshal(Repository{Repo: "other/repo", Revision: "other"})
					if e != nil {
						t.Fatal(e)
					}
					err = os.WriteFile(evidence, raw, 0600)
				case "downgrade":
					p.Version = ""
					p.EvidenceHash = ""
					p.EvidencePath = ""
					err = saveProposal(dir, p)
				case "yaml":
					p.YAML += "# changed\n"
					err = saveProposal(dir, p)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err = InspectProposal(dir, p.ID); err == nil {
					t.Fatal("unverified review")
				}
				if _, err = Review(dir, p.ID, "approve"); err == nil {
					t.Fatal("unverified activation")
				}
				after, _ := os.ReadFile(filepath.Join(dir, "lab.json"))
				if string(cfg) != string(after) {
					t.Fatal("rejection changed config")
				}
			})
		}
	}
}

func TestHistoricalSavedReviewRetainsExactBytesAndStates(t *testing.T) {
	dir := t.TempDir()
	p, err := SaveProposal(dir, recipe)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "assays", p.ID+".json")
	before, _ := os.ReadFile(file)
	got, err := InspectProposal(dir, p.ID)
	if err != nil || got.YAML != recipe || got.ID != p.ID {
		t.Fatalf("%+v %v", got, err)
	}
	after, _ := os.ReadFile(file)
	if string(before) != string(after) {
		t.Fatal("review rewrote legacy")
	}
	if _, err = Review(dir, p.ID, "dismiss"); err != nil {
		t.Fatal(err)
	}
	got, err = InspectProposal(dir, p.ID)
	if err != nil || got.Status != "dismissed" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestPartialEvidencePersistenceDoesNotExposeProposal(t *testing.T) {
	root, runner := repoFixture(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assays"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assays/evidence"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Launcher: "/cli", Candidates: []Candidate{{ID: "codex", Executable: "/codex", Readiness: "ready"}}, Runner: runner})
	if err == nil {
		t.Fatal("partial persistence accepted")
	}
	entries, e := os.ReadDir(filepath.Join(dir, "assays"))
	if e != nil {
		t.Fatal(e)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "assay-") {
			t.Fatal("exposed incomplete proposal")
		}
	}
}

func TestEvidenceValidationRejectsUnsafeAndMismatchedInput(t *testing.T) {
	root, runner := repoFixture(t)
	r, err := ReadRepository(context.Background(), root, runner)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"path", "hash", "revision", "oversize"} {
		t.Run(mutation, func(t *testing.T) {
			copy := r
			copy.Evidence = append([]readiness.FileEvidence(nil), r.Evidence...)
			switch mutation {
			case "path":
				copy.Evidence[0].Path = "../escape"
			case "hash":
				copy.Evidence[0].SHA256 = strings.Repeat("0", 64)
			case "revision":
				copy.Revision = "other"
			case "oversize":
				copy.Evidence[0].Content = strings.Repeat("x", 12_000_001)
				copy.Evidence[0].SHA256 = hash([]byte(copy.Evidence[0].Content))
			}
			dir := t.TempDir()
			text, err := yaml.Marshal(map[string]any{"kind": "formula", "id": "test", "intake": map[string]any{"repo": r.Repo}, "repositoryContext": r.Compact(), "atoms": []map[string]any{{"type": "gate"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = saveWithEvidence(dir, string(text), &copy); err == nil {
				t.Fatal("invalid evidence persisted")
			}
			files, _ := filepath.Glob(filepath.Join(dir, "assays", "assay-*.json"))
			if len(files) != 0 {
				t.Fatal("invalid proposal exposed")
			}
		})
	}
}

func TestSavedCapabilityIntegrity(t *testing.T) {
	dir := t.TempDir()
	f, err := formula.Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	report := BindingReport{Command: f.Harness.Command, Candidate: Candidate{ID: "codex", Capabilities: harness.AdapterCapabilities("codex")}, LearnBound: false}
	p, err := saveWithEvidence(dir, recipe, nil, &report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = InspectProposal(dir, p.ID); err != nil {
		t.Fatal(err)
	}
	original := p
	for _, mutation := range []string{"remove", "substitute", "version"} {
		p = original
		switch mutation {
		case "remove":
			p.CapabilityEvidence = nil
		case "substitute":
			copyReport := report
			copyReport.Command = []string{"different"}
			p.CapabilityEvidence = &copyReport
		case "version":
			p.CapabilityVersion = ""
		}
		if err = saveProposal(dir, p); err != nil {
			t.Fatal(err)
		}
		if _, err = InspectProposal(dir, p.ID); err == nil {
			t.Fatalf("accepted %s", mutation)
		}
		if _, err = Review(dir, p.ID, "approve"); err == nil {
			t.Fatalf("approved %s", mutation)
		}
		if _, err = os.Stat(filepath.Join(dir, "lab.json")); !os.IsNotExist(err) {
			t.Fatal("activated tampered metadata")
		}
	}
}

func TestSavedCapabilitiesRefusedOnApprovedRetry(t *testing.T) {
	dir := t.TempDir()
	f, _ := formula.Parse([]byte(recipe))
	report := BindingReport{Command: f.Harness.Command, Candidate: Candidate{ID: f.Harness.Binding, Capabilities: harness.AdapterCapabilities("codex")}}
	p, err := saveWithEvidence(dir, recipe, nil, &report)
	if err != nil {
		t.Fatal(err)
	}
	p, err = Review(dir, p.ID, "approve")
	if err != nil {
		t.Fatal(err)
	}
	config, _ := os.ReadFile(filepath.Join(dir, "lab.json"))
	active, _ := formula.Load(dir, "")
	p.CapabilityEvidence = nil
	if err = saveProposal(dir, p); err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, p.ID, "approve"); err == nil {
		t.Fatal("approved retry accepted removal")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "lab.json"))
	loaded, _ := formula.Load(dir, "")
	if string(config) != string(after) || active.Formula.YAML != loaded.Formula.YAML {
		t.Fatal("changed active bytes")
	}
}
