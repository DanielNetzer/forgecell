package harness

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// claudeCmd is a native claude-code binding command whose provider path is absolute.
func claudeCmd() []string {
	return []string{"/launcher", "__adapter", "claude-code", "/usr/bin/claude"}
}

// wantInt64 fails unless p is non-nil and equal to want; a nil pointer means the field
// was unreported, which these assertions treat as a mismatch.
func wantInt64(t *testing.T, name string, p *int64, want int64) {
	t.Helper()
	if p == nil {
		t.Fatalf("%s = unreported, want %d", name, want)
	}
	if *p != want {
		t.Fatalf("%s = %d, want %d", name, *p, want)
	}
}

// TestUsageParsesVerifiedKeys parses the exact keys observed live with CLI 2.1.296
// (#149), including cache and thinking tokens, costBasis and canonicalModel.
func TestUsageParsesVerifiedKeys(t *testing.T) {
	raw := `{
		"total_cost_usd": 0.1234,
		"duration_ms": 4200,
		"duration_api_ms": 3900,
		"num_turns": 7,
		"ttft_ms": 800,
		"usage": {"input_tokens": 999999, "output_tokens": 999999},
		"modelUsage": {
			"claude-opus-4-8": {
				"inputTokens": 100,
				"outputTokens": 50,
				"cacheReadInputTokens": 10,
				"cacheCreationInputTokens": 5,
				"thinkingTokens": 3,
				"costUSD": 0.1234,
				"canonicalModel": "claude-opus-4-8",
				"provider": "anthropic",
				"costBasis": "list"
			}
		}
	}`
	u := ParseUsage(raw)
	if u == nil {
		t.Fatal("expected usage, got nil")
	}
	wantInt64(t, "total.input", u.Total.Input, 100)
	wantInt64(t, "total.output", u.Total.Output, 50)
	wantInt64(t, "total.cacheRead", u.Total.CacheRead, 10)
	wantInt64(t, "total.cacheCreation", u.Total.CacheCreation, 5)
	if u.TotalCostUSD == nil || *u.TotalCostUSD != 0.1234 {
		t.Fatalf("total cost = %v", u.TotalCostUSD)
	}
	if u.CostBasis != "list" {
		t.Fatalf("cost basis = %q", u.CostBasis)
	}
	if !slices.Equal(u.EffectiveModels, []string{"claude-opus-4-8"}) {
		t.Fatalf("effective models = %v", u.EffectiveModels)
	}
	wantInt64(t, "duration_ms", u.DurationMS, 4200)
	wantInt64(t, "num_turns", u.NumTurns, 7)
	wantInt64(t, "ttft_ms", u.TTFTMS, 800)
	if len(u.Models) != 1 || u.Models[0].Thinking == nil || *u.Models[0].Thinking != 3 || u.Models[0].Provider != "anthropic" || u.Models[0].Degraded {
		t.Fatalf("per-model entry = %+v", u.Models)
	}
	// The raw top-level usage aggregate (999999) must never be embedded (#149 item 6).
	if *u.Total.Input == 999999 || *u.Total.Output == 999999 {
		t.Fatal("top-level usage aggregate was embedded; it double counts")
	}
}

// TestUsageToleratesMissingFields keeps parsing when optional fields are absent and
// reports each absent category as unreported (nil), never as zero (#149 review fix).
func TestUsageToleratesMissingFields(t *testing.T) {
	u := ParseUsage(`{"modelUsage":{"m":{"inputTokens":4}}}`)
	if u == nil {
		t.Fatal("expected usage")
	}
	wantInt64(t, "total.input", u.Total.Input, 4)
	if u.Total.Output != nil || u.Total.CacheRead != nil || u.Total.CacheCreation != nil {
		t.Fatalf("absent categories must be unreported, not zero: %+v", u.Total)
	}
	if u.TotalCostUSD != nil || u.CostBasis != "" || u.DurationMS != nil {
		t.Fatalf("absent fields should stay nil/empty: %+v", u)
	}
	if u.Models[0].Degraded {
		t.Fatal("a merely-absent field must not mark the entry degraded")
	}
	// Missing canonicalModel falls back to the map key.
	if !slices.Equal(u.EffectiveModels, []string{"m"}) {
		t.Fatalf("effective models = %v", u.EffectiveModels)
	}
}

