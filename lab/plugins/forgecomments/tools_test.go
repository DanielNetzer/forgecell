package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/plugins/internal/mcpserver"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func reviewerTool(t *testing.T, name string) *contract.Tool {
	t.Helper()
	c, _ := contract.Lookup("reviewer")
	tool, ok := c.Tool(name)
	if !ok {
		t.Fatalf("no contract tool %s", name)
	}
	return tool
}

// call runs a tool's bound handler, with schema validation and the contract
// timeout, as the server does.
func call(t *testing.T, name string, args map[string]any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return bind(reviewerTool(t, name))(context.Background(), raw)
}

// decoded calls a tool, requires success and decodes the output as the engine
// does, so every output is proved valid under reviewer.v1.json. It also
// returns the plugin's own output, before the engine's sanitizing.
func decoded[T any](t *testing.T, name string, args map[string]any) (T, T) {
	t.Helper()
	out, err := call(t, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	v, err := reviewerTool(t, name).Decode(raw)
	if err != nil {
		t.Fatalf("%s output is invalid: %v", name, err)
	}
	return out.(T), v.(T)
}

// failure requires a tool failure with reason failed and returns its text.
func failure(t *testing.T, err error) string {
	t.Helper()
	var e *mcpserver.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not a tool failure", err)
	}
	if e.Reason() != mcpserver.Failed {
		t.Fatalf("reason %s (%v), want failed", e.Reason(), err)
	}
	return err.Error()
}

// findingsArgs are reviewer_findings arguments carrying comments; a nil config
// is left out.
func findingsArgs(config map[string]any, comments []map[string]any) map[string]any {
	args := map[string]any{"forge": map[string]any{"kind": "gitlab"}, "ref": "feature/x", "commit": testCommit, "comments": comments}
	if config != nil {
		args["config"] = config
	}
	return args
}

// botComment is a bot comment created n minutes after a fixed time.
func botComment(id, body string, n int) map[string]any {
	return map[string]any{"id": id, "author": "review-bot", "isBot": true, "body": body, "createdAt": createdAt(n)}
}

func createdAt(n int) string {
	return fmt.Sprintf("2026-11-01T%02d:%02d:00Z", n/60%24, n%60)
}

