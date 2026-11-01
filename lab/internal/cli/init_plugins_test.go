package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitPluginFlagsAreRefusedWithoutCompanionsOrWithOtherModes(t *testing.T) {
	approval := "assay-" + strings.Repeat("a", 64)
	all := []string{"--forge-plugin", "plugins/gitlab/plugin.json", "--project", "acme/backend", "--tracker-plugin", "plugins/jira/plugin.json", "--tracker-config", "tracker.json"}
	for name, c := range map[string]struct {
		args  []string
		noLab bool
		want  string
	}{
		"forge plugin alone": {args: []string{"--forge-plugin", "plugins/gitlab/plugin.json"}, want: "--forge-plugin, --project, --tracker-plugin and --tracker-config always come together"},
		"project alone":      {args: []string{"--project", "acme/backend"}, want: "always come together"},
		"no tracker config":  {args: all[:6], want: "always come together"},
		"empty project":      {args: []string{"--forge-plugin", "a.json", "--project", "", "--tracker-plugin", "b.json", "--tracker-config", "c.json"}, want: "always come together"},
		"no explicit lab":    {args: all, noLab: true, want: "need an explicit --lab"},
		"rebind":             {args: append([]string{"--rebind", "codex"}, all...), want: "do not combine them with --rebind, --model, --role, --enable-review or --refresh-baseline"},
		"model":              {args: append([]string{"--model", "claude-opus-4-8"}, all...), want: "do not combine them with --rebind"},
		"role":               {args: append([]string{"--role", "coding"}, all...), want: "do not combine them with --rebind"},
		"enable review":      {args: append([]string{"--enable-review"}, all...), want: "do not combine them with --rebind"},
		"refresh baseline":   {args: append([]string{"--refresh-baseline"}, all...), want: "do not combine them with --rebind"},
		"review":             {args: append([]string{"--review", approval}, all...), want: "propose a change; review, approve or dismiss the saved proposal in a separate command"},
		"approve":            {args: append([]string{"--approve", approval}, all...), want: "propose a change"},
		"dismiss":            {args: append([]string{"--dismiss", approval}, all...), want: "propose a change"},
	} {
		t.Run(name, func(t *testing.T) {
			gsEnv(t, "")
			lab := t.TempDir()
			// Outside any checkout, with the default Lab set, so no real Lab is resolved.
			t.Chdir(t.TempDir())
			t.Setenv("FORGECELL_LAB", lab)
			args := []string{"init"}
			if !c.noLab {
				args = append(args, "--lab", lab)
			}
			code, out, errOut := gsRun(append(args, c.args...))
			if code != 1 || out != "" || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d stdout %q stderr %q, want %q", code, out, errOut, c.want)
			}
			if entries, _ := os.ReadDir(lab); len(entries) != 0 {
				t.Fatalf("a refused plugin init wrote %v", entries)
			}
		})
	}
}

func TestInitPluginFlagsReachPrepare(t *testing.T) {
	gsEnv(t, "")
	lab := t.TempDir()
	t.Chdir(t.TempDir())
	t.Setenv("FORGECELL_LAB", lab)
	// Prepare refuses the nested group before it reads any manifest, so the
	// refusal proves the flags arrived.
	code, _, errOut := gsRun([]string{"init", "--lab", lab, "--forge-plugin", "plugins/gitlab/plugin.json", "--project", "group/sub/backend", "--tracker-plugin", "plugins/jira/plugin.json", "--tracker-config", "tracker.json"})
	if code != 1 || !strings.Contains(errOut, "nested groups such as group/subgroup/repo are not supported yet") {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
}

const pluginDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// writeDoctorFormula writes and approves an active Formula bound to codex whose
// forge and tracker are plugins when plugins is true, and GitHub otherwise.
func writeDoctorFormula(t *testing.T, lab, command string, plugins bool) {
	t.Helper()
	os.MkdirAll(filepath.Join(lab, "formulas"), 0700)
	os.WriteFile(filepath.Join(lab, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"f"}`), 0600)
	recipe := "kind: formula\nid: f\nintake: {repo: acme/widgets}\n"
	if plugins {
		recipe = "kind: formula\nid: f\nplugins:\n  - {name: gl, manifest: plugins/gl.json, sha256: " + pluginDigest + "}\n  - {name: jr, manifest: plugins/jr.json, sha256: " + pluginDigest + "}\nforge: {plugin: gl, project: acme/widgets}\nintake: {source: jr, repo: acme/widgets}\n"
	}
	recipe += "harness:\n  binding: codex\n  command: " + command + "\n" + roleAtoms
	if err := os.WriteFile(filepath.Join(lab, "formulas", "f.yaml"), []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	approveSavedFormula(t, lab)
}

func TestDoctorDoesNotRequireGHForAPluginForgeAndTracker(t *testing.T) {
	for _, plugins := range []bool{true, false} {
		name := "github"
		if plugins {
			name = "plugins"
		}
		t.Run(name, func(t *testing.T) {
			bin, lab := doctorFixture(t)
			marker := filepath.Join(t.TempDir(), "gh-ran")
			// PATH holds only the fixture tools, so the marker uses a shell builtin.
			writeTool(t, bin, "gh", "#!/bin/sh\n: > '"+marker+"'\nexit 1\n")
			launcher, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			codex := writeTool(t, bin, "codex", strings.Replace(codexTool, "LOGIN", "Logged in using ChatGPT", 1))
			writeDoctorFormula(t, lab, argv(t, launcher, "__adapter", "codex", codex), plugins)
			code, out := runDoctor(t, lab)
			report := decodeDoctor(t, out)
			_, ranGH := os.Stat(marker)
			required, hasRequired := report["githubRequired"]
			if plugins {
				if code != 0 || !hasRequired || required != false || ranGH == nil {
					t.Fatalf("a plugin Lab must be ready without gh: exit %d, githubRequired %v, gh ran %v\n%s", code, required, ranGH == nil, out)
				}
			} else if code != 2 || hasRequired || report["githubAuthenticated"] != false || ranGH != nil {
				t.Fatalf("a GitHub Lab must still need gh: exit %d, githubRequired %v, gh ran %v\n%s", code, required, ranGH == nil, out)
			}
			_, human := runDoctor(t, lab, "--verbose")
			line := "GitHub CLI authenticated: false\n"
			if plugins {
				line = "GitHub CLI authenticated: not required (the forge and tracker are plugins)\n"
			}
			if !strings.Contains(human, line) {
				t.Fatalf("human view lacks %q:\n%s", line, human)
			}
		})
	}
}
