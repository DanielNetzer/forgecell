package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultLabLivesOutsideCheckoutAndTracksCheckoutIdentity(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	t.Setenv("HOME", home)
	t.Setenv("FORGECELL_HOME", "")
	t.Setenv("FORGECELL_LAB", "")
	t.Setenv("PATH", filepath.Join(base, "empty-bin"))
	for _, name := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(base, name, "forgecell", ".git"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	first := filepath.Join(base, "one", "forgecell")
	second := filepath.Join(base, "two", "forgecell")
	if err := os.MkdirAll(filepath.Join(first, "lab", "internal"), 0700); err != nil {
		t.Fatal(err)
	}
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)
	location := func(dir string) string {
		t.Helper()
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		Run(context.Background(), []string{"doctor", "--json"}, strings.NewReader(""), &out, &stderr, "dev")
		var report struct {
			LabDir string `json:"labDir"`
		}
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("doctor output %q, error %q: %v", out.String(), stderr.String(), err)
		}
		return report.LabDir
	}
	lab := location(first)
	if !strings.HasPrefix(lab, filepath.Join(home, ".forgecell", "labs")+string(filepath.Separator)) {
		t.Fatalf("Lab was not under the user's home: %q", lab)
	}
	if nested := location(filepath.Join(first, "lab", "internal")); nested != lab {
		t.Fatalf("same checkout resolved to different Labs: %q vs %q", lab, nested)
	}
	if other := location(second); other == lab {
		t.Fatal("different checkouts with the same name shared one Lab")
	}
	if _, err := os.Stat(filepath.Join(first, ".forgecell")); !os.IsNotExist(err) {
		t.Fatal("doctor created a Lab inside the checkout")
	}

	if err := os.MkdirAll(filepath.Join(lab, "ledgers"), 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"schemaVersion":"v0","kind":"molecule","id":"mol-test","formulaId":"f","status":"waiting"}`)
	if err := os.WriteFile(filepath.Join(lab, "ledgers", "mol-test.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(first); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"ledger", "mol-test"}, strings.NewReader(""), &out, &stderr, "dev"); code != 0 || !bytes.Equal(bytes.TrimSpace(out.Bytes()), raw) {
		t.Fatalf("ledger did not use home Lab: code %d, out %q, err %q", code, out.String(), stderr.String())
	}
}

func TestInitDoesNotSilentlyReplaceAnExistingCheckoutLab(t *testing.T) {
	repo := t.TempDir()
	legacy := filepath.Join(repo, ".forgecell")
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "lab.json"), []byte(`{"activeFormulaId":"old"}`), 0600); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	t.Setenv("FORGECELL_HOME", "")
	t.Setenv("FORGECELL_LAB", "")
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"init", "--json"}, strings.NewReader(""), &out, &stderr, "dev")
	if code == 0 || !strings.Contains(stderr.String(), "existing checkout Lab") || !strings.Contains(stderr.String(), "--lab") {
		t.Fatalf("legacy Lab was silently ignored: code %d, out %q, err %q", code, out.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".forgecell", "labs")); !os.IsNotExist(err) {
		t.Fatal("init wrote a new Lab while an existing checkout Lab was present")
	}
}

func TestRelativeCustomHomeIsStableWithinCheckout(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "lab", "internal")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FORGECELL_LAB", "")
	t.Setenv("FORGECELL_HOME", "../forgecell-data")
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	rootLab, err := defaultLabDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	nestedLab, err := defaultLabDir()
	if err != nil {
		t.Fatal(err)
	}
	if rootLab != nestedLab {
		t.Fatalf("same checkout mapped to different Labs with relative FORGECELL_HOME: %q vs %q", rootLab, nestedLab)
	}
}
