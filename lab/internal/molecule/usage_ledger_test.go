package molecule

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/harness"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// cat builds a fully-reported token category set.
func cat(in, out, cr, cc int64) harness.TokenCategories {
	return harness.TokenCategories{Input: i64(in), Output: i64(out), CacheRead: i64(cr), CacheCreation: i64(cc)}
}

// usageWith builds a reported usage object for one effective model with a list-price
// cost and the given mismatch flag.
func usageWith(model string, cost float64, mismatch bool) *harness.Usage {
	return &harness.Usage{
		Models:          []harness.ModelUsage{{Model: model, Tokens: cat(100, 50, 10, 5), CostUSD: f64(cost), CostBasis: "list"}},
		Total:           cat(100, 50, 10, 5),
		EffectiveModels: []string{model},
		TotalCostUSD:    f64(cost),
		CostBasis:       "list",
		ModelMismatch:   mismatch,
	}
}

// TestUsageSummaryCoversEveryRoleAndUnavailable renders a per-Molecule summary over
// analysis, coding and review attempts, totals the token categories across models,
// shows coverage "N of M" with the list-price label, keeps an unavailable attempt as
// an explicit line item (#149 item 7), and raises a non-blocking mismatch notice.
func TestUsageSummaryCoversEveryRoleAndUnavailable(t *testing.T) {
	r := Record{
		AnalysisAttempts: []AnalysisAttempt{{Ordinal: 1, Usage: usageWith("claude-opus-4-8", 0.10, true)}},
		HarnessAttempts:  []HarnessAttempt{{ID: "abc123", Usage: usageWith("claude-opus-4-8", 0.20, false)}},
		ReviewAttempts:   []ReviewAttempt{{Ordinal: 0, Usage: nil}}, // Codex review, say: unavailable
	}
	lines := UsageSummaryLines(r)
	if lines == nil {
		t.Fatal("expected a summary")
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"analysis attempt 1: effective models claude-opus-4-8",
		"MODEL MISMATCH vs pin (non-blocking)",
		"coding attempt abc123:",
		"review attempt review-0: unavailable",
		"Totals: input 200, output 100, cache-read 20, cache-creation 10 tokens across effective models claude-opus-4-8",
		"cost reported for 2 of 3 harness attempts (CLI list-price estimate)",
		"$0.300000 total",
		"NOTICE: effective models differ from the pinned model for 1 of 3 harness attempt(s); this is non-blocking and never fails readiness or a gate.",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

// TestUsageSummaryHiddenWhenNoneReported shows nothing until an attempt reports usage
// (#149 item 11): an attempt with no usage object yields no summary.
func TestUsageSummaryHiddenWhenNoneReported(t *testing.T) {
	r := Record{HarnessAttempts: []HarnessAttempt{{ID: "x"}}, AnalysisAttempts: []AnalysisAttempt{{Ordinal: 1}}}
	if lines := UsageSummaryLines(r); lines != nil {
		t.Fatalf("expected no summary, got:\n%s", strings.Join(lines, "\n"))
	}
}

// TestUsageSummaryCostUnavailableButTokensReported covers a Claude run that reported
// tokens but no total_cost_usd: the attempt still appears, cost reads unavailable, and
// coverage is 0 of 1.
func TestUsageSummaryCostUnavailableButTokensReported(t *testing.T) {
	u := &harness.Usage{
		Total:           harness.TokenCategories{Input: i64(7)},
		EffectiveModels: []string{"claude-opus-4-8"},
	}
	r := Record{HarnessAttempts: []HarnessAttempt{{ID: "y", Usage: u}}}
	joined := strings.Join(UsageSummaryLines(r), "\n")
	for _, want := range []string{"coding attempt y: effective models claude-opus-4-8", "output unreported", "cost unavailable", "Reported cost: unavailable — cost reported for 0 of 1 harness attempts"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

// TestUsageOldLedgerDecodesAsUnavailable proves an additive field: a ledger written
// before #149 decodes with no error and renders no usage (never zero).
func TestUsageOldLedgerDecodesAsUnavailable(t *testing.T) {
	old := []byte(`{
		"schemaVersion":"v0",
		"analysisAttempts":[{"ordinal":1,"snapshotSha256":"s","startedAt":"t","status":"completed"}],
		"harnessAttempts":[{"id":"abc","planDigest":"d","model":"claude-opus-4-8"}],
		"reviewAttempts":[{"ordinal":0,"harnessAttemptId":"abc","planDigest":"d","tree":"t","startedAt":"t","status":"no-high-findings","high":0}]
	}`)
	var r Record
	if err := json.Unmarshal(old, &r); err != nil {
		t.Fatalf("old ledger failed to decode: %v", err)
	}
	if r.HarnessAttempts[0].Usage != nil || r.AnalysisAttempts[0].Usage != nil || r.ReviewAttempts[0].Usage != nil {
		t.Fatal("absent usage should decode as nil, not zero")
	}
	if lines := UsageSummaryLines(r); lines != nil {
		t.Fatalf("old ledger should render no usage summary, got:\n%s", strings.Join(lines, "\n"))
	}
}

// TestUsageRoundTripsThroughLedger keeps the recorded usage object through a JSON
// marshal/unmarshal cycle, as the ledger does.
func TestUsageRoundTripsThroughLedger(t *testing.T) {
	r := Record{HarnessAttempts: []HarnessAttempt{{ID: "abc", Usage: usageWith("claude-opus-4-8", 0.5, false)}}}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back Record
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	u := back.HarnessAttempts[0].Usage
	if u == nil || u.TotalCostUSD == nil || *u.TotalCostUSD != 0.5 || u.CostBasis != "list" || u.Total.Input == nil || *u.Total.Input != 100 {
		t.Fatalf("usage not preserved: %+v", u)
	}
}

// TestUsageSummaryUnreportedCategoriesAndTruncation renders an unreported category as
// "unreported" (never 0) both per attempt and in the totals, and shows "+N more" for
// models the parser dropped from untrusted output (#149 review fixes 1 and 6).
func TestUsageSummaryUnreportedCategoriesAndTruncation(t *testing.T) {
	u := &harness.Usage{
		Models:          []harness.ModelUsage{{Model: "claude-opus-4-8", Tokens: harness.TokenCategories{Input: i64(5)}}},
		Total:           harness.TokenCategories{Input: i64(5)},
		EffectiveModels: []string{"claude-opus-4-8"},
		TruncatedModels: 3,
	}
	r := Record{HarnessAttempts: []HarnessAttempt{{ID: "z", Usage: u}}}
	joined := strings.Join(UsageSummaryLines(r), "\n")
	for _, want := range []string{
		"output unreported, cache-read unreported, cache-creation unreported",
		"(+3 more)",
		"Totals: input 5, output unreported, cache-read unreported, cache-creation unreported",
		"meta/formula-improvement harness usage is deferred to slice B",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

// TestUsageSummaryBasisLabels labels a cost whose basis is empty as "basis
// unavailable" and never prints a bare dollar figure; disagreeing attempt bases make
// the aggregate "mixed" (#149 review fix 4).
func TestUsageSummaryBasisLabels(t *testing.T) {
	noBasis := &harness.Usage{Total: cat(1, 1, 1, 1), EffectiveModels: []string{"claude-opus-4-8"}, TotalCostUSD: f64(0.10)}
	listBasis := usageWith("claude-opus-4-8", 0.20, false)
	r := Record{HarnessAttempts: []HarnessAttempt{{ID: "a", Usage: noBasis}, {ID: "b", Usage: listBasis}}}
	joined := strings.Join(UsageSummaryLines(r), "\n")
	for _, want := range []string{
		"cost $0.100000 (basis unavailable)",
		"cost $0.200000 (CLI list-price estimate)",
		"Reported cost: $0.300000 total — cost reported for 2 of 2 harness attempts (mixed cost basis; see per-attempt line items)",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}
