package forgesync

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// The literal GraphQL text slice 1 must keep sending, byte for byte.
const (
	syncCharacterizationIssueFields = `number state stateReason createdAt updatedAt title locked labels(first:20){nodes{name}} milestone{title} reopened:timelineItems(last:1,itemTypes:[REOPENED_EVENT]){nodes{... on ReopenedEvent{createdAt}}}`
	syncCharacterizationStateFields = syncCharacterizationIssueFields + ` closedAt timelineItems(last:1,itemTypes:[CLOSED_EVENT]){nodes{... on ClosedEvent{closer{__typename ... on PullRequest{number} ... on Commit{oid}}}}}`
	syncCharacterizationPRFields    = `number state isDraft mergedAt mergeCommit{oid} headRefOid headRef{name} reviewDecision commits(last:1){nodes{commit{statusCheckRollup{state}}}}`
	syncCharacterizationListQuery   = `query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){issues(states:OPEN,first:100,after:$after,orderBy:{field:CREATED_AT,direction:ASC}){totalCount pageInfo{hasNextPage endCursor} nodes{` + syncCharacterizationIssueFields + `}}}}`
)

type syncCharacterizationRecorder struct {
	calls   []process.Options
	replies []process.Result
}

func (r *syncCharacterizationRecorder) run(_ context.Context, o process.Options) process.Result {
	r.calls = append(r.calls, o)
	if len(r.calls) > len(r.replies) {
		return process.Result{Code: 1, Error: "unexpected call"}
	}
	return r.replies[len(r.calls)-1]
}

func syncCharacterizationFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "characterization", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func syncCharacterizationOptions(t *testing.T, calls []process.Options) {
	t.Helper()
	for i, c := range calls {
		if c.Timeout != 20*time.Second || c.MaxOutputBytes != 4_000_000 || c.Stdin == nil || len(c.Stdin) != 0 || c.Dir != "" || c.Input != nil {
			t.Fatalf("call %d options = %+v", i, c)
		}
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationOpenIssuesArgvAndDecoding(t *testing.T) {
	page2 := `{"data":{"repository":{"issues":{"totalCount":3,"pageInfo":{"hasNextPage":false,"endCursor":"Y3Vyc29yOjM="},"nodes":[` +
		`{"number":5,"state":"OPEN","stateReason":null,"createdAt":"2026-10-04T00:00:00Z","updatedAt":"not a time","title":"Second","locked":false,"labels":{"nodes":[]},"milestone":null,"reopened":{"nodes":[]}},` +
		`{"number":8,"state":"OPEN","stateReason":"COMPLETED","createdAt":"2026-10-06T00:00:00Z","updatedAt":"2026-10-06T00:00:00Z","title":"Third","locked":false}]}}}}`
	rec := &syncCharacterizationRecorder{replies: []process.Result{
		{OK: true, Stdout: syncCharacterizationFixture(t, "open-issues-page.json")},
		{OK: true, Stdout: page2},
	}}
	got, err := syncCharacterizationGH(rec.run).OpenIssues(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("OpenIssues: %v", err)
	}
	wantArgv := [][]string{
		{"gh", "api", "graphql", "--hostname", "github.com", "-f", "query=" + syncCharacterizationListQuery, "-f", "name=repo", "-f", "owner=owner"},
		{"gh", "api", "graphql", "--hostname", "github.com", "-f", "query=" + syncCharacterizationListQuery, "-f", "after=Y3Vyc29yOjI=", "-f", "name=repo", "-f", "owner=owner"},
	}
	if len(rec.calls) != len(wantArgv) {
		t.Fatalf("calls = %d", len(rec.calls))
	}
	for i := range wantArgv {
		if !reflect.DeepEqual(rec.calls[i].Argv, wantArgv[i]) {
			t.Fatalf("call %d argv =\n%q\nwant\n%q", i, rec.calls[i].Argv, wantArgv[i])
		}
	}
	syncCharacterizationOptions(t, rec.calls)
	want := Listing{Total: 3, Issues: []Issue{
		{Number: 3, Found: true, State: IssueOpen, StateReason: ReasonReopened, Title: "First issue", Labels: []string{"bug"}, Milestone: "v1", CreatedAt: "2026-10-01T07:00:00Z", UpdatedAt: "2026-10-02T10:00:00Z", Locked: true, ReopenedAt: "2026-10-03T00:00:00Z", URL: "https://github.com/owner/repo/issues/3"},
		{Number: 5, Found: true, State: IssueOpen, Title: "Second", Labels: []string{}, CreatedAt: "2026-10-04T00:00:00Z", URL: "https://github.com/owner/repo/issues/5"},
		{Number: 8, Found: true, State: IssueOpen, StateReason: ReasonCompleted, Title: "Third", Labels: []string{}, CreatedAt: "2026-10-06T00:00:00Z", UpdatedAt: "2026-10-06T00:00:00Z", URL: "https://github.com/owner/repo/issues/8"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listing =\n%+v\nwant\n%+v", got, want)
	}
}

// syncCharacterizationGH is a GH with production bounds and a fake runner.
func syncCharacterizationGH(run func(context.Context, process.Options) process.Result) *GH {
	g := NewGH()
	g.Run = run
	return g
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationStatesArgvAndDecoding(t *testing.T) {
	// gh exits non-zero when a node is missing; the answer is still kept.
	rec := &syncCharacterizationRecorder{replies: []process.Result{
		{Code: 1, Error: "exit status 1", Stderr: "gh: Could not resolve to an Issue", Stdout: syncCharacterizationFixture(t, "states-batch.json")},
	}}
	got, err := syncCharacterizationGH(rec.run).States(context.Background(), "owner/repo", []int64{4, 7, 11, 12, 4, 0, -1}, []int64{9, 9})
	if err != nil {
		t.Fatalf("States: %v", err)
	}
	query := `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){` +
		`i12:issue(number:12){` + syncCharacterizationStateFields + `} ` +
		`i11:issue(number:11){` + syncCharacterizationStateFields + `} ` +
		`p9:pullRequest(number:9){` + syncCharacterizationPRFields + `} ` +
		`i7:issue(number:7){` + syncCharacterizationStateFields + `} ` +
		`i4:issue(number:4){` + syncCharacterizationStateFields + `} ` +
		`}}`
	wantArgv := []string{"gh", "api", "graphql", "--hostname", "github.com", "-f", "query=" + query, "-f", "name=repo", "-f", "owner=owner"}
	if len(rec.calls) != 1 || !reflect.DeepEqual(rec.calls[0].Argv, wantArgv) {
		t.Fatalf("calls = %+v\nwant argv %q", rec.calls, wantArgv)
	}
	syncCharacterizationOptions(t, rec.calls)
	want := States{
		Issues: map[int64]Issue{
			12: {Number: 12, Labels: []string{}, Unavailable: UnavailableNoAccess},
			11: {Number: 11, Labels: []string{}, Unavailable: UnavailableNotFound},
			7:  {Number: 7, Found: true, State: IssueOpen, Title: "Open one", Labels: []string{}, CreatedAt: "2026-10-01T00:00:00Z", UpdatedAt: "2026-10-01T00:00:00Z", URL: "https://github.com/owner/repo/issues/7"},
			4: {Number: 4, Found: true, State: IssueClosed, StateReason: ReasonCompleted, ClosedAt: "2026-10-05T12:00:00Z", ClosedBy: &Link{Kind: "pull", Number: 9, URL: "https://github.com/owner/repo/pull/9"},
				Title: "Closed one", Labels: []string{"done"}, CreatedAt: "2026-09-01T00:00:00Z", UpdatedAt: "2026-10-05T12:00:00Z", URL: "https://github.com/owner/repo/issues/4"},
		},
		PRs: map[int64]PR{
			9: {Number: 9, Found: true, State: PRMerged, MergedAt: "2026-10-05T11:00:00Z", MergeCommit: "1111111111111111111111111111111111111111", HeadRefOid: "2222222222222222222222222222222222222222", HeadDeleted: true, ReviewDecision: "APPROVED", Checks: "SUCCESS", URL: "https://github.com/owner/repo/pull/9"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("states =\n%+v\nwant\n%+v", got, want)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationSyncReadFailures(t *testing.T) {
	cases := []struct {
		name   string
		result process.Result
		reason string
	}{
		{"malformed json", process.Result{OK: true, Stdout: "{"}, ReasonMalformed},
		{"null repository", process.Result{OK: true, Stdout: `{"data":{"repository":null}}`}, ReasonWrongHost},
		{"rate limited", process.Result{Code: 1, Stdout: `{"data":{"repository":null},"errors":[{"type":"RATE_LIMITED","message":"limit"}]}`}, ReasonRateLimited},
		{"non-zero exit unauthenticated", process.Result{Code: 1, Error: "exit status 4", Stderr: "HTTP 401: Bad credentials"}, ReasonNotAuthenticated},
		{"non-zero exit offline", process.Result{Code: 1, Error: "exit status 1", Stderr: "dial tcp: lookup api.github.com: no such host"}, ReasonOffline},
		{"non-zero exit other", process.Result{Code: 1, Error: "exit status 1", Stderr: "something else"}, ReasonFailed},
		{"gh missing", process.Result{Code: -1, Error: `exec: "gh": executable file not found in $PATH`}, ReasonGHMissing},
		{"overflow", process.Result{OK: true, Overflow: true}, ReasonOversized},
		{"timeout", process.Result{TimedOut: true}, ReasonTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			list := &syncCharacterizationRecorder{replies: []process.Result{c.result}}
			if _, err := syncCharacterizationGH(list.run).OpenIssues(context.Background(), "owner/repo"); ReasonOf(err) != c.reason || len(list.calls) != 1 {
				t.Fatalf("OpenIssues err = %v calls = %d, want %q", err, len(list.calls), c.reason)
			}
			states := &syncCharacterizationRecorder{replies: []process.Result{c.result}}
			if _, err := syncCharacterizationGH(states.run).States(context.Background(), "owner/repo", []int64{1}, nil); ReasonOf(err) != c.reason || len(states.calls) != 1 {
				t.Fatalf("States err = %v calls = %d, want %q", err, len(states.calls), c.reason)
			}
		})
	}
	// A node whose number differs from the alias asked for is refused.
	wrong := &syncCharacterizationRecorder{replies: []process.Result{{OK: true, Stdout: `{"data":{"repository":{"i1":{"number":2,"state":"OPEN","title":"x"}}}}`}}}
	if _, err := syncCharacterizationGH(wrong.run).States(context.Background(), "owner/repo", []int64{1}, nil); ReasonOf(err) != ReasonMalformed {
		t.Fatalf("mismatched node err = %v", err)
	}
	// An invalid repository never reaches gh.
	none := &syncCharacterizationRecorder{}
	if _, err := syncCharacterizationGH(none.run).OpenIssues(context.Background(), "not a repo"); ReasonOf(err) != ReasonRepository || len(none.calls) != 0 {
		t.Fatalf("invalid repository err = %v calls = %d", err, len(none.calls))
	}
}
