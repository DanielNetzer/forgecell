package intake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func loopPage(start, count int) string {
	items := make([]string, 0, count)
	for n := start; n < start+count; n++ {
		items = append(items, fmt.Sprintf(`{"number":%d,"title":"t%d","state":"open","html_url":"https://github.com/o/r/issues/%d","created_at":"2026-01-01T00:00:%02dZ","labels":[{"name":"bug"}],"milestone":{"title":"v1"}}`, n, n, n, n%60))
	}
	return "[" + strings.Join(items, ",") + "]"
}

// loopGH installs a fake gh that serves pages by their page number and logs every call.
func loopGH(t *testing.T, pages map[int]string) string {
	t.Helper()
	dir := t.TempDir()
	for n, raw := range pages {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("page-%d.json", n)), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "calls.log")
	queueGH(t, "echo \"$*\" >> '"+log+"'\nfor last; do :; done\npage=${last##*page=}\ncat '"+dir+"'/page-$page.json 2>/dev/null || printf '[]'\n")
	return log
}

func loopCalls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestLoopOpenIssuesReadsPagesOldestFirstAndSkipsPullRequests(t *testing.T) {
	second := `[{"number":31,"title":"pull","state":"open","html_url":"https://github.com/o/r/pull/31","created_at":"2026-01-02T00:00:00Z","pull_request":{"url":"x"}},` +
		`{"number":32,"title":"last","state":"open","html_url":"https://github.com/o/r/issues/32","created_at":"2026-01-03T00:00:00Z","labels":[{"name":"enhancement"},{"name":"good first issue"}],"milestone":null}]`
	log := loopGH(t, map[int]string{1: loopPage(1, 30), 2: second})
	got, err := ReadOpenIssues(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if got.Truncated || len(got.Issues) != 31 {
		t.Fatalf("truncated=%t issues=%d", got.Truncated, len(got.Issues))
	}
	first, last := got.Issues[0], got.Issues[30]
	if first.Number != 1 || first.Repository != "o/r" || first.Title != "t1" || first.Milestone != "v1" || len(first.Labels) != 1 || first.Labels[0] != "bug" {
		t.Fatalf("%+v", first)
	}
	if !first.CreatedAt.Equal(time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)) {
		t.Fatalf("created at %v", first.CreatedAt)
	}
	if last.Number != 32 || last.Milestone != "" || strings.Join(last.Labels, ",") != "enhancement,good first issue" {
		t.Fatalf("%+v", last)
	}
	calls := loopCalls(t, log)
	if len(calls) != 2 {
		t.Fatalf("calls: %v", calls)
	}
	for i, call := range calls {
		want := fmt.Sprintf("api --hostname github.com --method GET repos/o/r/issues?state=open&sort=created&direction=asc&per_page=30&page=%d", i+1)
		if call != want {
			t.Fatalf("call %d: %q", i, call)
		}
	}
}

func TestLoopOpenIssuesIsBoundedAndReportsTruncation(t *testing.T) {
	pages := map[int]string{}
	for p := 1; p <= 10; p++ {
		pages[p] = loopPage((p-1)*30+1, 30)
	}
	pages[11] = loopPage(301, 5)
	log := loopGH(t, pages)
	got, err := ReadOpenIssues(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Issues) != 300 {
		t.Fatalf("truncated=%t issues=%d", got.Truncated, len(got.Issues))
	}
	if calls := loopCalls(t, log); len(calls) != 10 {
		t.Fatalf("a bounded read made %d calls", len(calls))
	}
}

func TestLoopOpenIssuesRefusesMalformedData(t *testing.T) {
	good := `{"number":1,"title":"t","state":"open","html_url":"https://github.com/o/r/issues/1","created_at":"2026-01-01T00:00:00Z"}`
	for _, tc := range []struct{ name, raw string }{
		{"null", "null"},
		{"object", "{}"},
		{"truncated", "["},
		{"closed", strings.Replace("["+good+"]", `"open"`, `"closed"`, 1)},
		{"no title", strings.Replace("["+good+"]", `"title":"t"`, `"title":""`, 1)},
		{"bad created_at", strings.Replace("["+good+"]", "2026-01-01T00:00:00Z", "yesterday", 1)},
		{"no created_at", strings.Replace("["+good+"]", `,"created_at":"2026-01-01T00:00:00Z"`, "", 1)},
		{"other repository", strings.Replace("["+good+"]", "o/r", "x/r", 1)},
		{"number mismatch", strings.Replace("["+good+"]", `"number":1`, `"number":2`, 1)},
		{"duplicate", "[" + good + "," + good + "]"},
		{"bad pull request", `[{"pull_request":"bad"}]`},
		{"oversized page", loopPage(1, 31)},
		{"url is not an issue", strings.Replace("["+good+"]", "/issues/1", "/pull/1", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loopGH(t, map[int]string{1: tc.raw})
			if got, err := ReadOpenIssues(context.Background(), "o/r"); err == nil {
				t.Fatalf("accepted: %+v", got)
			}
		})
	}
}