// TestUsageMultipleModelsSumAndSort totals across models and sorts effective ids.
func TestUsageMultipleModelsSumAndSort(t *testing.T) {
	raw := `{
		"total_cost_usd": 0.3,
		"modelUsage": {
			"zeta": {"inputTokens": 1, "outputTokens": 2, "cacheReadInputTokens": 3, "cacheCreationInputTokens": 4, "canonicalModel": "claude-sonnet-4-8", "costBasis": "list"},
			"alpha": {"inputTokens": 10, "outputTokens": 20, "cacheReadInputTokens": 30, "cacheCreationInputTokens": 40, "canonicalModel": "claude-opus-4-8", "costBasis": "list"}
		}
	}`
	u := ParseUsage(raw)
	if u == nil {
		t.Fatal("expected usage")
	}
	wantInt64(t, "total.input", u.Total.Input, 11)
	wantInt64(t, "total.output", u.Total.Output, 22)
	wantInt64(t, "total.cacheRead", u.Total.CacheRead, 33)
	wantInt64(t, "total.cacheCreation", u.Total.CacheCreation, 44)
	if !slices.Equal(u.EffectiveModels, []string{"claude-opus-4-8", "claude-sonnet-4-8"}) {
		t.Fatalf("effective models not sorted by canonical id: %v", u.EffectiveModels)
	}
	if u.CostBasis != "list" {
		t.Fatalf("agreeing per-model bases should collapse to one: %q", u.CostBasis)
	}
}

// TestUsageMixedBasis flags disagreeing or partial per-model cost bases as mixed.
func TestUsageMixedBasis(t *testing.T) {
	disagree := ParseUsage(`{"modelUsage":{
		"a":{"inputTokens":1,"canonicalModel":"a","costBasis":"list"},
		"b":{"inputTokens":1,"canonicalModel":"b","costBasis":"billed"}
	}}`)
	if disagree == nil || disagree.CostBasis != "mixed" {
		t.Fatalf("disagreeing basis = %+v", disagree)
	}
	// Only some models carry a basis -> mixed (#149 review fix 4).
	partial := ParseUsage(`{"modelUsage":{
		"a":{"inputTokens":1,"canonicalModel":"a","costBasis":"list"},
		"b":{"inputTokens":1,"canonicalModel":"b"}
	}}`)
	if partial == nil || partial.CostBasis != "mixed" {
		t.Fatalf("partial basis = %+v", partial)
	}
}

// TestUsageUnavailable returns nil for runs that carry no usage evidence: a Claude run
// that emitted none, empty output, and non-JSON output all read as unavailable (#149).
func TestUsageUnavailable(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", `{"result":"done"}`, `{"modelUsage":{}}`, `{"modelUsage":[1,2,3]}`} {
		if u := ParseUsage(raw); u != nil {
			t.Fatalf("ParseUsage(%q) = %+v, want nil", raw, u)
		}
	}
}

