package tracker

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

func TestKeyedGitHubDescribe(t *testing.T) {
	d, err := KeyedGitHub{}.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := contract.TrackerDescription{DisplayName: "GitHub Issues", KeyPattern: GitHubKeyPattern, States: []string{"open", "closed"}, Selectors: []string{"oldest", "label:", "milestone:"}}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("Describe = %+v", d)
	}
	re := regexp.MustCompile(d.KeyPattern)
	for key, ok := range map[string]bool{"#1": true, "#9223372036854775807": true, "#0": false, "#01": false, "1": false, "#1 ": false, "x#1": false} {
		if re.MatchString(key) != ok {
			t.Errorf("keyPattern match %q = %t", key, !ok)
		}
	}
}

func TestParseGitHubKey(t *testing.T) {
	for key, want := range map[string]int64{"#1": 1, "#12": 12, "#9223372036854775807": 9223372036854775807} {
		if n, err := ParseGitHubKey(key); err != nil || n != want || GitHubKey(n) != key {
			t.Errorf("ParseGitHubKey(%q) = %d, %v", key, n, err)
		}
	}
	for _, key := range []string{"", "#", "#0", "#01", "12", "#-1", "#+1", "# 1", "#1 ", "#1\n", "#\xef\xbc\x91", "#9223372036854775808", "#99999999999999999999", "#1e3", "##1"} {
		if _, err := ParseGitHubKey(key); err == nil {
			t.Errorf("ParseGitHubKey(%q) accepted", key)
		}
	}
}

// TestKeyedGitHubGetMapsThroughRun checks #N reaches gh as number N through the
// Run seam and the answer becomes a contract Issue under the same key.
func TestKeyedGitHubGetMapsThroughRun(t *testing.T) {
	rec := &recorder{reply: process.Result{OK: true, Stdout: `{"number":12,"title":"T","body":"B","state":"CLOSED","url":"https://github.com/acme/widgets/issues/12","updatedAt":"2026-10-30T00:00:00Z"}`}}
	k := KeyedGitHub{GitHub: GitHub{Run: rec.run}, Repo: "acme/widgets", Dir: "work/dir"}
	got, err := k.Get(context.Background(), "#12")
	if err != nil {
		t.Fatal(err)
	}
	want := contract.TrackerIssue{Key: "#12", Title: "T", Body: "B", State: "closed", URL: "https://github.com/acme/widgets/issues/12", UpdatedAt: "2026-10-30T00:00:00Z", Labels: []string{}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %+v, want %+v", got, want)
	}
	if len(rec.calls) != 1 || !reflect.DeepEqual(rec.calls[0].Argv[:6], []string{"gh", "issue", "view", "12", "--repo", "acme/widgets"}) || rec.calls[0].Dir != "work/dir" {
		t.Fatalf("calls = %#v", rec.calls)
	}
}

func TestKeyedGitHubGetFailures(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	k := KeyedGitHub{GitHub: GitHub{Run: rec.run}, Repo: "acme/widgets"}
	for _, key := range []string{"12", "#0", "#1; rm -rf /", "#1\xe2\x80\xae", "FAKE-1"} {
		if _, err := k.Get(ctx, key); err == nil {
			t.Fatalf("Get(%q) accepted", key)
		}
	}
	if len(rec.calls) != 0 {
		t.Fatalf("gh ran for a hostile key: %#v", rec.calls)
	}
	for name, stdout := range map[string]string{
		"another Issue": `{"number":13,"title":"T","body":"","state":"OPEN","url":"https://github.com/acme/widgets/issues/13","updatedAt":""}`,
		"odd state":     `{"number":12,"title":"T","body":"","state":"MERGED","url":"https://github.com/acme/widgets/issues/12","updatedAt":""}`,
		"malformed":     `{"number":`,
	} {
		rec := &recorder{reply: process.Result{OK: true, Stdout: stdout}}
		got, err := KeyedGitHub{GitHub: GitHub{Run: rec.run}, Repo: "acme/widgets"}.Get(ctx, "#12")
		if err == nil || !reflect.DeepEqual(got, contract.TrackerIssue{}) {
			t.Fatalf("%s: Get = %+v, %v", name, got, err)
		}
	}
	rec = &recorder{reply: process.Result{Error: "exit status 1", Stderr: "boom"}}
	if _, err := (KeyedGitHub{GitHub: GitHub{Run: rec.run}, Repo: "acme/widgets"}).Get(ctx, "#12"); err == nil {
		t.Fatal("a gh failure was accepted")
	}
}

