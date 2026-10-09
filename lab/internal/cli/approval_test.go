package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/learning"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitFormulaCannotApproveBareYAML(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "formulas"), 0700)
	os.WriteFile(filepath.Join(dir, "lab.json"), []byte(`{"schemaVersion":"v0","activeFormulaId":"active"}`), 0600)
	yaml := "kind: formula\nid: injected\nharness: {command: [should-never-run]}\natoms: [{type: harness}]\n"
	os.WriteFile(filepath.Join(dir, "formulas/injected.yaml"), []byte(yaml), 0600)
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"run", "12", "--lab", dir, "--formula", "injected"}, strings.NewReader(""), &out, &stderr, "test")
	if code == 0 || !strings.Contains(stderr.String(), "init --lab") || !strings.Contains(stderr.String(), "--approve PROPOSAL_ID") {
		t.Fatalf("code=%d error=%s", code, &stderr)
	}
	if _, e := os.Stat(filepath.Join(dir, "workspaces")); !os.IsNotExist(e) {
		t.Fatal("rejection prepared a workspace", e)
	}
}
func TestDirectAdapterRejectedBeforeProvider(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "called")
	provider := filepath.Join(dir, "provider")
	os.WriteFile(provider, []byte("#!/bin/sh\necho called > "+marker+"\n"), 0700)
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"__adapter", "codex", provider}, strings.NewReader(`{"kind":"molecule"}`), &out, &stderr, "test")
	if code == 0 || !strings.Contains(stderr.String(), "unsupported") {
		t.Fatalf("%d %s", code, &stderr)
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("provider invoked", e)
	}
}

func TestInvalidSuggestionInspectionAndApproval(t *testing.T) {
	dir := t.TempDir()
	raw := "kind: formula\nid: historical\nharness: {command: [should-never-run]}\natoms: [{type: harness}]\n"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))
	p := learning.Suggestion{ID: "old", FormulaID: "historical", Status: "pending", OriginalYAML: raw, ProposedYAML: raw + "# historical change\n", OriginalHash: digest, ProposedHash: fmt.Sprintf("%x", sha256.Sum256([]byte(raw+"# historical change\n"))), ProposedReady: true}
	b, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	os.MkdirAll(filepath.Join(dir, "suggestions"), 0700)
	file := filepath.Join(dir, "suggestions/old.json")
	if e = os.WriteFile(file, b, 0600); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"text", "json", "approve", "dismiss"} {
		var out, stderr bytes.Buffer
		args := []string{"suggestion", "old", "--lab", dir}
		if mode != "text" {
			args = append(args, "--"+mode)
		}
		code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test")
		switch mode {
		case "text":
			if code != 0 || !strings.Contains(out.String(), "ticket readiness requires") || strings.Contains(out.String(), "--approve") || !strings.Contains(out.String(), "--dismiss") {
				t.Fatalf("%d %s %s", code, &out, &stderr)
			}
		case "json":
			var got learning.Suggestion
			if code != 0 || json.Unmarshal(out.Bytes(), &got) != nil || got.ProposedReady || got.ReadinessError == "" || got.ProposedYAML != p.ProposedYAML {
				t.Fatalf("%d %s %s", code, &out, &stderr)
			}
		case "approve":
			after, _ := os.ReadFile(file)
			if code != 1 || !strings.Contains(stderr.String(), "ticket readiness requires") || string(after) != string(b) {
				t.Fatalf("%d %s", code, &stderr)
			}
		case "dismiss":
			if code != 0 {
				t.Fatalf("%d %s", code, &stderr)
			}
		}
	}
	for _, name := range []string{"workspaces", "formula-decisions", "lab.json", "formulas", "ledgers"} {
		if _, e := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(e) {
			t.Fatalf("unexpected %s: %v", name, e)
		}
	}
}
