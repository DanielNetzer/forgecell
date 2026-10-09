package evaluation

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func approved(t *testing.T) []byte {
	t.Helper()
	base := "schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: owner/repo}\nharness:\n  command: [test-harness]\natoms: [{id: gate, type: gate}]\n"
	candidate := strings.Replace(base, "  command:", "  instructions: Report actual outcomes separately.\n  command:", 1)
	raw, err := json.Marshal(Approval{ID: "suggestion-fixture", Status: "approved", ReviewedAt: "2026-09-28T05:45:48Z", OriginalYAML: base, ProposedYAML: candidate, OriginalHash: Hash([]byte(base)), ProposedHash: Hash([]byte(candidate))})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestApprovedProcessOnlyPair(t *testing.T) {
	p, err := ValidateApproval(approved(t))
	if err != nil || p.Baseline.Harness.Instructions != "" || p.Candidate.Harness.Instructions == "" {
		t.Fatalf("%+v %v", p, err)
	}
}
func TestRejectUnapprovedOrChangedExecution(t *testing.T) {
	for _, kind := range []string{"pending", "no decision", "tamper", "binding", "gate", "no change"} {
		t.Run(kind, func(t *testing.T) {
			var a Approval
			json.Unmarshal(approved(t), &a)
			switch kind {
			case "pending":
				a.Status = "pending"
			case "no decision":
				a.ReviewedAt = ""
			case "tamper":
				a.ProposedHash = strings.Repeat("a", 64)
			case "binding":
				a.ProposedYAML = strings.Replace(a.ProposedYAML, "test-harness", "other-harness", 1)
				a.ProposedHash = Hash([]byte(a.ProposedYAML))
			case "gate":
				a.ProposedYAML = strings.Replace(a.ProposedYAML, "type: gate", "type: ship", 1)
				a.ProposedHash = Hash([]byte(a.ProposedYAML))
			case "no change":
				a.ProposedYAML = a.OriginalYAML
				a.ProposedHash = a.OriginalHash
			}
			raw, _ := json.Marshal(a)
			if _, err := ValidateApproval(raw); err == nil {
				t.Fatal("invalid process evaluation accepted")
			}
		})
	}
}

// This fixed synthetic fixture is test data, not a historical human approval.
func TestFrozenSyntheticApprovedPair(t *testing.T) {
	const syntheticApprovalJSON = `{
  "id": "synthetic-approved-pair-test",
  "status": "approved",
  "reviewedAt": "2000-01-01T00:00:00Z",
  "originalYaml": "schemaVersion: v0\nkind: formula\nid: synthetic-approval-test\nintake: {source: github-issues, repo: example/synthetic-test}\nharness:\n  command: [synthetic-test-harness]\n  timeoutMs: 1000\natoms: [{id: synthetic-gate, type: gate}]\n",
  "proposedYaml": "schemaVersion: v0\nkind: formula\nid: synthetic-approval-test\nintake: {source: github-issues, repo: example/synthetic-test}\nharness:\n  instructions: Report synthetic test outcomes separately.\n  command: [synthetic-test-harness]\n  timeoutMs: 1000\natoms: [{id: synthetic-gate, type: gate}]\n",
  "originalHash": "c6947439744abd7385358202fae428043dcf397e446c9f0e09a0dd02ab051e3b",
  "proposedHash": "b0981a805a00e3de40b9a0f566e1cd3b42cb961f899dd7e8a36fed1bc65c248a"
}`
	pair, err := ValidateApproval([]byte(syntheticApprovalJSON))
	if err != nil {
		t.Fatal(err)
	}
	if pair.Approval.ID != "synthetic-approved-pair-test" {
		t.Fatal("synthetic approval identity changed")
	}
	if pair.Approval.OriginalHash != "c6947439744abd7385358202fae428043dcf397e446c9f0e09a0dd02ab051e3b" || pair.Baseline.SHA256 != "c6947439744abd7385358202fae428043dcf397e446c9f0e09a0dd02ab051e3b" {
		t.Fatal("synthetic baseline hash changed")
	}
	if pair.Approval.ProposedHash != "b0981a805a00e3de40b9a0f566e1cd3b42cb961f899dd7e8a36fed1bc65c248a" || pair.Candidate.SHA256 != "b0981a805a00e3de40b9a0f566e1cd3b42cb961f899dd7e8a36fed1bc65c248a" {
		t.Fatal("synthetic candidate hash changed")
	}
	if pair.Baseline.Harness.Instructions != "" || pair.Candidate.Harness.Instructions != "Report synthetic test outcomes separately." {
		t.Fatal("synthetic process instruction changed")
	}
	baseline, candidate := pair.Baseline, pair.Candidate
	candidate.Harness.Instructions = baseline.Harness.Instructions
	candidate.YAML, candidate.SHA256 = baseline.YAML, baseline.SHA256
	if !reflect.DeepEqual(baseline, candidate) {
		t.Fatal("synthetic pair differs beyond harness instructions")
	}
}