func TestLoopOpenIssuesDeduplicatesAnIssueThatMovedAcrossPages(t *testing.T) {
	loopGH(t, map[int]string{1: loopPage(1, 30), 2: loopPage(30, 2)})
	got, err := ReadOpenIssues(context.Background(), "o/r")
	if err != nil {
		t.Fatalf("a repeated Issue number failed the whole read: %v", err)
	}
	if got.Truncated || len(got.Issues) != 31 || got.Issues[29].Number != 30 || got.Issues[30].Number != 31 {
		t.Fatalf("truncated=%t issues=%d", got.Truncated, len(got.Issues))
	}
}

func TestLoopOpenIssuesFailuresAreNotHidden(t *testing.T) {
	queueGH(t, "exit 1\n")
	if _, err := ReadOpenIssues(context.Background(), "o/r"); err == nil {
		t.Fatal("gh failure hidden")
	}
	log := loopGH(t, map[int]string{1: loopPage(1, 1)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadOpenIssues(ctx, "o/r"); err == nil {
		t.Fatal("cancellation hidden")
	}
	before := len(loopCalls(t, log))
	for _, repo := range []string{"", "o", "o/r/x", "../r"} {
		if _, err := ReadOpenIssues(context.Background(), repo); err == nil {
			t.Fatalf("repository %q accepted", repo)
		}
	}
	if after := len(loopCalls(t, log)); after != before {
		t.Fatalf("gh was called for an invalid repository: %d calls became %d", before, after)
	}
}

func TestLoopSelectorParsing(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  Selector
	}{
		{"oldest", Selector{Kind: "oldest"}},
		{"label:bug", Selector{Kind: "label", Value: "bug"}},
		{"label:good first", Selector{Kind: "label", Value: "good first"}},
		{"milestone:v1.0", Selector{Kind: "milestone", Value: "v1.0"}},
		{"label:a:b", Selector{Kind: "label", Value: "a:b"}},
		{"milestone:Sprint 4", Selector{Kind: "milestone", Value: "Sprint 4"}},
	} {
		got, err := ParseSelector(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("%q: %+v %v", tc.input, got, err)
		}
		if got.String() != tc.input {
			t.Fatalf("%q renders as %q", tc.input, got.String())
		}
	}
	for _, input := range []string{"", "newest", "label", "label:", "label:  ", "milestone:", "milestone:\t", "oldest:x", "Label:bug", "label=bug", " oldest"} {
		if got, err := ParseSelector(input); err == nil {
			t.Fatalf("%q accepted as %+v", input, got)
		}
	}
	if (Selector{}).String() != "oldest" {
		t.Fatal("the zero selector is not oldest")
	}
}

func TestLoopSelectorOrdersOldestFirstAndMatchesLabelsCaselessAndMilestonesExactly(t *testing.T) {
	at := func(minute int) time.Time { return time.Date(2026, 1, 1, 0, minute, 0, 0, time.UTC) }
	issues := []OpenIssue{
		{Number: 5, CreatedAt: at(30), Labels: []string{"bug"}, Milestone: "v1"},
		{Number: 9, CreatedAt: at(10), Labels: []string{"bugfix"}, Milestone: "v10"},
		{Number: 7, CreatedAt: at(10), Labels: []string{"Bug", "bug"}},
		{Number: 2, CreatedAt: at(20)},
	}
	numbers := func(s Selector) string {
		var out []string
		for _, i := range s.Apply(issues) {
			out = append(out, fmt.Sprint(i.Number))
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		input, want string
	}{
		{"oldest", "7,9,2,5"},
		{"label:bug", "7,5"},
		{"label:Bug", "7,5"},
		{"label:BUG", "7,5"},
		{"label:BugFix", "9"},
		{"label:bugf", ""},
		{"milestone:v1", "5"},
		{"milestone:V1", ""},
		{"milestone:v10", "9"},
		{"milestone:v", ""},
	} {
		s, err := ParseSelector(tc.input)
		if err != nil {
			t.Fatal(err)
		}
		if got := numbers(s); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.input, got, tc.want)
		}
	}
	if issues[0].Number != 5 || issues[3].Number != 2 {
		t.Fatal("Apply reordered its input")
	}
}
