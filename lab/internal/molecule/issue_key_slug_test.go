package molecule

import (
	"strings"
	"testing"
)

func TestIssueIsKeyed(t *testing.T) {
	if (Issue{Number: 12}).IsKeyed() || !(Issue{Key: "ENG-1"}).IsKeyed() || !(Issue{Key: "###"}).IsKeyed() {
		t.Fatal("IsKeyed must be true exactly when Key is set")
	}
}

func TestIssueKeySlug(t *testing.T) {
	cases := map[string]string{
		"":                             "",
		"ENG-1234":                     "eng-1234",
		"eng-1234":                     "eng-1234",
		"PROJ_42":                      "proj-42",
		"  ENG 12  ":                   "eng-12",
		"a--b__c..d":                   "a-b-c-d",
		"-ENG-1-":                      "eng-1",
		"ÉNG-12":                       "ng-12",
		"日本-7":                         "7",
		"ENG\xe2\x80\xae-1":            "eng-1",
		"../../etc/passwd":             "etc-passwd",
		"###":                          "",
		"ÉÜ":                           "",
		"---":                          "",
		strings.Repeat("a", 50):        strings.Repeat("a", 40),
		strings.Repeat("a", 39) + "-b": strings.Repeat("a", 39),
		strings.Repeat("ab-", 30):      strings.Repeat("ab-", 13) + "a",
	}
	for key, want := range cases {
		if got := (Issue{Key: key}).KeySlug(); got != want {
			t.Errorf("KeySlug(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestIssueKeySlugIsAlwaysSafe(t *testing.T) {
	keys := []string{"ENG-1234", strings.Repeat("Z-", 10_000), "a\x00b", "\xe2\x80\xa8x\xe2\x80\xa9", "x/../y", strings.Repeat("é", 1000) + "1"}
	for _, key := range keys {
		s := (Issue{Key: key}).KeySlug()
		if len(s) > maxKeySlug || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.Contains(s, "--") || strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			t.Errorf("KeySlug(%.20q) = %q", key, s)
		}
	}
}

func TestIssueIdentified(t *testing.T) {
	cases := []struct {
		issue Issue
		want  bool
	}{
		{Issue{Number: 12}, true},
		{Issue{}, false},
		{Issue{Number: -1}, false},
		{Issue{Key: "ENG-1"}, true},
		{Issue{Key: "ENG-1", Number: 1}, false},
		{Issue{Key: "###"}, false},
	}
	for _, c := range cases {
		if got := issueIdentified(c.issue); got != c.want {
			t.Errorf("issueIdentified(%+v) = %t, want %t", c.issue, got, c.want)
		}
	}
}

// TestFreshIntakeActionStaysNumeric pins that a keyed record gets no fresh-intake
// action until the UI and CLI have a keyed-run entry point, while a numeric one
// keeps it.
func TestFreshIntakeActionStaysNumeric(t *testing.T) {
	notes := []string{"Supplemental evidence passes exhausted; unmet requests: x"}
	if _, ok := freshIntakeAction(Record{ID: "mol-12-abc", Status: "blocked", Issue: Issue{Number: 12, Repo: "acme/widgets"}, Notes: notes}); !ok {
		t.Fatal("numeric blocked record lost its fresh intake")
	}
	if _, ok := freshIntakeAction(Record{ID: "mol-eng-1-abc", Status: "blocked", Issue: Issue{Key: "ENG-1", Repo: "acme/widgets"}, Notes: notes}); ok {
		t.Fatal("keyed blocked record offered a fresh intake")
	}
	if _, ok := freshIntakeAction(Record{ID: "mol-x-abc", Status: "blocked", Issue: Issue{Key: "###", Repo: "acme/widgets"}, Notes: notes}); ok {
		t.Fatal("unusable key offered a fresh intake")
	}
}
