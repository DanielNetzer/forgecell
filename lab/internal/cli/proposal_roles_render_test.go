package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
)

func TestReviewShowsSavedPerRoleCapabilityEvidence(t *testing.T) {
	dir := t.TempDir()
	yaml := "kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: cursor, command: [/forgecell, __adapter, cursor, /cursor]}\natoms: [{type: gate}]\n"
	p, err := onboarding.SaveProposal(dir, yaml)
	if err != nil {
		t.Fatal(err)
	}
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	c := onboarding.Candidate{ID: "cursor", Readiness: "ready", Detail: "Authenticated CLI probe; model access unverified.", Capabilities: harness.AdapterCapabilities("cursor")}
	p.CapabilityEvidence = &onboarding.BindingReport{Candidate: c, Command: f.Harness.Command, Workflow: c.WorkflowCapability(), Roles: onboarding.RoleCapabilities(f)}
	t.Setenv("PATH", t.TempDir())

	var out, stderr bytes.Buffer
	if code := renderProposal(&out, &stderr, p, dir, false); code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	for _, want := range []string{
		"Role capability analysis (default): cursor adapter support unsupported",
		harness.AdapterCapabilities("cursor").Analysis.Reason,
		"Role capability coding (default): cursor adapter support supported",
		"[saved observation; not current verification]",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := renderProposal(&out, &stderr, p, dir, true); code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	var view map[string]json.RawMessage
	if err = json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	var evidence onboarding.BindingReport
	if err = json.Unmarshal(view["capabilityEvidence"], &evidence); err != nil {
		t.Fatal(err)
	}
	var source string
	if err = json.Unmarshal(view["capabilityEvidenceSource"], &source); err != nil {
		t.Fatal(err)
	}
	roles := evidence.Roles
	if len(roles) != 2 || roles[0].Role != "analysis" || roles[0].Capability.State != "unsupported" || roles[1].Role != "coding" || source != "saved observation; not current verification" {
		t.Fatalf("%+v %q", roles, source)
	}
}

func TestReviewOfAHistoricalProposalShowsNoRoleEvidence(t *testing.T) {
	dir := t.TempDir()
	p, err := onboarding.SaveProposal(dir, "kind: formula\nid: sample\nintake: {repo: acme/widgets}\nharness: {binding: codex}\natoms: [{type: gate}]\n")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	var out, stderr bytes.Buffer
	if code := renderProposal(&out, &stderr, p, dir, false); code != 0 {
		t.Fatalf("%d %s", code, stderr.String())
	}
	if strings.Contains(out.String(), "Role capability") {
		t.Fatalf("a proposal saved without role evidence must not invent it:\n%s", out.String())
	}
}
