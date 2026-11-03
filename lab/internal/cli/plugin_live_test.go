package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const liveTestCommit = "0123456789abcdef0123456789abcdef01234567"

// useRecordedPluginTestOptions is usePluginTestOptions with the fake server's
// call record: FAKE_RECORD names a file under the test's temporary directory.
// It returns the run root and the record path.
func useRecordedPluginTestOptions(t *testing.T) (string, string) {
	t.Helper()
	root := plugintest.TempDir(t)
	record := filepath.Join(plugintest.TempDir(t), "calls.jsonl")
	previous := pluginTestOptions
	pluginTestOptions = func() conformance.Options {
		return conformance.Options{Root: root, Grace: firedGrace, LookupEnv: func(name string) (string, bool) {
			if name == "FAKE_RECORD" {
				return record, true
			}
			return "", false
		}}
	}
	t.Cleanup(func() { pluginTestOptions = previous })
	return root, record
}

// recordedTools returns the tool names in the fake's call record, in order.
func recordedTools(t *testing.T, record string) []string {
	t.Helper()
	raw, err := os.ReadFile(record)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var tools []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var call struct {
			Tool string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(line), &call); err != nil {
			t.Fatal(err)
		}
		tools = append(tools, call.Tool)
	}
	return tools
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(plugintest.TempDir(t), "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPluginTestLiveRefusals(t *testing.T) {
	server := plugintest.Server(t)
	manifest := func(kind string) string {
		return plugintest.WriteManifest(t, plugintest.Manifest(t, server, kind, "live", "FAKE_RECORD"))
	}
	tracker, forge, reviewer := manifest("tracker"), manifest("forge"), manifest("reviewer")
	const secret = "cli-config-secret-0451"
	good := writeConfig(t, `{"token": "`+secret+`"}`)
	link := filepath.Join(plugintest.TempDir(t), "link.json")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	props := []string{}
	for i := 0; i < 65; i++ {
		props = append(props, fmt.Sprintf(`"k%d": "%s"`, i, secret))
	}
	writeOnly := []string{"--allow-write", "--head", "forgecell/check", "--base", "main", "--commit", liveTestCommit}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"target without live", []string{tracker, "--target", "FAKE"}, "--target needs --live"},
		{"config without live", []string{tracker, "--config", good}, "--config needs --live"},
		{"allow-write without live", []string{forge, "--allow-write"}, "--allow-write needs --live"},
		{"live without target", []string{tracker, "--live"}, "--live requires --target"},
		{"target without value", []string{tracker, "--live", "--target"}, "--target needs a value"},
		{"target given twice", []string{tracker, "--live", "--target", "A", "--target", "B"}, "--target is given more than once"},
		{"change without commit", []string{forge, "--live", "--target", "p", "--change", "!1"}, "--change requires --commit"},
		{"commit alone", []string{forge, "--live", "--target", "p", "--commit", liveTestCommit}, "--commit requires --change or --allow-write"},
		{"head without allow-write", []string{forge, "--live", "--target", "p", "--head", "h"}, "--head and --base need --allow-write"},
		{"allow-write without head", []string{forge, "--live", "--target", "p", "--allow-write", "--base", "main", "--commit", liveTestCommit}, "--allow-write requires --head, --base and --commit"},
		{"allow-write without commit", []string{forge, "--live", "--target", "p", "--allow-write", "--head", "h", "--base", "main"}, "--allow-write requires --head, --base and --commit"},
		{"allow-write for a tracker", append([]string{tracker, "--live", "--target", "FAKE"}, writeOnly...), "--allow-write is only for a forge plugin"},
		{"allow-write for a reviewer", append([]string{reviewer, "--live", "--target", "f", "--change", "!1"}, writeOnly...), "--allow-write is only for a forge plugin"},
		{"reviewer without change", []string{reviewer, "--live", "--target", "f"}, "a reviewer live run requires --change and --commit"},
		{"config symlink", []string{tracker, "--live", "--target", "FAKE", "--config", link}, "--config must name a readable regular file, not a symlink"},
		{"config missing", []string{tracker, "--live", "--target", "FAKE", "--config", good + ".missing"}, "--config must name a readable regular file"},
		{"config directory", []string{tracker, "--live", "--target", "FAKE", "--config", filepath.Dir(good)}, "--config must name a readable regular file"},
		{"config too large", []string{tracker, "--live", "--target", "FAKE", "--config", writeConfig(t, `{"a": "`+strings.Repeat(secret, 3000)+`"}`)}, "--config exceeds 64 KiB"},
		{"config too many properties", []string{tracker, "--live", "--target", "FAKE", "--config", writeConfig(t, "{"+strings.Join(props, ",")+"}")}, "--config holds more than 64 properties"},
		{"config array", []string{tracker, "--live", "--target", "FAKE", "--config", writeConfig(t, `["`+secret+`"]`)}, "--config must hold one JSON object"},
		{"config null", []string{tracker, "--live", "--target", "FAKE", "--config", writeConfig(t, `null`)}, "--config must hold one JSON object"},
		{"config repeated key", []string{tracker, "--live", "--target", "FAKE", "--config", writeConfig(t, `{"a": "`+secret+`", "a": "x"}`)}, "--config must hold one JSON object with no repeated key"},
		{"config trailing data", []string{tracker, "--live", "--target", "FAKE", "--config", writeConfig(t, `{"a": "`+secret+`"} {}`)}, "--config must hold one JSON object"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, record := useRecordedPluginTestOptions(t)
			code, out, errOut := runCLI(append([]string{"plugin", "test"}, c.args...)...)
			if code != 1 || out != "" || !strings.Contains(errOut, c.want) || !strings.Contains(errOut, pluginTestLiveUsage) {
				t.Fatalf("exit %d, stdout %q, stderr %q, want %q", code, out, errOut, c.want)
			}
			if strings.Contains(errOut, secret) || strings.Contains(errOut, filepath.Dir(good)) || strings.Contains(errOut, plugintest.TempDir(t)) {
				t.Fatalf("the refusal leaks a value or path: %q", errOut)
			}
			if tools := recordedTools(t, record); len(tools) != 0 {
				t.Fatalf("a refused run called %v", tools)
			}
		})
	}
}

