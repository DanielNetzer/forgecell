package onboarding

import (
	"context"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"os"
	"path/filepath"
	"testing"
)

func TestBoundReadiness(t *testing.T) {
	self, _ := os.Executable()
	for _, tc := range []struct {
		name    string
		command []string
		status  string
		called  bool
	}{
		{"saved missing adapter", []string{"/missing/forgecell", "__adapter", "codex", "/saved/codex"}, "blocked", false},
		{"custom never executed", []string{"arbitrary-script", "--do-work"}, "unknown", false},
		{"provider mismatch", []string{self, "__adapter", "cursor", "/saved/codex"}, "blocked", false},
		{"saved provider checked", []string{self, "__adapter", "codex", "/saved/codex"}, "ready", true},
		{"unbound", nil, "blocked", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: tc.command}, t.TempDir(), func(_ context.Context, o process.Options) process.Result {
				called = true
				if o.Argv[0] != "/saved/codex" {
					t.Fatal("checked PATH provider instead of saved provider")
				}
				return process.Result{OK: true, Stdout: "codex logged in --sandbox --output-last-message --output-schema --ephemeral"}
			})
			if c.Readiness != tc.status || called != tc.called {
				t.Fatalf("%+v called=%v", c, called)
			}
		})
	}
	t.Run("different executable", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "forgecell")
		os.WriteFile(other, []byte("#!/bin/sh\nexit 0\n"), 0700)
		c := ProbeBinding(context.Background(), formula.Binding{Binding: "codex", Command: []string{other, "__adapter", "codex", "codex"}}, "", nil)
		if c.Readiness != "blocked" {
			t.Fatal(c)
		}
	})
}

func TestCustomCapabilitiesPreserveCommand(t *testing.T) {
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "custom", Command: []string{"untrusted-command"}}, t.TempDir(), func(context.Context, process.Options) process.Result {
		t.Fatal("custom command executed")
		return process.Result{}
	})
	if c.Capabilities.Coding.State != "unknown" || c.Capabilities.Meta.State != "unknown" || c.Capabilities.Analysis.State != "unsupported" || c.WorkflowCapability().State != "unsupported" {
		t.Fatalf("%+v", c)
	}
}
