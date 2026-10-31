package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
)

const forgePluginFormulaYAML = "schemaVersion: v0\nkind: formula\nid: sample\nintake:\n  source: github-issues\n  repo: acme/widgets\natoms:\n  - id: intake\n    type: intake\n  - id: work\n    type: harness\n"

// forgeLedgerLab writes a Lab with one ledger whose Formula snapshot declares a
// forge plugin when plugin is set, and GitHub otherwise.
func forgeLedgerLab(t *testing.T, id string, plugin bool) string {
	t.Helper()
	lab := t.TempDir()
	yaml := forgePluginFormulaYAML
	if plugin {
		yaml += "plugins:\n  - name: gitlab\n    manifest: plugins/gitlab/plugin.json\n    sha256: " + strings.Repeat("a", 64) + "\nforge:\n  plugin: gitlab\n  project: acme/widgets\n"
	}
	raw, err := json.Marshal(map[string]any{"schemaVersion": "v0", "kind": "molecule", "id": id, "formulaSnapshot": map[string]string{"yaml": yaml, "sha256": strings.Repeat("b", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "ledgers", id+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return lab
}

func runChecks(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), append([]string{"checks"}, args...), strings.NewReader(""), &out, &errb, "test")
	return code, out.String(), errb.String()
}

func TestSplitPluginCheckFlags(t *testing.T) {
	cases := []struct {
		args    []string
		rest    []string
		project string
		ref     string
		set     bool
	}{
		{[]string{"--repo", "o/r", "--pr", "9"}, []string{"--repo", "o/r", "--pr", "9"}, "", "", false},
		{[]string{"--project", "acme/widgets", "--ref", "!7", "--commit", "c"}, []string{"--commit", "c"}, "acme/widgets", "!7", true},
		{[]string{"-project=acme/widgets", "--ref=!7"}, []string{}, "acme/widgets", "!7", true},
		// A value is never read as a flag.
		{[]string{"--required", "--project", "--ref", "!7"}, []string{"--required", "--project"}, "", "!7", true},
		{[]string{"--required=--ref"}, []string{"--required=--ref"}, "", "", false},
		// Scanning stops at the terminator and the first positional argument.
		{[]string{"--", "--project", "p"}, []string{"--", "--project", "p"}, "", "", false},
		{[]string{"extra", "--project", "p"}, []string{"extra", "--project", "p"}, "", "", false},
	}
	for _, c := range cases {
		rest, flags, err := splitPluginCheckFlags(c.args)
		if err != nil || !reflect.DeepEqual(rest, c.rest) || flags.project != c.project || flags.ref != c.ref || flags.set != c.set {
			t.Fatalf("%q: rest %q flags %+v err %v", c.args, rest, flags, err)
		}
	}
	if _, _, err := splitPluginCheckFlags([]string{"--commit", "c", "--ref"}); err == nil || err.Error() != "--ref requires a value" {
		t.Fatalf("missing value: %v", err)
	}
}

func TestChecksRefusesMixedOrMisplacedForgeFlags(t *testing.T) {
	commit := strings.Repeat("a", 40)
	github := forgeLedgerLab(t, "mol-github", false)
	plugin := forgeLedgerLab(t, "mol-plugin", true)
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"mixed", []string{"--lab", plugin, "--molecule", "mol-plugin", "--project", "acme/widgets", "--repo", "acme/widgets", "--ref", "!7", "--commit", commit, "--required", "build"},
			"forgecell: use --project and --ref for a plugin forge, or --repo and --pr for GitHub, not both\n"},
		{"plugin flags on GitHub", []string{"--lab", github, "--molecule", "mol-github", "--project", "acme/widgets", "--ref", "!7", "--commit", commit, "--required", "build"},
			"forgecell: --project and --ref are for a Molecule whose Formula delivers through a forge plugin; this one delivers through GitHub, so use --repo and --pr\n"},
		{"plugin flags without a ledger", []string{"--lab", github, "--molecule", "mol-absent", "--project", "acme/widgets", "--ref", "!7", "--commit", commit, "--required", "build"},
			"forgecell: --project and --ref are for a Molecule whose Formula delivers through a forge plugin; this one delivers through GitHub, so use --repo and --pr\n"},
		{"GitHub flags on a plugin forge", []string{"--lab", plugin, "--molecule", "mol-plugin", "--repo", "acme/widgets", "--pr", "7", "--commit", commit, "--required", "build"},
			"forgecell: this Molecule's Formula delivers through the forge plugin gitlab; use --project and --ref in place of --repo and --pr\n"},
		{"missing value", []string{"--lab", plugin, "--molecule", "mol-plugin", "--ref"}, "forgecell: --ref requires a value\n"},
	}
	for _, c := range cases {
		code, out, errText := runChecks(t, c.args...)
		if code != 1 || out != "" || errText != c.want {
			t.Fatalf("%s: code %d out %q stderr %q", c.name, code, out, errText)
		}
	}
	if _, err := os.Lstat(filepath.Join(plugin, "plugin-runs")); !os.IsNotExist(err) {
		t.Fatalf("a refused checks call prepared a plugin run root: %v", err)
	}
}

