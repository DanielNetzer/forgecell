package main

import (
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
)

const (
	// markerLines is how many leading lines of a comment may carry an explicit
	// severity marker.
	markerLines = 5
	// maxFindings is the contract bound of one reviewer_findings answer.
	maxFindings = 500
)

var (
	// severities are the contract severities, highest first.
	severities = []string{"high", "medium", "low", "info"}
	// namedMarker is BugBot's `High Severity` form.
	namedMarker = regexp.MustCompile(`(?i)\b(high|medium|low)[ \t]+severity\b`)
	// fieldMarker is `severity: <level>`, allowing markdown emphasis after the
	// colon, as in `**Severity:** high`.
	fieldMarker = regexp.MustCompile(`(?i)\bseverity[ \t]*:[ \t*_]*(high|medium|low|info)\b`)
	// tagMarker is a leading `[<level>]`, read after markdown markers.
	tagMarker = regexp.MustCompile(`(?i)^\[(high|medium|low|info)\]`)
)

// validSeverity reports whether s is a contract severity.
func validSeverity(s string) bool {
	return slices.Contains(severities, s)
}

// severityRank orders severities, highest first; an unknown one sorts last.
func severityRank(s string) int {
	if i := slices.Index(severities, s); i >= 0 {
		return i
	}
	return len(severities)
}

// severityOf reads the first explicit marker in the first five lines of body,
// case-insensitively, and returns fallback when there is none. Format
// characters, such as bidi controls, are dropped first, so they cannot hide a
// marker. Only those lines are split off, so a long
// body costs no more than its first lines.
func severityOf(body, fallback string) string {
	lines := strings.SplitN(body, "\n", markerLines+1)
	if len(lines) > markerLines {
		lines = lines[:markerLines]
	}
	for _, line := range lines {
		line = strings.Map(visible, line)
		for _, re := range []*regexp.Regexp{namedMarker, fieldMarker} {
			if m := re.FindStringSubmatch(line); m != nil {
				return strings.ToLower(m[1])
			}
		}
		if m := tagMarker.FindStringSubmatch(stripMarkers(line)); m != nil {
			return strings.ToLower(m[1])
		}
	}
	return fallback
}

// candidate is one finding with the comment's creation time and its position
// in the call, the keys that order findings within a severity.
type candidate struct {
	finding contract.ReviewerFinding
	at      time.Time
	order   int
}

// boundFindings orders candidates highest severity first and latest first
// within a severity, with a later position in the call breaking ties, so the
// answer never depends on map or sort instability. It keeps the first finding
// for a repeated id and at most limit findings, so the highest severities and
// the latest comments are the ones kept.
func boundFindings(cs []candidate, limit int) []contract.ReviewerFinding {
	slices.SortStableFunc(cs, func(a, b candidate) int {
		if d := severityRank(a.finding.Severity) - severityRank(b.finding.Severity); d != 0 {
			return d
		}
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
		return b.order - a.order
	})
	out := []contract.ReviewerFinding{}
	seen := map[string]bool{}
	for _, c := range cs {
		if len(out) == limit {
			break
		}
		if seen[c.finding.ID] {
			continue
		}
		seen[c.finding.ID] = true
		out = append(out, c.finding)
	}
	return out
}