func TestPluginTestLiveRuns(t *testing.T) {
	server := plugintest.Server(t)
	manifest := func(t *testing.T, kind, mode string) string {
		return plugintest.WriteManifest(t, plugintest.Manifest(t, server, kind, mode, "FAKE_RECORD"))
	}

	t.Run("tracker", func(t *testing.T) {
		root, record := useRecordedPluginTestOptions(t)
		const secret = "cli-config-secret-0451"
		config := writeConfig(t, `{"token": "`+secret+`"}`)
		path := manifest(t, "tracker", "live")
		code, out, errOut := runCLI("plugin", "test", path, "--live", "--target", "FAKE", "--config", config)
		if code != 0 || !strings.Contains(out, "PASS live-list") || !strings.Contains(out, "PASS live-states") || !strings.Contains(out, "Result: PASS (11 of 11 checks passed; live mode, target FAKE)") {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		for _, leak := range []string{secret, "token", root, filepath.Dir(path), filepath.Dir(config), plugintest.PlantedSecret} {
			if strings.Contains(out+errOut, leak) {
				t.Fatalf("the report leaks %q:\n%s%s", leak, out, errOut)
			}
		}
		if raw, err := os.ReadFile(record); err != nil || !strings.Contains(string(raw), secret) {
			t.Fatalf("the config did not reach the plugin: %v", err)
		}
		code, out, _ = runCLI("plugin", "test", "--json", path, "--live", "--target", "FAKE", "--config", config)
		var r conformance.Report
		if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || !r.Passed || r.Mode != "live" || r.Target != "FAKE" || len(r.Checks) != 11 {
			t.Fatalf("exit %d, report %+v, %v", code, r, err)
		}
		if strings.Contains(out, secret) {
			t.Fatalf("the JSON report leaks the config:\n%s", out)
		}
	})

	t.Run("tracker failure", func(t *testing.T) {
		useRecordedPluginTestOptions(t)
		code, out, _ := runCLI("plugin", "test", manifest(t, "tracker", "live-bad-key"), "--live", "--target", "FAKE")
		if code != 2 || !strings.Contains(out, "FAIL live-list: failed") || !strings.Contains(out, "SKIP live-get") || !strings.Contains(out, "Result: FAIL") {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})

	t.Run("forge read-only", func(t *testing.T) {
		_, record := useRecordedPluginTestOptions(t)
		code, out, _ := runCLI("plugin", "test", manifest(t, "forge", "live"), "--live", "--target", "example/project", "--change", "!1", "--commit", liveTestCommit)
		if code != 0 || !strings.Contains(out, "PASS live-change") || strings.Contains(out, "live-publish") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		for _, tool := range recordedTools(t, record) {
			if tool == "forge_publish_draft" {
				t.Fatal("a read-only live run called forge_publish_draft")
			}
		}
	})

	t.Run("forge allow-write", func(t *testing.T) {
		_, record := useRecordedPluginTestOptions(t)
		code, out, _ := runCLI("plugin", "test", manifest(t, "forge", "live"), "--live", "--target", "example/project",
			"--allow-write", "--head", "forgecell/check", "--base", "main", "--commit", liveTestCommit)
		if code != 0 || !strings.Contains(out, "PASS live-publish") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		published := 0
		for _, tool := range recordedTools(t, record) {
			if tool == "forge_publish_draft" {
				published++
			}
		}
		if published != 1 {
			t.Fatalf("forge_publish_draft called %d times", published)
		}
	})

	t.Run("reviewer rate-limited", func(t *testing.T) {
		_, record := useRecordedPluginTestOptions(t)
		config := writeConfig(t, `{"fail": "rate-limited"}`)
		code, out, _ := runCLI("plugin", "test", manifest(t, "reviewer", "good"), "--live", "--target", "forge.example/example/project",
			"--change", "!1", "--commit", liveTestCommit, "--config", config)
		if code != 2 || !strings.Contains(out, "SKIP live-findings: rate-limited") || !strings.Contains(out, "Result: FAIL") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		findings := 0
		for _, tool := range recordedTools(t, record) {
			if tool == "reviewer_findings" {
				findings++
			}
		}
		if findings != 1 {
			t.Fatalf("reviewer_findings called %d times, want 1 with no retry", findings)
		}
	})

	t.Run("contract mode unchanged", func(t *testing.T) {
		useRecordedPluginTestOptions(t)
		path := manifest(t, "forge", "live")
		code, out, _ := runCLI("plugin", "test", path)
		if code != 0 || strings.Contains(out, "live") || !strings.Contains(out, "Result: PASS (8 of 8 checks passed; contract mode, offline)") {
			t.Fatalf("exit %d\n%s", code, out)
		}
		code, out, _ = runCLI("plugin", "test", "--json", path)
		if code != 0 || strings.Contains(out, `"target"`) || strings.Contains(out, "live-") || !strings.Contains(out, `"mode": "contract"`) {
			t.Fatalf("exit %d\n%s", code, out)
		}
	})
}
