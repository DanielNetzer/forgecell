package molecule

import (
	"encoding/json"
	"math"
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
	t.Parallel()
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
	t.Parallel()
	r := Record{HarnessAttempts: []HarnessAttempt{{ID: "x"}}, AnalysisAttempts: []AnalysisAttempt{{Ordinal: 1}}}
	if lines := UsageSummaryLines(r); lines != nil {
		t.Fatalf("expected no summary, got:\n%s", strings.Join(lines, "\n"))
	}
}

// TestUsageSummaryCostUnavailableButTokensReported covers a Claude run that reported
// tokens but no total_cost_usd: the attempt still appears, cost reads unavailable, and
// coverage is 0 of 1.
func TestUsageSummaryCostUnavailableButTokensReported(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// TestUsageSummaryRecordRendersExactLines pins UsageSummaryLines byte for byte, as
// rendered from the structured UsageSummary (#200), for the every-role fixture.
func TestUsageSummaryRecordRendersExactLines(t *testing.T) {
	t.Parallel()
	r := Record{
		AnalysisAttempts: []AnalysisAttempt{{Ordinal: 1, Usage: usageWith("claude-opus-4-8", 0.10, true)}},
		HarnessAttempts:  []HarnessAttempt{{ID: "abc123", Usage: usageWith("claude-opus-4-8", 0.20, false)}},
		ReviewAttempts:   []ReviewAttempt{{Ordinal: 0}},
	}
	want := []string{
		"- analysis attempt 1: effective models claude-opus-4-8 · tokens input 100, output 50, cache-read 10, cache-creation 5 · cost $0.100000 (CLI list-price estimate) · MODEL MISMATCH vs pin (non-blocking)",
		"- coding attempt abc123: effective models claude-opus-4-8 · tokens input 100, output 50, cache-read 10, cache-creation 5 · cost $0.200000 (CLI list-price estimate)",
		"- review attempt review-0: unavailable",
		"Totals: input 200, output 100, cache-read 20, cache-creation 10 tokens across effective models claude-opus-4-8",
		"Reported cost: $0.300000 total — cost reported for 2 of 3 harness attempts (CLI list-price estimate)",
		"NOTICE: effective models differ from the pinned model for 1 of 3 harness attempt(s); this is non-blocking and never fails readiness or a gate.",
		"Note: usage capture covers analysis, coding and review harness attempts; meta/formula-improvement harness usage is deferred to slice B (#149).",
	}
	if got := UsageSummaryLines(r); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("lines changed:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestUsageSummaryRecordPerAttemptAndCoverage covers the structured summary: every
// attempt once in summary order (a retried coding attempt is its own entry), nil
// cost and tokens where unreported, and coverage n of m counting attempts with no
// usage in m only.
func TestUsageSummaryRecordPerAttemptAndCoverage(t *testing.T) {
	t.Parallel()
	tokensOnly := &harness.Usage{Total: harness.TokenCategories{Input: i64(7)}, EffectiveModels: []string{"claude-opus-4-8"}, TruncatedModels: 2}
	r := Record{
		AnalysisAttempts: []AnalysisAttempt{{Ordinal: 1, Usage: usageWith("claude-opus-4-8", 0.10, true)}},
		HarnessAttempts:  []HarnessAttempt{{ID: "h1", Classification: "transient-provider-error"}, {ID: "h2", Resumed: "resumed after transient-provider-error (1 of 2)", Usage: tokensOnly}},
		ReviewAttempts:   []ReviewAttempt{{Ordinal: 0}},
	}
	s := UsageSummary(r)
	if len(s.Attempts) != 4 {
		t.Fatalf("attempts %+v", s.Attempts)
	}
	for i, want := range []struct{ kind, id, label string }{{"analysis", "analysis-1", "analysis attempt 1"}, {"coding", "h1", "coding attempt h1"}, {"coding", "h2", "coding attempt h2"}, {"review", "review-0", "review attempt review-0"}} {
		if a := s.Attempts[i]; a.Kind != want.kind || a.ID != want.id || a.Label != want.label {
			t.Errorf("attempt %d = %+v, want %+v", i, a, want)
		}
	}
	a, h1, h2 := s.Attempts[0], s.Attempts[1], s.Attempts[2]
	if !a.Reported || a.CostUSD == nil || *a.CostUSD != 0.10 || a.CostBasis != "list" || !a.ModelMismatch || *a.Tokens.Input != 100 {
		t.Fatalf("analysis entry %+v", a)
	}
	if h1.Reported || h1.CostUSD != nil || h1.Tokens.Input != nil || h1.EffectiveModels != nil {
		t.Fatalf("missing usage read as reported: %+v", h1)
	}
	if !h2.Reported || h2.CostUSD != nil || *h2.Tokens.Input != 7 || h2.Tokens.Output != nil || h2.TruncatedModels != 2 || len(h2.EffectiveModels) != 1 {
		t.Fatalf("tokens-only entry %+v", h2)
	}
	if s.UsageReported != 2 || s.CostCoverage != (UsageCoverage{Reported: 1, Attempts: 4}) || s.CostUSD == nil || *s.CostUSD != 0.10 || s.CostBasis != "list" || s.Mismatches != 1 {
		t.Fatalf("summary %+v", s)
	}
	if *s.Tokens.Input != 107 || *s.Tokens.Output != 50 || len(s.Models) != 1 {
		t.Fatalf("totals %+v models %v", s.Tokens, s.Models)
	}
	// The summary copies the record: changing it later does not change the summary.
	*r.AnalysisAttempts[0].Usage.TotalCostUSD = 9
	if *a.CostUSD != 0.10 {
		t.Fatal("the summary aliases the record's cost")
	}
}

// TestUsageSummaryRecordUnavailableOverflowAndMixedBasis covers the failure paths:
// no usage at all (nil cost, zero of m), a cost sum that is not finite, a token
// total that overflows, and bases that disagree.
func TestUsageSummaryRecordUnavailableOverflowAndMixedBasis(t *testing.T) {
	t.Parallel()
	none := UsageSummary(Record{HarnessAttempts: []HarnessAttempt{{ID: "x"}}, AnalysisAttempts: []AnalysisAttempt{{Ordinal: 1}}})
	if none.UsageReported != 0 || none.CostUSD != nil || none.CostCoverage != (UsageCoverage{0, 2}) || none.Tokens.Input != nil || none.CostBasis != "" || len(none.Models) != 0 {
		t.Fatalf("no usage %+v", none)
	}
	huge := func(basis string) *harness.Usage {
		return &harness.Usage{Total: harness.TokenCategories{Input: i64(math.MaxInt64)}, TotalCostUSD: f64(math.MaxFloat64), CostBasis: basis}
	}
	r := Record{HarnessAttempts: []HarnessAttempt{{ID: "a", Usage: huge("list")}, {ID: "b", Usage: huge("")}}}
	s := UsageSummary(r)
	if !s.CostNotFinite || s.CostUSD != nil || s.CostCoverage != (UsageCoverage{2, 2}) || s.CostBasis != "mixed" || s.Tokens.Input != nil {
		t.Fatalf("overflow %+v", s)
	}
	joined := strings.Join(UsageSummaryLines(r), "\n")
	for _, want := range []string{"Totals: input unreported", "Reported cost: unavailable (sum not finite) — cost reported for 2 of 2 harness attempts (mixed cost basis; see per-attempt line items)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

// TestUsageSummaryBasisLabels labels a cost whose basis is empty as "basis
// unavailable" and never prints a bare dollar figure; disagreeing attempt bases make
// the aggregate "mixed" (#149 review fix 4).
func TestUsageSummaryBasisLabels(t *testing.T) {
	t.Parallel()
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
