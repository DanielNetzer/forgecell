package mcpadapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

// forgeFormula parses a Formula whose forge block names the fixture's
// manifest, declared as the plugin "gitlab", with project.
func forgeFormula(t *testing.T, pin, project string) formula.Formula {
	t.Helper()
	yaml := "schemaVersion: v0\nkind: formula\nid: sample\nintake:\n  source: github-issues\n  repo: acme/widgets\natoms:\n  - id: intake\n    type: intake\n  - id: work\n    type: harness\n" +
		"plugins:\n  - name: gitlab\n    manifest: " + manifestRel + "\n    sha256: " + pin + "\nforge:\n  plugin: gitlab\n  project: " + project + "\n"
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestForgeForKeepsGitHub(t *testing.T) {
	lab := plugintest.TempDir(t)
	for _, f := range []formula.Formula{selectFormula(t, "github-issues", ""), {}} {
		fg, ok, err := ForgeFor(lab, f, Options{})
		if fg != nil || ok || err != nil {
			t.Fatalf("ForgeFor = %v, %t, %v", fg, ok, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(lab, RunRootName)); !os.IsNotExist(err) {
		t.Fatalf("the GitHub path made a run root: %v", err)
	}
	// A forge block that names no declared plugin is refused, never taken as GitHub.
	undeclared := formula.Formula{Forge: &formula.ForgeRef{Plugin: "gitlab", Project: "acme/widgets"}}
	if fg, ok, err := ForgeFor(lab, undeclared, Options{}); fg != nil || ok || mcp.ReasonOf(err) != mcp.Failed {
		t.Fatalf("an undeclared forge plugin: %v, %t, %v", fg, ok, err)
	}
	if _, err := os.Lstat(filepath.Join(lab, RunRootName)); !os.IsNotExist(err) {
		t.Fatalf("a refused forge made a run root: %v", err)
	}
}

func TestForgeForServesTheDeclaredPlugin(t *testing.T) {
	f := newFixture(t, "forge", "good")
	form := forgeFormula(t, f.opts.Ref.SHA256, "acme/widgets")
	var records []CallRecord
	opts := Options{LookupEnv: f.opts.LookupEnv, Observer: func(r CallRecord) { records = append(records, r) }}
	fg, ok, err := ForgeFor(f.lab, form, opts)
	if err != nil || !ok || fg == nil {
		t.Fatalf("ForgeFor = %v, %t, %v", fg, ok, err)
	}
	d, err := fg.Describe(context.Background())
	if err != nil || d.Host != "forge.example" || d.ChangeNoun != "change request" {
		t.Fatalf("Describe = %+v, %v", d, err)
	}
	// {project} comes from forge.project: the call's arguments are exactly it.
	if len(records) != 1 || records[0].Tool != "forge_describe" || records[0].Plugin != "fake-forge" || records[0].ArgsSHA256 != digest([]byte(`{"project":"acme/widgets"}`)) {
		t.Fatalf("records = %+v", records)
	}
	root := filepath.Join(f.lab, RunRootName)
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("run root = %v, %v", info, err)
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Fatalf("Shutdown left %d run directories", len(left))
	}
	if _, err := fg.Policy(context.Background(), "main"); err != nil || len(records) != 2 || records[1].ArgsSHA256 != digest([]byte(`{"branch":"main","project":"acme/widgets"}`)) {
		t.Fatalf("Policy: %v, records %+v", err, records)
	}

	// A project the adapter cannot send is refused before any start.
	records = nil
	long := forgeFormula(t, f.opts.Ref.SHA256, strings.Repeat("p", maxProjectRunes+1))
	if fg, ok, err := ForgeFor(f.lab, long, opts); fg != nil || !ok || mcp.ReasonOf(err) != mcp.SchemaInvalid || len(records) != 0 {
		t.Fatalf("an oversized project: %v, %t, %v, %+v", fg, ok, err, records)
	}
}

func TestForgeForRefusesAnUnsafeLab(t *testing.T) {
	f := newFixture(t, "forge", "good")
	form := forgeFormula(t, f.opts.Ref.SHA256, "acme/widgets")
	outside := plugintest.TempDir(t)
	link := filepath.Join(outside, "lab-link")
	if err := os.Symlink(f.lab, link); err != nil {
		t.Fatal(err)
	}
	for name, lab := range map[string]string{"relative": "lab", "symlinked": link} {
		fg, _, err := ForgeFor(lab, form, Options{})
		if fg != nil || mcp.ReasonOf(err) != mcp.Failed || strings.Contains(err.Error(), f.lab) || strings.Contains(err.Error(), outside) {
			t.Fatalf("%s Lab: %v, %v", name, fg, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.lab, RunRootName)); !os.IsNotExist(err) {
		t.Fatalf("a refused Lab got a run root: %v", err)
	}
}
