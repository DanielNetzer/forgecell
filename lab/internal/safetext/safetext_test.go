package safetext

import (
	"strings"
	"testing"
)

func TestTextStripsFormatAndControlCharacters(t *testing.T) {
	cases := map[string]struct {
		in    string
		limit int
		want  string
	}{
		"bidi override":   {"safe‮exe.txt", 50, "safeexe.txt"},
		"zero width":      {"a‍b​c", 50, "abc"},
		"controls":        {"a\x00b\tc\nd", 50, "a b c d"},
		"invalid utf8":    {"a\xffb", 50, "a�b"},
		"rune bound":      {"héllo wörld", 5, "héllo…"},
		"trimmed":         {"  \n padded \t ", 50, "padded"},
		"exact bound":     {"abc", 3, "abc"},
		"bound after Cf":  {"‮abc", 3, "abc"},
		"empty":           {"", 10, ""},
		"only formatting": {"‮​", 10, ""},
	}
	for name, c := range cases {
		if got := Text(c.in, c.limit); got != c.want {
			t.Errorf("%s: Text(%q, %d) = %q, want %q", name, c.in, c.limit, got, c.want)
		}
	}
}

func TestMultilineKeepsLinesOnly(t *testing.T) {
	if got := Multiline(" a\r\nb\n\tc\x00d‮e\r ", 100); got != "a\nb\n\tc de" {
		t.Fatalf("Multiline = %q", got)
	}
	if got := Multiline("abcdef", 3); got != "abc…" {
		t.Fatalf("Multiline bound = %q", got)
	}
}

func TestLineAndParagraphSeparatorsBecomeSpaces(t *testing.T) {
	const ls, ps = "\xe2\x80\xa8", "\xe2\x80\xa9" // U+2028, U+2029
	if got := Text("a"+ls+"b"+ps+"c", 50); got != "a b c" {
		t.Fatalf("Text = %q", got)
	}
	if got := Multiline("a"+ls+"b\nc"+ps+"d", 50); got != "a b\nc d" {
		t.Fatalf("Multiline = %q", got)
	}
	if got := Text(ls+ps, 50); got != "" {
		t.Fatalf("Text of separators only = %q", got)
	}
}

func TestRedactSecretsCoversTheSharedRules(t *testing.T) {
	for _, secret := range []string{
		"Authorization: Bearer abcdefghijklmnopqrstuvwxyz",
		"GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123",
		"key sk-ant-abcdefghijklmnop1234",
		"https://user:hunter2@example.com/repo",
		"glpat-abcdefghijklmnopqrstuv",
		`password="correct horse battery"`,
		"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY-----",
	} {
		got := RedactSecrets(secret)
		if !strings.Contains(got, SecretMark) {
			t.Errorf("RedactSecrets(%q) = %q, want a redaction", secret, got)
		}
		for _, leaked := range []string{"abcdefghijklmnop", "hunter2", "horse", "MIIabc"} {
			if strings.Contains(got, leaked) {
				t.Errorf("RedactSecrets(%q) = %q leaks %q", secret, got, leaked)
			}
		}
	}
	if got := RedactSecrets("plain prose about bearer tokens"); got != "plain prose about bearer tokens" {
		t.Fatalf("prose was redacted: %q", got)
	}
}

func TestCutRedactedNeverServesPartOfAStraddlingSecret(t *testing.T) {
	s := "log GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123 tail"
	cut := strings.Index(s, "ghp_") + 6
	got := CutRedacted(s, cut)
	if strings.Contains(got, "ghp_ab") || !strings.HasSuffix(got, SecretMark) {
		t.Fatalf("CutRedacted at %d = %q", cut, got)
	}
	if got := CutRedacted("héllo", 2); got != "h" {
		t.Fatalf("cut inside a rune = %q, want it backed off to a rune start", got)
	}
	if got := CutRedacted("abc", 99); got != "abc" {
		t.Fatalf("limit past the end = %q", got)
	}
}

func TestSecretSpansAreSortedAndMerged(t *testing.T) {
	s := "API_KEY=sk-abcdefghijklmnopqrstu and ghp_abcdefghijklmnopqrstuvwxyz"
	spans := SecretSpans(s)
	if len(spans) != 2 {
		t.Fatalf("spans = %v, want two merged spans", spans)
	}
	if spans[0][1] > spans[1][0] {
		t.Fatalf("spans overlap or are unsorted: %v", spans)
	}
}

func TestRedactSecretsIsBoundedOnHostileInput(t *testing.T) {
	huge := strings.Repeat("token=", 20000) + strings.Repeat("x", 100000)
	if got := RedactSecrets(huge); strings.Contains(got, strings.Repeat("x", 10)) {
		t.Fatal("a long secret value survived redaction")
	}
}
