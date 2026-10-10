package onboarding

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMetaOptInConsent(t *testing.T) {
	root, run := repoFixture(t)
	calls := 0
	runner := func(ctx context.Context, options process.Options) process.Result {
		if options.Argv[0] == "/missing-provider" {
			calls++
			return process.Result{Error: "unexpected model invocation"}
		}
		return run(ctx, options)
	}
	o := InitOptions{Cwd: root, LabDir: t.TempDir(), Launcher: "/cli", Runner: runner, Candidates: []Candidate{{ID: "codex", Executable: "/missing-provider", Readiness: "ready", Capabilities: harness.AdapterCapabilities("codex")}}}
	plain, err := Prepare(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := formula.Parse([]byte(plain.Proposal.YAML))
	if LearnBound(f) {
		t.Fatal("default configured learning")
	}
	again, err := Prepare(context.Background(), o)
	if err != nil || again.Proposal.ID != plain.Proposal.ID {
		t.Fatal("pending replaced", err)
	}
	if _, err = Review(o.LabDir, plain.Proposal.ID, "dismiss"); err != nil {
		t.Fatal(err)
	}
	o.MetaHarness = "codex"
	out, err := Prepare(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	f, err = formula.Parse([]byte(out.Proposal.YAML))
	if err != nil || len(f.Atoms) != 8 || !reflect.DeepEqual(f.Atoms[7].Command, []string{"/cli", "__adapter", "codex", "/missing-provider"}) || f.Atoms[7].TimeoutMS != 900000 {
		t.Fatal(f, err)
	}
	if _, err = os.Stat(filepath.Join(o.LabDir, "lab.json")); !os.IsNotExist(err) {
		t.Fatal("activated before consent")
	}
	p := *out.Proposal
	report := *p.CapabilityEvidence
	report.Meta = nil
	p.CapabilityEvidence = &report
	if err = saveProposal(o.LabDir, p); err != nil {
		t.Fatal(err)
	}
	if _, err = Review(o.LabDir, p.ID, "approve"); err == nil {
		t.Fatal("approved removed metadata")
	}
	if err = saveProposal(o.LabDir, *out.Proposal); err != nil {
		t.Fatal(err)
	}
	if _, err = Review(o.LabDir, p.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	o.MetaHarness = ""
	saved, err := Prepare(context.Background(), o)
	if err != nil || saved.Status != "unchanged" || saved.BeforeYAML != f.YAML {
		t.Fatal("approved changed", err)
	}
	if calls != 0 {
		t.Fatal("model invoked before learning", calls)
	}
	o.Preferred = "codex"
	rebound, err := Prepare(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	rf, _ := formula.Parse([]byte(rebound.Proposal.YAML))
	if !reflect.DeepEqual(f.Atoms[7], rf.Atoms[7]) {
		t.Fatal("meta migrated")
	}
}

func TestMetaOptInUnsupportedAndBYO(t *testing.T) {
	root, run := repoFixture(t)
	for _, state := range []string{"unknown", "unsupported"} {
		c := Candidate{ID: "codex", Executable: "/provider", Readiness: "ready", Capabilities: harness.AdapterCapabilities("codex")}
		c.Capabilities.Meta = harness.Capability{State: state}
		o := InitOptions{Cwd: root, LabDir: t.TempDir(), Launcher: "/cli", Runner: run, Candidates: []Candidate{c}, MetaHarness: "codex"}
		out, err := Prepare(context.Background(), o)
		if err != nil || out.Proposal != nil || out.Meta.Capability.State != state {
			t.Fatal(out, err)
		}
		o.MetaHarness = ""
		o.MetaCommand = []string{"/never-invoke", "arg"}
		out, err = Prepare(context.Background(), o)
		if err != nil || out.Proposal == nil || out.Meta.Capability.State != "unknown" || out.Meta.Containment.State != "unknown" {
			t.Fatal(out, err)
		}
		if _, err = Review(o.LabDir, out.Proposal.ID, "dismiss"); err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(filepath.Join(o.LabDir, "lab.json")); !os.IsNotExist(err) {
			t.Fatal("dismiss activated")
		}
	}
}

func TestMetaOptInExactRecovery(t *testing.T) {
	for _, stage := range []string{"intent", "applied", "decision", "complete"} {
		t.Run(stage, func(t *testing.T) {
			root, run := repoFixture(t)
			o := InitOptions{Cwd: root, LabDir: t.TempDir(), Launcher: "/cli", Runner: run, Candidates: []Candidate{{ID: "codex", Executable: "/provider", Readiness: "ready", Capabilities: harness.AdapterCapabilities("codex")}}, MetaCommand: []string{"/custom-meta"}}
			out, err := Prepare(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			p := *out.Proposal
			f, _ := formula.Parse([]byte(p.YAML))
			approval := formula.Approval{FormulaID: f.ID, SHA256: p.YAMLHash, DecisionID: p.ID, SourceKind: "bootstrap-assay", SourceID: p.ID}
			if err = formula.BeginActivation(o.LabDir, p.YAML, approval, p.ConfigBefore, p.TargetBefore); err != nil {
				t.Fatal(err)
			}
			p.Applying = true
			if stage != "intent" {
				if err = formula.ApplyActivation(o.LabDir, approval); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "decision" || stage == "complete" {
				p.Status = "approved"
			}
			if err = saveProposal(o.LabDir, p); err != nil {
				t.Fatal(err)
			}
			if stage == "complete" {
				if err = formula.CompleteActivation(o.LabDir, approval); err != nil {
					t.Fatal(err)
				}
			}
			if stage != "complete" {
				before, _ := os.ReadFile(filepath.Join(o.LabDir, "assays", p.ID+".json"))
				again, err := Prepare(context.Background(), o)
				if err != nil || again.Proposal == nil || again.Proposal.ID != p.ID {
					t.Fatal("recovery replaced", err)
				}
				after, _ := os.ReadFile(filepath.Join(o.LabDir, "assays", p.ID+".json"))
				if string(before) != string(after) {
					t.Fatal("recovery rewritten")
				}
			}
			cfg, _ := os.ReadFile(filepath.Join(o.LabDir, "lab.json"))
			target, _ := os.ReadFile(filepath.Join(o.LabDir, "formulas", f.ID+".yaml"))
			for _, mutation := range []string{"remove", "command", "version", "before"} {
				tampered := p
				report := *p.CapabilityEvidence
				meta := *report.Meta
				report.Meta = &meta
				tampered.CapabilityEvidence = &report
				switch mutation {
				case "remove":
					report.Meta = nil
				case "command":
					meta.Command = []string{"/other"}
				case "version":
					tampered.CapabilityVersion = "v1"
				case "before":
					meta.BeforeYAML += "# edited\n"
				}
				if err = saveProposal(o.LabDir, tampered); err != nil {
					t.Fatal(err)
				}
				if _, err = Review(o.LabDir, p.ID, "approve"); err == nil {
					t.Fatal("tampered recovery accepted", mutation)
				}
				gotCfg, _ := os.ReadFile(filepath.Join(o.LabDir, "lab.json"))
				gotTarget, _ := os.ReadFile(filepath.Join(o.LabDir, "formulas", f.ID+".yaml"))
				if string(cfg) != string(gotCfg) || string(target) != string(gotTarget) {
					t.Fatal("rejection changed active bytes")
				}
			}
			if err = saveProposal(o.LabDir, p); err != nil {
				t.Fatal(err)
			}
			if _, err = Review(o.LabDir, p.ID, "approve"); err != nil {
				t.Fatal(err)
			}
			loaded, err := formula.Load(o.LabDir, "")
			if err != nil || loaded.Formula.YAML != p.YAML || loaded.Approval != approval || !loaded.Approved {
				t.Fatal("exact approval lost", err)
			}
		})
	}
}

func TestMetaOptInPreservesApprovedCustomCoding(t *testing.T) {
	root, run := repoFixture(t)
	dir := t.TempDir()
	original := "kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: custom, command: [/coding, arg], instructions: keep lesson, timeoutMs: 1200}\natoms:\n - {id: scope, type: gate, purpose: scope, mode: human}\n - {id: work, type: harness}\n - {id: review, type: gate, purpose: review, mode: human}\n"
	p, err := SaveProposal(dir, original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, p.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Runner: run, Candidates: []Candidate{}, MetaCommand: []string{"/meta"}})
	if err != nil || out.Proposal == nil {
		t.Fatal(out, err)
	}
	before, _ := formula.Parse([]byte(original))
	after, _ := formula.Parse([]byte(out.Proposal.YAML))
	if !reflect.DeepEqual(before.Harness, after.Harness) || !reflect.DeepEqual(before.Atoms, after.Atoms[:len(before.Atoms)]) || out.Meta.BeforeYAML != original {
		t.Fatal("coding process changed")
	}
	if _, err = Review(dir, out.Proposal.ID, "dismiss"); err != nil {
		t.Fatal(err)
	}
	loaded, err := formula.Load(dir, "")
	if err != nil || loaded.Formula.YAML != original {
		t.Fatal("dismiss changed active YAML", err)
	}
}

func TestMetaOptInHistoricalPendingReport(t *testing.T) {
	root, run := repoFixture(t)
	dir := t.TempDir()
	p, err := SaveProposal(dir, "kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: byo, command: [/coding]}\natoms:\n - {type: learn, binding: byo, command: [/meta], timeoutMs: 1200}\n")
	if err != nil {
		t.Fatal(err)
	}
	out, err := Prepare(context.Background(), InitOptions{Cwd: root, LabDir: dir, Runner: run, Candidates: []Candidate{}})
	if err != nil || out.Proposal.ID != p.ID || !reflect.DeepEqual(out.Meta.Command, []string{"/meta"}) || out.Meta.Capability.State != "unknown" {
		t.Fatal(out, err)
	}
}
