package onboarding

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
)

// approveText approves the exact text as the active Formula.
func approveText(t *testing.T, dir, text string) {
	t.Helper()
	p, err := SaveProposal(dir, text)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Review(dir, p.ID, "approve"); err != nil {
		t.Fatal(err)
	}
}

func TestModelPinProposalChangesOnlyModelLinesOfAHandAuthoredFormula(t *testing.T) {
	root, runner, dir := claudeLab(t, allClaudeRoles)
	active, err := formula.Inspect(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	text := "# Reviewed by hand; keep this header.\n" + active.Formula.YAML
	text = strings.Replace(text, "binding: claude-code", "binding: &b claude-code", 1)
	text = strings.ReplaceAll(text, "binding: claude-code", "binding: *b")
	text = strings.Replace(text, "\nharness:\n", "\n# the default harness; keep this comment\nharness:\n", 1)
	for _, want := range []string{"# Reviewed by hand", "&b claude-code", "binding: *b", "# the default harness"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the fixture lacks %q:\n%s", want, text)
		}
	}
	approveText(t, dir, text)

	out, err := pinModel(root, runner, dir, pinnedModel, "")
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	if out.BeforeYAML != text {
		t.Fatal("the before YAML is not the approved text")
	}
	added, kept := 0, []string{}
	for _, line := range strings.Split(out.Proposal.YAML, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "model: "+pinnedModel) {
			added++
			continue
		}
		kept = append(kept, line)
	}
	if added != 5 {
		t.Fatalf("%d model lines were added, want 5:\n%s", added, out.Proposal.YAML)
	}
	if strings.Join(kept, "\n") != text {
		t.Fatalf("lines other than model lines changed:\n%s\n---\n%s", out.Proposal.YAML, text)
	}

	if _, err = Review(dir, out.Proposal.ID, "approve"); err != nil {
		t.Fatal(err)
	}
	removal, err := pinModel(root, runner, dir, "", "")
	if err != nil || removal.Proposal == nil || removal.Proposal.YAML != text {
		t.Fatalf("removing the pin did not restore the approved text: %v", err)
	}
}

func TestModelPinProposalPersistsItsBeforeYAML(t *testing.T) {
	root, runner, dir := claudeLab(t, nil)
	active, err := formula.Inspect(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pinModel(root, runner, dir, pinnedModel, "")
	if err != nil || out.Proposal == nil {
		t.Fatalf("%+v %v", out, err)
	}
	saved, err := InspectProposal(dir, out.Proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CapabilityEvidence == nil || saved.CapabilityEvidence.BeforeYAML != active.Formula.YAML {
		t.Fatalf("the saved proposal does not carry the approved YAML: %+v", saved.CapabilityEvidence)
	}
	tampered := saved
	evidence := *saved.CapabilityEvidence
	evidence.BeforeYAML += "# tampered\n"
	tampered.CapabilityEvidence = &evidence
	f, err := formula.Parse([]byte(saved.YAML))
	if err != nil {
		t.Fatal(err)
	}
	if err = validateMetaReport(f, tampered); err == nil || !strings.Contains(err.Error(), "original YAML mismatch") {
		t.Fatalf("a before YAML that is not the approved Formula was accepted: %v", err)
	}
}

func TestModelPinLeavesHistoricalProposalIdentityUnchanged(t *testing.T) {
	_, _, dir := claudeLab(t, nil)
	entries, err := filepath.Glob(filepath.Join(dir, "assays", "assay-*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("%v %v", entries, err)
	}
	data, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "beforeYaml") {
		t.Fatalf("a proposal without a before YAML serialises the key:\n%s", data)
	}
	var p Proposal
	if err = json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if proposalID(p) != p.ID {
		t.Fatal("the proposal identity moved")
	}
	if _, err = InspectProposal(dir, p.ID); err != nil {
		t.Fatalf("a saved proposal without beforeYaml no longer inspects: %v", err)
	}
}
