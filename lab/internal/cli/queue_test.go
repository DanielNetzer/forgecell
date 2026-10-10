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

func TestIssueQueueCLI(t *testing.T) {
	bin := t.TempDir()
	lab := filepath.Join(t.TempDir(), "absent")
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\ncat <<'DATA'\n"+raw+"\nDATA\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	issue := `{"number":1,"title":"hello","state":"open","html_url":"https://github.com/o/r/issues/1"}`
	write("[" + issue + "]")
	for _, jsonMode := range []bool{false, true} {
		args := []string{"issues", "--repo", "o/r", "--lab", lab}
		if jsonMode {
			args = append(args, "--json")
		}
		var out, stderr bytes.Buffer
		if c := Run(context.Background(), args, nil, &out, &stderr, "test"); c != 0 {
			t.Fatalf("%d %s", c, &stderr)
		}
		if !strings.Contains(out.String(), "untouched") || jsonMode && !json.Valid(out.Bytes()) {
			t.Fatal(out.String())
		}
	}
	write(issue)
	var out, stderr bytes.Buffer
	if c := Run(context.Background(), []string{"issues", "--repo", "o/r", "--select", "1", "--lab", lab, "--json"}, nil, &out, &stderr, "test"); c != 0 || !strings.Contains(out.String(), `"selected":true`) {
		t.Fatalf("%d %s %s", c, &out, &stderr)
	}
	if _, err := os.Stat(lab); !os.IsNotExist(err) {
		t.Fatal("created lab", err)
	}
	for _, extra := range [][]string{{"--approve", "digest"}, {"--run"}, {"--page", "0"}, {"--page-size", "101"}, {"--select", "x/r#1"}, {"--select", "1", "--page", "1"}, {"stray"}} {
		out.Reset()
		stderr.Reset()
		args := append([]string{"issues", "--repo", "o/r", "--lab", lab}, extra...)
		if Run(context.Background(), args, nil, &out, &stderr, "test") == 0 {
			t.Fatal(args)
		}
	}
	write("null")
	out.Reset()
	stderr.Reset()
	if c := Run(context.Background(), []string{"issues", "--repo", "o/r", "--lab", lab, "--json"}, nil, &out, &stderr, "test"); c == 0 || !json.Valid(out.Bytes()) || !strings.Contains(out.String(), "unavailable") {
		t.Fatalf("%d %s %s", c, &out, &stderr)
	}
	out.Reset()
	if Run(context.Background(), []string{"help"}, nil, &out, &stderr, "test") != 0 {
		t.Fatal(&stderr)
	}
	for _, s := range []string{"issues --repo", "--link-evaluation DIR", "--baseline-ledger ID", "--evaluation-parent DIR", "checks --lab DIR --molecule ID"} {
		if !strings.Contains(out.String(), s) {
			t.Fatal("missing help", s)
		}
	}
}
func TestIssueQueueExistingLabUnchanged(t *testing.T) {
	lab := t.TempDir()
	dir := filepath.Join(lab, "ledgers")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"schemaVersion":"v0","kind":"molecule","id":"m1","formulaId":"f","status":"running","issue":{"repo":"o/r","number":1},"startedAt":"2026-01-01T00:00:00Z"}`)
	file := filepath.Join(dir, "m1.json")
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nprintf '%s' '[{\"number\":1,\"title\":\"hello\",\"state\":\"open\",\"html_url\":\"https://github.com/o/r/issues/1\"}]'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if c := Run(context.Background(), []string{"issues", "--repo", "o/r", "--lab", lab, "--json"}, nil, &out, &stderr, "test"); c != 0 {
		t.Fatal(c, &stderr)
	}
	var report issueQueueReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Issues) != 1 || report.Issues[0].Molecule.Active != 1 || report.Issues[0].Molecule.Latest == nil || report.Issues[0].Molecule.Latest.Status != "running" {
		t.Fatal(report)
	}
	after, err := os.ReadFile(file)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("ledger changed", err)
	}
	for _, d := range []string{lab, dir} {
		entries, err := os.ReadDir(d)
		if err != nil || len(entries) != 1 {
			t.Fatal(entries, err)
		}
	}
}
