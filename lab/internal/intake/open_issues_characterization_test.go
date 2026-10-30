package intake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// intakeCharacterizationReply is what the fake gh prints for one call, in call order.
type intakeCharacterizationReply struct {
	out  string
	exit int
}

// intakeCharacterizationGH puts a fake gh first on PATH. It answers the n-th call
// with replies[n-1], logs every argument on its own line, and marks non-empty
// stdin. It returns a reader for the logged argv, one slice per call.
func intakeCharacterizationGH(t *testing.T, replies ...intakeCharacterizationReply) func() [][]string {
	t.Helper()
	dir := t.TempDir()
	for i, r := range replies {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("out-%d", i+1)), []byte(r.out), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("exit-%d", i+1)), []byte(fmt.Sprint(r.exit)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
dir='` + dir + `'
n=$(cat "$dir/count" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$dir/count"
for arg; do printf '%s\n' "$arg" >> "$dir/calls.log"; done
if [ -n "$(cat)" ]; then echo '<stdin>' >> "$dir/calls.log"; fi
echo '--' >> "$dir/calls.log"
[ -f "$dir/out-$n" ] || { echo "unexpected call $n" >&2; exit 97; }
cat "$dir/out-$n"
exit "$(cat "$dir/exit-$n")"
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() [][]string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, "calls.log"))
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		var calls [][]string
		var current []string
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if line == "--" {
				calls = append(calls, current)
				current = nil
				continue
			}
			current = append(current, line)
		}
		return calls
	}
}

func intakeCharacterizationFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "characterization", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func intakeCharacterizationGET(endpoint string) []string {
	return []string{"api", "--hostname", "github.com", "--method", "GET", endpoint}
}

func intakeCharacterizationIssuesEndpoint(filter string, page int) string {
	return fmt.Sprintf("repos/owner/repo/issues?state=open&sort=created&direction=asc&per_page=30%s&page=%d", filter, page)
}

// intakeCharacterizationPage is a full or partial page of open Issues numbered from start.
func intakeCharacterizationPage(start, count int) string {
	items := make([]string, 0, count)
	for n := start; n < start+count; n++ {
		items = append(items, fmt.Sprintf(`{"number":%d,"title":"t%d","state":"open","html_url":"https://github.com/owner/repo/issues/%d","created_at":"2026-01-01T00:00:00Z"}`, n, n, n))
	}
	return "[" + strings.Join(items, ",") + "]"
}

