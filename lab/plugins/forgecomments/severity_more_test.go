package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

func TestMarkersInContext(t *testing.T) {
	const fallback = "info"
	cases := []struct{ body, want string }{
		// Fenced code and blockquotes are never read.
		{"```\nHigh Severity\n```", fallback},
		{"~~~go\n[high] x\n~~~", fallback},
		{"  ```\nseverity: high\n  ```\n[low] after", "low"},
		{"```\n~~~\nHigh Severity\n```", fallback},
		{"~~~\n```\n~~~\n[low] x", "low"},
		{"```\r\nHigh Severity\r\n```\r\n[low] x", "low"},
		// A fence left open hides the rest of the lines.
		{"```\n\n\n\nHigh Severity", fallback},
		{"> High Severity", fallback},
		{"   > severity: high", fallback},
		{">[high] x", fallback},
		{"> quoted\nMedium Severity", "medium"},
		// The named marker counts at the start of a line or as a whole bold span.
		{"see High Severity later", fallback},
		{"this is high severity", fallback},
		{"**High Severity**", "high"},
		{"## High Severity", "high"},
		{"see **Low Severity** here", "low"},
		{"see ** Low Severity ** here", "low"},
		{"**Low Severity, maybe**", "low"},
		{"**Bug: High Severity**", fallback},
		// A preceding not or no refuses the match.
		{"not severity: high", fallback},
		{"Not **severity:** high", fallback},
		{"**No High Severity**", fallback},
		{"no **High Severity**", fallback},
		{"**NOT** **Medium Severity**", fallback},
		{"not severity: high, severity: low", "low"},
		{"note severity: high", "high"},
		{"know severity: low", "low"},
		// A tag that starts a markdown link does not count.
		{"[High](https://example.invalid)", fallback},
		{"## [low](#anchor) x", fallback},
		{"[high] x", "high"},
		{"[high] (not a link)", "high"},
		{"[High](https://example.invalid)\nSeverity: low", "low"},
	}
	for _, c := range cases {
		if got := severityOf(c.body, fallback); got != c.want {
			t.Fatalf("severityOf(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestTheFirstMarkerByPositionWins(t *testing.T) {
	cases := []struct{ body, want string }{
		{"[low] see severity: high", "low"},
		{"[info] **High Severity**", "info"},
		{"Low Severity, not severity: high", "low"},
		{"Severity: low **High Severity**", "low"},
		{"**High Severity** severity: low", "high"},
		{"x severity: info, severity: high", "info"},
		{"High Severity severity: low", "high"},
	}
	for _, c := range cases {
		if got := severityOf(c.body, "medium"); got != c.want {
			t.Fatalf("severityOf(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestTheFiveLineBoundaryHoldsWithCRLF(t *testing.T) {
	if got := severityOf("1\r\n2\r\n3\r\n4\r\nHigh Severity", "low"); got != "high" {
		t.Fatalf("line five: %q", got)
	}
	if got := severityOf("1\r\n2\r\n3\r\n4\r\nHigh Severity\r\n", "low"); got != "high" {
		t.Fatalf("line five with a trailing CRLF: %q", got)
	}
	for _, body := range []string{"1\r\n2\r\n3\r\n4\r\n5\r\nHigh Severity", "1\r\n2\r\n3\r\n4\r\n5\r\n[high] late", "\r\n\r\n\r\n\r\n\r\nseverity: high"} {
		if got := severityOf(body, "low"); got != "low" {
			t.Fatalf("severityOf(%q) = %q", body, got)
		}
	}
}

func TestFencesFollowCommonMarkRunLength(t *testing.T) {
	cases := []struct{ body, want string }{
		// A shorter run, or one with trailing text, does not close the fence.
		{"````md\n```\nHigh Severity\n````", "low"},
		{"```\n```js\nseverity: high", "low"},
		{"~~~~\n~~~\n[high] x", "low"},
		// A run of the other character does not close it either.
		{"```\n~~~\nseverity: high", "low"},
		// A longer run of the same character, with trailing spaces, closes it.
		{"```\nseverity: info\n`````  \n[high] x", "high"},
		{"~~~~\nx\n~~~~\nMedium Severity", "medium"},
		// Two characters are not a fence.
		{"``\n[high] x", "high"},
	}
	for _, c := range cases {
		if got := severityOf(c.body, "low"); got != c.want {
			t.Fatalf("severityOf(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestABlockquoteInTheLeadingMarkersHidesTheLine(t *testing.T) {
	for _, body := range []string{"- > severity: high", "* > High Severity", "## > [high] x", "+ > [high] x", "  -> severity: high", "**>** High Severity"} {
		if got := severityOf(body, "low"); got != "low" {
			t.Fatalf("severityOf(%q) = %q", body, got)
		}
	}
	// A list item without a quote is still read, and so is a later `>`.
	for _, body := range []string{"- severity: high", "* [high] x", "severity: high > low"} {
		if got := severityOf(body, "low"); got != "high" {
			t.Fatalf("severityOf(%q) = %q", body, got)
		}
	}
}

func TestEveryLineBreakCountsTowardTheFiveLines(t *testing.T) {
	for _, body := range []string{
		"a\rb\rc\rd\re\rf\rseverity: high",
		"a b c d e severity: high",
		"a b c d e severity: high",
		"a\u0085b\u0085c\u0085d\u0085e\u0085severity: high",
		"a\r\nb\rc\nd e\u0085severity: high",
		// A fence opened after a lone CR hides the lines after it.
		"x\r```\rHigh Severity",
		"x ~~~ [high] x",
	} {
		if got := severityOf(body, "low"); got != "low" {
			t.Fatalf("severityOf(%q) = %q", body, got)
		}
	}
	// CRLF stays one break, so line five is still read.
	for _, body := range []string{"a\r\nb\r\nc\r\nd\r\nseverity: high", "a\rb\rc\rd\rseverity: high", "a\u0085b c d\rseverity: high"} {
		if got := severityOf(body, "low"); got != "high" {
			t.Fatalf("severityOf(%q) = %q", body, got)
		}
	}
	if got := firstLines("a\r\n\r\nb", 5); !reflect.DeepEqual(got, []string{"a", "", "b"}) {
		t.Fatalf("lines %q", got)
	}
	if got := firstLines("1\n2\n3\n4\n5\n6\n7", 5); !reflect.DeepEqual(got, []string{"1", "2", "3", "4", "5"}) {
		t.Fatalf("lines %q", got)
	}
}

func TestBotsNullAndDuplicateNamesAreRefused(t *testing.T) {
	cases := []struct {
		name   string
		config map[string]any
		reason string
	}{
		{"null bots", map[string]any{"bots": nil}, "bots must be a list of author names"},
		{"repeated bot", map[string]any{"bots": []string{"cursor[bot]", "gitlab-duo", "cursor[bot]"}}, "lists an author name more than once"},
		{"repeated bot in another case", map[string]any{"bots": []string{"cursor[bot]", "CURSOR[BOT]"}}, "lists an author name more than once"},
	}
	for _, c := range cases {
		for _, tool := range []string{"reviewer_describe", "reviewer_findings"} {
			args := map[string]any{"config": c.config}
			if tool == "reviewer_findings" {
				args = findingsArgs(c.config, []map[string]any{botComment("c1", "x", 0)})
			}
			out, err := call(t, tool, args)
			if out != nil {
				t.Fatalf("%s %s: output %+v", c.name, tool, out)
			}
			msg := failure(t, err)
			if !strings.Contains(msg, c.reason) {
				t.Fatalf("%s %s: %q, want %q", c.name, tool, msg, c.reason)
			}
			for _, leak := range []string{"cursor", "CURSOR", "gitlab"} {
				if strings.Contains(msg, leak) {
					t.Fatalf("%s %s: the reason names a config value: %q", c.name, tool, msg)
				}
			}
		}
	}
	// Distinct names and an empty list are still accepted.
	for _, cfg := range []map[string]any{{"bots": []string{"cursor[bot]", "cursor"}}, {"bots": []string{}}} {
		if _, err := call(t, "reviewer_describe", map[string]any{"config": cfg}); err != nil {
			t.Fatalf("config %v: %v", cfg, err)
		}
	}
}

func TestRepeatedCommentIdsAreSeparateFindings(t *testing.T) {
	comments := []map[string]any{
		botComment("same", "[high] first", 1),
		botComment("same", "[low] second", 2),
		botComment("other", "plain", 3),
	}
	got, engine := decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(nil, comments))
	want := []string{"same", "other", "same"}
	if !reflect.DeepEqual(ids(got.Findings), want) || !reflect.DeepEqual(ids(engine.Findings), want) {
		t.Fatalf("findings %v, engine %v", ids(got.Findings), ids(engine.Findings))
	}
	if got.Findings[0].Severity != "high" || got.Findings[2].Severity != "low" || got.Findings[0].Body == got.Findings[2].Body {
		t.Fatalf("findings %+v", got.Findings)
	}

	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cs := []candidate{
		{finding: contract.ReviewerFinding{ID: "repeat", Severity: "high", Title: "a"}, at: at, order: 0},
		{finding: contract.ReviewerFinding{ID: "repeat", Severity: "high", Title: "b"}, at: at, order: 1},
	}
	kept := boundFindings(cs, maxFindings)
	if len(kept) != 2 || kept[0].Title != "b" || kept[1].Title != "a" {
		t.Fatalf("kept %+v", kept)
	}
	if kept := boundFindings(cs, 1); len(kept) != 1 || kept[0].Title != "b" {
		t.Fatalf("bounded %+v", kept)
	}
}

// TestACancelledContextFailsTheFindingsCall drives the timeout path with a
// context that is already done, so no wall-clock budget is involved.
func TestACancelledContextFailsTheFindingsCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	comments := []comment{{ID: "c1", IsBot: true, Body: "[high] x", CreatedAt: createdAt(0)}}
	out, err := findingsOf(ctx, reviewConfig{defaultSeverity: "medium"}, comments, outputBudget(reviewerTool(t, "reviewer_findings")))
	if msg := failure(t, err); msg != "failed: reading the comments took too long" {
		t.Fatalf("message %q", msg)
	}
	if out.Status != "" || len(out.Findings) != 0 {
		t.Fatalf("output %+v", out)
	}
	// Through the bound handler, as the server calls it.
	raw, err := json.Marshal(findingsArgs(nil, []map[string]any{botComment("c1", "[high] x", 0)}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := bind(reviewerTool(t, "reviewer_findings"))(ctx, raw)
	if res != nil {
		t.Fatalf("output %+v", res)
	}
	if msg := failure(t, err); msg != "failed: reading the comments took too long" {
		t.Fatalf("message %q", msg)
	}
}
