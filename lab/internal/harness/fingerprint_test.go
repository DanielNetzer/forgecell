package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprintIgnoresForgecellBuildButNotProtocolArgvOrProvider(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	provider := filepath.Join(t.TempDir(), "codex")
	os.WriteFile(provider, []byte("v1"), 0700)
	buildA, _ := fakeBuild(t, "1.0.0", AdapterProtocolVersion)
	buildB, _ := fakeBuild(t, "2.5.0", AdapterProtocolVersion)
	os.WriteFile(buildB, []byte("#!/bin/sh\n# a different build\n"), 0700)
	a, err := Fingerprint([]string{buildA, "__adapter", "codex", provider})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Fingerprint([]string{buildB, "__adapter", "codex", provider})
	if err != nil || a != b {
		t.Fatalf("a Forgecell rebuild or upgrade within one protocol changed the approval fingerprint: %v", err)
	}
	if missing, err := Fingerprint([]string{"/missing/forgecell", "__adapter", "codex", provider}); err != nil || missing != a {
		t.Fatalf("launcher availability must not gate the fingerprint: %v", err)
	}
	bumped, err := fingerprint([]string{buildA, "__adapter", "codex", provider}, "3")
	if err != nil || bumped == a {
		t.Fatal("protocol bump invisible")
	}
	other, err := Fingerprint([]string{buildA, "__adapter", "claude-code", provider})
	if err != nil || other == a {
		t.Fatal("adapter argv contract change invisible")
	}
	os.WriteFile(provider, []byte("v2"), 0700)
	drifted, err := Fingerprint([]string{buildA, "__adapter", "codex", provider})
	if err != nil || drifted == a {
		t.Fatal("provider drift invisible")
	}
}

func TestFingerprintStillBindsCustomCommandsToTheirExecutable(t *testing.T) {
	script := filepath.Join(t.TempDir(), "custom")
	os.WriteFile(script, []byte("one"), 0700)
	a, err := Fingerprint([]string{script, "--run"})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(script, []byte("two"), 0700)
	b, err := Fingerprint([]string{script, "--run"})
	if err != nil || a == b {
		t.Fatal("custom executable drift invisible")
	}
	if _, err := Fingerprint([]string{"/missing/custom"}); err == nil {
		t.Fatal("missing custom executable accepted")
	}
}

func TestFingerprintIncludesExecutableAndModelConfiguration(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "provider")
	config := filepath.Join(dir, "config.toml")
	os.WriteFile(exe, []byte("v1"), 0700)
	os.WriteFile(config, []byte("model='one'"), 0600)
	a, e := fingerprintFiles([]string{exe, config})
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(config, []byte("model='two'"), 0600)
	b, e := fingerprintFiles([]string{exe, config})
	if e != nil || a == b {
		t.Fatal("model drift invisible")
	}
	os.WriteFile(exe, []byte("v2"), 0700)
	c, e := fingerprintFiles([]string{exe, config})
	if e != nil || b == c {
		t.Fatal("binary drift invisible")
	}
}
