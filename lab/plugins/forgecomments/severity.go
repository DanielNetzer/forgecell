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
// marker. Lines inside a fenced code block, fence lines, blockquote lines and
// indented code lines are never read, and a fence left open hides the rest of
// those lines. Text inside an inline code span never counts. Only those lines
// are split off, so a long body costs no more than its first lines.
func severityOf(body, fallback string) string {
	for _, l := range readLines(body) {
		if s, ok := maskedSeverity(l.text, l.masked); ok {
			return s
		}
	}
	return fallback
}

// codeIndent is the indent, in columns, at which a line outside a list is an
// indented code line.
const codeIndent = 4

// readLine is a line that may carry a marker, with format characters dropped,
// and the same line with its inline code spans masked byte for byte, so an
// index into one is an index into the other.
type readLine struct {
	text, masked string
}

// readLines is the lines of the first five that are read for a marker, in
// order. It drops fenced code, fence lines, blockquote lines, blank lines and,
// outside a list, lines indented by four or more columns, which CommonMark
// reads as indented code. A paragraph is a run of read lines that no blank or
// dropped line, heading or list item breaks, and its code spans are masked
// across its lines, as a code span may continue onto the next line.
func readLines(body string) []readLine {
	var out []readLine
	var fenceChar byte
	fenceLen, para := 0, 0
	inList := false
	flush := func() {
		maskParagraph(out[para:])
		para = len(out)
	}
	for _, raw := range firstLines(body, markerLines) {
		indent := indentOf(raw)
		line := strings.Map(visible, raw)
		lead := strings.TrimLeftFunc(line, unicode.IsSpace)
		c, n := fenceRun(lead)
		if fenceLen > 0 {
			// As in CommonMark, only a run of the opener's character, at least as
			// long, with nothing after it and indented by less than four columns
			// outside a list, closes the fence.
			if c == fenceChar && n >= fenceLen && strings.TrimSpace(lead[n:]) == "" && (indent < codeIndent || inList) {
				fenceLen = 0
			}
			continue
		}
		blank := strings.TrimSpace(line) == ""
		code := indent >= codeIndent && !inList && !blank
		item := listItem(lead)
		// A list item starts or continues a list. Any other line indented by less
		// than four columns ends it, even a lazy continuation, so a later indented
		// line is read as code: refusing a marker is the safe side.
		if !blank && !code {
			if item {
				inList = true
			} else if indent < codeIndent {
				inList = false
			}
		}
		switch {
		case code, blank:
			// An indented code line, or a run of ` or ~ on one, never opens a fence.
			flush()
			continue
		case n > 0:
			flush()
			fenceChar, fenceLen = c, n
			continue
		case quoted(line):
			flush()
			continue
		}
		head := heading(lead)
		if item || head {
			flush()
		}
		out = append(out, readLine{text: line})
		if head {
			flush()
		}
	}
	flush()
	return out
}

// indentOf is the leading indent of raw in columns, as CommonMark counts it: a
// space is one column and a tab reaches the next multiple of four. Format
// characters are skipped, as they are dropped before a line is read. It stops
// counting at four columns, so its work is bounded.
func indentOf(raw string) int {
	col := 0
	for _, r := range raw {
		switch {
		case r == ' ':
			col++
		case r == '\t':
			col += 4 - col%4
		case unicode.Is(unicode.Cf, r):
		default:
			return col
		}
		if col >= codeIndent {
			return col
		}
	}
	return col
}

// listItem reports whether lead, a line without its indent, starts a list
// item: a `-`, `+` or `*` bullet, or one to nine digits and `.` or `)`,
// followed by a space or the end of the line.
func listItem(lead string) bool {
	i := 0
	for i < len(lead) && i < 9 && lead[i] >= '0' && lead[i] <= '9' {
		i++
	}
	switch {
	case i == 0 && lead != "" && strings.IndexByte("-+*", lead[0]) >= 0:
		i = 1
	case i > 0 && i < len(lead) && (lead[i] == '.' || lead[i] == ')'):
		i++
	default:
		return false
	}
	return i == len(lead) || lead[i] == ' '
}

// heading reports whether lead, a line without its indent, is an ATX heading:
// one to six `#` followed by a space or the end of the line.
func heading(lead string) bool {
	n := 0
	for n < len(lead) && n < 7 && lead[n] == '#' {
		n++
	}
	return n >= 1 && n <= 6 && (n == len(lead) || lead[n] == ' ')
}

// maskParagraph masks the code spans of one paragraph, read as its lines
// joined by line breaks.
func maskParagraph(lines []readLine) {
	if len(lines) == 0 {
		return
	}
	texts := make([]string, len(lines))
	for i, l := range lines {
		texts[i] = l.text
	}
	for i, m := range strings.Split(maskCodeSpans(strings.Join(texts, "\n")), "\n") {
		lines[i].masked = m
	}
}