// TestUsageMalformedFieldsDegradeNotDrop keeps a model entry and the whole Usage when
// individual fields are the wrong type or non-integer: the bad field is unreported and
// the entry is flagged degraded, while sound fields survive (#149 review fix 2).
func TestUsageMalformedFieldsDegradeNotDrop(t *testing.T) {
	raw := `{
		"total_cost_usd": 0.5,
		"duration_ms": "oops",
		"num_turns": 1.5,
		"modelUsage": {
			"m": {
				"inputTokens": "nope",
				"outputTokens": 1.5,
				"cacheReadInputTokens": 1e3,
				"cacheCreationInputTokens": 7,
				"costUSD": "free",
				"canonicalModel": "claude-opus-4-8",
				"provider": 42,
				"costBasis": "list"
			}
		}
	}`
	u := ParseUsage(raw)
	if u == nil {
		t.Fatal("one malformed field must not fail the whole parse")
	}
	// A malformed top-level duration does not fail Usage; it is simply unreported.
	if u.DurationMS != nil || u.NumTurns != nil {
		t.Fatalf("malformed durations must be unreported: %+v", u)
	}
	if u.TotalCostUSD == nil || *u.TotalCostUSD != 0.5 {
		t.Fatalf("sound total cost should survive: %v", u.TotalCostUSD)
	}
	if len(u.Models) != 1 {
		t.Fatalf("entry must be kept, got %d", len(u.Models))
	}
	m := u.Models[0]
	if !m.Degraded {
		t.Fatal("entry with malformed fields must be flagged degraded")
	}
	if m.Tokens.Input != nil || m.Tokens.Output != nil || m.Tokens.CacheRead != nil {
		t.Fatalf("string, 1.5 and 1e3 must each be unreported: %+v", m.Tokens)
	}
	wantInt64(t, "cacheCreation (sound)", m.Tokens.CacheCreation, 7)
	if m.CostUSD != nil {
		t.Fatal("non-numeric cost must be unreported")
	}
	if m.Provider != "" {
		t.Fatalf("non-string provider must be dropped, got %q", m.Provider)
	}
	if m.Model != "claude-opus-4-8" || m.CostBasis != "list" {
		t.Fatalf("sound strings should survive: %+v", m)
	}
}

// TestUsageAllEntriesUnusableTokensUnavailable reports tokens as unreported, not zero,
// when no model carried a usable category but a cost was still reported (#149 fix 2).
func TestUsageAllEntriesUnusableTokensUnavailable(t *testing.T) {
	u := ParseUsage(`{"total_cost_usd":0.1,"modelUsage":{"m":{"inputTokens":"x","canonicalModel":"claude-opus-4-8"}}}`)
	if u == nil {
		t.Fatal("expected usage from the reported cost")
	}
	if u.Total.Input != nil || u.Total.Output != nil || u.Total.CacheRead != nil || u.Total.CacheCreation != nil {
		t.Fatalf("every category must be unreported, not zero: %+v", u.Total)
	}
}

// TestUsageRangeValidation drops negative tokens and costs, overflowing sums, and
// non-finite costs to unreported, and never keeps them (#149 review fix 3).
func TestUsageRangeValidation(t *testing.T) {
	neg := ParseUsage(`{"modelUsage":{"m":{"inputTokens":-5,"costUSD":-1.0,"canonicalModel":"claude-opus-4-8"}}}`)
	if neg == nil {
		t.Fatal("expected usage")
	}
	if neg.Total.Input != nil || neg.Models[0].CostUSD != nil || !neg.Models[0].Degraded {
		t.Fatalf("negative token/cost must be unreported and degraded: %+v", neg.Models[0])
	}
	big := fmt.Sprintf("%d", int64(9223372036854775807))
	overflow := ParseUsage(fmt.Sprintf(`{"modelUsage":{
		"a":{"inputTokens":%s,"canonicalModel":"a"},
		"b":{"inputTokens":%s,"canonicalModel":"b"}
	}}`, big, big))
	if overflow == nil {
		t.Fatal("expected usage")
	}
	if overflow.Total.Input != nil {
		t.Fatalf("overflowing token total must be unreported, got %v", *overflow.Total.Input)
	}
	// A per-model max int64 still survives; only the sum overflows.
	wantInt64(t, "model a input", overflow.Models[0].Tokens.Input, 9223372036854775807)
}

