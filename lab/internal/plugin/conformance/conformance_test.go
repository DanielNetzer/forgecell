package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

func TestMain(m *testing.M) { os.Exit(plugintest.Main(m)) }

func fired() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

func run(t *testing.T, kind, mode string) (Report, string) {
	t.Helper()
	server := plugintest.Server(t)
	path := plugintest.WriteManifest(t, plugintest.Manifest(t, server, kind, mode))
	root := plugintest.TempDir(t)
	opts := Options{Root: root, Grace: fired}
	if mode == "slow" {
		// The slow server never answers, so any timeout gives the same outcome.
		opts.Timeout = 10 * time.Millisecond
	}
	r := Run(context.Background(), path, opts)
	if left, err := os.ReadDir(root); err != nil || len(left) != 0 {
		t.Fatalf("the run left %d entries behind (%v)", len(left), err)
	}
	return r, path
}

func TestGoodPluginsPassEveryCheck(t *testing.T) {
	for _, kind := range []string{"tracker", "forge", "reviewer"} {
		r, _ := run(t, kind, "good")
		if !r.Passed || len(r.Checks) != len(checkOrder) {
			t.Fatalf("%s: %+v", kind, r)
		}
		for _, c := range r.Checks {
			if c.Status != Pass {
				t.Fatalf("%s: %s did not pass: %+v", kind, c.ID, c)
			}
		}
		if r.Plugin != "fake-"+kind || r.Contract != "forgecell."+kind+"/v1" || len(r.ManifestSHA256) != 64 || r.Manifest != "plugin.json" {
			t.Fatalf("%s report identity: %+v", kind, r)
		}
	}
}

func TestFailingPluginsNameTheFailedCheck(t *testing.T) {
	cases := []struct {
		kind, mode, check, reason string
	}{
		{"forge", "wrong-contract", CheckInitialize, "contract-mismatch"},
		{"tracker", "missing-tool", CheckToolsList, "contract-mismatch"},
		{"reviewer", "crash", CheckDescribe, "failed"},
		{"forge", "bad-input-crash", CheckMalformed, "failed"},
		{"tracker", "oversized", CheckDescribe, "output-too-large"},
		{"reviewer", "slow", CheckDescribe, "timeout"},
		{"tracker", "bad-framing", CheckInitialize, "schema-invalid"},
	}
	for _, c := range cases {
		r, _ := run(t, c.kind, c.mode)
		failed, ok := r.Failed()
		if r.Passed || !ok || failed.ID != c.check || failed.Reason != c.reason || failed.Status != Fail {
			t.Fatalf("%s/%s: first failure %+v, want %s %s", c.kind, c.mode, failed, c.check, c.reason)
		}
		bound := map[string]string{"oversized": CheckOutputBound, "slow": CheckTimeoutBound}[c.mode]
		for _, check := range r.Checks {
			if check.ID == bound && check.Status != Fail {
				t.Fatalf("%s: %s = %+v", c.mode, bound, check)
			}
		}
	}
}

func TestServerRequestsAreRefusedDuringARun(t *testing.T) {
	r, _ := run(t, "forge", "server-request")
	if !r.Passed {
		t.Fatalf("a server request broke the run: %+v", r)
	}
}

func TestUnreadableManifestFailsStart(t *testing.T) {
	r := Run(context.Background(), filepath.Join(plugintest.TempDir(t), "missing.json"), Options{Root: plugintest.TempDir(t), Grace: fired})
	if r.Passed || r.Checks[0].ID != CheckStart || r.Checks[0].Reason != "not-installed" {
		t.Fatalf("report = %+v", r)
	}
	for _, c := range r.Checks[1:] {
		if c.Status != Skip {
			t.Fatalf("%s ran after a failed start", c.ID)
		}
	}
}

func TestTamperedServerFailsStart(t *testing.T) {
	server := plugintest.Server(t)
	path := plugintest.WriteManifest(t, plugintest.Manifest(t, server, "tracker", "good"))
	if err := os.WriteFile(server.Path, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := Run(context.Background(), path, Options{Root: plugintest.TempDir(t), Grace: fired})
	if failed, _ := r.Failed(); failed.ID != CheckStart || failed.Reason != "manifest-changed" {
		t.Fatalf("report = %+v", r)
	}
}

func TestReportsCarryNoLocalPaths(t *testing.T) {
	r, path := run(t, "reviewer", "slow")
	var text, js bytes.Buffer
	if err := r.WriteText(&text); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteJSON(&js); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{text.String(), js.String()} {
		if strings.Contains(out, filepath.Dir(path)) || strings.Contains(out, plugintest.PlantedSecret) || strings.Contains(out, os.TempDir()) {
			t.Fatalf("report leaks a local path or stderr:\n%s", out)
		}
	}
	var decoded Report
	if err := json.Unmarshal(js.Bytes(), &decoded); err != nil || decoded.Passed || len(decoded.Checks) != len(checkOrder) {
		t.Fatalf("JSON report = %+v, %v", decoded, err)
	}
	if !strings.Contains(text.String(), "FAIL describe: timeout") || !strings.Contains(text.String(), "Result: FAIL") {
		t.Fatalf("text report:\n%s", text.String())
	}
}
