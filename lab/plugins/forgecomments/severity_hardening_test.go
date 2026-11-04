package main

import (
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

// severityCases checks each body against the severity it should yield.
func severityCases(t *testing.T, fallback string, cases []struct{ body, want string }) {
	t.Helper()
	for _, c := range cases {
		if got := severityOf(c.body, fallback); got != c.want {
			t.Fatalf("severityOf(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestInlineCodeSpansAreNeverRead(t *testing.T) {
	const fallback = "info"
	severityCases(t, fallback, []struct{ body, want string }{
		// A marker inside a code span, of any run length, does not count.
		{"`High Severity`", fallback},
		{"``High Severity``", fallback},
		{"`severity: high`", fallback},
		{"see `severity: high` here", fallback},
		{"**`High Severity`**", fallback},
		{"`[high]` x", fallback},
		{"``a ` severity: high ` b``", fallback},
		// A code span may continue onto the next line of its paragraph.
		{"see `code\nHigh Severity` here", fallback},
		{"see `code\nseverity: high`", fallback},
		// A marker outside a code span on the same line still counts.
		{"`x` severity: high", "high"},
		{"severity: low `severity: high`", "low"},
		{"`severity: high` then severity: low", "low"},
		{"`a` `b` **Medium Severity**", "medium"},
		// A run with no closer of the same length is literal.
		{"`` severity: high `", "high"},
		{"` severity: high", "high"},
		{"``High Severity", "high"},
		// A blank line, a heading or a list item ends the paragraph, so a span
		// does not continue past it.
		{"see `code\n\nHigh Severity `x`", "high"},
		{"# Title `\nHigh Severity `x`", "high"},
		{"- a `x\n- severity: high `y", "high"},
		// A code span does not hide a negation before it.
		{"not `x` severity: high", "high"},
		{"`not` severity: high", fallback},
	})
	// The masking keeps every index and line break.
	if got := maskCodeSpans("a `b\nc` d"); got != "a \x00\x00\n\x00\x00 d" {
		t.Fatalf("masked %q", got)
	}
	if got := maskCodeSpans("``x` y"); got != "``x` y" {
		t.Fatalf("unmatched runs %q", got)
	}
}

// TestCodeSpanMaskingIsLinear drives many backtick runs, each with no closer,
// through the masking; a quadratic scan would make this case very slow, while
// the result must still be exact.
func TestCodeSpanMaskingIsLinear(t *testing.T) {
	var b strings.Builder
	for n := 1; n <= 300; n++ {
		b.WriteString(strings.Repeat("`", n))
		b.WriteString(" x ")
	}
	line := b.String() + " severity: high"
	if got := maskCodeSpans(line); got != line {
		t.Fatal("runs of distinct lengths must stay literal")
	}
	if got := severityOf(line, "low"); got != "high" {
		t.Fatalf("severity %q", got)
	}
	many := strings.Repeat("`a` ", 50000) + "severity: high"
	if got := severityOf(many, "low"); got != "high" {
		t.Fatalf("severity %q", got)
	}
	if got := severityOf("x "+strings.Repeat("`", 200000)+" severity: high", "low"); got != "high" {
		t.Fatalf("one long run %q", got)
	}
}

func TestReferenceStyleLinkTagsAreRefused(t *testing.T) {
	const fallback = "info"
	severityCases(t, fallback, []struct{ body, want string }{
		{"[high][ref]", fallback},
		{"[high][]", fallback},
		{"[high]: http://x", fallback},
		{"## [low][anchor] x", fallback},
		{"**[medium]:** x", fallback},
		{"[High](https://example.invalid)", fallback},
		// A tag that does not start a link still counts.
		{"[high] x", "high"},
		{"[high] (not a link)", "high"},
		{"[high] [ref]", "high"},
		{"[high] : x", "high"},
		{"[LOW]", "low"},
		// A refused tag leaves a later marker readable.
		{"[high][ref] severity: low", "low"},
		{"[high]: http://x\nMedium Severity", "medium"},
	})
}

func TestMoreNegationFormsRefuseTheMatch(t *testing.T) {
	const fallback = "info"
	severityCases(t, fallback, []struct{ body, want string }{
		{"isn't severity: high", fallback},
		{"ISN'T severity: high", fallback},
		{"this isn’t severity: high", fallback},
		{"don't **High Severity**", fallback},
		{"not: severity: high", fallback},
		{"not, severity: high", fallback},
		{"No. severity: high", fallback},
		{"**not**: severity: high", fallback},
		{"(not) severity: high", fallback},
		{"not — severity: high", fallback},
		// Words that only look like a negation still allow the marker.
		{"note severity: high", "high"},
		{"know severity: low", "low"},
		{"nothing: severity: high", "high"},
		{"t severity: high", "high"},
		{"n't severity: high", fallback},
		// A negated marker leaves a later one readable.
		{"not severity: high, severity: low", "low"},
		{"isn't severity: high, severity: low", "low"},
		{"Low Severity, not severity: high", "low"},
	})
	for before, want := range map[string]bool{"": false, "not": true, "no ": true, "isn't, ": true, "won’t ": true, "nota": false, "nt": false, "x't": false} {
		if got := negated(before); got != want {
			t.Fatalf("negated(%q) = %v, want %v", before, got, want)
		}
	}
}

func TestASeverityWordRunningOnIntoALetterIsNotAMarker(t *testing.T) {
	const fallback = "info"
	severityCases(t, fallback, []struct{ body, want string }{
		{"severity: highé", fallback},
		{"severity: high́", fallback},
		{"severity: high٣", fallback},
		{"severity: lowя", fallback},
		{"High Severityé", fallback},
		{"**High Severityé**", fallback},
		{"## Low Severityж x", fallback},
		{"éseverity: high", fallback},
		{"日severity: high", fallback},
		// A format character between them is dropped first, so it does not help.
		{"severity: high‍é", fallback},
		// Spaces, punctuation and symbols still end the word.
		{"severity: high", "high"},
		{"severity:HIGH", "high"},
		{"severity: high.", "high"},
		{"severity: high—", "high"},
		{"severity: high →", "high"},
		{"LOW SEVERITY: cosmetic", "low"},
		{"Severity :  Medium", "medium"},
		{"High Severity — race", "high"},
		{"**High Severity**é", "high"},
		{"é severity: high", "high"},
		// A refused match leaves a later one readable.
		{"severity: highé severity: low", "low"},
	})
}

func TestIndentedCodeLinesAreNotRead(t *testing.T) {
	const fallback = "info"
	severityCases(t, fallback, []struct{ body, want string }{
		{"    severity: high", fallback},
		{"     High Severity", fallback},
		{"\tseverity: high", fallback},
		{"  \t[high] x", fallback},
		{" ​   severity: high", fallback},
		{"para\n    severity: high", fallback},
		{"    - severity: high", fallback},
		{"    severity: high\n[low] x", "low"},
		// An indented run of ` or ~ does not open a fence, so a marker on the next
		// line is still read.
		{"    ```\nseverity: high", "high"},
		{"    ~~~\n[high] x", "high"},
		{"\t```\nHigh Severity", "high"},
		// Inside a fence, an indented run does not close it.
		{"```\n    ```\nseverity: high", fallback},
		// Fences indented by up to three spaces behave as before.
		{"   ```\nseverity: high\n   ```\n[low] x", "low"},
		{"   ```\nseverity: high", fallback},
		{"   severity: high", "high"},
		// Inside a list, an indented line is list content, not code.
		{"- a\n    severity: high", "high"},
		{"1. a\n    - severity: high", "high"},
		{"- a\n\n    severity: high", "high"},
		{"* a\n      ```\n      severity: low\n      ```\n[high] x", "high"},
		// Any other line indented by less than four columns ends the list.
		{"- a\nb\n    severity: high", fallback},
		{"- a\n\nPara\n\n    severity: high", fallback},
		// Blockquotes and the five-line bound are unchanged.
		{"- > severity: high", fallback},
		{"1\n2\n3\n4\n    x\nHigh Severity", fallback},
		{"    x\n2\n3\n4\nHigh Severity", "high"},
	})
	for raw, want := range map[string]int{"": 0, "x": 0, "   x": 3, "\tx": 4, " \tx": 4, "   \tx": 4, "​​  x": 2, "    x": 0, strings.Repeat(" ", 1000) + "x": 4} {
		if got := indentOf(raw); got != want {
			t.Fatalf("indentOf(%q) = %d, want %d", raw, got, want)
		}
	}
	for lead, want := range map[string]bool{"- x": true, "-": true, "1. x": true, "123456789) x": true, "1234567890. x": false, "-x": false, "**x**": false, "1.x": false, "---": false, "": false} {
		if got := listItem(lead); got != want {
			t.Fatalf("listItem(%q) = %v, want %v", lead, got, want)
		}
	}
}

// TestHardenedMarkersEndToEnd drives the new rules through the findings tool.
func TestHardenedMarkersEndToEnd(t *testing.T) {
	comments := []map[string]any{
		botComment("span", "`High Severity` in a code span", 1),
		botComment("ref", "[high][ref] reference link", 2),
		botComment("negated", "isn't severity: high", 3),
		botComment("letter", "severity: highé", 4),
		botComment("indented", "    severity: high", 5),
		botComment("real", "severity: high", 0),
	}
	got, _ := decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(map[string]any{"defaultSeverity": "low"}, comments))
	if len(got.Findings) != 6 || got.Findings[0].ID != "real" || got.Findings[0].Severity != "high" {
		t.Fatalf("findings %+v", got.Findings)
	}
	for _, f := range got.Findings[1:] {
		if f.Severity != "low" {
			t.Fatalf("finding %s has severity %q, want the default", f.ID, f.Severity)
		}
	}
}
