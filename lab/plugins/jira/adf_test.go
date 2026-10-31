package main

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func flatten(t *testing.T, doc string) string {
	t.Helper()
	got, err := descriptionText(json.RawMessage(doc), maxBodyRunes)
	if err != nil {
		t.Fatalf("description %s: %v", doc, err)
	}
	return got
}

func text(s string, marks ...string) string {
	if len(marks) == 0 {
		return `{"type":"text","text":` + quote(s) + `}`
	}
	ms := []string{}
	for _, m := range marks {
		ms = append(ms, `{"type":"`+m+`","attrs":{"href":"https://example.com"}}`)
	}
	return `{"type":"text","text":` + quote(s) + `,"marks":[` + strings.Join(ms, ",") + `]}`
}

func quote(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func para(inline ...string) string {
	return `{"type":"paragraph","content":[` + strings.Join(inline, ",") + `]}`
}

func item(blocks ...string) string {
	return `{"type":"listItem","content":[` + strings.Join(blocks, ",") + `]}`
}

func doc(blocks ...string) string {
	return `{"type":"doc","version":1,"content":[` + strings.Join(blocks, ",") + `]}`
}

func TestADFFlattensBlocksAndMarks(t *testing.T) {
	got := flatten(t, doc(
		`{"type":"heading","attrs":{"level":2},"content":[`+text("Summary")+`]}`,
		para(text("Some "), text("bold", "strong"), text(" and "), text("linked", "link", "em"), `{"type":"hardBreak"}`, text("next line")),
		`{"type":"codeBlock","attrs":{"language":"go"},"content":[`+text("func main() {\n\tprintln(1)\n}")+`]}`,
		para(`{"type":"mention","attrs":{"id":"1","text":"@Alice"}}`, text(" "), `{"type":"emoji","attrs":{"shortName":":smile:"}}`, text(" "), `{"type":"inlineCard","attrs":{"url":"https://example.com/x"}}`, text(" "), `{"type":"status","attrs":{"text":"BLOCKED"}}`),
		`{"type":"blockquote","content":[`+para(text("quoted"))+`]}`,
		`{"type":"rule"}`,
		`{"type":"table","content":[{"type":"tableRow","content":[{"type":"tableHeader","content":[`+para(text("A"))+`]},{"type":"tableHeader","content":[`+para(text("B"))+`]}]},{"type":"tableRow","content":[{"type":"tableCell","content":[`+para(text("1"))+`]},{"type":"tableCell","content":[`+para(text("2"))+`]}]}]}`,
	))
	want := strings.Join([]string{
		"Summary",
		"",
		"Some bold and linked",
		"next line",
		"",
		"func main() {",
		"\tprintln(1)",
		"}",
		"",
		"@Alice :smile: https://example.com/x BLOCKED",
		"",
		"> quoted",
		"",
		"----",
		"",
		"A | B",
		"1 | 2",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestADFFlattensNestedLists(t *testing.T) {
	got := flatten(t, doc(
		`{"type":"bulletList","content":[`+
			item(para(text("one")))+`,`+
			item(para(text("two")), `{"type":"orderedList","attrs":{"order":3},"content":[`+item(para(text("three")))+`,`+item(para(text("four")), `{"type":"bulletList","content":[`+item(para(text("deep")))+`]}`)+`]}`)+`,`+
			item(`{"type":"codeBlock","content":[`+text("code\nlines")+`]}`)+
			`]}`,
		`{"type":"orderedList","content":[`+item(para(text("first")))+`,`+item(para(text("second")))+`]}`,
	))
	want := strings.Join([]string{
		"- one",
		"- two",
		"  3. three",
		"  4. four",
		"     - deep",
		"-",
		"  code",
		"  lines",
		"",
		"1. first",
		"2. second",
	}, "\n")
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestADFSkipsUnknownNodes(t *testing.T) {
	got := flatten(t, doc(
		`{"type":"mediaSingle","content":[{"type":"media","attrs":{"id":"x","type":"file"}}]}`,
		para(text("kept"), `{"type":"futureInline","text":"dropped","content":[`+text("dropped too")+`]}`),
		`{"type":"futureBlock","content":[`+para(text("hidden"))+`]}`,
		`{"type":"bulletList","content":[{"type":"notAnItem","content":[`+para(text("hidden"))+`]}]}`,
		`{"type":"panel","attrs":{"panelType":"info"},"content":[`+para(text("in a panel"))+`]}`,
		`{"type":"expand","attrs":{"title":"Details"},"content":[`+para(text("expanded"))+`]}`,
	))
	want := "kept\n\nin a panel\n\nDetails\nexpanded"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestADFDescriptionShapes(t *testing.T) {
	for raw, want := range map[string]string{"": "", "null": "", ` null `: "", `"plain text"`: "plain text", doc(): ""} {
		if got := flatten(t, raw); got != want {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{"7", "[]", `{"type":`, `{"type":"doc","content":"x"}`, "true"} {
		if _, err := descriptionText(json.RawMessage(raw), maxBodyRunes); err == nil {
			t.Errorf("%q was accepted", raw)
		}
	}
	long, err := descriptionText(json.RawMessage(quote(strings.Repeat("é", maxBodyRunes+10))), maxBodyRunes)
	if err != nil || utf8.RuneCountInString(long) != maxBodyRunes || !strings.HasSuffix(long, truncatedMark) {
		t.Fatalf("a long plain description: %d runes, %v", utf8.RuneCountInString(long), err)
	}
}

func TestADFBoundsAHugeDocument(t *testing.T) {
	paras := []string{}
	for range 5000 {
		paras = append(paras, para(text(strings.Repeat("word ", 20))))
	}
	got := flatten(t, doc(paras...))
	if n := utf8.RuneCountInString(got); n > maxBodyRunes || n < maxBodyRunes-200 {
		t.Fatalf("%d runes", n)
	}
	if !strings.HasSuffix(got, truncatedMark) {
		t.Fatal("the cut is not marked")
	}
	// One text node past the bound is cut too.
	got = flatten(t, doc(para(text(strings.Repeat("x", 3*maxBodyRunes)))))
	if utf8.RuneCountInString(got) != maxBodyRunes || !strings.HasSuffix(got, truncatedMark) {
		t.Fatalf("%d runes", utf8.RuneCountInString(got))
	}
}

func TestADFBoundsNodeCount(t *testing.T) {
	inline := []string{}
	for range maxADFNodes + 10 {
		inline = append(inline, `{"type":"hardBreak"}`)
	}
	got := flatten(t, doc(para(inline...), para(text("after"))))
	if strings.Contains(got, "after") || !strings.HasSuffix(got, truncatedMark) {
		t.Fatalf("the node bound did not cut: %q", got[max(0, len(got)-40):])
	}
}

func TestADFBoundsADeepDocument(t *testing.T) {
	deep := para(text("bottom"))
	for range 200 {
		deep = `{"type":"blockquote","content":[` + deep + `]}`
	}
	got := flatten(t, doc(para(text("top")), deep))
	if strings.Contains(got, "bottom") || !strings.HasPrefix(got, "top") || !strings.HasSuffix(got, truncatedMark) {
		t.Fatalf("got %q", got)
	}
	lists := para(text("leaf"))
	for range 100 {
		lists = `{"type":"bulletList","content":[` + item(lists) + `]}`
	}
	got = flatten(t, doc(lists))
	if strings.Contains(got, "leaf") || !strings.HasSuffix(got, truncatedMark) || utf8.RuneCountInString(got) > maxBodyRunes {
		t.Fatalf("got %q", got)
	}
	// Within the bound, nothing is cut.
	shallow := para(text("bottom"))
	for range 10 {
		shallow = `{"type":"blockquote","content":[` + shallow + `]}`
	}
	if got := flatten(t, doc(shallow)); got != strings.Repeat("> ", 10)+"bottom" {
		t.Fatalf("got %q", got)
	}
}
