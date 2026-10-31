package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const pluginListFormulaHead = `schemaVersion: v0
kind: formula
id: sample
intake:
  source: %s
  repo: acme/widgets
atoms:
  - id: intake
    type: intake
  - id: work
    type: harness
`

// pluginListPin is one plugins entry; the manifest bytes are written into the
// Lab at its path, and SHA256 is pinned (the bytes' own digest when empty).
type pluginListPin struct {
	name, path string
	raw        []byte
	sha256     string
}

// pluginListLab writes the pinned manifests and a Formula with the given tail
// (forge and reviewers keys) into a fresh Lab, and approves it exactly.
func pluginListLab(t *testing.T, source, tail string, pins ...pluginListPin) string {
	t.Helper()
	lab := plugintest.TempDir(t)
	yaml := fmt.Sprintf(pluginListFormulaHead, source)
	if len(pins) > 0 {
		yaml += "plugins:\n"
	}
	for _, p := range pins {
		digest := p.sha256
		if p.raw != nil {
			path := filepath.Join(lab, filepath.FromSlash(p.path))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, p.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if digest == "" {
				sum := sha256.Sum256(p.raw)
				digest = hex.EncodeToString(sum[:])
			}
		}
		yaml += fmt.Sprintf("  - name: %s\n    manifest: %s\n    sha256: %s\n", p.name, p.path, digest)
	}
	yaml += tail
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	a := formula.Approval{FormulaID: f.ID, SHA256: f.SHA256, DecisionID: "plugin-list", SourceKind: "test-review", SourceID: "plugin-list"}
	if err = formula.BeginActivation(lab, yaml, a, "absent", "absent"); err != nil {
		t.Fatal(err)
	}
	if err = formula.ApplyActivation(lab, a); err != nil {
		t.Fatal(err)
	}
	if err = formula.CompleteActivation(lab, a); err != nil {
		t.Fatal(err)
	}
	return lab
}

