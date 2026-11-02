package main

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/plugins/internal/mcpserver"
)

const (
	displayName = "Review bot comments"
	// maxConfigBytes bounds the reviewed config object, compacted.
	maxConfigBytes = 4 << 10
	// maxBots and maxBotRunes bound config.bots: how many author names, and
	// each name, as the contract bounds a comment's author.
	maxBots     = 16
	maxBotRunes = 100
	// maxTitleRunes and maxBodyRunes are the contract bounds of a finding's
	// title and body.
	maxTitleRunes = 300
	maxBodyRunes  = 65536
	// defaultSeverity applies when the config names none.
	defaultSeverity = "medium"
	// fallbackTitle is the title of a comment with no text on any line.
	fallbackTitle = "Review bot comment"
	// responseEnvelope is the room left for the JSON-RPC envelope around one
	// answer.
	responseEnvelope = 4 << 10
	truncatedMark    = "\n[truncated]"
	// lineCutMark marks a cut title.
	lineCutMark = "…"

	needsComments = "this reviewer needs the forge's review comments"
)

// capabilities are reviewer_describe's capabilities: the engine sends the
// forge's review comments only to a reviewer that lists forge-comments.
var capabilities = []string{contract.ReviewerForgeComments}

type handler func(ctx context.Context, t *contract.Tool, args json.RawMessage) (any, error)

// bind returns the handler for one contract tool: it refuses arguments the
// contract input schema refuses and bounds the call by the tool's contract
// timeout. A contract tool with no handler is a programming error.
func bind(t *contract.Tool) mcpserver.Handler {
	handlers := map[string]handler{
		"reviewer_describe": describe,
		"reviewer_findings": findings,
	}
	h := handlers[t.Name]
	if h == nil {
		panic("forgecomments: no handler for " + t.Name)
	}
	return func(ctx context.Context, args json.RawMessage) (any, error) {
		if err := t.ValidateInputJSON(args); err != nil {
			return nil, mcpserver.Fail(mcpserver.Failed, "the arguments do not match the input schema")
		}
		ctx, cancel := context.WithTimeout(ctx, t.Timeout())
		defer cancel()
		return h(ctx, t, args)
	}
}

// decodeArgs decodes schema-checked arguments into the tool's input struct.
func decodeArgs(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return mcpserver.Fail(mcpserver.Failed, "the arguments do not match the input schema")
	}
	return nil
}

// reviewConfig is the checked config object.
type reviewConfig struct {
	bots            []string
	defaultSeverity string
}

// parseConfig checks the reviewed config: a JSON object of at most 4 KiB
// holding only bots, a list of at most 16 author names, and defaultSeverity, one
// of the contract severities. An absent config is the defaults. Its errors are
// fixed text that never names a config value.
func parseConfig(raw json.RawMessage) (reviewConfig, error) {
	cfg := reviewConfig{defaultSeverity: defaultSeverity}
	if len(raw) == 0 || string(raw) == "null" {
		return cfg, nil
	}
	var compact bytes.Buffer
	var keys map[string]json.RawMessage
	if json.Compact(&compact, raw) != nil || compact.Len() > maxConfigBytes || json.Unmarshal(raw, &keys) != nil || keys == nil {
		return reviewConfig{}, mcpserver.Fail(mcpserver.Failed, "the config must be a JSON object of at most 4096 bytes")
	}
	for key := range keys {
		if key != "bots" && key != "defaultSeverity" {
			return reviewConfig{}, mcpserver.Fail(mcpserver.Failed, "the config holds only bots and defaultSeverity")
		}
	}
	if v, ok := keys["bots"]; ok {
		var bots []string
		if json.Unmarshal(v, &bots) != nil {
			return reviewConfig{}, mcpserver.Fail(mcpserver.Failed, "the config bots must be a list of author names")
		}
		if len(bots) > maxBots {
			return reviewConfig{}, mcpserver.Fail(mcpserver.Failed, "the config bots lists more than 16 author names")
		}
		for _, b := range bots {
			if !validBot(b) {
				return reviewConfig{}, mcpserver.Fail(mcpserver.Failed, "each config bot must be an author name of 1 to 100 characters with no control characters or surrounding spaces")
			}
		}
		cfg.bots = bots
	}
	if v, ok := keys["defaultSeverity"]; ok {
		var s string
		if json.Unmarshal(v, &s) != nil || !validSeverity(s) {
			return reviewConfig{}, mcpserver.Fail(mcpserver.Failed, "the config defaultSeverity must be high, medium, low or info")
		}
		cfg.defaultSeverity = s
	}
	return cfg, nil
}

