package intake

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadIssueQueuePage(t *testing.T) {
	calls := intakeCharacterizationGH(t, intakeCharacterizationReply{out: intakeCharacterizationFixture(t, "queue-page.json")})
	got, err := ReadIssueQueue(context.Background(), "owner/repo", 2, 3, "")
	if err != nil {
		t.Fatalf("ReadIssueQueue: %v", err)
	}
	intakeCharacterizationCalls(t, calls(), "repos/owner/repo/issues?state=open&per_page=3&page=2")
	want := IssuePage{
		Issues: []QueueIssue{
			{Repository: "owner/repo", Number: 1, Title: "Open one", State: "OPEN", URL: "https://github.com/owner/repo/issues/1"},
			{Repository: "owner/repo", Number: 2, Title: "Closed one", State: "CLOSED", URL: "https://github.com/owner/repo/issues/2"},
		},
		Page: 2, PageSize: 3, RemoteItems: 3, MorePossible: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("page =\n%+v\nwant\n%+v", got, want)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadIssueQueueSelection(t *testing.T) {
	calls := intakeCharacterizationGH(t, intakeCharacterizationReply{out: `{"number":7,"title":"Chosen","state":"closed","html_url":"https://github.com/owner/repo/issues/7"}`})
	got, err := ReadIssueQueue(context.Background(), "owner/repo", 1, 30, "owner/repo#7")
	if err != nil {
		t.Fatalf("ReadIssueQueue: %v", err)
	}
	intakeCharacterizationCalls(t, calls(), "repos/owner/repo/issues/7")
	want := IssuePage{
		Issues:      []QueueIssue{{Repository: "owner/repo", Number: 7, Title: "Chosen", State: "CLOSED", URL: "https://github.com/owner/repo/issues/7"}},
		Page:        1,
		PageSize:    30,
		RemoteItems: 1,
		Selected:    true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("page =\n%+v\nwant\n%+v", got, want)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadIssueQueueFailures(t *testing.T) {
	cases := []struct {
		name       string
		page, size int
		selection  string
		replies    []intakeCharacterizationReply
		calls      int
		err        string
	}{
		{name: "non-zero exit", page: 1, size: 3, replies: []intakeCharacterizationReply{{exit: 1}}, calls: 1, err: "GitHub data unavailable: exit status 1"},
		{name: "malformed json", page: 1, size: 3, replies: []intakeCharacterizationReply{{out: "[{"}}, calls: 1, err: "GitHub data unavailable: malformed or oversized page"},
		{name: "page over size", page: 1, size: 2, replies: []intakeCharacterizationReply{{out: intakeCharacterizationFixture(t, "queue-page.json")}}, calls: 1, err: "GitHub data unavailable: malformed or oversized page"},
		{name: "overflow", page: 1, size: 3, replies: []intakeCharacterizationReply{{out: strings.Repeat(" ", 1_000_001)}}, calls: 1, err: "GitHub data unavailable: response exceeds 1000000 bytes"},
		{name: "selected pull request", page: 1, size: 3, selection: "#3", replies: []intakeCharacterizationReply{{out: `{"number":3,"title":"p","state":"open","html_url":"https://github.com/owner/repo/pull/3","pull_request":{"url":"x"}}`}}, calls: 1, err: "GitHub selection is a pull request"},
		{name: "selected malformed json", page: 1, size: 3, selection: "#3", replies: []intakeCharacterizationReply{{out: "{"}}, calls: 1, err: "GitHub data unavailable: malformed or oversized page"},
		{name: "selected other number", page: 1, size: 3, selection: "#3", replies: []intakeCharacterizationReply{{out: `{"number":4,"title":"x","state":"open","html_url":"https://github.com/owner/repo/issues/4"}`}}, calls: 1, err: "GitHub data unavailable: invalid Issue identity or metadata"},
		{name: "unknown state", page: 1, size: 3, replies: []intakeCharacterizationReply{{out: `[{"number":4,"title":"x","state":"locked","html_url":"https://github.com/owner/repo/issues/4"}]`}}, calls: 1, err: "GitHub data unavailable: invalid Issue identity or metadata"},
		{name: "page zero", page: 0, size: 3, calls: 0, err: "page must be positive; page-size must be 1–100"},
		{name: "size over bound", page: 1, size: 101, calls: 0, err: "page must be positive; page-size must be 1–100"},
		{name: "selection in another repository", page: 1, size: 3, selection: "other/repo#7", calls: 0, err: "issue repository does not match the approved Formula"},
		{name: "invalid repository", page: 1, size: 3, calls: 0, err: "invalid repository binding"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := intakeCharacterizationGH(t, c.replies...)
			repository := "owner/repo"
			if c.name == "invalid repository" {
				repository = "owner"
			}
			_, err := ReadIssueQueue(context.Background(), repository, c.page, c.size, c.selection)
			if err == nil || err.Error() != c.err {
				t.Fatalf("err = %v, want %q", err, c.err)
			}
			if n := len(calls()); n != c.calls {
				t.Fatalf("calls = %d, want %d", n, c.calls)
			}
		})
	}
}
