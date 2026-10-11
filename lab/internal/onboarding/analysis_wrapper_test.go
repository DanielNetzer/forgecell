package onboarding

import (
	"context"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

func TestAnalysisWrapperInspectionReportsDeclaredUnverifiedWithoutExecuting(t *testing.T) {
	called := false
	run := func(context.Context, process.Options) process.Result {
		called = true
		return process.Result{}
	}
	b := formula.Binding{Binding: "custom", Command: []string{"/nonexistent/forgecell-custom-harness"}, AnalysisWrapper: &formula.AnalysisWrapperRef{Manifest: "analysis-wrappers/w.json", SHA256: strings.Repeat("a", 64)}}
	c := ProbeBinding(context.Background(), b, t.TempDir(), run)
	if called {
		t.Fatal("inspection must not execute the wrapper or the custom command")
	}
	if c.Readiness != "unknown" || c.Capabilities.Analysis.State != "unknown" || c.Capabilities.AnalysisControls.State != "unknown" {
		t.Fatalf("%+v", c)
	}
	for _, text := range []string{c.Capabilities.Analysis.Reason, c.Detail} {
		if !strings.Contains(text, "declared") || !strings.Contains(text, "unverified") {
			t.Fatalf("report must say the capability is declared and unverified: %q", text)
		}
	}
	if w := c.WorkflowCapability(); w.State != "unknown" {
		t.Fatalf("a declared wrapper must not look ready: %+v", w)
	}
}

func TestAnalysisWrapperHistoricalCustomBindingIsNotUpgraded(t *testing.T) {
	c := ProbeBinding(context.Background(), formula.Binding{Binding: "custom", Command: []string{"/nonexistent/forgecell-custom-harness"}}, t.TempDir(), nil)
	if c.Capabilities.Analysis.State != "unsupported" || c.Capabilities.AnalysisControls.State != "unsupported" || c.Readiness != "unknown" {
		t.Fatalf("a custom binding without a reference keeps unsupported analysis: %+v", c)
	}
	if w := c.WorkflowCapability(); w.State != "unsupported" {
		t.Fatalf("%+v", w)
	}
}
