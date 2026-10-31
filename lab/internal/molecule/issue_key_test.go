package molecule

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIssueDisplayKey(t *testing.T) {
	for _, tc := range []struct {
		issue Issue
		want  string
	}{
		{Issue{Number: 12}, "#12"},
		{Issue{Number: 12, Key: "ENG-12"}, "ENG-12"},
		{Issue{}, "#0"},
	} {
		if got := tc.issue.DisplayKey(); got != tc.want {
			t.Errorf("%+v.DisplayKey() = %q, want %q", tc.issue, got, tc.want)
		}
	}
}

func TestIssueMarshalsWithoutEmptyKey(t *testing.T) {
	raw, err := json.Marshal(Issue{Number: 12, Repo: "owner/repo", Title: "t", State: "OPEN"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"key"`) {
		t.Fatalf("Issue with an empty Key marshalled a key field: %s", raw)
	}
	raw, err = json.Marshal(Issue{Number: 12, Key: "ENG-12"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"key":"ENG-12"`) {
		t.Fatalf("Issue with a Key did not marshal it: %s", raw)
	}
}
