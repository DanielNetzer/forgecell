package molecule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/resources"
)

// codexCacheFixture binds the approved Formula to a fake codex executable behind the
// native adapter. It records its argv and Go cache environment outside the checkout.
func codexCacheFixture(t *testing.T) (Options, string, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	o, repo := fixture(t)
	root := filepath.Dir(repo)
	provider := filepath.Join(root, "codex")
	argv := filepath.Join(root, "argv.txt")
	env := filepath.Join(root, "env.txt")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > %q\nenv | grep -E '^(GOCACHE|GOTMPDIR)=' | sort > %q\nout=\"\"\nwhile [ \"$#\" -gt 0 ]; do\n  if [ \"$1\" = \"--output-last-message\" ]; then shift; out=\"$1\"; fi\n  shift\ndone\ncat >/dev/null\nprintf changed > code.txt\nprintf '%%s' '{\"schemaVersion\":\"v1\",\"outcome\":\"completed\",\"reason\":\"implemented\",\"paths\":[]}' > \"$out\"\n", argv, env)
	if err = writeStub(provider, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal([]string{filepath.Join(root, "harness")})
	command, _ := json.Marshal([]string{exe, "__adapter", "codex", provider})
	recipe := strings.Replace(string(raw), string(old), string(command), 1)
	recipe = strings.Replace(recipe, "binding: custom", "binding: codex", 1)
	if !strings.Contains(recipe, string(command)) {
		t.Fatal("fixture Formula was not rewritten")
	}
	if err = os.WriteFile(file, []byte(recipe), 0600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, recipe, "codex-cache")
	return o, argv, env
}

func TestHarnessCacheRecordedOnCodexCodingAttempt(t *testing.T) {
	t.Parallel()
	o, argv, env := codexCacheFixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	want := resources.HarnessCacheAt(filepath.Join(o.LabDir, "caches", "harness"))
	if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].HarnessCache == nil || *r.HarnessAttempts[0].HarnessCache != want {
		t.Fatalf("attempt did not record the harness cache: %+v", r.HarnessAttempts)
	}
	data, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved Record
	if err = json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.HarnessAttempts[0].HarnessCache == nil || *saved.HarnessAttempts[0].HarnessCache != want {
		t.Fatalf("persisted ledger lost the harness cache: %+v", saved.HarnessAttempts[0])
	}
	rawArgv, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(rawArgv)), "\n")
	if i := slices.Index(args, "--add-dir"); i < 0 || args[i+1] != want.Root {
		t.Fatalf("provider argv lacks --add-dir %s: %q", want.Root, args)
	}
	rawEnv, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(rawEnv)); !slices.Equal(got, []string{"GOCACHE=" + want.BuildCache, "GOTMPDIR=" + want.TmpDir}) {
		t.Fatalf("provider environment %q", got)
	}
	for _, dir := range []string{want.BuildCache, want.TmpDir} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatalf("%s: %v %v", dir, info, err)
		}
	}
}

func TestHarnessCacheSymlinkStartsNoCodingHarness(t *testing.T) {
	t.Parallel()
	o, argv, _ := codexCacheFixture(t)
	if err := os.MkdirAll(filepath.Join(o.LabDir, "caches"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(o.LabDir, "caches", "harness")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "failed" || len(r.HarnessAttempts) != 0 {
		t.Fatalf("a symlinked cache reached coding: %s %+v", r.Status, r.HarnessAttempts)
	}
	if detail := r.Readiness.Events[len(r.Readiness.Events)-1].Detail; !strings.Contains(detail, "symbolic link") || !strings.Contains(detail, link) {
		t.Fatalf("refusal does not name the path: %q", detail)
	}
	if _, err = os.Stat(argv); !os.IsNotExist(err) {
		t.Fatal("provider started despite an unsafe harness cache")
	}
}

func TestHarnessCacheAbsentForOtherBindingsAndEarlierLedgers(t *testing.T) {
	t.Parallel()
	o, _ := claudeAllowlistFixture(t)
	r, err := runApprovedFixture(t, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.HarnessAttempts) != 1 || r.HarnessAttempts[0].HarnessCache != nil {
		t.Fatalf("a Claude Code attempt recorded a harness cache: %+v", r.HarnessAttempts)
	}
	if _, err = os.Lstat(filepath.Join(o.LabDir, "caches")); !os.IsNotExist(err) {
		t.Fatalf("a Claude Code run created the harness cache: %v", err)
	}
	var legacy HarnessAttempt
	if err = json.Unmarshal([]byte(`{"id":"legacy","planDigest":"d","result":{}}`), &legacy); err != nil || legacy.HarnessCache != nil {
		t.Fatalf("an earlier attempt gained a harness cache: %+v %v", legacy, err)
	}
}