func ids(fs []contract.ReviewerFinding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func TestDescribeListsForgeCommentsAfterCheckingTheConfig(t *testing.T) {
	want := contract.ReviewerDescription{DisplayName: "Review bot comments", Severities: []string{"high", "medium", "low", "info"}, Capabilities: []string{"forge-comments"}}
	for _, cfg := range []map[string]any{{}, {"bots": []string{"cursor[bot]"}, "defaultSeverity": "high"}, {"bots": []string{}}} {
		for range 2 {
			got, engine := decoded[contract.ReviewerDescription](t, "reviewer_describe", map[string]any{"config": cfg})
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(engine, want) || !engine.Has(contract.ReviewerForgeComments) {
				t.Fatalf("describe %+v, want %+v", got, want)
			}
		}
	}
}

func TestTheConfigIsRefusedWithAPlainReason(t *testing.T) {
	seventeen := []string{}
	for i := range 17 {
		seventeen = append(seventeen, fmt.Sprintf("bot-%d", i))
	}
	// Sixteen valid names of 100 three-byte runes exceed 4 KiB.
	wide := []string{}
	for range 16 {
		wide = append(wide, strings.Repeat("€", 100))
	}
	cases := []struct {
		name   string
		config map[string]any
		reason string
	}{
		{"unknown key", map[string]any{"bots": []string{}, "secretive-key": true}, "the config holds only bots and defaultSeverity"},
		{"too many bots", map[string]any{"bots": seventeen}, "more than 16 author names"},
		{"bad severity", map[string]any{"defaultSeverity": "critical"}, "defaultSeverity must be high, medium, low or info"},
		{"uppercase severity", map[string]any{"defaultSeverity": "HIGH"}, "defaultSeverity must be"},
		{"null severity", map[string]any{"defaultSeverity": nil}, "defaultSeverity must be"},
		{"numeric severity", map[string]any{"defaultSeverity": 3}, "defaultSeverity must be"},
		{"bots not a list", map[string]any{"bots": "cursor"}, "bots must be a list of author names"},
		{"bot not a string", map[string]any{"bots": []any{7}}, "bots must be a list of author names"},
		{"empty bot", map[string]any{"bots": []string{""}}, "each config bot must be"},
		{"spaced bot", map[string]any{"bots": []string{" cursor"}}, "each config bot must be"},
		{"bidi bot", map[string]any{"bots": []string{"cur‮sor"}}, "each config bot must be"},
		{"control bot", map[string]any{"bots": []string{"cur\x07sor"}}, "each config bot must be"},
		{"long bot", map[string]any{"bots": []string{strings.Repeat("b", 101)}}, "each config bot must be"},
		{"over 4 KiB", map[string]any{"bots": wide}, "at most 4096 bytes"},
	}
	for _, c := range cases {
		for _, tool := range []string{"reviewer_describe", "reviewer_findings"} {
			args := map[string]any{"config": c.config}
			if tool == "reviewer_findings" {
				args = findingsArgs(c.config, []map[string]any{botComment("c1", "x", 0)})
			}
			_, err := call(t, tool, args)
			msg := failure(t, err)
			if !strings.Contains(msg, c.reason) {
				t.Fatalf("%s %s: %q, want %q", c.name, tool, msg, c.reason)
			}
			for _, leak := range []string{"secretive", "critical", "cursor", "bot-16", "€"} {
				if strings.Contains(msg, leak) {
					t.Fatalf("%s %s: the reason names a config value: %q", c.name, tool, msg)
				}
			}
		}
	}
	// The bounds themselves are accepted: 16 names of 100 runes within 4 KiB.
	sixteen := []string{}
	for i := range 16 {
		sixteen = append(sixteen, fmt.Sprintf("%03d", i)+strings.Repeat("b", 97))
	}
	if _, err := call(t, "reviewer_describe", map[string]any{"config": map[string]any{"bots": sixteen, "defaultSeverity": "info"}}); err != nil {
		t.Fatalf("a config at the bounds: %v", err)
	}
}

func TestFindingsWithoutCommentsAreAToolError(t *testing.T) {
	for _, cfg := range []map[string]any{nil, {}, {"bots": []string{"cursor"}}} {
		args := findingsArgs(cfg, nil)
		delete(args, "comments")
		out, err := call(t, "reviewer_findings", args)
		if out != nil {
			t.Fatalf("output %+v", out)
		}
		if msg := failure(t, err); msg != "failed: this reviewer needs the forge's review comments" {
			t.Fatalf("message %q", msg)
		}
	}
	// An empty list is the forge's answer: no comments, so a complete review
	// with no findings, encoded as an empty list.
	got, _ := decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(nil, []map[string]any{}))
	raw, _ := json.Marshal(got)
	if string(raw) != `{"status":"complete","findings":[]}` {
		t.Fatalf("output %s", raw)
	}
}

