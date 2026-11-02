package main

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

func TestEverySeverityMarkerFormIsRead(t *testing.T) {
	cases := []struct{ body, want string }{
		// BugBot's named form, anywhere in a line and in any case.
		{"### Race in the cache\n\n**High Severity**\n\nDetails", "high"},
		{"medium severity", "medium"},
		{"LOW SEVERITY: cosmetic", "low"},
		// The severity field.
		{"Severity: info", "info"},
		{"severity:HIGH", "high"},
		{"**Severity:** low", "low"},
		{"Severity :  Medium", "medium"},
		// A leading tag, after markdown markers and spaces.
		{"[high] Unchecked error", "high"},
		{"[Medium] x", "medium"},
		{"[LOW]", "low"},
		{"[info] note", "info"},
		{"> **[high]** quoted", "high"},
		{"  ## [low] heading", "low"},
		// The first marker wins.
		{"Low Severity\nHigh Severity", "low"},
		{"title\n[info] first\nSeverity: high", "info"},
		// Lines two to five still count.
		{"1\n2\n3\n4\nHigh Severity", "high"},
		// Format characters cannot hide a marker.
		{"⁧[high]⁩ hidden", "high"},
		{"Hi​gh Severity", "high"},
	}
	for _, c := range cases {
		if got := severityOf(c.body, "medium"); got != c.want {
			t.Fatalf("severityOf(%q) = %q, want %q", c.body, got, c.want)
		}
	}
}

func TestTheDefaultSeverityAppliesWithoutAMarker(t *testing.T) {
	for _, body := range []string{
		"",
		"Plain comment",
		// A marker past the fifth line is ignored.
		"1\n2\n3\n4\n5\nHigh Severity",
		"1\n2\n3\n4\n5\n[high] late",
		// A tag that does not lead its line, and words that are not markers.
		"see [high] later",
		"Critical Severity",
		"highseverity",
		"severity: critical",
		"[hi] x",
	} {
		for _, fallback := range []string{"medium", "info", "high"} {
			if got := severityOf(body, fallback); got != fallback {
				t.Fatalf("severityOf(%q, %s) = %q", body, fallback, got)
			}
		}
	}
	// End to end, an unconfigured default is medium and a configured one
	// applies.
	comments := []map[string]any{botComment("marked", "[low] x", 1), botComment("plain", "no marker", 0)}
	got, _ := decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(nil, comments))
	if len(got.Findings) != 2 || got.Findings[0].ID != "plain" || got.Findings[0].Severity != "medium" || got.Findings[1].Severity != "low" {
		t.Fatalf("findings %+v", got.Findings)
	}
	got, _ = decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(map[string]any{"defaultSeverity": "info"}, comments))
	if len(got.Findings) != 2 || got.Findings[0].ID != "marked" || got.Findings[0].Severity != "low" || got.Findings[1].Severity != "info" {
		t.Fatalf("findings %+v", got.Findings)
	}
}

// Test501BotCommentsKeepThe500HighestSeverityFindingsLatestFirst runs past the
// contract's own 500-comment input bound, as a direct call can.
func Test501BotCommentsKeepThe500HighestSeverityFindingsLatestFirst(t *testing.T) {
	marker := map[string]string{"high": "High Severity", "medium": "Severity: medium", "low": "[low] x", "info": "[info] x"}
	comments := []comment{}
	want := map[string][]string{}
	// 100 high, 150 medium, 150 low and 101 info comments, partly interleaved
	// and each a minute later than the one before.
	for i := range 501 {
		sev := []string{"high", "medium", "low", "info"}[i%4]
		switch {
		case i == 500:
			sev = "info"
		case i >= 450:
			sev = "low"
		case i >= 400:
			sev = "medium"
		}
		id := fmt.Sprintf("c%03d", i)
		comments = append(comments, comment{ID: id, Author: "bot", IsBot: true, Body: marker[sev] + "\n" + id, CreatedAt: createdAt(i)})
		want[sev] = append(want[sev], id)
	}
	counts := map[string]int{}
	for sev, list := range want {
		counts[sev] = len(list)
	}
	if !reflect.DeepEqual(counts, map[string]int{"high": 100, "medium": 150, "low": 150, "info": 101}) {
		t.Fatalf("setup counts %v", counts)
	}
	out, err := findingsOf(context.Background(), reviewConfig{defaultSeverity: "medium"}, comments, outputBudget(reviewerTool(t, "reviewer_findings")))
	if err != nil {
		t.Fatal(err)
	}
	// Highest severity first, latest first within each; the earliest info
	// comment, c003, is the one left out.
	expected := []string{}
	for _, sev := range severities {
		list := slices.Clone(want[sev])
		slices.Reverse(list)
		expected = append(expected, list...)
	}
	expected = expected[:500]
	if out.Status != "complete" || !reflect.DeepEqual(ids(out.Findings), expected) || slices.Contains(ids(out.Findings), "c003") {
		t.Fatalf("kept %v", ids(out.Findings))
	}
}

func TestBoundFindingsOrdersTiesAndRepeatsDeterministically(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := func(id, sev string) contract.ReviewerFinding {
		return contract.ReviewerFinding{ID: id, Severity: sev, Title: id}
	}
	cs := []candidate{
		{finding: f("same-time-first", "low"), at: at, order: 0},
		{finding: f("same-time-second", "low"), at: at, order: 1},
		{finding: f("undated", "low"), order: 2},
		{finding: f("repeat", "high"), at: at, order: 3},
		{finding: f("repeat", "info"), at: at.Add(time.Hour), order: 4},
		{finding: f("newer-info", "info"), at: at.Add(2 * time.Hour), order: 5},
	}
	got := ids(boundFindings(cs, maxFindings))
	want := []string{"repeat", "same-time-second", "same-time-first", "undated", "newer-info"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	if got := ids(boundFindings(cs, 2)); !reflect.DeepEqual(got, want[:2]) {
		t.Fatalf("bounded %v", got)
	}
	if got := boundFindings(nil, maxFindings); got == nil || len(got) != 0 {
		t.Fatalf("empty %v", got)
	}
}
