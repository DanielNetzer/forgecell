package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

// pluginDoctorSecret is the value of the one set needs.env name; it must never
// be printed.
const pluginDoctorSecret = "s3cr3t-doctor-value-0123"

// usePluginDoctorSeams fires the shutdown grace at once and serves a fixed
// environment in which FAKE_SET is set and nothing else is.
func usePluginDoctorSeams(t *testing.T) {
	t.Helper()
	previousOptions, previousEnv := pluginDoctorOptions, pluginLookupEnv
	pluginDoctorOptions = func(o mcpadapter.Options) mcpadapter.Options {
		o.Grace = firedGrace
		return o
	}
	pluginLookupEnv = func(name string) (string, bool) {
		if name == "FAKE_SET" {
			return pluginDoctorSecret, true
		}
		return "", false
	}
	t.Cleanup(func() { pluginDoctorOptions, pluginLookupEnv = previousOptions, previousEnv })
}

func pluginDoctorJSON(t *testing.T, args ...string) (int, []pluginDoctorRow, string) {
	t.Helper()
	code, out, errOut := runCLI(append([]string{"plugin", "doctor", "--json"}, args...)...)
	var got struct {
		Plugins []pluginDoctorRow `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Plugins == nil {
		t.Fatalf("exit %d, %v\n%s%s", code, err, out, errOut)
	}
	return code, got.Plugins, out + errOut
}

// requireNoLeak fails when output names a local path, the planted secret or
// the set environment value.
func requireNoLeak(t *testing.T, output string, paths ...string) {
	t.Helper()
	for _, leak := range append(paths, plugintest.PlantedSecret, pluginDoctorSecret, "Bearer", "plugin.json", "plugin-runs") {
		if leak != "" && strings.Contains(output, leak) {
			t.Fatalf("the output leaks %q:\n%s", leak, output)
		}
	}
}

func requireRunRootEmpty(t *testing.T, lab string) {
	t.Helper()
	if left, _ := os.ReadDir(filepath.Join(lab, mcpadapter.RunRootName)); len(left) != 0 {
		t.Fatalf("plugin doctor left %d run directories", len(left))
	}
}

func TestPluginDoctor(t *testing.T) {
	usePluginDoctorSeams(t)
	// One build of the fake server serves every case.
	server := plugintest.Server(t)
	manifest := func(kind, mode string, env ...string) []byte {
		return plugintest.Manifest(t, server, kind, mode, env...)
	}
	tail := "forge:\n  plugin: hub\n  project: acme/widgets\nreviewers:\n  - plugin: critic\n    autoFix: low\n"

	t.Run("ok", func(t *testing.T) {
		lab := pluginListLab(t, "jira", tail,
			pluginListPin{name: "jira", path: "plugins/jira/plugin.json", raw: manifest("tracker", "good", "FAKE_SET", "FAKE_TOKEN")},
			pluginListPin{name: "hub", path: "plugins/hub.json", raw: manifest("forge", "good")},
			pluginListPin{name: "critic", path: "plugins/critic.json", raw: manifest("reviewer", "good")})
		code, out, errOut := runCLI("plugin", "doctor", "--lab", lab)
		if code != 0 || errOut != "" {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 7 || strings.Fields(lines[0])[0] != "NAME" {
			t.Fatalf("want a header, three rows and three summaries:\n%s", out)
		}
		for i, want := range [][]string{
			{"jira", "tracker", contractID(t, "tracker"), "tracker", "ok"},
			{"hub", "forge", contractID(t, "forge"), "forge", "ok"},
			{"critic", "reviewer", contractID(t, "reviewer"), "reviewer", "ok"},
		} {
			if got := strings.Fields(lines[i+1]); !reflect.DeepEqual(got, want) {
				t.Errorf("row %d: %v, want %v", i, got, want)
			}
		}
		for _, want := range []string{
			"jira: Fake tracker · key pattern ^FAKE-[0-9]+$ · selectors all · unset env FAKE_TOKEN",
			"hub: Fake forge · identity forge.example/example/project · CI none",
			"critic: Fake reviewer",
		} {
			if !strings.Contains(out, want+"\n") {
				t.Errorf("missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "FAKE_SET") {
			t.Fatalf("a set env name is listed as unset:\n%s", out)
		}
		requireNoLeak(t, out, lab, filepath.Dir(server.Path))
		requireRunRootEmpty(t, lab)
	})

	t.Run("json and NAME", func(t *testing.T) {
		lab := pluginListLab(t, "jira", tail,
			pluginListPin{name: "jira", path: "plugins/jira.json", raw: manifest("tracker", "good", "FAKE_TOKEN")},
			pluginListPin{name: "hub", path: "plugins/hub.json", raw: manifest("forge", "good")},
			pluginListPin{name: "critic", path: "plugins/critic.json", raw: manifest("reviewer", "good")})
		code, rows, output := pluginDoctorJSON(t, "--lab", lab)
		want := []pluginDoctorRow{
			{Name: "jira", Kind: "tracker", Role: "tracker", Contract: contractID(t, "tracker"), Status: "ok", DisplayName: "Fake tracker", KeyPattern: "^FAKE-[0-9]+$", Selectors: []string{"all"}, MissingEnv: []string{"FAKE_TOKEN"}},
			{Name: "hub", Kind: "forge", Role: "forge", Contract: contractID(t, "forge"), Status: "ok", DisplayName: "Fake forge", Identity: "forge.example/example/project", CIKind: "none", MissingEnv: []string{}},
			{Name: "critic", Kind: "reviewer", Role: "reviewer", Contract: contractID(t, "reviewer"), Status: "ok", DisplayName: "Fake reviewer", MissingEnv: []string{}},
		}
		if code != 0 || !reflect.DeepEqual(rows, want) {
			t.Fatalf("exit %d, rows\n%+v\nwant\n%+v", code, rows, want)
		}
		requireNoLeak(t, output, lab)
		// NAME checks only that plugin, before or after the flags.
		for _, args := range [][]string{{"hub", "--lab", lab}, {"--lab", lab, "hub"}} {
			code, rows, _ = pluginDoctorJSON(t, args...)
			if code != 0 || len(rows) != 1 || !reflect.DeepEqual(rows[0], want[1]) {
				t.Fatalf("%v: exit %d, rows %+v", args, code, rows)
			}
		}
		// An undeclared NAME is refused without echoing it.
		code, out, errOut := runCLI("plugin", "doctor", "no-such-plugin", "--lab", lab)
		if code != 1 || out != "" || !strings.Contains(errOut, "declares no plugin by that name") || strings.Contains(errOut, "no-such-plugin") {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		requireRunRootEmpty(t, lab)
	})

	t.Run("fixed reasons", func(t *testing.T) {
		lab := pluginListLab(t, "crashing", "",
			pluginListPin{name: "crashing", path: "plugins/crash.json", raw: manifest("tracker", "crash")},
			pluginListPin{name: "contract", path: "plugins/contract.json", raw: manifest("forge", "wrong-contract")},
			pluginListPin{name: "partial", path: "plugins/partial.json", raw: manifest("tracker", "missing-tool")},
			pluginListPin{name: "absent", path: "plugins/absent.json", sha256: strings.Repeat("a", 64)},
			pluginListPin{name: "good", path: "plugins/good.json", raw: manifest("reviewer", "good")})
		code, rows, output := pluginDoctorJSON(t, "--lab", lab)
		statuses := map[string]string{}
		for _, r := range rows {
			statuses[r.Name] = r.Status
			if r.Status != "ok" && r.DisplayName != "" {
				t.Errorf("%s: a failed row carries a describe summary: %+v", r.Name, r)
			}
		}
		wantStatuses := map[string]string{"crashing": "failed", "contract": "contract-mismatch", "partial": "contract-mismatch", "absent": "not-installed", "good": "ok"}
		if code != 2 || !reflect.DeepEqual(statuses, wantStatuses) {
			t.Fatalf("exit %d, statuses %v", code, statuses)
		}
		requireNoLeak(t, output, lab, filepath.Dir(server.Path))
		code, out, errOut := runCLI("plugin", "doctor", "--lab", lab)
		if code != 2 || errOut != "" || !strings.Contains(out, "contract-mismatch") || !strings.Contains(out, "not-installed") {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		requireNoLeak(t, out, lab, filepath.Dir(server.Path))
		requireRunRootEmpty(t, lab)
		// NAME selects a failing row only, and the exit code follows it.
		if code, rows, _ := pluginDoctorJSON(t, "good", "--lab", lab); code != 0 || len(rows) != 1 || rows[0].Status != "ok" {
			t.Fatalf("good: exit %d, rows %+v", code, rows)
		}
		if code, rows, _ := pluginDoctorJSON(t, "crashing", "--lab", lab); code != 2 || len(rows) != 1 || rows[0].Status != "failed" {
			t.Fatalf("crashing: exit %d, rows %+v", code, rows)
		}
	})
}

func TestPluginDoctorWithoutPluginsAndUsage(t *testing.T) {
	usePluginDoctorSeams(t)
	lab := pluginListLab(t, "github-issues", "")
	code, out, errOut := runCLI("plugin", "doctor", "--lab", lab)
	if code != 0 || !strings.Contains(out, "pins no plugins") || errOut != "" {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if code, rows, _ := pluginDoctorJSON(t, "--lab", lab); code != 0 || len(rows) != 0 {
		t.Fatalf("exit %d, rows %+v", code, rows)
	}
	if _, err := os.Lstat(filepath.Join(lab, mcpadapter.RunRootName)); !os.IsNotExist(err) {
		t.Fatalf("a Lab without plugins got a run root: %v", err)
	}
	empty := plugintest.TempDir(t)
	if code, out, errOut := runCLI("plugin", "doctor", "--lab", empty); code != 1 || out != "" || !strings.Contains(errOut, "needs a Lab with an active approved Formula") || strings.Contains(errOut, empty) {
		t.Fatalf("an empty Lab: exit %d\n%s%s", code, out, errOut)
	}
	for _, args := range [][]string{
		{"plugin", "doctor", "a", "b", "--lab", lab},
		{"plugin", "doctor", "--verbose", "--lab", lab},
		{"plugin", "doctor", "--lab"},
	} {
		if code, _, errOut := runCLI(args...); code != 1 || !strings.Contains(errOut, pluginUsage) {
			t.Errorf("%v: exit %d, %q", args, code, errOut)
		}
	}
}

// TestDoctorSummarizesPluginsWithoutStartingThem: the main doctor shows one
// plugins line, with counts and a pointer to plugin doctor, and starts nothing.
func TestDoctorSummarizesPluginsWithoutStartingThem(t *testing.T) {
	doctorFixture(t)
	lab := pluginListLab(t, "jira", "reviewers:\n  - plugin: critic\n    autoFix: low\n",
		pluginListPin{name: "jira", path: "plugins/jira.json", raw: []byte(`{"not":"started"}`)},
		pluginListPin{name: "critic", path: "plugins/critic.json", raw: []byte(`{"not":"started"}`)},
		pluginListPin{name: "spare", path: "plugins/spare.json", raw: []byte(`{"not":"started"}`)})
	const line = "Plugins: 3 declared (1 tracker, 0 forge, 1 reviewer, 1 unused); none started here. Run forgecell plugin doctor to start and check them."
	_, out := runDoctor(t, lab, "--json")
	plugins, _ := decodeDoctor(t, out)["plugins"].(map[string]any)
	if plugins["declared"] != float64(3) || plugins["summary"] != line || plugins["nextCommand"] != "forgecell plugin doctor" {
		t.Fatalf("doctor plugins = %v", plugins)
	}
	if _, text := runDoctor(t, lab, "--verbose"); !strings.Contains(text, "\n"+line+"\n") {
		t.Fatalf("doctor text:\n%s", text)
	}
	if _, err := os.Lstat(filepath.Join(lab, mcpadapter.RunRootName)); !os.IsNotExist(err) {
		t.Fatalf("doctor prepared a plugin run root: %v", err)
	}

	// A Formula without plugins has neither the key nor the line.
	bare := pluginListLab(t, "github-issues", "")
	_, out = runDoctor(t, bare, "--json")
	if _, ok := decodeDoctor(t, out)["plugins"]; ok {
		t.Fatalf("doctor reports plugins for a Formula with none:\n%s", out)
	}
	if _, text := runDoctor(t, bare, "--verbose"); strings.Contains(text, "Plugins:") {
		t.Fatalf("doctor text:\n%s", text)
	}
}