func TestChecksUsageTextUnchanged(t *testing.T) {
	code, _, errText := runChecks(t, "--lab", t.TempDir(), "-h")
	if code != 1 || !strings.Contains(errText, "-repo") || strings.Contains(errText, "-project") || strings.Contains(errText, "-ref ") {
		t.Fatalf("usage = %d %q", code, errText)
	}
}

func TestMoleculeForgeFormula(t *testing.T) {
	plugin := forgeLedgerLab(t, "mol-plugin", true)
	if f, ok, err := moleculeForgeFormula(plugin, "mol-plugin"); err != nil || !ok || f.Forge == nil || f.Forge.Plugin != "gitlab" || f.Forge.Project != "acme/widgets" {
		t.Fatalf("plugin ledger = %+v, %t, %v", f.Forge, ok, err)
	}
	github := forgeLedgerLab(t, "mol-github", false)
	writeLedger(t, github, "mol-nosnapshot", `{"schemaVersion":"v0","kind":"molecule","id":"mol-nosnapshot"}`)
	for name, c := range map[string][2]string{"GitHub": {github, "mol-github"}, "absent": {github, "mol-x"}, "no snapshot": {github, "mol-nosnapshot"}, "unsafe id": {plugin, "../ledgers/mol-plugin"}} {
		if _, ok, err := moleculeForgeFormula(c[0], c[1]); ok || err != nil {
			t.Fatalf("%s: selected %t, %v; want GitHub", name, ok, err)
		}
	}
	// A symlinked ledger is never followed, and never taken as GitHub.
	link := filepath.Join(github, "ledgers", "mol-link.json")
	if err := os.Symlink(filepath.Join(plugin, "ledgers", "mol-plugin.json"), link); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := moleculeForgeFormula(github, "mol-link"); ok || err == nil {
		t.Fatalf("a symlinked ledger: %t, %v", ok, err)
	}
	// Present but unusable ledgers are errors, never GitHub.
	for name, c := range forgeLedgerFailures(t) {
		_, ok, err := moleculeForgeFormula(c.lab, "mol-bad")
		if ok || err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), c.lab) {
			t.Fatalf("%s: %t, %v", name, ok, err)
		}
	}
}

func writeLedger(t *testing.T, lab, id, raw string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "ledgers", id+".json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

type forgeLedgerFailure struct{ lab, want string }

// forgeLedgerFailures are Labs whose ledger mol-bad is present but unusable,
// with the plain reason each must give.
func forgeLedgerFailures(t *testing.T) map[string]forgeLedgerFailure {
	t.Helper()
	corrupt := t.TempDir()
	writeLedger(t, corrupt, "mol-bad", `{"formulaSnapshot":{"yaml":`)
	unparsable := t.TempDir()
	snapshot, err := json.Marshal(map[string]any{"id": "mol-bad", "formulaSnapshot": map[string]string{"yaml": "schemaVersion: v0\nkind: formula\nforge: [not, a, block\n"}})
	if err != nil {
		t.Fatal(err)
	}
	writeLedger(t, unparsable, "mol-bad", string(snapshot))
	oversized := t.TempDir()
	writeLedger(t, oversized, "mol-bad", `{"pad":"`+strings.Repeat("x", maxForgeLedgerBytes)+`"}`)
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "ledgers", "mol-bad.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	return map[string]forgeLedgerFailure{
		"corrupt ledger":      {corrupt, "the Molecule ledger is malformed"},
		"unparsable snapshot": {unparsable, "Formula snapshot does not parse"},
		"oversized ledger":    {oversized, "exceeds its size bound"},
		"ledger not a file":   {directory, "the Molecule ledger cannot be read"},
	}
}