func intakeCharacterizationFixtureIssues(t *testing.T, got []OpenIssue) {
	t.Helper()
	want := []OpenIssue{
		{Repository: "owner/repo", Number: 2, Title: "Oldest", URL: "https://github.com/owner/repo/issues/2", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Labels: []string{"bug", "help wanted"}, Milestone: "v1"},
		{Repository: "owner/repo", Number: 4, Title: "Newer", URL: "https://github.com/owner/repo/issues/4", CreatedAt: time.Date(2026, 1, 2, 22, 0, 0, 0, time.UTC)},
	}
	if len(got) != len(want) {
		t.Fatalf("issues = %+v", got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if !g.CreatedAt.Equal(w.CreatedAt) {
			t.Fatalf("issue %d createdAt = %v, want %v", i, g.CreatedAt, w.CreatedAt)
		}
		g.CreatedAt, w.CreatedAt = time.Time{}, time.Time{}
		if !reflect.DeepEqual(g, w) {
			t.Fatalf("issue %d =\n%+v\nwant\n%+v", i, g, w)
		}
	}
}

func intakeCharacterizationCalls(t *testing.T, got [][]string, endpoints ...string) {
	t.Helper()
	want := make([][]string, len(endpoints))
	for i, e := range endpoints {
		want[i] = intakeCharacterizationGET(e)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("calls =\n%q\nwant\n%q", got, want)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadOpenIssuesOldest(t *testing.T) {
	calls := intakeCharacterizationGH(t, intakeCharacterizationReply{out: intakeCharacterizationFixture(t, "open-issues-page.json")})
	got, err := ReadOpenIssues(context.Background(), "owner/repo")
	if err != nil {
		t.Fatalf("ReadOpenIssues: %v", err)
	}
	intakeCharacterizationCalls(t, calls(), intakeCharacterizationIssuesEndpoint("", 1))
	if got.Truncated {
		t.Fatal("a short page was reported truncated")
	}
	intakeCharacterizationFixtureIssues(t, got.Issues)

	sel, err := ParseSelector("oldest")
	if err != nil || sel != (Selector{Kind: SelectOldest}) || sel.String() != "oldest" {
		t.Fatalf("selector = %+v %v", sel, err)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadSelectedOpenIssuesLabelAndMilestone(t *testing.T) {
	t.Run("label", func(t *testing.T) {
		calls := intakeCharacterizationGH(t, intakeCharacterizationReply{out: intakeCharacterizationFixture(t, "open-issues-page.json")})
		sel, err := ParseSelector("label:good first/issue")
		if err != nil || sel.String() != "label:good first/issue" {
			t.Fatalf("selector = %+v %v", sel, err)
		}
		got, err := ReadSelectedOpenIssues(context.Background(), "owner/repo", sel)
		if err != nil {
			t.Fatalf("ReadSelectedOpenIssues: %v", err)
		}
		intakeCharacterizationCalls(t, calls(), intakeCharacterizationIssuesEndpoint("&labels=good+first%2Fissue", 1))
		intakeCharacterizationFixtureIssues(t, got.Issues)
	})
	t.Run("milestone", func(t *testing.T) {
		calls := intakeCharacterizationGH(t,
			intakeCharacterizationReply{out: intakeCharacterizationFixture(t, "milestones-page.json")},
			intakeCharacterizationReply{out: intakeCharacterizationFixture(t, "open-issues-page.json")},
		)
		got, err := ReadSelectedOpenIssues(context.Background(), "owner/repo", Selector{Kind: SelectMilestone, Value: "v1"})
		if err != nil {
			t.Fatalf("ReadSelectedOpenIssues: %v", err)
		}
		intakeCharacterizationCalls(t, calls(), "repos/owner/repo/milestones?state=all&per_page=100&page=1", intakeCharacterizationIssuesEndpoint("&milestone=3", 1))
		intakeCharacterizationFixtureIssues(t, got.Issues)
	})
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadOpenIssuesPagingBounds(t *testing.T) {
	t.Run("ten full pages are truncated", func(t *testing.T) {
		replies := make([]intakeCharacterizationReply, 10)
		endpoints := make([]string, 10)
		for i := range replies {
			replies[i] = intakeCharacterizationReply{out: intakeCharacterizationPage(i*30+1, 30)}
			endpoints[i] = intakeCharacterizationIssuesEndpoint("", i+1)
		}
		calls := intakeCharacterizationGH(t, replies...)
		got, err := ReadOpenIssues(context.Background(), "owner/repo")
		if err != nil || !got.Truncated || len(got.Issues) != 300 || got.Issues[299].Number != 300 {
			t.Fatalf("truncated = %v issues = %d err = %v", got.Truncated, len(got.Issues), err)
		}
		intakeCharacterizationCalls(t, calls(), endpoints...)
	})
	t.Run("ten full pages under a selection is an error", func(t *testing.T) {
		replies := make([]intakeCharacterizationReply, 10)
		for i := range replies {
			replies[i] = intakeCharacterizationReply{out: intakeCharacterizationPage(i*30+1, 30)}
		}
		calls := intakeCharacterizationGH(t, replies...)
		_, err := ReadSelectedOpenIssues(context.Background(), "owner/repo", Selector{Kind: SelectLabel, Value: "bug"})
		want := "label:bug matches more than 300 open Issues, so the listing is incomplete and nothing was selected; narrow the selection"
		if err == nil || err.Error() != want || len(calls()) != 10 {
			t.Fatalf("err = %v calls = %d", err, len(calls()))
		}
	})
	t.Run("an Issue that moved across pages appears once", func(t *testing.T) {
		calls := intakeCharacterizationGH(t,
			intakeCharacterizationReply{out: intakeCharacterizationPage(1, 30)},
			intakeCharacterizationReply{out: intakeCharacterizationPage(30, 2)},
		)
		got, err := ReadOpenIssues(context.Background(), "owner/repo")
		if err != nil || got.Truncated || len(got.Issues) != 31 || got.Issues[30].Number != 31 {
			t.Fatalf("issues = %d err = %v", len(got.Issues), err)
		}
		intakeCharacterizationCalls(t, calls(), intakeCharacterizationIssuesEndpoint("", 1), intakeCharacterizationIssuesEndpoint("", 2))
	})
	t.Run("milestones are read at most five pages", func(t *testing.T) {
		replies := make([]intakeCharacterizationReply, 5)
		endpoints := make([]string, 5)
		for i := range replies {
			items := make([]string, 100)
			for j := range items {
				items[j] = fmt.Sprintf(`{"number":%d,"title":"m%d"}`, i*100+j+1, i*100+j+1)
			}
			replies[i] = intakeCharacterizationReply{out: "[" + strings.Join(items, ",") + "]"}
			endpoints[i] = fmt.Sprintf("repos/owner/repo/milestones?state=all&per_page=100&page=%d", i+1)
		}
		calls := intakeCharacterizationGH(t, replies...)
		_, err := ReadSelectedOpenIssues(context.Background(), "owner/repo", Selector{Kind: SelectMilestone, Value: "zz"})
		if err == nil || err.Error() != `milestone "zz" was not found in the first 500 milestones, so it cannot be selected` {
			t.Fatalf("err = %v", err)
		}
		intakeCharacterizationCalls(t, calls(), endpoints...)
	})
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadOpenIssuesFailures(t *testing.T) {
	issue := func(n int, url string) string {
		return fmt.Sprintf(`{"number":%d,"title":"t","state":"open","html_url":%q,"created_at":"2026-01-01T00:00:00Z"}`, n, url)
	}
	cases := []struct {
		name     string
		selector Selector
		replies  []intakeCharacterizationReply
		calls    int
		err      string
	}{
		{name: "non-zero exit", replies: []intakeCharacterizationReply{{out: "", exit: 1}}, calls: 1, err: "GitHub data unavailable: exit status 1"},
		{name: "malformed json", replies: []intakeCharacterizationReply{{out: "[{"}}, calls: 1, err: "GitHub data unavailable: malformed or oversized page"},
		{name: "not an array", replies: []intakeCharacterizationReply{{out: "null"}}, calls: 1, err: "GitHub data unavailable: malformed or oversized page"},
		{name: "page over size", replies: []intakeCharacterizationReply{{out: intakeCharacterizationPage(1, 31)}}, calls: 1, err: "GitHub data unavailable: malformed or oversized page"},
		{name: "overflow", replies: []intakeCharacterizationReply{{out: strings.Repeat(" ", 4_000_001)}}, calls: 1, err: "GitHub data unavailable: response exceeds 4000000 bytes"},
		{name: "duplicate within a page", replies: []intakeCharacterizationReply{{out: "[" + issue(2, "https://github.com/owner/repo/issues/2") + "," + issue(2, "https://github.com/owner/repo/issues/2") + "]"}}, calls: 1, err: "GitHub data unavailable: duplicate Issue 2 within one page"},
		{name: "malformed pull request", replies: []intakeCharacterizationReply{{out: `[{"number":3,"pull_request":"x"}]`}}, calls: 1, err: "GitHub data unavailable: malformed pull request metadata"},
		{name: "other repository", replies: []intakeCharacterizationReply{{out: "[" + issue(2, "https://github.com/other/repo/issues/2") + "]"}}, calls: 1, err: "GitHub data unavailable: invalid Issue identity or metadata"},
		{name: "number mismatch", replies: []intakeCharacterizationReply{{out: "[" + issue(2, "https://github.com/owner/repo/issues/5") + "]"}}, calls: 1, err: "GitHub data unavailable: invalid Issue identity or metadata"},
		{name: "label with a comma", selector: Selector{Kind: SelectLabel, Value: "a,b"}, calls: 0, err: `label "a,b" cannot be selected: GitHub's labels filter treats a comma as a separator`},
		{name: "milestone missing", selector: Selector{Kind: SelectMilestone, Value: "v9"}, replies: []intakeCharacterizationReply{{out: `[{"number":3,"title":"v1"}]`}}, calls: 1, err: `milestone "v9" does not exist in owner/repo`},
		{name: "milestone ambiguous", selector: Selector{Kind: SelectMilestone, Value: "v1"}, replies: []intakeCharacterizationReply{{out: `[{"number":3,"title":"v1"},{"number":4,"title":"v1"}]`}}, calls: 1, err: `milestone "v1" is ambiguous: GitHub returned 2 milestones with that title`},
		{name: "milestone invalid", selector: Selector{Kind: SelectMilestone, Value: "v1"}, replies: []intakeCharacterizationReply{{out: `[{"number":0,"title":"v1"}]`}}, calls: 1, err: "GitHub data unavailable: invalid milestone metadata"},
		{name: "milestone non-zero exit", selector: Selector{Kind: SelectMilestone, Value: "v1"}, replies: []intakeCharacterizationReply{{exit: 4}}, calls: 1, err: "GitHub data unavailable: exit status 4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := intakeCharacterizationGH(t, c.replies...)
			got, err := ReadSelectedOpenIssues(context.Background(), "owner/repo", c.selector)
			if err == nil || err.Error() != c.err {
				t.Fatalf("err = %v, want %q", err, c.err)
			}
			if got.Truncated {
				t.Fatalf("failed read reported truncation: %+v", got)
			}
			if n := len(calls()); n != c.calls {
				t.Fatalf("calls = %d, want %d", n, c.calls)
			}
		})
	}
	calls := intakeCharacterizationGH(t)
	if _, err := ReadOpenIssues(context.Background(), "not a repo"); err == nil || err.Error() != "invalid repository binding" || len(calls()) != 0 {
		t.Fatalf("invalid repository err = %v", err)
	}
}
