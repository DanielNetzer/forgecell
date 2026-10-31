package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/conformance"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

func firedGrace() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

// usePluginTestOptions keeps plugin runs under the test's temporary directory
// and fires the shutdown grace at once.
func usePluginTestOptions(t *testing.T, timeout time.Duration) string {
	t.Helper()
	root := plugintest.TempDir(t)
	previous := pluginTestOptions
	pluginTestOptions = func() conformance.Options {
		return conformance.Options{Root: root, LookupEnv: func(string) (string, bool) { return "", false }, Grace: firedGrace, Timeout: timeout}
	}
	t.Cleanup(func() { pluginTestOptions = previous })
	return root
}

func TestPluginTestAgainstTheFakeServer(t *testing.T) {
	// One build of the fake server serves every case; each case writes its own
	// manifest that pins it.
	server := plugintest.Server(t)
	manifest := func(t *testing.T, kind, mode string) string {
		return plugintest.WriteManifest(t, plugintest.Manifest(t, server, kind, mode))
	}

	t.Run("pass", func(t *testing.T) {
		root := usePluginTestOptions(t, 0)
		path := manifest(t, "forge", "good")
		code, out, errOut := runCLI("plugin", "test", path)
		if code != 0 || !strings.Contains(out, "PASS describe") || !strings.Contains(out, "Result: PASS (8 of 8 checks passed") {
			t.Fatalf("exit %d\n%s%s", code, out, errOut)
		}
		if strings.Contains(out, filepath.Dir(path)) || strings.Contains(out, root) || strings.Contains(out, plugintest.PlantedSecret) {
			t.Fatalf("the report leaks a local path or stderr:\n%s", out)
		}
	})

	t.Run("json", func(t *testing.T) {
		usePluginTestOptions(t, 0)
		code, out, _ := runCLI("plugin", "test", "--json", manifest(t, "tracker", "good"))
		var r conformance.Report
		if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || !r.Passed || r.Plugin != "fake-tracker" || r.Manifest != "plugin.json" {
			t.Fatalf("exit %d, report %+v, %v\n%s", code, r, err, out)
		}
	})

	for _, c := range []struct{ kind, mode, failed string }{
		{"forge", "wrong-contract", "FAIL initialize: contract-mismatch"},
		{"tracker", "missing-tool", "FAIL tools-list: contract-mismatch"},
		{"reviewer", "crash", "FAIL describe: failed"},
		{"forge", "bad-input-crash", "FAIL malformed-input: failed"},
		{"tracker", "oversized", "FAIL describe: output-too-large"},
		{"reviewer", "slow", "FAIL describe: timeout"},
	} {
		t.Run(c.mode, func(t *testing.T) {
			timeout := time.Duration(0)
			if c.mode == "slow" {
				// The slow server never answers, so any timeout gives the same outcome.
				timeout = 10 * time.Millisecond
			}
			root := usePluginTestOptions(t, timeout)
			code, out, _ := runCLI("plugin", "test", manifest(t, c.kind, c.mode), "--json")
			var r conformance.Report
			if err := json.Unmarshal([]byte(out), &r); err != nil || code != 2 || r.Passed {
				t.Fatalf("exit %d, %v\n%s", code, err, out)
			}
			code, out, _ = runCLI("plugin", "test", manifest(t, c.kind, c.mode))
			if code != 2 || !strings.Contains(out, c.failed) || !strings.Contains(out, "Result: FAIL") {
				t.Fatalf("exit %d, want %q\n%s", code, c.failed, out)
			}
			if left, _ := os.ReadDir(root); len(left) != 0 {
				t.Fatalf("the run left %d entries behind", len(left))
			}
		})
	}
}

func TestPluginTestUsage(t *testing.T) {
	usePluginTestOptions(t, 0)
	for _, args := range [][]string{
		{"plugin"},
		{"plugin", "frobnicate"},
		{"plugin", "test"},
		{"plugin", "test", "a.json", "b.json"},
		{"plugin", "test", "--verbose", "a.json"},
	} {
		if code, _, errOut := runCLI(args...); code != 1 || !strings.Contains(errOut, pluginUsage) {
			t.Errorf("%v: exit %d, %q", args, code, errOut)
		}
	}
	missing := filepath.Join(plugintest.TempDir(t), "missing.json")
	code, out, _ := runCLI("plugin", "test", missing)
	if code != 2 || !strings.Contains(out, "FAIL start: not-installed") || strings.Contains(out, filepath.Dir(missing)) {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestHelpListsPluginTest(t *testing.T) {
	_, out, _ := runCLI("--help")
	if !strings.Contains(out, "plugin test <manifest> [--json]") {
		t.Fatal("help does not list plugin test")
	}
}
