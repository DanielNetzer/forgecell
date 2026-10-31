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

// selectFormula parses a Formula whose intake.source is source; with a pin it
// declares the fixture's manifest as the plugin "jira".
func selectFormula(t *testing.T, source, pin string) formula.Formula {
	t.Helper()
	yaml := "schemaVersion: v0\nkind: formula\nid: sample\nintake:\n  source: " + source + "\n  repo: acme/widgets\natoms:\n  - id: intake\n    type: intake\n  - id: work\n    type: harness\n"
	if pin != "" {
		yaml += "plugins:\n  - name: jira\n    manifest: " + manifestRel + "\n    sha256: " + pin + "\n"
	}
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTrackerForKeepsGitHubIssues(t *testing.T) {
	lab := plugintest.TempDir(t)
	for _, f := range []formula.Formula{selectFormula(t, "github-issues", ""), {}} {
		tr, ok, err := TrackerFor(lab, f, Options{})
		if tr != nil || ok || err != nil {
			t.Fatalf("TrackerFor = %v, %t, %v", tr, ok, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(lab, RunRootName)); !os.IsNotExist(err) {
		t.Fatalf("the GitHub path made a run root: %v", err)
	}
	// A source that names no declared plugin is refused, never taken as GitHub.
	if tr, ok, err := TrackerFor(lab, formula.Formula{Intake: formula.Intake{Source: "jira"}}, Options{}); tr != nil || ok || err == nil {
		t.Fatalf("an undeclared source: %v, %t, %v", tr, ok, err)
	}
}

func TestTrackerForServesTheDeclaredPlugin(t *testing.T) {
	f := newFixture(t, "tracker", "good")
	form := selectFormula(t, "jira", f.opts.Ref.SHA256)
	var records []CallRecord
	opts := Options{LookupEnv: f.opts.LookupEnv, Observer: func(r CallRecord) { records = append(records, r) }}
	tr, ok, err := TrackerFor(f.lab, form, opts)
	if err != nil || !ok || tr == nil {
		t.Fatalf("TrackerFor = %v, %t, %v", tr, ok, err)
	}
	d, err := tr.Describe(context.Background())
	if err != nil || d.DisplayName != "Fake tracker" || d.KeyPattern != "^FAKE-[0-9]+$" {
		t.Fatalf("Describe = %+v, %v", d, err)
	}
	if len(records) != 1 || records[0].Tool != "tracker_describe" || records[0].Plugin != "fake-tracker" {
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

	// intake.config is the {config} of every call: the fake fails with the
	// reason it names, at the first call.
	form.Intake.Config = map[string]any{"fail": "auth-missing"}
	tr, _, err = TrackerFor(f.lab, form, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Get(context.Background(), "FAKE-1"); mcp.ReasonOf(err) != mcp.AuthMissing {
		t.Fatalf("config did not reach the call: %v", err)
	}

	// An intake.config over the adapter's bound is refused before any start.
	form.Intake.Config = map[string]any{"jql": strings.Repeat("x", MaxConfigBytes)}
	records = nil
	if tr, ok, err := TrackerFor(f.lab, form, opts); tr != nil || !ok || mcp.ReasonOf(err) != mcp.SchemaInvalid {
		t.Fatalf("an oversized config: %v, %t, %v", tr, ok, err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %+v", records)
	}
}

func TestTrackerForRefusesAnUnsafeRunRoot(t *testing.T) {
	f := newFixture(t, "tracker", "good")
	form := selectFormula(t, "jira", f.opts.Ref.SHA256)
	root := filepath.Join(f.lab, RunRootName)
	outside := plugintest.TempDir(t)
	refused := func(name, lab string) {
		t.Helper()
		tr, _, err := TrackerFor(lab, form, Options{})
		if tr != nil || err == nil || mcp.ReasonOf(err) != mcp.Failed {
			t.Fatalf("%s: TrackerFor = %v, %v", name, tr, err)
		}
		if strings.Contains(err.Error(), f.lab) || strings.Contains(err.Error(), outside) {
			t.Fatalf("%s: the error names a path: %q", name, err)
		}
	}

	// A run root that is a symlink, even to a private directory outside the Lab.
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	refused("symlink", f.lab)
	if left, _ := os.ReadDir(outside); len(left) != 0 {
		t.Fatalf("the symlink target was used: %d entries", len(left))
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}

	// An existing run root open to others is refused, never repaired.
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	refused("open mode", f.lab)
	if info, _ := os.Lstat(root); info.Mode().Perm() != 0o755 {
		t.Fatalf("the run root mode was changed to %v", info.Mode().Perm())
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}

	// A run root that is a file.
	if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused("file", f.lab)
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}

	// A Lab given as a relative path, or through a symlink.
	refused("relative Lab", "lab")
	link := filepath.Join(outside, "lab-link")
	if err := os.Symlink(f.lab, link); err != nil {
		t.Fatal(err)
	}
	refused("symlinked Lab", link)
	if _, err := os.Lstat(root); !os.IsNotExist(err) {
		t.Fatalf("a refused Lab got a run root: %v", err)
	}
}

func TestDescribeReviewer(t *testing.T) {
	f := newFixture(t, "reviewer", "good")
	d, err := DescribeReviewer(context.Background(), f.opts, map[string]any{"mode": "strict"})
	if err != nil || d.DisplayName != "Fake reviewer" {
		t.Fatalf("DescribeReviewer = %+v, %v", d, err)
	}
	if len(f.records) != 1 || f.records[0].Tool != "reviewer_describe" || f.records[0].Reason != "" {
		t.Fatalf("records = %+v", f.records)
	}
	f.records = nil
	if _, err := DescribeReviewer(context.Background(), f.opts, []string{"a"}); mcp.ReasonOf(err) != mcp.SchemaInvalid || len(f.records) != 0 {
		t.Fatalf("a non-object config: %v, %+v", err, f.records)
	}
	f.requireCleaned(t)

	// A tracker is not a reviewer.
	tracker := newFixture(t, "tracker", "good")
	_, err = DescribeReviewer(context.Background(), tracker.opts, nil)
	tracker.requireReason(t, err, mcp.ContractMismatch, "plugin fake-tracker load: contract-mismatch")
}