func TestKeyedGitHubStatesMapsThroughRun(t *testing.T) {
	rec := &recorder{reply: process.Result{OK: true, Stdout: trackerStatesAnswer}}
	k := KeyedGitHub{GitHub: GitHub{Run: rec.run}, Repo: "acme/widgets"}
	got, err := k.States(context.Background(), []string{"#7"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, contract.TrackerStates{States: map[string]contract.TrackerState{"#7": {State: "open"}}}) {
		t.Fatalf("States = %+v", got)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("calls = %d", len(rec.calls))
	}
	// Issue 8 was asked for but GitHub has none: it is left out, not invented.
	rec = &recorder{reply: process.Result{OK: true, Stdout: strings.Replace(trackerStatesAnswer, `"i7":`, `"i8":null,"i7":`, 1)}}
	got, err = KeyedGitHub{GitHub: GitHub{Run: rec.run}, Repo: "acme/widgets"}.States(context.Background(), []string{"#7", "#8"})
	if err != nil || len(got.States) != 1 || got.States["#7"].State != "open" {
		t.Fatalf("States with a missing Issue = %+v, %v", got, err)
	}
}

func TestKeyedGitHubStatesBounds(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{reply: process.Result{OK: true, Stdout: trackerStatesAnswer}}
	k := KeyedGitHub{GitHub: GitHub{Run: rec.run}, Repo: "acme/widgets"}
	many := make([]string, MaxStateKeys+1)
	for i := range many {
		many[i] = GitHubKey(int64(i + 1))
	}
	for name, keys := range map[string][]string{"too many": many, "duplicate": {"#7", "#7"}, "hostile": {"#7", "#7\n"}} {
		if _, err := k.States(ctx, keys); err == nil {
			t.Fatalf("%s: States accepted", name)
		}
	}
	if len(rec.calls) != 0 {
		t.Fatalf("gh ran for refused keys: %d calls", len(rec.calls))
	}
	if got, err := k.States(ctx, nil); err != nil || got.States == nil || len(got.States) != 0 || len(rec.calls) != 0 {
		t.Fatalf("States(nil) = %+v, %v", got, err)
	}
	rec.reply = process.Result{Code: 1, Error: "exit status 1", Stderr: "HTTP 401: Bad credentials"}
	if _, err := k.States(ctx, []string{"#7"}); err == nil {
		t.Fatal("a gh failure was accepted")
	}
}

const keyedListPage = `[{"number":3,"title":"Three","state":"open","html_url":"https://github.com/acme/widgets/issues/3","created_at":"2026-01-02T03:04:05Z","labels":[{"name":"bug"}],"milestone":{"title":"v1"}},{"number":5,"title":"Five","state":"open","html_url":"https://github.com/acme/widgets/issues/5","created_at":"2026-01-03T03:04:05Z","labels":[],"milestone":null}]`

func TestKeyedGitHubListPagesTheListing(t *testing.T) {
	ctx := context.Background()
	k := KeyedGitHub{Repo: "acme/widgets"}
	trackerFakeGH(t, keyedListPage, 0)
	first, err := k.List(ctx, "oldest", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	milestone := "v1"
	if len(first.Items) != 1 || !reflect.DeepEqual(first.Items[0], contract.TrackerItem{Key: "#3", Title: "Three", Labels: []string{"bug"}, Milestone: &milestone, UpdatedAt: "2026-01-02T03:04:05Z", State: "open"}) {
		t.Fatalf("first page = %+v", first.Items)
	}
	if first.NextCursor == nil || *first.NextCursor != "3.1767323045000000000" || first.Total == nil || *first.Total != 2 {
		t.Fatalf("first page cursor or total = %v %v", first.NextCursor, first.Total)
	}
	second, err := k.List(ctx, "oldest", *first.NextCursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Key != "#5" || second.Items[0].Milestone != nil || second.Items[0].Labels == nil || second.NextCursor != nil {
		t.Fatalf("second page = %+v", second)
	}
}

func TestKeyedGitHubListRefusesBadInputBeforeGH(t *testing.T) {
	ctx := context.Background()
	calls := trackerFakeGH(t, keyedListPage, 0)
	k := KeyedGitHub{Repo: "acme/widgets"}
	type args struct {
		selector, cursor string
		limit            int
	}
	for _, a := range []args{
		{"oldest", "", 0}, {"oldest", "", 101}, {"oldest", "", -1},
		{"newest", "", 10}, {"", "", 10}, {"label:", "", 10}, {"label: ", "", 10}, {"milestone:", "", 10}, {"jql:x", "", 10},
		{"label:a\nb", "", 10}, {"label:" + strings.Repeat("x", 200), "", 10}, {" oldest", "", 10},
		{"oldest", "x", 10}, {"oldest", "-1", 10}, {"oldest", "01", 10}, {"oldest", "9999999", 10}, {"oldest", "1", 10},
		{"oldest", "3.", 10}, {"oldest", ".5", 10}, {"oldest", "0.5", 10}, {"oldest", "03.5", 10}, {"oldest", "3.05", 10},
		{"oldest", "3.-5", 10}, {"oldest", "3.5.6", 10}, {"oldest", "+3.5", 10}, {"oldest", "3.99999999999999999999", 10},
	} {
		if _, err := k.List(ctx, a.selector, a.cursor, a.limit); err == nil {
			t.Fatalf("List(%q, %q, %d) accepted", a.selector, a.cursor, a.limit)
		}
	}
	if c := calls(); len(c) != 0 {
		t.Fatalf("gh ran for refused input: %q", c)
	}
}

const keyedListThree = `[{"number":3,"title":"Three","state":"open","html_url":"https://github.com/acme/widgets/issues/3","created_at":"2026-01-02T03:04:05Z","labels":[],"milestone":null},{"number":5,"title":"Five","state":"open","html_url":"https://github.com/acme/widgets/issues/5","created_at":"2026-01-03T03:04:05Z","labels":[],"milestone":null},{"number":7,"title":"Seven","state":"open","html_url":"https://github.com/acme/widgets/issues/7","created_at":"2026-01-04T03:04:05Z","labels":[],"milestone":null}]`

// TestKeyedGitHubListCursorSurvivesAClosedIssue closes an Issue of the first
// page before the second page is read: with an offset cursor the second page
// would skip #5; the last-seen cursor returns every remaining Issue once.
func TestKeyedGitHubListCursorSurvivesAClosedIssue(t *testing.T) {
	ctx := context.Background()
	k := KeyedGitHub{Repo: "acme/widgets"}
	trackerFakeGH(t, keyedListThree, 0)
	first, err := k.List(ctx, "oldest", "", 1)
	if err != nil || len(first.Items) != 1 || first.Items[0].Key != "#3" || first.NextCursor == nil {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	trackerFakeGH(t, strings.Replace(keyedListThree, `{"number":3,"title":"Three","state":"open","html_url":"https://github.com/acme/widgets/issues/3","created_at":"2026-01-02T03:04:05Z","labels":[],"milestone":null},`, "", 1), 0)
	var keys []string
	cursor := *first.NextCursor
	for range 3 {
		page, err := k.List(ctx, "oldest", cursor, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			keys = append(keys, item.Key)
		}
		if page.NextCursor == nil {
			break
		}
		if *page.NextCursor == cursor {
			t.Fatalf("the cursor did not advance: %q", cursor)
		}
		cursor = *page.NextCursor
	}
	if !reflect.DeepEqual(keys, []string{"#5", "#7"}) {
		t.Fatalf("pages after closing #3 = %v, want #5 and #7 once each", keys)
	}

	// A cursor past every remaining Issue is an empty last page.
	trackerFakeGH(t, keyedListThree, 0)
	last, err := k.List(ctx, "oldest", "7.1767495845000000000", 10)
	if err != nil || len(last.Items) != 0 || last.NextCursor != nil {
		t.Fatalf("page after the last Issue = %+v, %v", last, err)
	}
}
