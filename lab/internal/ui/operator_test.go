package ui

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// GET /api/lab serves the configured operator name bounded, redacted and never
// as an email, with initials for the avatar; an unusable value is the neutral
// fallback.
func TestUIOperatorOnTheLabIsBoundedRedactedAndNeverAnEmail(t *testing.T) {
	long := strings.Repeat("é", 200)
	for _, c := range []struct {
		name, raw, want, initials string
		set                       bool
	}{
		{"plain", "Ada Lovelace", "Ada Lovelace", "AL", true},
		{"one word", "ada", "ada", "A", true},
		{"three words", "Grace Brewster Hopper", "Grace Brewster Hopper", "GH", true},
		{"unset", "", OperatorFallback, "", false},
		{"blank", " \t\n ", OperatorFallback, "", false},
		{"email only", "ada@example.com", OperatorFallback, "", false},
		{"name and email", "Ada Lovelace <ada@example.com>", "Ada Lovelace", "AL", true},
		{"fullwidth at", "＠handle Ada", "Ada", "A", true},
		{"control and bidi", "Ada\x1b[31m‮ Love\x00lace\n", "Ada Love lace", "AL", true},
		{"secret", "Ada token=ghp_abcdefghijklmnopqrstuvwxyz0123", "Ada token=[redacted]", "AT", true},
		{"local path", "Ada /Users/someone/src/forgecell", "Ada [local path]", "A", true},
		{"no letter left", "/Users/someone 1234", OperatorFallback, "", false},
		{"oversized", strings.Repeat("a", maxOperatorBytes+1), OperatorFallback, "", false},
		{"invalid utf-8", "Ada \xff\xfe Lovelace", "Ada Lovelace", "AL", true},
	} {
		w := do(t, testHandler(t, Options{LabDir: t.TempDir(), Operator: c.raw}), authed("/api/lab"))
		raw := w.Body.String()
		var v LabView
		decode(t, w, &v)
		if v.Operator != (OperatorView{Name: c.want, Initials: c.initials, Set: c.set}) {
			t.Errorf("%s: operator %+v, want %q %q %t", c.name, v.Operator, c.want, c.initials, c.set)
		}
		for _, leak := range []string{"@", "＠", "example.com", "ghp_", "/Users/someone"} {
			if strings.Contains(raw, leak) {
				t.Errorf("%s: /api/lab carries %q: %s", c.name, leak, raw)
			}
		}
	}

	// A long name is cut to maxOperatorRunes and says so, never silently.
	var v LabView
	decode(t, do(t, testHandler(t, Options{LabDir: t.TempDir(), Operator: long}), authed("/api/lab")), &v)
	if n := utf8.RuneCountInString(v.Operator.Name); n != maxOperatorRunes || !strings.HasSuffix(v.Operator.Name, "…") || v.Operator.Initials != "É" || !v.Operator.Set {
		t.Fatalf("long operator: %d runes %+v", n, v.Operator)
	}
	// Every other LabView field is as before.
	if len(v.Gates) != 2 || v.Sample || v.ReadOnly || v.Formula.State != "none" {
		t.Fatalf("lab %+v", v)
	}
}

// Sample data shows a clearly synthetic operator, never the configured one.
func TestUIOperatorInDemoIsSynthetic(t *testing.T) {
	w := do(t, testHandler(t, Options{Demo: true, Operator: "Real Person"}), authed("/api/lab"))
	var v LabView
	decode(t, w, &v)
	if v.Operator != demoOperator || !strings.HasPrefix(v.Operator.Name, "Sample") || strings.Contains(w.Body.String(), "Real Person") {
		t.Fatalf("demo operator %+v", v.Operator)
	}
}
