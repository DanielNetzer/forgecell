package cli

import (
	"bytes"
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/onboarding"
	"strings"
	"testing"
)

func TestMetaOptInSavedReview(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	p, err := onboarding.SaveProposal(dir, "kind: formula\nid: sample\nharness: {binding: byo, command: [/coding]}\natoms:\n - {type: learn, binding: byo, command: [/meta], timeoutMs: 900000}\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "--json"} {
		args := []string{"init", "--lab", dir, "--review", p.ID}
		if mode != "" {
			args = append(args, mode)
		}
		var out, stderr bytes.Buffer
		if code := Run(context.Background(), args, strings.NewReader(""), &out, &stderr, "test"); code != 0 {
			t.Fatal(code, stderr.String())
		}
		if !strings.Contains(out.String(), "/meta") || !strings.Contains(out.String(), "containment") {
			t.Fatal(out.String())
		}
	}
}

func TestMetaOptInArgumentValidation(t *testing.T) {
	for _, args := range [][]string{{"--meta-command", "[]"}, {"--meta-command", "null"}, {"--meta-command", "[\"\"]"}, {"--meta-harness", ""}, {"--meta-harness", "codex", "--review", "x"}, {"--meta-harness", "codex", "--meta-command", "[\"/meta\"]"}} {
		var out, stderr bytes.Buffer
		code := Run(context.Background(), append([]string{"init", "--lab", t.TempDir()}, args...), strings.NewReader(""), &out, &stderr, "test")
		if code != 1 || strings.Contains(stderr.String(), "flag provided but not defined") {
			t.Fatal(args, code, stderr.String())
		}
	}
}
