package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func TestAcceptProtectedFailuresPrintsTheDeltasAndTheNewDigestWithoutApproving(t *testing.T) {
	parent := gatePlan()
	parent.Revision = 1
	next := gatePlan(gateDelta())
	next.Revision, next.Continuation = 2, "checks-only"
	r := molecule.Record{ID: "mol-1", Status: "waiting", Readiness: &readiness.State{Phase: "scope-waiting", Plans: []readiness.Proposal{{Plan: parent, Digest: "digest-1"}, {Plan: next, Digest: "digest-2"}}}}
	var out, errOut bytes.Buffer
	if code := presentAmend(context.Background(), &out, &errOut, filepath.Join(t.TempDir(), "lab"), r, false); code != 0 {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
	for _, want := range []string{"Amended mol-1 to revision 2: expected protected failures: 1, continuation: checks-only\n", "--approve digest-2\n", "Continuation: checks only", "EXPECTED PROTECTED-TEST FAILURES — approving this exact digest accepts these failures in the protected regression run:", gatePackage + " " + gateTest + " · check go-test-lab · justified by scope path " + gateFixture} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestAcceptProtectedFailuresFlagRefusesWithoutAProtectedFailure(t *testing.T) {
	m := gsSeedMolecule(t)
	before := gsRead(t, m.ledger())
	plan := filepath.Join(m.root, "plan.json")
	gsWrite(t, plan, "{}", 0600)
	for name, args := range map[string][]string{
		"no verification": {"--accept-protected-failures"},
		"with --run":      {"--accept-protected-failures", "--run"},
		"with add-scope":  {"--accept-protected-failures", "--add-scope", "src/new.go", "--reason", "New"},
		"with --plan":     {"--accept-protected-failures", "--parent", m.digest, "--plan", plan},
		"with rebase":     {"--accept-protected-failures", "--parent", m.digest, "--rebase-formula"},
	} {
		code, out, errOut := gsRun(append([]string{"amend", m.record.ID, "--lab", m.lab}, args...))
		if code == 0 || strings.Contains(out, "--approve") {
			t.Fatalf("%s: accepted: %s", name, out)
		}
		if strings.Contains(errOut, m.lab) || strings.Contains(out, m.lab) {
			t.Fatalf("%s: output names the local Lab directory: %s", name, errOut)
		}
		if !bytes.Equal(before, gsRead(t, m.ledger())) {
			t.Fatalf("%s: a refused amend changed the ledger", name)
		}
	}
	if _, _, errOut := gsRun([]string{"amend", m.record.ID, "--lab", m.lab, "--accept-protected-failures"}); !strings.Contains(errOut, "--accept-protected-failures refused") {
		t.Fatalf("refusal not named: %s", errOut)
	}
}

func TestAcceptProtectedFailuresRefusesRunAndNamesTheApprovalCommand(t *testing.T) {
	m := gsSeedMolecule(t)
	before := gsRead(t, m.ledger())
	code, out, errOut := gsRun([]string{"amend", m.record.ID, "--lab", m.lab, "--accept-protected-failures", "--run"})
	if code == 0 || out != "" || !strings.Contains(errOut, "takes no --run") || !strings.Contains(errOut, "forgecell run <issue> --approve <digest>") {
		t.Fatalf("code %d out %q err %q", code, out, errOut)
	}
	if !bytes.Equal(before, gsRead(t, m.ledger())) {
		t.Fatal("a refused amend changed the ledger")
	}
}

func TestAcceptProtectedFailuresHelpLine(t *testing.T) {
	code, out, _ := gsRun([]string{"--help"})
	if code != 0 || !strings.Contains(out, "amend <molecule> --accept-protected-failures [--add-acceptance TEXT]... [--note TEXT] [--lab DIR]  (") || !strings.Contains(out, "takes no --run") {
		t.Fatalf("help lacks the --accept-protected-failures line:\n%s", out)
	}
}
