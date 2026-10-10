package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type labsJSON struct {
	ReadOnly  bool `json:"readOnly"`
	Truncated bool `json:"truncated"`
	Labs      []struct {
		Directory       string `json:"directory"`
		Repo            string `json:"repo"`
		ActiveFormulaID string `json:"activeFormulaId"`
		Binding         string `json:"binding"`
		LastMolecule    *struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			StartedAt string `json:"startedAt"`
		} `json:"lastMolecule"`
		StatusCounts      map[string]int `json:"statusCounts"`
		InvalidRecords    int            `json:"invalidRecords"`
		OrderingUncertain bool           `json:"orderingUncertain"`
		Truncated         bool           `json:"truncated"`
		Error             string         `json:"error"`
	} `json:"labs"`
}

func labsList(t *testing.T) labsJSON {
	t.Helper()
	code, out, stderr := identityRun("labs", "--json")
	if code != 0 {
		t.Fatalf("labs --json: code %d, out %q, err %q", code, out, stderr)
	}
	var report labsJSON
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("labs --json output %q: %v", out, err)
	}
	return report
}

func labsLedger(t *testing.T, lab, id, status, startedAt string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	started := ""
	if startedAt != "" {
		started = fmt.Sprintf(`"startedAt":%q,`, startedAt)
	}
	raw := fmt.Sprintf(`{"schemaVersion":"v0","kind":"molecule","id":%q,"formulaId":"f","status":%q,%s"issue":{"repo":"o/r","number":1},"trace":"RAW-TRACE-DO-NOT-PRINT"}`, id, status, started)
	if err := os.WriteFile(filepath.Join(lab, "ledgers", id+".json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLabsCommandWithoutStorageIsAnEmptyListNotAnError(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	report := labsList(t)
	if !report.ReadOnly || report.Labs == nil || len(report.Labs) != 0 {
		t.Fatalf("expected an empty list, got %+v", report)
	}
	code, out, stderr := identityRun("labs")
	if code != 0 || !strings.Contains(out, "No Labs") {
		t.Fatalf("text mode: code %d, out %q, err %q", code, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".forgecell")); !os.IsNotExist(err) {
		t.Fatal("labs created storage")
	}
	if err := os.MkdirAll(filepath.Join(home, ".forgecell", "labs"), 0700); err != nil {
		t.Fatal(err)
	}
	if report = labsList(t); len(report.Labs) != 0 {
		t.Fatalf("empty labs directory listed %+v", report.Labs)
	}
}

func TestLabsCommandListsLegacyAndIdentityLabsWithCountsAndLastMolecule(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	labs := filepath.Join(home, ".forgecell", "labs")
	legacy := filepath.Join(labs, "forgecell-core-0123456789abcdef")
	identity := filepath.Join(labs, "owner-repo")
	empty := filepath.Join(labs, "aaa-empty")
	identityLab(t, legacy, "Owner/Repo")
	identityLab(t, identity, "other/project")
	if err := os.MkdirAll(empty, 0700); err != nil {
		t.Fatal(err)
	}
	labsLedger(t, legacy, "mol-a", "passed", "2026-01-01T00:00:00Z")
	labsLedger(t, legacy, "mol-b", "waiting", "2026-03-01T00:00:00Z")
	labsLedger(t, legacy, "mol-c", "waiting", "2026-02-01T00:00:00Z")
	labsLedger(t, legacy, "mol-d", "blocked", "2026-03-01T00:00:00.5Z")
	os.WriteFile(filepath.Join(legacy, "ledgers", "mol-bad.json"), []byte(`{"schemaVersion":"v0"}`), 0600)
	os.WriteFile(filepath.Join(legacy, "ledgers", "mol-mismatch.json"), []byte(`{"schemaVersion":"v0","kind":"molecule","id":"mol-other","formulaId":"f","status":"passed"}`), 0600)
	os.WriteFile(filepath.Join(legacy, "ledgers", "notes.txt"), []byte("ignored"), 0600)
	os.WriteFile(filepath.Join(labs, "stray-file"), []byte("not a Lab"), 0600)
	before := identityTree(t, labs)

	report := labsList(t)
	if identityTree(t, labs) != before {
		t.Fatal("labs modified the Lab tree")
	}
	var names []string
	for _, lab := range report.Labs {
		names = append(names, filepath.Base(lab.Directory))
	}
	if strings.Join(names, ",") != "aaa-empty,forgecell-core-0123456789abcdef,owner-repo" {
		t.Fatalf("Labs must be every directory, sorted by name: %v", names)
	}
	l := report.Labs[1]
	if l.Directory != legacy || l.Repo != "Owner/Repo" || l.ActiveFormulaID != "f" || l.Binding != "codex" {
		t.Fatalf("legacy Lab identity: %+v", l)
	}
	if l.LastMolecule == nil || l.LastMolecule.ID != "mol-d" || l.LastMolecule.Status != "blocked" || l.LastMolecule.StartedAt != "2026-03-01T00:00:00.5Z" {
		t.Fatalf("last Molecule: %+v", l.LastMolecule)
	}
	if got := fmt.Sprint(l.StatusCounts); got != "map[blocked:1 passed:1 waiting:2]" || l.InvalidRecords != 2 || l.OrderingUncertain || l.Truncated {
		t.Fatalf("counts %s, invalid %d, uncertain %t, truncated %t", got, l.InvalidRecords, l.OrderingUncertain, l.Truncated)
	}
	if o := report.Labs[2]; o.Repo != "other/project" || o.LastMolecule != nil || len(o.StatusCounts) != 0 {
		t.Fatalf("identity Lab without ledgers: %+v", o)
	}
	if e := report.Labs[0]; e.Repo != "" || e.Error != "" || e.LastMolecule != nil {
		t.Fatalf("a Lab without a Formula is listed without error: %+v", e)
	}

	code, out, stderr := identityRun("labs")
	for _, want := range []string{legacy, identity, "Owner/Repo", "mol-d", "blocked=1", "waiting=2", "passed=1"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("text output missing %q: code %d, out %q, err %q", want, code, out, stderr)
		}
	}
}

func TestLabsCommandTieBreaksTheLastMoleculeByID(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	lab := filepath.Join(home, ".forgecell", "labs", "tie")
	labsLedger(t, lab, "mol-z", "passed", "2026-03-01T00:00:00Z")
	labsLedger(t, lab, "mol-a", "waiting", "2026-03-01T00:00:00Z")
	if got := labsList(t).Labs[0].LastMolecule; got == nil || got.ID != "mol-a" {
		t.Fatalf("ties should resolve by id: %+v", got)
	}
}

func TestLabsCommandDoesNotGuessLastMoleculeWhenOrderingIsUncertain(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	lab := filepath.Join(home, ".forgecell", "labs", "uncertain")
	labsLedger(t, lab, "mol-a", "passed", "2026-03-01T00:00:00Z")
	labsLedger(t, lab, "mol-b", "waiting", "")
	l := labsList(t).Labs[0]
	if l.LastMolecule != nil || !l.OrderingUncertain || l.StatusCounts["waiting"] != 1 || l.StatusCounts["passed"] != 1 {
		t.Fatalf("expected counts but no guessed last Molecule: %+v", l)
	}
}

func TestLabsCommandReportsTruncatedLedgerScans(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	lab := filepath.Join(home, ".forgecell", "labs", "busy")
	for _, id := range []string{"mol-1", "mol-2", "mol-3"} {
		labsLedger(t, lab, id, "passed", "2026-03-01T00:00:00Z")
	}
	previous := maxLabLedgerFiles
	maxLabLedgerFiles = 2
	t.Cleanup(func() { maxLabLedgerFiles = previous })
	report := labsList(t)
	l := report.Labs[0]
	if !l.Truncated || !report.Truncated || l.StatusCounts["passed"] != 2 {
		t.Fatalf("truncation was not reported: %+v", report)
	}
	if _, out, _ := identityRun("labs"); !strings.Contains(out, "truncated") {
		t.Fatalf("text mode hides truncation: %q", out)
	}
}

func TestLabsCommandKeepsListingWhenOneLabIsCorruptOrSymlinked(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	labs := filepath.Join(home, ".forgecell", "labs")
	identityLab(t, filepath.Join(labs, "good"), "owner/repo")
	if err := os.MkdirAll(filepath.Join(labs, "broken"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(labs, "broken", "lab.json"), []byte("{"), 0600)
	if err := os.MkdirAll(filepath.Join(labs, "nomissing", "x"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(labs, "nomissing", "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"gone"}`), 0600)
	target := filepath.Join(t.TempDir(), "elsewhere")
	identityLab(t, target, "owner/repo")
	if err := os.Symlink(target, filepath.Join(labs, "linked")); err != nil {
		t.Fatal(err)
	}
	repos, errs := map[string]string{}, map[string]string{}
	for _, lab := range labsList(t).Labs {
		repos[filepath.Base(lab.Directory)], errs[filepath.Base(lab.Directory)] = lab.Repo, lab.Error
	}
	if len(repos) != 4 {
		t.Fatalf("every directory entry must be listed: %v", repos)
	}
	if repos["good"] != "owner/repo" || errs["good"] != "" {
		t.Fatalf("good Lab: %q %q", repos["good"], errs["good"])
	}
	for _, name := range []string{"broken", "nomissing", "linked"} {
		if repos[name] != "" || errs[name] == "" {
			t.Fatalf("%s should be listed with an error and no repository: %q %q", name, repos[name], errs[name])
		}
	}
}

func TestLabsCommandNeverPrintsHarnessCommandsOrLedgerBodies(t *testing.T) {
	home := labIdentityEnv(t)
	identityFakeCheckout(t)
	lab := filepath.Join(home, ".forgecell", "labs", "secretive")
	if err := os.MkdirAll(filepath.Join(lab, "formulas"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f","currentApproval":{"formulaId":"f","sha256":"SECRET-APPROVAL"}}`), 0600)
	recipe := "kind: formula\nid: f\nintake: {repo: owner/repo}\nharness: {binding: codex, command: [run-model, --token, SECRET-ARGV], instructions: SECRET-INSTRUCTIONS}\natoms: [{type: harness}]\n"
	os.WriteFile(filepath.Join(lab, "formulas", "f.yaml"), []byte(recipe), 0600)
	labsLedger(t, lab, "mol-a", "passed", "2026-03-01T00:00:00Z")
	for _, args := range [][]string{{"labs"}, {"labs", "--json"}} {
		code, out, stderr := identityRun(args...)
		if code != 0 || !strings.Contains(out, "codex") {
			t.Fatalf("%v: code %d, out %q, err %q", args, code, out, stderr)
		}
		for _, secret := range []string{"SECRET-ARGV", "run-model", "SECRET-INSTRUCTIONS", "SECRET-APPROVAL", "RAW-TRACE-DO-NOT-PRINT"} {
			if strings.Contains(out+stderr, secret) {
				t.Fatalf("%v leaked %q: %q", args, secret, out)
			}
		}
	}
}

func TestLabsCommandHonoursForgecellHomeAndRejectsExtraArguments(t *testing.T) {
	labIdentityEnv(t)
	identityFakeCheckout(t)
	custom := filepath.Join(t.TempDir(), "custom-home")
	t.Setenv("FORGECELL_HOME", custom)
	identityLab(t, filepath.Join(custom, "labs", "custom"), "owner/repo")
	if report := labsList(t); len(report.Labs) != 1 || filepath.Base(report.Labs[0].Directory) != "custom" {
		t.Fatalf("FORGECELL_HOME ignored: %+v", report)
	}
	for _, args := range [][]string{{"labs", "extra"}, {"labs", "--lab", custom}, {"labs", "--bogus"}} {
		if code, _, _ := identityRun(args...); code != 1 {
			t.Fatalf("%v accepted", args)
		}
	}
}
