package onboarding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

func probedReport(t *testing.T) BindingReport {
	t.Helper()
	f, err := formula.Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{ID: f.Harness.Binding, Readiness: "blocked", Detail: "probe failed",
		Failures: []Step{{Step: "version", Status: "failed", Reason: "exited 1", Argv: []string{"/Users/me/bin/codex", "--version"}}},
		Binding:  &BindingDiagnostics{FormulaBinding: "codex", Adapter: "codex", Launcher: "/Users/me/bin/forgecell", Provider: "/Users/me/bin/codex"}}
	return BindingReport{Command: f.Harness.Command, Candidate: candidate, LearnBound: false}
}

func TestProposalOmitsProbeDetailFromNewProposals(t *testing.T) {
	dir := t.TempDir()
	report := probedReport(t)
	p, err := saveWithEvidence(dir, recipe, nil, &report)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "assays", p.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	candidate := saved["capabilityEvidence"].(map[string]any)["candidate"].(map[string]any)
	for _, key := range []string{"binding", "failures"} {
		if _, present := candidate[key]; present {
			t.Fatalf("a new saved proposal must not carry candidate.%s: %s", key, raw)
		}
	}
	if strings.Contains(string(raw), "/Users/me/") {
		t.Fatalf("a local path reached the saved proposal: %s", raw)
	}
	if report.Candidate.Binding == nil || len(report.Candidate.Failures) != 1 {
		t.Fatal("saving must not strip the caller's report, which init still prints")
	}
	if p.CapabilityEvidence.Candidate.Binding != nil || p.CapabilityEvidence.Candidate.Failures != nil {
		t.Fatal("the returned proposal must match what was saved")
	}
	if inspected, err := InspectProposal(dir, p.ID); err != nil || inspected.ID != p.ID {
		t.Fatalf("a new proposal must verify its identity: %v", err)
	}
}

func TestProposalOmitsProbeDetailWhenMetaCarriesIt(t *testing.T) {
	report := probedReport(t)
	meta := MetaReport{Command: []string{"/cli"}, Binding: "x", Candidate: report.Candidate}
	report.Meta = &meta
	cleaned := withoutProbeDetail(report)
	if cleaned.Meta.Candidate.Binding != nil || cleaned.Meta.Candidate.Failures != nil || cleaned.Candidate.Binding != nil {
		t.Fatalf("meta.candidate must be cleaned too: %+v", cleaned.Meta.Candidate)
	}
	if meta.Candidate.Binding == nil || report.Meta.Candidate.Binding == nil {
		t.Fatal("the caller's meta report was mutated")
	}
}

func TestProposalOmitsProbeDetailKeepsHistoricalIdentity(t *testing.T) {
	dir := t.TempDir()
	report := probedReport(t)
	report.Candidate = report.Candidate.CompleteUnknowns()
	report.Workflow = report.Candidate.WorkflowCapability()
	f, err := formula.Parse([]byte(recipe))
	if err != nil {
		t.Fatal(err)
	}
	report.Roles = RoleCapabilities(f)
	historical := Proposal{ConfigBefore: "absent", YAML: recipe, YAMLHash: f.SHA256, Baseline: "b", TargetBefore: "absent", CreatedAt: "2026-01-01T00:00:00Z", Status: "pending", CapabilityVersion: "v1", CapabilityEvidence: &report}
	historical.ID = proposalID(historical)
	if err = saveProposal(dir, historical); err != nil {
		t.Fatal(err)
	}
	got, err := InspectProposal(dir, historical.ID)
	if err != nil {
		t.Fatalf("a historical proposal that carries binding and failures must still verify: %v", err)
	}
	if got.CapabilityEvidence.Candidate.Binding == nil || len(got.CapabilityEvidence.Candidate.Failures) != 1 {
		t.Fatalf("the historical fields must still decode: %+v", got.CapabilityEvidence.Candidate)
	}
	if proposalID(got) != historical.ID {
		t.Fatal("re-marshalling a historical proposal changed its identity")
	}
}

func TestProposalOmitsProbeDetailDoctorViewKeepsFields(t *testing.T) {
	c := probedReport(t).Candidate
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"binding":`) || !strings.Contains(string(raw), `"failures":`) {
		t.Fatalf("the Candidate JSON shape used by doctor must keep binding and failures: %s", raw)
	}
	empty, _ := json.Marshal(Candidate{ID: "x", Readiness: "ready"})
	if strings.Contains(string(empty), "binding") || strings.Contains(string(empty), "failures") {
		t.Fatalf("empty fields must stay omitted: %s", empty)
	}
}