func pluginListJSON(t *testing.T, lab string) (int, []pluginListRow) {
	t.Helper()
	code, out, errOut := runCLI("plugin", "list", "--lab", lab, "--json")
	var got struct {
		Plugins []pluginListRow `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Plugins == nil {
		t.Fatalf("exit %d, %v\n%s%s", code, err, out, errOut)
	}
	return code, got.Plugins
}

func contractID(t *testing.T, kind string) string {
	t.Helper()
	c, ok := contract.Lookup(kind)
	if !ok {
		t.Fatalf("no contract for %s", kind)
	}
	return c.ID
}

func TestPluginList(t *testing.T) {
	// One build of the fake server serves every case; nothing is started.
	server := plugintest.Server(t)
	manifest := func(kind, mode string) []byte { return plugintest.Manifest(t, server, kind, mode) }

	t.Run("ok", func(t *testing.T) {
		lab := pluginListLab(t, "jira", "forge:\n  plugin: hub\n  project: acme/widgets\nreviewers:\n  - plugin: critic\n    autoFix: low\n",
			pluginListPin{name: "jira", path: "plugins/jira/plugin.json", raw: manifest("tracker", "good")},
			pluginListPin{name: "hub", path: "plugins/hub.json", raw: manifest("forge", "good")},
			pluginListPin{name: "critic", path: "plugins/critic.json", raw: manifest("reviewer", "good")},
			pluginListPin{name: "spare", path: "plugins/spare.json", raw: manifest("forge", "good")})
		code, out, errOut := runCLI("plugin", "list", "--lab", lab)
		if code != 0 || errOut != "" {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 5 || strings.Fields(lines[0])[0] != "NAME" {
			t.Fatalf("want a header and four rows:\n%s", out)
		}
		for i, want := range [][]string{
			{"jira", "tracker", contractID(t, "tracker"), "tracker", "ok"},
			{"hub", "forge", contractID(t, "forge"), "forge", "ok"},
			{"critic", "reviewer", contractID(t, "reviewer"), "reviewer", "ok"},
			{"spare", "forge", contractID(t, "forge"), "unused", "ok"},
		} {
			if got := strings.Fields(lines[i+1]); !reflect.DeepEqual(got, want) {
				t.Errorf("row %d: %v, want %v", i, got, want)
			}
		}
		if strings.Contains(out, lab) || strings.Contains(out, filepath.Dir(server.Path)) || strings.Contains(out, "plugin.json") {
			t.Fatalf("the list leaks a local path:\n%s", out)
		}
		if left, _ := os.ReadDir(filepath.Join(lab, "plugins")); len(left) != 4 {
			t.Fatalf("plugin list changed the Lab: %d entries", len(left))
		}
	})

	t.Run("json", func(t *testing.T) {
		lab := pluginListLab(t, "jira", "",
			pluginListPin{name: "jira", path: "plugins/jira.json", raw: manifest("tracker", "good")})
		code, rows := pluginListJSON(t, lab)
		want := []pluginListRow{{Name: "jira", Kind: "tracker", Contract: contractID(t, "tracker"), Role: "tracker", Status: "ok"}}
		if code != 0 || !reflect.DeepEqual(rows, want) {
			t.Fatalf("exit %d, rows %+v", code, rows)
		}
	})

	t.Run("not ok", func(t *testing.T) {
		good := manifest("forge", "good")
		changed := manifest("forge", "crash")
		goodSum := sha256.Sum256(good)
		lab := pluginListLab(t, "both", "forge:\n  plugin: wrong\n  project: acme/widgets\nreviewers:\n  - plugin: both\n    autoFix: none\n",
			pluginListPin{name: "absent", path: "plugins/absent.json", sha256: strings.Repeat("a", 64)},
			pluginListPin{name: "edited", path: "plugins/edited.json", raw: changed, sha256: hex.EncodeToString(goodSum[:])},
			pluginListPin{name: "wrong", path: "plugins/wrong.json", raw: manifest("reviewer", "good")},
			pluginListPin{name: "both", path: "plugins/both.json", raw: manifest("tracker", "good")},
			pluginListPin{name: "broken", path: "plugins/broken.json", raw: []byte("{")},
			pluginListPin{name: "cached", path: "extensions/x/" + strings.Repeat("b", 64) + "/plugin.json", sha256: strings.Repeat("c", 64)})
		code, rows := pluginListJSON(t, lab)
		want := []pluginListRow{
			{Name: "absent", Role: "unused", Status: "not-installed"},
			{Name: "edited", Role: "unused", Status: "manifest-changed"},
			{Name: "wrong", Kind: "reviewer", Contract: contractID(t, "reviewer"), Role: "forge", Status: "kind-mismatch"},
			{Name: "both", Kind: "tracker", Contract: contractID(t, "tracker"), Role: "tracker,reviewer", Status: "kind-mismatch"},
			{Name: "broken", Role: "unused", Status: "invalid"},
			{Name: "cached", Role: "unused", Status: "failed"},
		}
		if code != 2 || !reflect.DeepEqual(rows, want) {
			t.Fatalf("exit %d, rows\n%+v\nwant\n%+v", code, rows, want)
		}
		code, out, _ := runCLI("plugin", "list", "--lab", lab)
		if code != 2 || !strings.Contains(out, "not-installed") || strings.Contains(out, lab) {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("a changed server binary", func(t *testing.T) {
		lab := pluginListLab(t, "github-issues", "",
			pluginListPin{name: "hub", path: "plugins/hub.json", raw: manifest("forge", "good")})
		if err := os.WriteFile(server.Path, []byte("tampered"), 0o700); err != nil {
			t.Fatal(err)
		}
		code, rows := pluginListJSON(t, lab)
		if code != 2 || len(rows) != 1 || rows[0].Status != "manifest-changed" {
			t.Fatalf("exit %d, rows %+v", code, rows)
		}
	})
}

func TestPluginListWithoutPlugins(t *testing.T) {
	lab := pluginListLab(t, "github-issues", "")
	code, out, errOut := runCLI("plugin", "list", "--lab", lab)
	if code != 0 || !strings.Contains(out, "pins no plugins") || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	code, rows := pluginListJSON(t, lab)
	if code != 0 || len(rows) != 0 {
		t.Fatalf("exit %d, rows %+v", code, rows)
	}
	// FORGECELL_LAB resolves the Lab as every other command does.
	t.Setenv("FORGECELL_LAB", lab)
	if code, out, errOut = runCLI("plugin", "list"); code != 0 || !strings.Contains(out, "pins no plugins") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestPluginListRefusals(t *testing.T) {
	// A Lab without an approved Formula, and one whose Formula is only saved.
	empty := plugintest.TempDir(t)
	unapproved := plugintest.TempDir(t)
	if err := os.MkdirAll(filepath.Join(unapproved, "formulas"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unapproved, "formulas", "sample.yaml"), []byte(fmt.Sprintf(pluginListFormulaHead, "github-issues")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unapproved, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"sample"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, lab := range []string{empty, unapproved, filepath.Join(empty, "missing")} {
		code, out, errOut := runCLI("plugin", "list", "--lab", lab)
		if code != 1 || out != "" || !strings.Contains(errOut, "needs a Lab with an active approved Formula") || strings.Contains(errOut, pluginUsage) || strings.Contains(errOut, lab) {
			t.Errorf("exit %d\n%s%s", code, out, errOut)
		}
	}

	// An active Formula that is invalid, and a Lab configuration that is not
	// JSON, could not be loaded: that is not the same as having none.
	invalid := plugintest.TempDir(t)
	if err := os.MkdirAll(filepath.Join(invalid, "formulas"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalid, "formulas", "sample.yaml"), []byte(fmt.Sprintf(pluginListFormulaHead, "undeclared")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalid, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"sample"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	garbled := plugintest.TempDir(t)
	if err := os.WriteFile(filepath.Join(garbled, "lab.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, lab := range []string{invalid, garbled} {
		code, out, errOut := runCLI("plugin", "list", "--lab", lab)
		if code != 1 || out != "" || !strings.Contains(errOut, "the active Formula could not be loaded") || strings.Contains(errOut, "needs a Lab") || strings.Contains(errOut, pluginUsage) || strings.Contains(errOut, lab) {
			t.Errorf("exit %d\n%s%s", code, out, errOut)
		}
	}

	previous := resolveLab
	resolveLab = func(context.Context, string) (labResolution, error) {
		return labResolution{}, errors.New("no Lab under /private/secret")
	}
	t.Cleanup(func() { resolveLab = previous })
	code, out, errOut := runCLI("plugin", "list")
	if code != 1 || out != "" || !strings.Contains(errOut, "cannot resolve a Lab") || strings.Contains(errOut, "/private/secret") || strings.Contains(errOut, pluginUsage) {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}

	for _, args := range [][]string{
		{"plugin", "list", "extra"},
		{"plugin", "list", "--verbose"},
		{"plugin", "list", "--lab"},
	} {
		if code, _, errOut := runCLI(args...); code != 1 || !strings.Contains(errOut, pluginUsage) {
			t.Errorf("%v: exit %d, %q", args, code, errOut)
		}
	}
	if code, _, errOut := runCLI("plugin", "list", "--lab", empty, "--lab-name", "x"); code != 1 || !strings.Contains(errOut, "cannot be combined") {
		t.Errorf("--lab with --lab-name: exit %d, %q", code, errOut)
	}
}
