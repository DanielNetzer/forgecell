package scaffold

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

// The contributor guide and the marked example tracker it holds.
const (
	guidePath          = "../../../plugins/README.md"
	exampleMarker      = "<!-- forgecell:example-tracker -->"
	exampleFence       = "```python"
	maxExampleLines    = 60
	maxGuideBytes      = 1 << 20
	exampleTrackerName = "demo-tracker"
)

// exampleBlock returns the lines of the fenced python block that directly
// follows the marker line. The marker must appear exactly once, the next line
// must open the block, the block must be closed and at most maxExampleLines
// long.
func exampleBlock(guide string) (string, error) {
	lines := strings.Split(guide, "\n")
	at := -1
	for i, l := range lines {
		if strings.TrimRight(l, "\r") != exampleMarker {
			continue
		}
		if at >= 0 {
			return "", errors.New("the example marker appears more than once")
		}
		at = i
	}
	switch {
	case at < 0:
		return "", errors.New("the example marker is missing")
	case at+1 >= len(lines) || strings.TrimRight(lines[at+1], "\r") != exampleFence:
		return "", errors.New("the line after the example marker does not open a python block")
	}
	for i := at + 2; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") != "```" {
			continue
		}
		if n := i - at - 2; n > maxExampleLines {
			return "", errors.New("the example block is over 60 lines")
		}
		return strings.Join(lines[at+2:i], "\n") + "\n", nil
	}
	return "", errors.New("the example block is not closed")
}

func readGuide(t *testing.T) string {
	t.Helper()
	return readDoc(t, guidePath)
}

// readDoc reads a repository document that must be a regular file, not a
// symlink, of at most maxGuideBytes.
func readDoc(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxGuideBytes {
		t.Fatalf("%s is not a regular file within %d bytes: %v", path, maxGuideBytes, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > maxGuideBytes {
		t.Fatalf("%s grew past %d bytes", path, maxGuideBytes)
	}
	return string(raw)
}

// pluginDocs are the plugin READMEs whose Formula snippets must parse.
var pluginDocs = []string{
	guidePath,
	"../../../plugins/forgecomments/README.md",
	"../../../plugins/gitlab/README.md",
	"../../../plugins/jira/README.md",
}

// formulaSnippets returns every ```yaml block in doc that declares a top-level
// plugins: key, with the fence's own indentation removed.
func formulaSnippets(doc string) ([]string, error) {
	var out []string
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		indent, ok := strings.CutSuffix(lines[i], "```yaml")
		if !ok || strings.TrimLeft(indent, " ") != "" {
			continue
		}
		var block []string
		closed := false
		for i++; i < len(lines); i++ {
			if lines[i] == indent+"```" {
				closed = true
				break
			}
			line, ok := strings.CutPrefix(lines[i], indent)
			if !ok && strings.TrimSpace(lines[i]) != "" {
				return nil, errors.New("a yaml block line is less indented than its fence")
			}
			block = append(block, line)
		}
		if !closed {
			return nil, errors.New("a yaml block is not closed")
		}
		if slices.Contains(block, "plugins:") {
			out = append(out, strings.Join(block, "\n")+"\n")
		}
	}
	return out, nil
}

// snippetFormula completes a README's Formula snippet into a whole Formula:
// the snippet's intake keys join a base intake that names a repository, its
// other keys are used as written, and "<manifest hex>" becomes a digest.
func snippetFormula(snippet string) ([]byte, error) {
	snippet = strings.ReplaceAll(snippet, "<manifest hex>", strings.Repeat("a", 64))
	var keys map[string]any
	if err := yaml.Unmarshal([]byte(snippet), &keys); err != nil {
		return nil, err
	}
	intake := map[string]any{"repo": "acme/widgets"}
	if value, ok := keys["intake"]; ok {
		m, ok := value.(map[string]any)
		if !ok {
			return nil, errors.New("the snippet's intake is not a mapping")
		}
		maps.Copy(intake, m)
	}
	whole := map[string]any{
		"schemaVersion": "v0", "kind": "formula", "id": "sample",
		"atoms": []any{map[string]any{"id": "intake", "type": "intake"}, map[string]any{"id": "work", "type": "harness"}},
	}
	maps.Copy(whole, keys)
	whole["intake"] = intake
	return yaml.Marshal(whole)
}