// validBot accepts an author name of 1 to 100 runes with no control, format or
// line separator characters and no surrounding space.
func validBot(s string) bool {
	if s == "" || !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxBotRunes || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return false
		}
	}
	return true
}

// isBot reports whether a comment is a bot's: the forge says so, or its author
// is a configured bot name, compared case-insensitively.
func (c reviewConfig) isBot(cm comment) bool {
	return cm.IsBot || slices.ContainsFunc(c.bots, func(b string) bool { return strings.EqualFold(b, cm.Author) })
}

func describe(_ context.Context, _ *contract.Tool, raw json.RawMessage) (any, error) {
	var in struct {
		Config json.RawMessage `json:"config"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	if _, err := parseConfig(in.Config); err != nil {
		return nil, err
	}
	return contract.ReviewerDescription{
		DisplayName:  displayName,
		Severities:   append([]string(nil), severities...),
		Capabilities: append([]string(nil), capabilities...),
	}, nil
}

// comment is one review comment of the reviewer_findings input.
type comment struct {
	ID        string  `json:"id"`
	Author    string  `json:"author"`
	IsBot     bool    `json:"isBot"`
	Body      string  `json:"body"`
	Path      *string `json:"path"`
	Line      *int64  `json:"line"`
	CreatedAt string  `json:"createdAt"`
}

func findings(ctx context.Context, t *contract.Tool, raw json.RawMessage) (any, error) {
	var in struct {
		Forge    json.RawMessage `json:"forge"`
		Ref      string          `json:"ref"`
		Commit   string          `json:"commit"`
		Config   json.RawMessage `json:"config"`
		Comments *[]comment      `json:"comments"`
		Checks   json.RawMessage `json:"checks"`
	}
	if err := decodeArgs(raw, &in); err != nil {
		return nil, err
	}
	cfg, err := parseConfig(in.Config)
	if err != nil {
		return nil, err
	}
	// Without the comments there is nothing to read, which must never look
	// like a finished review with no findings.
	if in.Comments == nil {
		return nil, mcpserver.Fail(mcpserver.Failed, needsComments)
	}
	out, err := findingsOf(ctx, cfg, *in.Comments, outputBudget(t))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// findingsOf turns every bot comment into one finding and ignores every other
// comment, keeps at most 500 findings and fits them into budget bytes of JSON.
func findingsOf(ctx context.Context, cfg reviewConfig, comments []comment, budget int) (contract.ReviewerFindings, error) {
	cs := []candidate{}
	for i, cm := range comments {
		if ctx.Err() != nil {
			return contract.ReviewerFindings{}, mcpserver.Fail(mcpserver.Failed, "reading the comments took too long")
		}
		if !cfg.isBot(cm) {
			continue
		}
		at, _ := time.Parse(time.RFC3339, cm.CreatedAt)
		cs = append(cs, candidate{finding: findingOf(cfg, cm), at: at, order: i})
	}
	out := contract.ReviewerFindings{Status: "complete", Findings: boundFindings(cs, maxFindings)}
	if err := fitBodies(&out, budget); err != nil {
		return contract.ReviewerFindings{}, err
	}
	return out, nil
}

// findingOf is a bot comment's finding. Comments carry no resolution, so a
// finding is never resolved.
func findingOf(cfg reviewConfig, cm comment) contract.ReviewerFinding {
	f := contract.ReviewerFinding{
		ID:       cm.ID,
		Severity: severityOf(cm.Body, cfg.defaultSeverity),
		Title:    titleOf(cm.Body),
		Body:     cut(cm.Body, maxBodyRunes),
	}
	if cm.Path != nil {
		p := *cm.Path
		f.Path = &p
	}
	if cm.Line != nil {
		l := *cm.Line
		f.Line = &l
	}
	return f
}

// titleOf is the first line with text: control characters become spaces,
// format characters are dropped, whitespace collapses and markdown heading and
// emphasis markers are stripped from the leading end only, so a closing
// backtick or a trailing underscore stays, then it is cut to 300 runes. A
// comment with no text on any line has a fixed title, as the contract requires
// one.
func titleOf(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		t := strings.TrimRightFunc(stripMarkers(strings.Join(strings.Fields(strings.Map(visible, line)), " ")), unicode.IsSpace)
		if t != "" {
			return cutLine(t, maxTitleRunes)
		}
	}
	return fallbackTitle
}

// visible maps a control or line separator character to a space and drops a
// format character, such as a bidi override.
func visible(r rune) rune {
	switch {
	case unicode.IsControl(r) || r == 0x2028 || r == 0x2029:
		return ' '
	case unicode.Is(unicode.Cf, r):
		return -1
	}
	return r
}

// markerOrSpace reports a markdown heading or emphasis marker or a space.
func markerOrSpace(r rune) bool {
	return strings.ContainsRune("#*_>`", r) || unicode.IsSpace(r)
}

// stripMarkers drops leading markdown markers and spaces.
func stripMarkers(s string) string {
	return strings.TrimLeftFunc(s, markerOrSpace)
}

// cut bounds s to limit runes, marking a cut so it is never silent.
func cut(s string, limit int) string {
	return cutWith(s, limit, truncatedMark)
}

// cutLine bounds a single-line value to limit runes with a single-line mark.
func cutLine(s string, limit int) string {
	return cutWith(s, limit, lineCutMark)
}

func cutWith(s string, limit int, mark string) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	keep := limit - utf8.RuneCountInString(mark)
	runes := []rune(s)
	return string(runes[:keep]) + mark
}

