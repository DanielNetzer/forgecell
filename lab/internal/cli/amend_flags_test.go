package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestAmendFlagsBuildARevisionAndPrintItsDigest(t *testing.T) {
	m := gsSeedMolecule(t)
	code, out, errOut := gsRun([]string{"amend", m.record.ID, "--lab", m.lab, "--add-scope", "src/new.go", "--reason", "New helper", "--add-acceptance", "Helper exists", "--note", "Resumed after review"})
	if code != 0 {
		t.Fatalf("amend failed: %d %s", code, errOut)
	}
	r := gsLoadRecord(t, m.ledger())
	if len(r.Readiness.Plans) != 2 {
		t.Fatalf("plans %d", len(r.Readiness.Plans))
	}
	latest := r.Readiness.Plans[1]
	if latest.Plan.ParentDigest != m.digest || latest.Plan.OperatorNote != "Resumed after review" {
		t.Fatalf("parent %s note %q", latest.Plan.ParentDigest, latest.Plan.OperatorNote)
	}
	manifest := ""
	for _, e := range latest.Plan.Evidence {
		if e.Path == "package.json" {
			manifest = e.ID
		}
	}
	scope := latest.Plan.Analysis.Scope
	if added := scope[len(scope)-1]; added.Path != "src/new.go" || len(added.Evidence) != 1 || added.Evidence[0] != manifest {
		t.Fatalf("added scope %+v, want evidence %s", added, manifest)
	}
	for _, want := range []string{"Amended " + r.ID + " to revision 2: +1 scope, +1 acceptance, note set\n", "--approve " + latest.Digest + "\n", "Operator note (shown to the coding harness; not acceptance): Resumed after review"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Coding harness attempt") || len(r.HarnessAttempts) != 0 {
		t.Fatal("amend without --run must not start coding")
	}
}

func TestAmendFlagConflictsAreRefused(t *testing.T) {
	m := gsSeedMolecule(t)
	before := gsRead(t, m.ledger())
	plan := filepath.Join(m.root, "plan.json")
	gsWrite(t, plan, "{}", 0600)
	for name, args := range map[string][]string{
		"plan with builder":      {"--parent", m.digest, "--plan", plan, "--note", "x"},
		"plan with from-latest":  {"--parent", m.digest, "--plan", plan, "--from-latest"},
		"parent without plan":    {"--parent", m.digest, "--note", "x"},
		"rebase with builder":    {"--parent", m.digest, "--rebase-formula", "--add-acceptance", "x"},
		"rebase with run":        {"--parent", m.digest, "--rebase-formula", "--run"},
		"other continuation":     {"--continuation", "adopt-failed-tree"},
		"scope without reason":   {"--add-scope", "src/new.go"},
		"reason without scope":   {"--reason", "why"},
		"two scopes, one reason": {"--add-scope", "src/a.go", "--add-scope", "src/b.go", "--reason", "why"},
		"two notes":              {"--note", "a", "--note", "b"},
		"frozen definition":      {"--add-scope", "package.json", "--reason", "Edit the checker"},
	} {
		code, out, errOut := gsRun(append([]string{"amend", m.record.ID, "--lab", m.lab}, args...))
		if code == 0 || strings.Contains(out, "--approve") {
			t.Fatalf("%s: accepted: %s", name, out)
		}
		if name == "frozen definition" && !strings.Contains(errOut, "drop or replace checks defined by in-scope files") {
			t.Fatalf("%s: want the intake message, got %s", name, errOut)
		}
		if !bytes.Equal(before, gsRead(t, m.ledger())) {
			t.Fatalf("%s: a refused amend changed the ledger", name)
		}
	}
}

func TestAmendRunStartsTheNewApproval(t *testing.T) {
	m := gsSeedMolecule(t)
	code, out, errOut := gsRun([]string{"amend", m.record.ID, "--lab", m.lab, "--add-acceptance", "Helper exists", "--run"})
	if code != 0 {
		t.Fatalf("amend --run failed: %d %s\n%s", code, errOut, out)
	}
	r := gsLoadRecord(t, m.ledger())
	latest := r.Readiness.Plans[len(r.Readiness.Plans)-1]
	if len(r.Readiness.Plans) != 2 || len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].PlanDigest != latest.Digest {
		t.Fatalf("the run did not use the new approval: plans=%d attempts=%d", len(r.Readiness.Plans), len(r.HarnessAttempts))
	}
	if r.Workspace.BaseCommit != latest.Plan.Inputs.BaseCommit || r.Verification == nil || !r.Verification.RequiredChecksPassed {
		t.Fatal("the run did not verify the recorded base")
	}
	if !strings.Contains(out, "--approve "+latest.Digest+"\n") || !strings.Contains(out, "Molecule "+r.ID+" · "+r.Status) {
		t.Fatalf("output lacks the digest or the run summary:\n%s", out)
	}
}