// TestPluginDocsFormulaSnippetsParse parses every plugins: snippet in the
// plugin READMEs with the formula package, so the docs show only the list form
// a Formula accepts.
func TestPluginDocsFormulaSnippetsParse(t *testing.T) {
	for _, path := range pluginDocs {
		snippets, err := formulaSnippets(readDoc(t, path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(snippets) == 0 {
			t.Fatalf("%s has no plugins: snippet", path)
		}
		for i, s := range snippets {
			raw, err := snippetFormula(s)
			if err != nil {
				t.Fatalf("%s snippet %d: %v", path, i, err)
			}
			f, err := formula.Parse(raw)
			if err != nil {
				t.Fatalf("%s snippet %d does not parse as a Formula: %v\n%s", path, i, err, s)
			}
			if len(f.Plugins) == 0 {
				t.Fatalf("%s snippet %d declares no plugin", path, i)
			}
		}
	}
}

func TestFormulaSnippetRefusals(t *testing.T) {
	for _, c := range []struct{ name, doc, want string }{
		{"unclosed", "```yaml\nplugins:\n", "not closed"},
		{"outdented line", "  ```yaml\nplugins:\n  ```\n", "less indented"},
	} {
		if _, err := formulaSnippets(c.doc); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
	// The old map form, which a Formula refuses, must fail the parse.
	raw, err := snippetFormula("plugins:\n  x: {manifest: plugins/x/plugin.json, sha256: \"<manifest hex>\", required: false}\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := formula.Parse(raw); err == nil || !strings.Contains(err.Error(), "plugins must be a list") {
		t.Fatalf("the map form parsed: %v", err)
	}
	if got, _ := formulaSnippets("```yaml\nforge: {}\n```\n"); len(got) != 0 {
		t.Fatalf("a block without plugins: was taken: %q", got)
	}
}

func TestExampleBlockRefusals(t *testing.T) {
	body := func(n int) string { return strings.Repeat("pass\n", n) }
	for _, c := range []struct{ name, guide, want string }{
		{"missing marker", "# Guide\n" + exampleFence + "\n" + body(1) + "```\n", "missing"},
		{"marker twice", exampleMarker + "\n" + exampleFence + "\n" + body(1) + "```\n" + exampleMarker + "\n", "more than once"},
		{"no fence after the marker", exampleMarker + "\n\n" + exampleFence + "\n" + body(1) + "```\n", "does not open"},
		{"another language", exampleMarker + "\n```go\n" + body(1) + "```\n", "does not open"},
		{"marker on the last line", "text\n" + exampleMarker, "does not open"},
		{"unclosed block", exampleMarker + "\n" + exampleFence + "\n" + body(3), "not closed"},
		{"61 lines", exampleMarker + "\n" + exampleFence + "\n" + body(maxExampleLines+1) + "```\n", "over 60 lines"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := exampleBlock(c.guide); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
	got, err := exampleBlock("intro\r\n" + exampleMarker + "\r\n" + exampleFence + "\r\n" + body(maxExampleLines) + "```\r\nafter\n")
	if err != nil || got != body(maxExampleLines) {
		t.Fatalf("a 60-line block: %q, %v", got, err)
	}
}

// TestGuideExampleTrackerPassesConformance pins the guide's example tracker
// with python3 and runs contract mode and live tracker mode on it. The marker
// and the line bound are checked even where python3 is missing.
func TestGuideExampleTrackerPassesConformance(t *testing.T) {
	block, err := exampleBlock(readGuide(t))
	if err != nil {
		t.Fatal(err)
	}
	interp := interpFor(t, "python")

	base := tempDir(t)
	dir := filepath.Join(base, exampleTrackerName)
	if err := Write(dir, Options{Kind: "tracker", Lang: "python", Name: exampleTrackerName}); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "server.py")
	if err := os.WriteFile(script, []byte(block), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PinInterpreted(dir, script, interp)
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, ManifestFile)
	opts := func(run string) conformance.Options {
		mkdir(t, run)
		return conformance.Options{Root: run, LookupEnv: func(string) (string, bool) { return "", false }, Grace: firedGrace}
	}
	check := func(mode, run string, report conformance.Report) {
		t.Helper()
		if !report.Passed || report.Mode != mode || report.ManifestSHA256 != p.ManifestSHA256 || report.Contract != contract.ID("tracker") {
			raw, _ := json.MarshalIndent(report, "", "  ")
			t.Fatalf("%s mode did not pass:\n%s", mode, raw)
		}
		for _, c := range report.Checks {
			if c.Status != conformance.Pass {
				t.Fatalf("%s mode: check %s is %s", mode, c.ID, c.Status)
			}
		}
		if left, _ := os.ReadDir(run); len(left) != 0 {
			t.Fatalf("the %s run left %d entries behind", mode, len(left))
		}
	}

	run := filepath.Join(base, "run-contract")
	check("contract", run, conformance.Run(context.Background(), manifest, opts(run)))

	run = filepath.Join(base, "run-live")
	live := conformance.RunLive(context.Background(), manifest, opts(run), conformance.Live{Target: "DEMO", Config: map[string]any{}})
	check("live", run, live)
	want := map[string]bool{conformance.CheckLiveList: true, conformance.CheckLiveGet: true, conformance.CheckLiveStates: true}
	for _, c := range live.Checks {
		delete(want, c.ID)
	}
	if len(want) != 0 {
		t.Fatalf("the live report lacks the checks %v", want)
	}
}
