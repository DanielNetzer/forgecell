package main

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// namedMarker is BugBot's `High Severity` form, read after markdown markers.
	namedMarker = regexp.MustCompile(`(?i)^(high|medium|low)[ \t]+severity\b`)
	// boldMarker is the named form as the whole content of `**…**`.
	boldMarker = regexp.MustCompile(`(?i)\*\*[ \t]*(high|medium|low)[ \t]+severity[ \t]*\*\*`)
	// fieldMarker is `severity: <level>`, allowing markdown emphasis after the
	// colon, as in `**Severity:** high`.
	fieldMarker = regexp.MustCompile(`(?i)\bseverity[ \t]*:[ \t*_]*(high|medium|low|info)\b`)
	// tagMarker is a leading `[<level>]`, read after markdown markers, that does
	// not start a markdown link.
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
// marker. Lines inside a fenced code block, fence lines and blockquote lines
// are never read, and a fence left open hides the rest of those lines. Only
// those lines are split off, so a long body costs no more than its first lines.
func severityOf(body, fallback string) string {
	var fenceChar byte
	fenceLen := 0
	for _, line := range firstLines(body, markerLines) {
		line = strings.Map(visible, line)
		lead := strings.TrimLeftFunc(line, unicode.IsSpace)
		c, n := fenceRun(lead)
		if fenceLen > 0 {
			// As in CommonMark, only a run of the opener's character, at least as
			// long, with nothing after it closes the fence.
			if c == fenceChar && n >= fenceLen && strings.TrimSpace(lead[n:]) == "" {
				fenceLen = 0
			}
			continue
		}
		if n > 0 {
			fenceChar, fenceLen = c, n
			continue
		}
		if quoted(line) {
			continue
		}
		if s, ok := lineSeverity(line); ok {
			return s
		}
	}
	return fallback
}

// firstLines splits off at most n leading lines of body. A lone CR, NEL,
// U+2028 and U+2029 each end a line, as LF does, and CRLF is one break.
func firstLines(body string, n int) []string {
	lines := make([]string, 0, n)
	start := 0
	for i := 0; i < len(body) && len(lines) < n; {
		r, size := utf8.DecodeRuneInString(body[i:])
		switch r {
		case '\n', '\r', 0x85, 0x2028, 0x2029:
			lines = append(lines, body[start:i])
			if r == '\r' && strings.HasPrefix(body[i+size:], "\n") {
				size++
			}
			start = i + size
		}
		i += size
	}
	if len(lines) < n {
		lines = append(lines, body[start:])
	}
	return lines
}

// fenceRun is the leading run of ` or ~ in lead that makes a fence line, its
// character and length, or a zero length when the run is shorter than three.
func fenceRun(lead string) (byte, int) {
	if lead == "" || (lead[0] != '`' && lead[0] != '~') {
		return 0, 0
	}
	n := 1
	for n < len(lead) && lead[n] == lead[0] {
		n++
	}
	if n < 3 {
		return 0, 0
	}
	return lead[0], n
}

// quoted reports whether a blockquote marker appears anywhere in the line's
// leading run of markdown markers, list bullets and spaces, as in `- > x`.
func quoted(line string) bool {
	end := strings.IndexFunc(line, func(r rune) bool { return !markerOrSpace(r) && r != '-' && r != '+' })
	if end < 0 {
		end = len(line)
	}
	return strings.Contains(line[:end], ">")
}

// lineSeverity is the severity of the marker with the smallest index in line,
// whatever its form. A marker that `not` or `no` precedes does not count.
func lineSeverity(line string) (string, bool) {
	best, level := -1, ""
	consider := func(at int, l string) {
		if (best < 0 || at < best) && !negated(line[:at]) {
			best, level = at, l
		}
	}
	rest := stripMarkers(line)
	start := len(line) - len(rest)
	if m := namedMarker.FindStringSubmatch(rest); m != nil {
		consider(start, m[1])
	}
	if m := tagMarker.FindStringSubmatch(rest); m != nil && !strings.HasPrefix(rest[len(m[0]):], "(") {
		consider(start, m[1])
	}
	for _, re := range []*regexp.Regexp{boldMarker, fieldMarker} {
		for _, m := range re.FindAllStringSubmatchIndex(line, -1) {
			consider(m[0], line[m[2]:m[3]])
		}
	}
	if best < 0 {
		return "", false
	}
	return strings.ToLower(level), true
}

// negated reports whether the text before a marker ends in the word `not` or
// `no`, any case, with markdown markers and spaces allowed after it.
func negated(before string) bool {
	before = strings.TrimRightFunc(before, markerOrSpace)
	word := before[strings.LastIndexFunc(before, func(r rune) bool { return !unicode.IsLetter(r) })+1:]
	return strings.EqualFold(word, "not") || strings.EqualFold(word, "no")
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
// answer never depends on map or sort instability. Comments with a repeated id
// are separate findings, as the contract does not require unique ids. It keeps
// at most limit findings, so the highest severities and the latest comments
// are the ones kept.
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
	for _, c := range cs {
		if len(out) == limit {
			break
		}
		out = append(out, c.finding)
	}
	return out
}