// shadowForgeTools puts gh and git on PATH that only record that they ran.
func shadowForgeTools(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	marker := filepath.Join(bin, "ran")
	for _, tool := range []string{"gh", "git"} {
		script := "#!/bin/sh\necho " + tool + " >> '" + marker + "'\nexit 1\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	return marker
}

func TestUnusableLedgerNeverFallsBackToGitHub(t *testing.T) {
	marker := shadowForgeTools(t)
	commit := strings.Repeat("a", 40)
	for name, c := range forgeLedgerFailures(t) {
		if name == "oversized ledger" || name == "ledger not a file" {
			// Covered by TestMoleculeForgeFormula; the CLI paths share it.
			continue
		}
		var out, errb bytes.Buffer
		code := Run(context.Background(), []string{"deliver", "mol-bad", "--lab", c.lab, "--base", "main", "--approve", strings.Repeat("a", 64), "--title", "Fixture", "--", "a.txt"}, strings.NewReader(""), &out, &errb, "test")
		if code != 1 || out.Len() != 0 || !strings.Contains(errb.String(), c.want) || !strings.Contains(errb.String(), "nothing was published") {
			t.Fatalf("deliver, %s: code %d out %q stderr %q", name, code, out.String(), errb.String())
		}
		code, stdout, stderr := runChecks(t, "--lab", c.lab, "--molecule", "mol-bad", "--repo", "acme/widgets", "--pr", "7", "--commit", commit, "--required", "build")
		if code != 1 || stdout != "" || !strings.Contains(stderr, c.want) {
			t.Fatalf("checks, %s: code %d out %q stderr %q", name, code, stdout, stderr)
		}
		code, stdout, stderr = runChecks(t, "--lab", c.lab, "--molecule", "mol-bad", "--project", "acme/widgets", "--ref", "!7", "--commit", commit, "--required", "build")
		if code != 1 || stdout != "" || !strings.Contains(stderr, c.want) {
			t.Fatalf("plugin checks, %s: code %d out %q stderr %q", name, code, stdout, stderr)
		}
		for _, dir := range []string{"deliveries", "ci", "plugin-runs"} {
			if _, err := os.Lstat(filepath.Join(c.lab, dir)); !os.IsNotExist(err) {
				t.Fatalf("%s: %s written: %v", name, dir, err)
			}
		}
	}
	if raw, err := os.ReadFile(marker); !os.IsNotExist(err) {
		t.Fatalf("a GitHub or git call ran: %q, %v", raw, err)
	}
}

func TestPluginForgeWording(t *testing.T) {
	url := "https://gitlab.example/acme/widgets/-/merge_requests/7"
	created := delivery.Receipt{Forge: "gitlab", State: "published", ChangeNoun: "merge request", CreationAttemptedAt: "2026-10-31T00:00:00Z", URL: url, Lifecycle: &delivery.LifecycleObservation{State: "OPEN"}}
	if got := pluginDeliverLine(created); got != "Draft merge request created and verified: "+url {
		t.Fatalf("created line = %q", got)
	}
	existing := created
	existing.CreationAttemptedAt = ""
	if got := pluginDeliverLine(existing); got != "Existing draft merge request verified: "+url {
		t.Fatalf("existing line = %q", got)
	}
	unnamed := existing
	unnamed.ChangeNoun = ""
	if got := pluginDeliverLine(unnamed); got != "Existing draft change request verified: "+url {
		t.Fatalf("default noun line = %q", got)
	}
	// GitHub receipts and unfinished deliveries print no line: GitHub output is unchanged.
	for _, r := range []delivery.Receipt{{State: "published", URL: "https://github.com/owner/repo/pull/9"}, {Forge: "gitlab", State: "committed"}} {
		if got := pluginDeliverLine(r); got != "" {
			t.Fatalf("line for %+v = %q", r, got)
		}
	}
	a := delivery.CIObservation{Identity: delivery.CIIdentity{Ref: "!7"}, Snapshot: checks.Snapshot{Commit: strings.Repeat("a", 40), Status: "passed"}}
	if got := pluginChecksLine("merge request", a); got != "Checks on merge request !7 at "+strings.Repeat("a", 40)+": passed" {
		t.Fatalf("checks line = %q", got)
	}
}
