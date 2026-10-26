package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

const (
	authCLIToken = "sk-ant-oat01-FAKEAUTHTOKEN0123456789abcdef"
	authCLIHome  = "/Users/forgecell-fake-home/.claude"
)

func assertNoCLIAuthLeak(t *testing.T, surface, text string) {
	t.Helper()
	for _, secret := range []string{authCLIToken, authCLIHome, "forgecell-fake-home", "OAuth session expired"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaks %q:\n%s", surface, secret, text)
		}
	}
}

// authBlockedRecord is a Molecule whose intake stopped on a classified analysis
// authentication failure, as the engine records it.
func authBlockedRecord() molecule.Record {
	hint := harness.NotAuthenticatedHint("claude-code")
	return molecule.Record{
		SchemaVersion: "v0", Kind: "molecule", ID: "mol-auth", FormulaID: "f", Status: "blocked", Mode: "ticket",
		Issue:            molecule.Issue{Number: 7, Repo: "acme/widgets", Title: "Change code"},
		Atoms:            []molecule.Atom{{ID: "intake", Type: "intake", Status: "blocked", Detail: hint}},
		AnalysisAttempts: []molecule.AnalysisAttempt{{Ordinal: 1, Status: "failed", Error: hint, FailureClass: harness.HarnessNotAuthenticated}},
		Notes:            []string{hint},
	}
}

func TestHarnessNotAuthenticatedRunOutcomeAndLedgerNameTheHint(t *testing.T) {
	want := harness.NotAuthenticatedHint("claude-code")
	r := authBlockedRecord()
	var out bytes.Buffer
	presentRun(&out, r)
	if !strings.Contains(out.String(), "Outcome: "+want+" (failureClass harness-not-authenticated)") {
		t.Fatalf("outcome text:\n%s", out.String())
	}
	assertNoCLIAuthLeak(t, "outcome", out.String())

	dir := t.TempDir()
	r.LabDir = dir
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(dir, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "ledgers", "mol-auth.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"ledger", "mol-auth", "--lab", dir}, strings.NewReader(""), &stdout, &stderr, "dev"); code != 0 {
		t.Fatalf("ledger exit %d: %s", code, stderr.String())
	}
	if !bytes.Equal(stdout.Bytes(), append(raw, '\n')) || !strings.Contains(stdout.String(), `"failureClass":"harness-not-authenticated"`) {
		t.Fatalf("ledger stdout must stay the exact ledger: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("ledger stderr lacks the hint: %q", stderr.String())
	}
	assertNoCLIAuthLeak(t, "ledger", stdout.String()+stderr.String())
}

func TestHarnessNotAuthenticatedLoopReportNamesTheStop(t *testing.T) {
	want := harness.NotAuthenticatedHint("claude-code")
	report := molecule.LoopReport{Repository: "acme/widgets", Passes: []molecule.LoopPass{{
		Items: []molecule.LoopItem{
			{Repository: "acme/widgets", Issue: 11, MoleculeID: "mol-11", Started: true, State: molecule.LoopStopped, Reason: "intake: " + want, Failed: true, FailureClass: harness.HarnessNotAuthenticated},
			{Repository: "acme/widgets", Issue: 12, State: molecule.LoopNotDispatched, Reason: want + "; the loop dispatched nothing further in this pass"},
		},
		Stopped: want + "; the loop dispatched no further Issue in this pass and starts no further pass",
	}}}
	var out bytes.Buffer
	renderLoop(&out, report, intake.Selector{}, 1)
	if !strings.Contains(out.String(), "Pass 1 stopped: "+want) || !strings.Contains(out.String(), "acme/widgets#12 · not-dispatched · Molecule none") {
		t.Fatalf("loop report:\n%s", out.String())
	}
	// The Molecule the pass started failed; the undispatched Issue is not non-progress.
	if code := loopExitCode(report); code != 2 {
		t.Fatalf("exit code %d", code)
	}
	assertNoCLIAuthLeak(t, "loop report", out.String())
}

func TestHarnessNotAuthenticatedDoctorReportsTheBoundHarness(t *testing.T) {
	bin, lab := doctorFixture(t)
	logged := strings.Replace(claudeTool, `echo '{"loggedIn":LOGGED}'`, `echo '{"loggedIn":false,"token":"`+authCLIToken+`","configDir":"`+authCLIHome+`"}'`, 1)
	provider := writeTool(t, bin, "claude", logged)
	launcherDir := t.TempDir()
	launcher := writeTool(t, launcherDir, "forgecell", "#!/bin/sh\nexit 0\n")
	os.WriteFile(filepath.Join(launcherDir, "manifest.json"), []byte(`{"version":"0.2.0-preview.1","sha256":"x"}`), 0600)
	saveBinding(t, lab, "claude-code", []string{launcher, "__adapter", "claude-code", provider})
	want := harness.NotAuthenticatedHint("claude-code")

	_, out := runDoctor(t, lab)
	report := decodeDoctor(t, out)
	saved := report["savedBinding"].(map[string]any)
	if saved["authHint"] != want || saved["authentication"].(map[string]any)["state"] != "login-required" {
		t.Fatalf("saved binding: %v", saved)
	}
	assertNoCLIAuthLeak(t, "doctor JSON", out)

	_, text := runDoctor(t, lab, "--verbose")
	if !strings.Contains(text, "Saved binding: claude-code") || !strings.Contains(text, "  "+want+"\n") {
		t.Fatalf("doctor text:\n%s", text)
	}
	assertNoCLIAuthLeak(t, "doctor text", text)
}

func TestHarnessNotAuthenticatedDoctorSaysNothingWhenLoggedIn(t *testing.T) {
	bin, lab := doctorFixture(t)
	writeTool(t, bin, "claude", strings.Replace(claudeTool, "LOGGED", "true", 1))
	_, out := runDoctor(t, lab)
	if strings.Contains(out, "authHint") || strings.Contains(out, "not authenticated") {
		t.Fatalf("logged-in harness reported unauthenticated: %s", out)
	}
}
