package mcpadapter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

func TestMain(m *testing.M) { os.Exit(runMain(m)) }

// runMain points TMPDIR at a private root removed afterwards, so the shared
// fake server build and every test's temporary directory stay out of the
// user's real temp root, then builds the fake server once for the package.
func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "mcpadapter-tmp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	if err := os.Setenv("TMPDIR", dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return plugintest.Main(m)
}

// fixture is a Lab with one pinned fake plugin manifest and a 0700 run root.
type fixture struct {
	lab     string
	root    string
	opts    Options
	records []CallRecord
}

const manifestRel = "plugins/fake/plugin.json"

// newFixture writes the fake server's manifest for kind and mode into a fresh
// Lab. The plugin environment is empty, so nothing of the caller's leaks in.
func newFixture(t *testing.T, kind, mode string) *fixture {
	t.Helper()
	server := plugintest.Server(t)
	lab := plugintest.TempDir(t)
	raw := plugintest.Manifest(t, server, kind, mode)
	dir := filepath.Join(lab, "plugins", "fake")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, filepath.FromSlash(manifestRel)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(lab, "runs")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	f := &fixture{lab: lab, root: root}
	f.opts = Options{
		LabDir:    lab,
		Ref:       Ref{Manifest: manifestRel, SHA256: hex.EncodeToString(sum[:])},
		RunRoot:   root,
		LookupEnv: func(string) (string, bool) { return "", false },
		Observer:  func(r CallRecord) { f.records = append(f.records, r) },
	}
	return f
}

// fired is a grace channel that has already fired, for servers that never
// exit on their own.
func fired() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}

// requireCleaned fails unless every run directory was removed, which only
// Shutdown does.
func (f *fixture) requireCleaned(t *testing.T) {
	t.Helper()
	left, err := os.ReadDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("Shutdown did not run: %d run directories were left", len(left))
	}
}

// requireReason checks the fixed reason and that the error text is exactly the
// display-safe form, carrying no server text, stderr, secret or local path.
func (f *fixture) requireReason(t *testing.T, err error, want mcp.Reason, display string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("no error, want %s", want)
	}
	if got := mcp.ReasonOf(err); got != want {
		t.Fatalf("reason = %s (%v), want %s", got, err, want)
	}
	if err.Error() != display {
		t.Fatalf("Error() = %q, want %q", err.Error(), display)
	}
	for _, leak := range []string{plugintest.PlantedSecret, "Bearer", "fake:", "debug", f.lab, f.root, "/"} {
		if leak == "/" && strings.Contains(display, "/") {
			continue
		}
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("Error() leaks %q: %q", leak, err.Error())
		}
	}
	e, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, want *Error", err)
	}
	if strings.Contains(e.Diagnostics, plugintest.PlantedSecret) || strings.Contains(e.Diagnostics, f.root) {
		t.Fatalf("diagnostics leak a secret or the run root:\n%s", e.Diagnostics)
	}
	return e
}