// maskCodeSpans replaces every byte of each inline code span in s, backticks
// included, with NUL, keeping line breaks, so no marker can match inside one
// and every index is unchanged. As in CommonMark, a run of backticks opens a
// span that the next run of exactly the same length closes, and a run with no
// such closer is literal. A backslash does not escape a backtick here, so a
// span is masked whenever it could be one. It runs in linear time: each run
// finds its closer through a table built in one pass from the end.
func maskCodeSpans(s string) string {
	type run struct{ at, n int }
	var runs []run
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		runs = append(runs, run{i, j - i})
		i = j
	}
	if len(runs) < 2 {
		return s
	}
	closer := make([]int, len(runs))
	nextOfLen := map[int]int{}
	for k := len(runs) - 1; k >= 0; k-- {
		closer[k] = -1
		if m, ok := nextOfLen[runs[k].n]; ok {
			closer[k] = m
		}
		nextOfLen[runs[k].n] = k
	}
	b := []byte(s)
	for k := 0; k < len(runs); {
		m := closer[k]
		if m < 0 {
			k++
			continue
		}
		for i := runs[k].at; i < runs[m].at+runs[m].n; i++ {
			if b[i] != '\n' {
				b[i] = 0
			}
		}
		k = m + 1
	}
	return string(b)
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
// whatever its form, with the line's own code spans masked. A marker that
// `not`, `no` or a word ending in `n't` precedes does not count.
func lineSeverity(line string) (string, bool) {
	return maskedSeverity(line, maskCodeSpans(line))
}

// maskedSeverity is lineSeverity for a line whose code spans are already
// masked in masked. Markers are matched in masked, so none inside a code span
// counts, and negation is read in line, so a masked span never hides a `not`.
// A severity word that runs on into a Unicode letter, mark or digit, as in
// `highé`, is not a marker, and a tag that starts a link, a reference link or
// a link reference definition does not count.
func maskedSeverity(line, masked string) (string, bool) {
	best, level := -1, ""
	consider := func(at int, l string) {
		if (best < 0 || at < best) && !negated(line[:at]) {
			best, level = at, l
		}
	}
	rest := stripMarkers(masked)
	start := len(masked) - len(rest)
	if m := namedMarker.FindStringSubmatch(rest); m != nil && !wordRuneAt(rest, len(m[0])) {
		consider(start, m[1])
	}
	if m := tagMarker.FindStringSubmatch(rest); m != nil && !startsLink(rest[len(m[0]):]) {
		consider(start, m[1])
	}
	for _, m := range boldMarker.FindAllStringSubmatchIndex(masked, -1) {
		consider(m[0], masked[m[2]:m[3]])
	}
	for _, m := range fieldMarker.FindAllStringSubmatchIndex(masked, -1) {
		if !wordRuneBefore(masked, m[0]) && !wordRuneAt(masked, m[3]) {
			consider(m[0], masked[m[2]:m[3]])
		}
	}
	if best < 0 {
		return "", false
	}
	return strings.ToLower(level), true
}

// startsLink reports whether the text after a `[level]` tag makes it a link,
// `[x](…)`, a reference link, `[x][ref]` or `[x][]`, or a link reference
// definition, `[x]: url`.
func startsLink(after string) bool {
	return after != "" && strings.IndexByte("([:", after[0]) >= 0
}

// wordRune reports whether r continues a word: a Unicode letter, mark or
// digit, or an underscore.
func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r) || r == '_'
}

// wordRuneAt reports whether s has a word rune at byte index at.
func wordRuneAt(s string, at int) bool {
	if at >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[at:])
	return wordRune(r)
}

// wordRuneBefore reports whether s has a word rune just before byte index at.
func wordRuneBefore(s string, at int) bool {
	if at <= 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:at])
	return wordRune(r)
}

// negated reports whether the text before a marker ends in the word `not` or
// `no`, any case, or in a word ending in `n't` or `n’t`, as in `isn't`, with
// markdown markers, punctuation and spaces allowed after it, as in `not:`.
func negated(before string) bool {
	before = strings.TrimRightFunc(before, func(r rune) bool { return markerOrSpace(r) || unicode.IsPunct(r) })
	word := before[strings.LastIndexFunc(before, func(r rune) bool { return !unicode.IsLetter(r) })+1:]
	if strings.EqualFold(word, "not") || strings.EqualFold(word, "no") {
		return true
	}
	for _, suffix := range []string{"n't", "n’t"} {
		if len(before) >= len(suffix) && strings.EqualFold(before[len(before)-len(suffix):], suffix) {
			return true
		}
	}
	return false
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
