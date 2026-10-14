package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// TestUsagePresentShowsSummaryAndMismatch renders the per-Molecule usage summary in
// presentRun: effective models, the coverage line with the list-price label, an
// explicit unavailable line item, and the non-blocking mismatch notice (#149).
func TestUsagePresentShowsSummaryAndMismatch(t *testing.T) {
	reported := &harness.Usage{
		Total:           harness.TokenCategories{Input: i64(100), Output: i64(50), CacheRead: i64(10), CacheCreation: i64(5)},
		EffectiveModels: []string{"claude-opus-4-8"},
		TotalCostUSD:    f64(0.25),
		CostBasis:       "list",
		ModelMismatch:   true,
	}
	r := molecule.Record{
		ID:              "mol-1",
		HarnessAttempts: []molecule.HarnessAttempt{{ID: "abc123", Usage: reported}},
		ReviewAttempts:  []molecule.ReviewAttempt{{Ordinal: 0}}, // unavailable
	}
	var out bytes.Buffer
	presentRun(&out, r)
	got := out.String()
	for _, want := range []string{
		"Usage and cost (evidence, never an approval):",
		"coding attempt abc123: effective models claude-opus-4-8",
		"review attempt review-0: unavailable",
		"cost reported for 1 of 2 harness attempts (CLI list-price estimate)",
		"$0.250000 total",
		"NOTICE: effective models differ from the pinned model for 1 of 2 harness attempt(s)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in presentRun output:\n%s", want, got)
		}
	}
}

// TestUsagePresentHiddenWhenNoneReported shows nothing when no attempt reported usage.
func TestUsagePresentHiddenWhenNoneReported(t *testing.T) {
	r := molecule.Record{ID: "mol-1", HarnessAttempts: []molecule.HarnessAttempt{{ID: "abc"}}}
	var out bytes.Buffer
	presentRun(&out, r)
	if strings.Contains(out.String(), "Usage and cost") {
		t.Fatalf("usage summary shown with no reported usage:\n%s", out.String())
	}
}