// TestTheServerAnswersAMissingCommentsCallWithAnError runs the server as main
// builds it over in-memory pipes.
func TestTheServerAnswersAMissingCommentsCallWithAnError(t *testing.T) {
	var stderr, out bytes.Buffer
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"reviewer_findings","arguments":{"forge":{},"ref":"main","commit":"` + testCommit + `"}}}` + "\n"
	if err := newServer(&stderr).Serve(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses %q", out.String())
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &list); err != nil || len(list.Result.Tools) != 2 || list.Result.Tools[0].Name != "reviewer_describe" || list.Result.Tools[1].Name != "reviewer_findings" {
		t.Fatalf("tools/list %s (%v)", lines[0], err)
	}
	var res struct {
		Result struct {
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &res); err != nil || !res.Result.IsError || res.Result.StructuredContent.Reason != "failed" || res.Result.StructuredContent.Message != needsComments {
		t.Fatalf("tools/call %s (%v)", lines[1], err)
	}
}

func TestBotCommentsBecomeFindingsAndHumanCommentsAreIgnored(t *testing.T) {
	comments := []map[string]any{
		{"id": "forge-bot", "author": "gitlab-duo", "isBot": true, "body": "Flagged by the forge", "createdAt": createdAt(1)},
		{"id": "listed", "author": "Cursor[bot]", "isBot": false, "body": "Listed author", "createdAt": createdAt(2)},
		{"id": "human", "author": "alice", "isBot": false, "body": "High Severity: looks wrong to me", "createdAt": createdAt(3)},
		{"id": "near-name", "author": "cursor", "isBot": false, "body": "Not quite the listed name", "createdAt": createdAt(4)},
	}
	got, _ := decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(map[string]any{"bots": []string{"CURSOR[BOT]"}}, comments))
	if got.Status != "complete" || !reflect.DeepEqual(ids(got.Findings), []string{"listed", "forge-bot"}) {
		t.Fatalf("findings %+v", got)
	}
	// Without the bots list only the forge's isBot counts.
	got, _ = decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(nil, comments))
	if !reflect.DeepEqual(ids(got.Findings), []string{"forge-bot"}) {
		t.Fatalf("findings %+v", got)
	}
}

func TestFindingFieldsComeFromTheComment(t *testing.T) {
	anchored := botComment("note-7", "### Unchecked error\n\nThe error from Close is dropped.", 2)
	anchored["path"] = "internal/store/file.go"
	anchored["line"] = 42
	loose := botComment("note-8", "Summary of the review", 1)
	got, engine := decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(nil, []map[string]any{anchored, loose}))
	path, line := "internal/store/file.go", int64(42)
	want := contract.ReviewerFindings{Status: "complete", Findings: []contract.ReviewerFinding{
		{ID: "note-7", Severity: "medium", Path: &path, Line: &line, Title: "Unchecked error", Body: "### Unchecked error\n\nThe error from Close is dropped.", Resolved: false},
		{ID: "note-8", Severity: "medium", Title: "Summary of the review", Body: "Summary of the review", Resolved: false},
	}}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(engine, want) {
		t.Fatalf("findings %+v, want %+v", got, want)
	}
}

func TestTitlesAreStrippedCollapsedAndCut(t *testing.T) {
	cases := []struct{ body, want string }{
		{"### Null dereference in parse\n\nbody", "Null dereference in parse"},
		{"**Bug:** `x` is nil", "Bug:** `x` is nil"},
		{"> *Quoted*   title\twith\ttabs", "Quoted* title with tabs"},
		{"__`Wrapped`__", "Wrapped`__"},
		{"### Race in `cache.Get`", "Race in `cache.Get`"},
		{"## snake_case_", "snake_case_"},
		{"> Flow is a -> b>  ", "Flow is a -> b>"},
		{"# Title  \t ", "Title"},
		{"\n\n   \r\n## \n`Second` line\nthird", "Second` line"},
		{"a‮b​c\x07d e", "abc d e"},
		{"", fallbackTitle},
		{"***\n###\n> \n``", fallbackTitle},
		{"‮⁦", fallbackTitle},
		{strings.Repeat("x", 400), strings.Repeat("x", 299) + "…"},
		{"# " + strings.Repeat("é", 300), strings.Repeat("é", 300)},
		{"# " + strings.Repeat("é", 301), strings.Repeat("é", 299) + "…"},
	}
	for _, c := range cases {
		if got := titleOf(c.body); got != c.want {
			t.Fatalf("titleOf(%q) = %q, want %q", c.body, got, c.want)
		}
	}
	got, _ := decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(nil, []map[string]any{botComment("c1", "## "+strings.Repeat("t", 500)+"\nrest", 0)}))
	if title := got.Findings[0].Title; utf8.RuneCountInString(title) != maxTitleRunes || !strings.HasSuffix(title, "…") {
		t.Fatalf("title %q", title)
	}
}

func TestHostileTextStaysInsideTheContractBounds(t *testing.T) {
	tool := reviewerTool(t, "reviewer_findings")
	unit := "‮<\x00\"é \\&\x1b"
	hostile := strings.Repeat(unit, 65536/utf8.RuneCountInString(unit)+1)
	runes := []rune(hostile)
	hostile = string(runes[:65536])
	marked := "⁧[high]⁩ " + string(runes[:300]) + "\n" + string(runes[:65000])
	comments := []map[string]any{botComment("c1", hostile, 0), botComment("c2", marked, 1)}
	got, engine := decoded[contract.ReviewerFindings](t, "reviewer_findings", findingsArgs(nil, comments))
	raw, _ := json.Marshal(got)
	if len(raw) > tool.MaxOutputBytes || len(got.Findings) != 2 || len(engine.Findings) != 2 {
		t.Fatalf("%d bytes, %d findings", len(raw), len(got.Findings))
	}
	for _, f := range got.Findings {
		if n := utf8.RuneCountInString(f.Title); n == 0 || n > maxTitleRunes {
			t.Fatalf("title of %d runes", n)
		}
		for _, r := range f.Title {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
				t.Fatalf("title %q keeps %U", f.Title, r)
			}
		}
		if utf8.RuneCountInString(f.Body) > maxBodyRunes {
			t.Fatalf("body of %d runes", utf8.RuneCountInString(f.Body))
		}
	}
	if got.Findings[0].ID != "c2" || got.Findings[0].Severity != "high" {
		t.Fatalf("the marker behind bidi characters was not read: %+v", got.Findings[0].Severity)
	}
}

