package intake

import (
	"strings"
	"testing"
)

const jiraPattern = `^[A-Z][A-Z0-9]+-[1-9][0-9]*$`

func TestParseIssueKeyAccepts(t *testing.T) {
	cases := []struct{ input, pattern string }{
		{"ENG-1234", jiraPattern},
		{"A1-9", jiraPattern},
		{"PROJ_42", `^[A-Z]+_[0-9]+$`},
		{"Ünï-1", `^\p{L}+-[0-9]+$`},
		{strings.Repeat("a", MaxIssueKeyRunes), `^a+$`},
		{strings.Repeat("é", MaxIssueKeyRunes), `^é+$`},
	}
	for _, c := range cases {
		got, err := ParseIssueKey(c.input, c.pattern)
		if err != nil || got != c.input {
			t.Errorf("ParseIssueKey(%.20q, %q) = %q, %v", c.input, c.pattern, got, err)
		}
	}
}

func TestParseIssueKeyRefuses(t *testing.T) {
	cases := []struct {
		name, input, pattern, want string
	}{
		{"no match", "eng-1234", jiraPattern, "issue key does not match the tracker's key pattern"},
		{"leading zero", "ENG-01", jiraPattern, "issue key does not match the tracker's key pattern"},
		{"surrounding space", " ENG-1", jiraPattern, "issue key does not match the tracker's key pattern"},
		{"empty", "", jiraPattern, "issue key is empty"},
		{"unanchored", "ENG-1", `[A-Z]+-[0-9]+`, "issue key pattern must be anchored with ^ and $"},
		{"no end anchor", "ENG-1", `^[A-Z]+-[0-9]+`, "issue key pattern must be anchored with ^ and $"},
		{"no start anchor", "ENG-1", `[A-Z]+-[0-9]+$`, "issue key pattern must be anchored with ^ and $"},
		{"bare anchors", "x", `$`, "issue key pattern must be anchored with ^ and $"},
		// The pattern is always applied to the whole key, so an alternation written
		// between the anchors cannot match a substring.
		{"alternation substring", "xxENG-1", `^ENG-1|ENG-1$`, "issue key does not match the tracker's key pattern"},
		{"multiline flag", "ENG-1", `^(?m)ENG$`, "issue key does not match the tracker's key pattern"},
		{"invalid regexp", "ENG-1", `^[A-Z$`, "issue key pattern is not a valid regular expression"},
		{"backreference", "aa", `^(a)\1$`, "issue key pattern is not a valid regular expression"},
		// Unbalanced groups would compile once wrapped in ^(?:…)$ as a top-level
		// alternation, so the raw pattern is compiled on its own first.
		{"unbalanced group x", "x", `^x)|(y$`, "issue key pattern is not a valid regular expression"},
		{"unbalanced group y", "y", `^x)|(y$`, "issue key pattern is not a valid regular expression"},
		{"overlong pattern", "ENG-1", "^" + strings.Repeat("a", MaxKeyPatternBytes) + "$", "issue key pattern is longer than 256 bytes"},
		{"overlong key", strings.Repeat("A", MaxIssueKeyRunes+1), `^A+$`, "issue key is longer than 128 characters"},
		{"overlong multibyte key", strings.Repeat("é", MaxIssueKeyRunes+1), `^é+$`, "issue key is longer than 128 characters"},
		{"huge key", strings.Repeat("A", 10<<20), `^A+$`, "issue key is longer than 128 characters"},
		{"invalid utf-8", "ENG-\xff1", `^.+$`, "issue key is not valid UTF-8"},
		{"bidi override", "ENG-1\xe2\x80\xae", `^.+$`, "issue key contains a control, format, bidi or line separator character"},
		{"bidi isolate", "\xe2\x81\xa6ENG-1", `^.+$`, "issue key contains a control, format, bidi or line separator character"},
		{"rtl mark", "ENG\xe2\x80\x8f-1", `^.+$`, "issue key contains a control, format, bidi or line separator character"},
		{"zero width joiner", "ENG\xe2\x80\x8d-1", `^.+$`, "issue key contains a control, format, bidi or line separator character"},
		{"line separator", "ENG-1\xe2\x80\xa8", `^.+$`, "issue key contains a control, format, bidi or line separator character"},
		{"paragraph separator", "ENG-1\xe2\x80\xa9", `^.+$`, "issue key contains a control, format, bidi or line separator character"},
		{"newline", "ENG-1\nENG-2", `^.+$`, "issue key contains a control, format, bidi or line separator character"},
		{"nul", "ENG\x00-1", `^.+$`, "issue key contains a control, format, bidi or line separator character"},
		{"c1 control", "ENG\u0085-1", `^.+$`, "issue key contains a control, format, bidi or line separator character"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseIssueKey(c.input, c.pattern)
			if err == nil || err.Error() != c.want || got != "" {
				t.Fatalf("ParseIssueKey = %q, %v; want error %q", got, err, c.want)
			}
		})
	}
}

func TestBindKeyRepository(t *testing.T) {
	if got, err := BindKeyRepository("Acme/widgets"); err != nil || got != "Acme/widgets" {
		t.Fatalf("BindKeyRepository = %q, %v", got, err)
	}
	for _, repo := range []string{"", "acme", "acme/", "../widgets", "acme/..", "acme/widgets/x", "acme widgets/x"} {
		if got, err := BindKeyRepository(repo); err == nil {
			t.Errorf("BindKeyRepository(%q) = %q", repo, got)
		}
	}
}