// outputBudget bounds the JSON of one findings answer: the tool's output
// bound, and a third of the response bound less its envelope, since the server
// repeats the JSON as text content in the same message and escaping it as a
// string at most doubles it.
func outputBudget(t *contract.Tool) int {
	return min(t.MaxOutputBytes, (mcpserver.DefaultMaxResponseBytes-responseEnvelope)/3)
}

// fitBodies cuts finding bodies, each marked as cut, until out encodes in at
// most budget bytes. Every body gets an equal share of the room the rest of the
// answer leaves, and a body shorter than its share gives the remainder to the
// longer ones. When even marked stubs do not fit, the call fails rather than
// dropping findings or text silently.
func fitBodies(out *contract.ReviewerFindings, budget int) error {
	tooLarge := mcpserver.Fail(mcpserver.Failed, "the review comments do not fit the output bound")
	raw, err := json.Marshal(out)
	if err != nil {
		return mcpserver.Fail(mcpserver.Failed, "the findings cannot be encoded")
	}
	if len(raw) <= budget {
		return nil
	}
	bodies := make([]string, len(out.Findings))
	for i := range out.Findings {
		bodies[i], out.Findings[i].Body = out.Findings[i].Body, ""
	}
	empty, err := json.Marshal(out)
	if err != nil {
		return mcpserver.Fail(mcpserver.Failed, "the findings cannot be encoded")
	}
	room := budget - len(empty)
	if room < 0 {
		return tooLarge
	}
	order := make([]int, len(bodies))
	sizes := make([]int, len(bodies))
	for i := range bodies {
		order[i], sizes[i] = i, encodedLen(bodies[i])
	}
	slices.SortStableFunc(order, func(a, b int) int { return sizes[a] - sizes[b] })
	for k, i := range order {
		body, ok := cutEncoded(bodies[i], sizes[i], room/(len(order)-k))
		if !ok {
			return tooLarge
		}
		out.Findings[i].Body = body
		room -= encodedLen(body)
	}
	if raw, err = json.Marshal(out); err != nil || len(raw) > budget {
		return tooLarge
	}
	return nil
}

// cutEncoded bounds s, whose escaped JSON content is size bytes, to limit
// bytes of escaped content and maxBodyRunes runes, ending a cut with the
// truncated mark. It reports false when not even the mark fits.
func cutEncoded(s string, size, limit int) (string, bool) {
	if size <= limit {
		return s, true
	}
	keep := limit - encodedLen(truncatedMark)
	if keep < 0 {
		return "", false
	}
	keepRunes := maxBodyRunes - utf8.RuneCountInString(truncatedMark)
	n, runes, end := 0, 0, 0
	for i, r := range s {
		w := encLen(r)
		if n+w > keep || runes == keepRunes {
			break
		}
		_, width := utf8.DecodeRuneInString(s[i:])
		n, runes, end = n+w, runes+1, i+width
	}
	return s[:end] + truncatedMark, true
}

// encodedLen bounds from above the bytes encoding/json writes for s inside
// its quotes.
func encodedLen(s string) int {
	n := 0
	for _, r := range s {
		n += encLen(r)
	}
	return n
}

// encLen bounds from above the bytes encoding/json writes for one rune of a
// string: two for a short escape, six for a \u escape (other controls, the
// HTML characters, the line separators and invalid bytes), its UTF-8 length
// otherwise.
func encLen(r rune) int {
	switch {
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20 || r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029 || r == utf8.RuneError:
		return 6
	}
	return utf8.RuneLen(r)
}