// TestBodiesAreCutToFitTheOutputBound fills a call with bodies that encode far
// over the bound, as a direct call can: every finding is kept, the long bodies
// are cut and marked, a short one is whole, and the answer fits.
func TestBodiesAreCutToFitTheOutputBound(t *testing.T) {
	tool := reviewerTool(t, "reviewer_findings")
	budget := outputBudget(tool)
	if budget > tool.MaxOutputBytes || budget <= 0 {
		t.Fatalf("budget %d", budget)
	}
	comments := []comment{{ID: "short", IsBot: true, Body: "Short and whole", CreatedAt: createdAt(0)}}
	for i := range 40 {
		comments = append(comments, comment{ID: fmt.Sprintf("long-%02d", i), IsBot: true, Body: strings.Repeat("<", maxBodyRunes), CreatedAt: createdAt(i + 1)})
	}
	out, err := findingsOf(context.Background(), reviewConfig{defaultSeverity: "medium"}, comments, budget)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > budget {
		t.Fatalf("%d bytes over the %d budget", len(raw), budget)
	}
	if _, err := tool.Decode(raw); err != nil {
		t.Fatalf("output invalid: %v", err)
	}
	if len(out.Findings) != 41 {
		t.Fatalf("%d findings", len(out.Findings))
	}
	for _, f := range out.Findings {
		switch {
		case f.ID == "short":
			if f.Body != "Short and whole" {
				t.Fatalf("short body %q", f.Body)
			}
		case !strings.HasSuffix(f.Body, truncatedMark) || len(f.Body) < 1000:
			t.Fatalf("%s body of %d bytes is not a marked cut", f.ID, len(f.Body))
		}
	}
	// When not even marked stubs fit, the call fails rather than answering
	// with less than it read.
	if _, err := findingsOf(context.Background(), reviewConfig{defaultSeverity: "medium"}, comments, 2000); !strings.Contains(failure(t, err), "do not fit the output bound") {
		t.Fatalf("error %v", err)
	}
}

func TestCutEncodedNeverSplitsARune(t *testing.T) {
	s := "aé€😀<\xff" + strings.Repeat("b", 20)
	for limit := 13; limit < encodedLen(s); limit++ {
		got, ok := cutEncoded(s, encodedLen(s), limit)
		kept := strings.TrimSuffix(got, truncatedMark)
		if !ok || !strings.HasPrefix(s, kept) || !utf8.RuneStart(s[len(kept)]) {
			t.Fatalf("limit %d: %q", limit, got)
		}
		if encodedLen(got) > limit || !strings.HasSuffix(got, truncatedMark) {
			t.Fatalf("limit %d: %q encodes to %d", limit, got, encodedLen(got))
		}
		enc, _ := json.Marshal(got)
		if len(enc)-2 > encodedLen(got) {
			t.Fatalf("encodedLen(%q) = %d under json's %d", got, encodedLen(got), len(enc)-2)
		}
	}
	if _, ok := cutEncoded(s, encodedLen(s), 12); ok {
		t.Fatal("a limit below the mark was accepted")
	}
}

func TestTheContractRefusesMoreThan500Comments(t *testing.T) {
	comments := []map[string]any{}
	for i := range 501 {
		comments = append(comments, botComment(fmt.Sprintf("c%d", i), "x", i))
	}
	_, err := call(t, "reviewer_findings", findingsArgs(nil, comments))
	if msg := failure(t, err); !strings.Contains(msg, "do not match the input schema") {
		t.Fatalf("message %q", msg)
	}
}