// TestUsageBoundsModelEntries keeps at most 16 entries from a flood of untrusted
// entries, counts the rest, keeps the highest-output ones, and sanitizes control and
// bidi characters out of model ids at parse time (#149 review fix 6).
func TestUsageBoundsModelEntries(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"modelUsage":{`)
	for i := 0; i < 1000; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		// Output tokens increase with i, so the highest ids are the ones kept.
		fmt.Fprintf(&b, `"m%04d":{"outputTokens":%d,"canonicalModel":"model-%04d"}`, i, i, i)
	}
	b.WriteString(`}}`)
	u := ParseUsage(b.String())
	if u == nil {
		t.Fatal("expected usage")
	}
	if len(u.Models) != 16 {
		t.Fatalf("kept %d models, want 16", len(u.Models))
	}
	if u.TruncatedModels != 1000-16 {
		t.Fatalf("truncatedModels = %d, want %d", u.TruncatedModels, 1000-16)
	}
	// The highest-output model (i=999) must be among those kept.
	if !slices.Contains(u.EffectiveModels, "model-0999") {
		t.Fatalf("highest-output model dropped: %v", u.EffectiveModels)
	}
	// ANSI and bidi characters in a model id are stripped at parse time. The JSON
	// carries them as \u escapes (a literal ESC byte would be invalid JSON).
	evil := ParseUsage(`{"modelUsage":{"x":{"inputTokens":1,"canonicalModel":"ev\u001b[31mil‮-model"}}}`)
	if evil == nil {
		t.Fatal("expected usage")
	}
	got := evil.Models[0].Model
	if strings.ContainsRune(got, '\x1b') || strings.ContainsRune(got, '‮') {
		t.Fatalf("model id not sanitized: %q", got)
	}
}

// TestAttemptUsageProviderGate records usage only for a native Claude binding; Codex,
// Cursor and custom commands record nothing (unavailable) in v1 (#149 item 2).
func TestAttemptUsageProviderGate(t *testing.T) {
	raw := `{"total_cost_usd":0.1,"modelUsage":{"m":{"inputTokens":1,"canonicalModel":"claude-opus-4-8"}}}`
	if u := AttemptUsage(claudeCmd(), raw, ModelUnpinned); u == nil {
		t.Fatal("claude binding should record usage")
	}
	for _, cmd := range [][]string{
		{"/launcher", "__adapter", "codex", "/usr/bin/codex"},
		{"/launcher", "__adapter", "cursor", "/usr/bin/cursor"},
		{"/usr/bin/custom", "run"},
	} {
		if u := AttemptUsage(cmd, raw, ModelUnpinned); u != nil {
			t.Fatalf("non-Claude binding %v recorded usage: %+v", cmd, u)
		}
	}
}

// TestAttemptUsageModelMismatch is non-blocking evidence. The effective set must be
// exactly the pin: a wrong pin or an extra model flags; an exact pin matches; a known
// alias resolving to a single model is informational (not a mismatch); and a trailing
// context-window suffix is ignored (#149 review fix 5).
func TestAttemptUsageModelMismatch(t *testing.T) {
	single := `{"modelUsage":{"m":{"inputTokens":1,"canonicalModel":"claude-opus-4-8"}}}`
	two := `{"modelUsage":{
		"a":{"inputTokens":1,"canonicalModel":"claude-opus-4-8"},
		"b":{"inputTokens":1,"canonicalModel":"claude-sonnet-4-8"}
	}}`
	suffix := `{"modelUsage":{"m":{"inputTokens":1,"canonicalModel":"claude-opus-4-8"}}}`
	cases := []struct {
		name string
		raw  string
		pin  string
		want bool
	}{
		{"known alias resolves to one model is informational", single, "opus", false},
		{"wrong concrete pin", single, "claude-sonnet-4-8", true},
		{"exact pin matches", single, "claude-opus-4-8", false},
		{"extra model beyond the pin flags", two, "claude-opus-4-8", true},
		{"alias with more than one model flags", two, "opus", true},
		{"context-window suffix on pin is ignored", suffix, "claude-opus-4-8 [1m]", false},
		{"unpinned sentinel", single, ModelUnpinned, false},
		{"empty pin", single, "", false},
		{"custom passthrough", single, "opus (passed to a custom command, not enforced)", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := AttemptUsage(claudeCmd(), tc.raw, tc.pin)
			if u == nil {
				t.Fatal("expected usage")
			}
			if u.ModelMismatch != tc.want {
				t.Fatalf("ModelMismatch = %v, want %v", u.ModelMismatch, tc.want)
			}
		})
	}
}
