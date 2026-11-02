package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const (
	findingsCLIID     = "mol-7"
	findingsCLICommit = "0123456789abcdef0123456789abcdef01234567"
)

// findingsCLIReviewers scripts the fake reviewer's findings through its
// Formula config: a high finding at a path, and a low one whose path
// traverses, so it is cleared.
const findingsCLIReviewers = `reviewers:
  - plugin: critic
    autoFix: medium
    config:
      findings:
        status: complete
        findings:
          - id: F-1
            severity: high
            path: src/app.go
            line: 12
            title: "Unchecked ‮error"
            body: "Body"
            resolved: false
          - id: F-2
            severity: low
            path: ../escape.go
            title: Style nit
            body: ""
            resolved: false
`

// findingsCLILab is an approved Lab whose Formula declares the fake reviewer,
// with a published GitHub delivery receipt for findingsCLIID.
func findingsCLILab(t *testing.T) string {
	t.Helper()
	server := plugintest.Server(t)
	lab := pluginListLab(t, "github-issues", findingsCLIReviewers,
		pluginListPin{name: "critic", path: "plugins/critic.json", raw: plugintest.Manifest(t, server, "reviewer", "good")})
	receipt := delivery.Receipt{Preview: delivery.PreviewResult{MoleculeID: findingsCLIID, Repo: "acme/widgets"}, Title: "Fix", State: "published", Commit: findingsCLICommit, URL: "https://github.com/acme/widgets/pull/7"}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(lab, "deliveries"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "deliveries", findingsCLIID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return lab
}

func TestFindingsCollectsAndShowsText(t *testing.T) {
	lab := findingsCLILab(t)
	receipt, _ := os.ReadFile(filepath.Join(lab, "deliveries", findingsCLIID+".json"))

	code, out, errOut := runCLI("findings", findingsCLIID, "--lab", lab)
	if code != 0 || !strings.Contains(out, "No findings collected for mol-7 yet") {
		t.Fatalf("before collecting: exit %d\n%s%s", code, out, errOut)
	}

	code, out, errOut = runCLI("findings", findingsCLIID, "--lab", lab, "--collect")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{
		"Findings on commit " + findingsCLICommit + " (round 1 of 1; untrusted evidence, never an approval):",
		"high   src/app.go:12  Unchecked error  (critic) [autoFix]",
		"low    -  Style nit  (critic)\n",
		"Open: 2 (high 1, medium 0, low 1, info 0); resolved: 0\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "‮") || strings.Contains(out, "escape.go") || strings.Contains(out, lab) || strings.Contains(out, plugintest.PlantedSecret) {
		t.Fatalf("unsafe output:\n%q", out)
	}
	if after, _ := os.ReadFile(filepath.Join(lab, "deliveries", findingsCLIID+".json")); string(after) != string(receipt) {
		t.Fatal("collecting changed the receipt")
	}

	// Without --collect the recorded round is shown, and nothing is started.
	code, again, errOut := runCLI("findings", findingsCLIID, "--lab", lab)
	if code != 0 || again != out {
		t.Fatalf("exit %d\n%s%s", code, again, errOut)
	}

	// --json prints the record.
	code, out, errOut = runCLI("findings", findingsCLIID, "--lab", lab, "--json")
	var record delivery.FindingsRecord
	if code != 0 || json.Unmarshal([]byte(out), &record) != nil {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if record.MoleculeID != findingsCLIID || len(record.Rounds) != 1 || len(record.Rounds[0].Reviewers) != 1 {
		t.Fatalf("record = %+v", record)
	}
	found := record.Rounds[0].Reviewers[0].Findings
	if len(found) != 2 || found[1].Path != "" || found[1].Note == "" || found[0].Fingerprint == "" {
		t.Fatalf("findings = %+v", found)
	}
}

func TestFindingsWithoutReviewers(t *testing.T) {
	lab := pluginListLab(t, "github-issues", "")
	for _, args := range [][]string{{"findings", findingsCLIID, "--lab", lab}, {"findings", findingsCLIID, "--lab", lab, "--collect"}, {"findings", findingsCLIID, "--lab", lab, "--json"}} {
		code, out, errOut := runCLI(args...)
		if code != 0 || out != "this Lab's Formula declares no reviewers\n" || errOut != "" {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
	if _, err := os.Lstat(filepath.Join(lab, "plugin-runs")); !os.IsNotExist(err) {
		t.Fatalf("a Lab without reviewers started a plugin: %v", err)
	}
}

func TestFindingsRefusesBadArguments(t *testing.T) {
	lab := pluginListLab(t, "github-issues", "")
	for _, args := range [][]string{
		{"findings", "--lab", lab},
		{"findings", "../mol-7", "--lab", lab},
		{"findings", findingsCLIID, "extra", "--lab", lab},
	} {
		code, out, errOut := runCLI(args...)
		if code == 0 || !strings.HasPrefix(errOut, "forgecell:") {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errOut)
		}
	}
	// A collection for an unpublished delivery is refused, with no record.
	server := plugintest.Server(t)
	reviewed := pluginListLab(t, "github-issues", "reviewers:\n  - plugin: critic\n    autoFix: high\n",
		pluginListPin{name: "critic", path: "plugins/critic.json", raw: plugintest.Manifest(t, server, "reviewer", "good")})
	code, out, errOut := runCLI("findings", findingsCLIID, "--lab", reviewed, "--collect")
	if code != 1 || !strings.Contains(errOut, "no delivery receipt exists") || strings.Contains(errOut, reviewed) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if _, err := os.Lstat(filepath.Join(reviewed, "deliveries", findingsCLIID+".findings.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused collection wrote a record: %v", err)
	}
}
