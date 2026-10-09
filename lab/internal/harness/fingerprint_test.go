package harness

import (
	"os"
	"path/filepath"
	"testing"
)

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
