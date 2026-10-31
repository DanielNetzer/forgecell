package tracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// recorder is a Run seam that answers every call with reply and keeps the
// options it was called with.
type recorder struct {
	reply process.Result
	calls []process.Options
}

func (r *recorder) run(_ context.Context, o process.Options) process.Result {
	r.calls = append(r.calls, o)
	return r.reply
}

// trackerFakeGH puts a fake gh first on PATH. It logs every call's arguments
// one per line, ends each call with "--", prints reply and exits with code.
func trackerFakeGH(t *testing.T, reply string, code int) func() [][]string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "reply"), []byte(reply), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> '" + dir + "/calls.log'; done\nprintf -- '--\\n' >> '" + dir + "/calls.log'\ncat '" + dir + "/reply'\nexit " + strconv.Itoa(code) + "\n"
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
		var cur []string
		for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if line == "--" {
				calls = append(calls, cur)
				cur = nil
				continue
			}
			cur = append(cur, line)
		}
		return calls
	}
}

func TestGitHubDescribe(t *testing.T) {
	d, err := GitHub{}.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Description{DisplayName: "GitHub Issues", KeyPattern: "#N", Selectors: []string{"oldest", "label:", "milestone:"}}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("Describe = %+v, want %+v", d, want)
	}
}

func TestGitHubGetSendsReadIssueInvocation(t *testing.T) {
	rec := &recorder{reply: process.Result{OK: true, Stdout: `{"number":12,"title":"T","body":"B","state":"OPEN","url":"https://github.com/acme/widgets/issues/12","updatedAt":"2026-10-30T00:00:00Z","repo":"evil/other"}`}}
	got, err := GitHub{Run: rec.run}.Get(context.Background(), "acme/widgets", 12, "work/dir")
	if err != nil {
		t.Fatal(err)
	}
	want := process.Options{Argv: []string{"gh", "issue", "view", "12", "--repo", "acme/widgets", "--json", "number,title,body,state,url,updatedAt"}, Dir: "work/dir", Stdin: []byte{}, Timeout: 20 * time.Second}
	if len(rec.calls) != 1 || !reflect.DeepEqual(rec.calls[0], want) {
		t.Fatalf("calls = %#v, want one %#v", rec.calls, want)
	}
	if rec.calls[0].Stdin == nil {
		t.Fatal("Stdin is nil, want an empty non-nil slice")
	}
	ticket := Ticket{UpdatedAt: "2026-10-30T00:00:00Z", Number: 12, Repo: "acme/widgets", Title: "T", Body: "B", URL: "https://github.com/acme/widgets/issues/12", State: "OPEN"}
	if got != ticket {
		t.Fatalf("Get = %+v, want %+v", got, ticket)
	}
}

func TestGitHubGetFailures(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{reply: process.Result{Error: "exit status 1", Stderr: "boom"}}
	got, err := GitHub{Run: rec.run}.Get(ctx, "acme/widgets", 12, "")
	if err == nil || err.Error() != "GitHub intake failed: exit status 1 boom" || got != (Ticket{}) {
		t.Fatalf("gh failure: Get = %+v, %v", got, err)
	}

	// A decode error comes back unchanged with the partly decoded value, and the
	// repository is not filled in.
	body := `{"number":12,"title":5}`
	rec = &recorder{reply: process.Result{OK: true, Stdout: body}}
	got, err = GitHub{Run: rec.run}.Get(ctx, "acme/widgets", 12, "")
	var direct Ticket
	wantErr := json.Unmarshal([]byte(body), &direct)
	if err == nil || err.Error() != wantErr.Error() || got != (Ticket{Number: 12}) {
		t.Fatalf("malformed JSON: Get = %+v, %v; want %+v, %v", got, err, Ticket{Number: 12}, wantErr)
	}
	rec = &recorder{reply: process.Result{OK: true, Stdout: `{"number":`}}
	if _, err = (GitHub{Run: rec.run}).Get(ctx, "acme/widgets", 12, ""); err == nil {
		t.Fatal("truncated JSON: Get returned no error")
	}

	// An empty repository is passed to gh as it always was, unvalidated.
	rec = &recorder{reply: process.Result{Error: "exit status 1"}}
	if _, err = (GitHub{Run: rec.run}).Get(ctx, "", 12, ""); err == nil || err.Error() != "GitHub intake failed: exit status 1 " {
		t.Fatalf("empty repo: err = %v", err)
	}
	if len(rec.calls) != 1 || rec.calls[0].Argv[5] != "" {
		t.Fatalf("empty repo: calls = %#v", rec.calls)
	}
}

func TestGitHubGetNilRunUsesProcessRun(t *testing.T) {
	calls := trackerFakeGH(t, `{"number":3,"title":"T","body":"","state":"OPEN","url":"https://github.com/acme/widgets/issues/3","updatedAt":""}`, 0)
	got, err := GitHub{}.Get(context.Background(), "acme/widgets", 3, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != 3 || got.Repo != "acme/widgets" || got.Title != "T" {
		t.Fatalf("Get = %+v", got)
	}
	want := [][]string{{"issue", "view", "3", "--repo", "acme/widgets", "--json", "number,title,body,state,url,updatedAt"}}
	if c := calls(); !reflect.DeepEqual(c, want) {
		t.Fatalf("gh calls = %q, want %q", c, want)
	}
}

func TestGitHubListMatchesReadSelectedOpenIssues(t *testing.T) {
	ctx := context.Background()
	page := `[{"number":3,"title":"Three","state":"open","html_url":"https://github.com/acme/widgets/issues/3","created_at":"2026-01-02T03:04:05Z","labels":[{"name":"bug"}],"milestone":null}]`
	for _, sel := range []intake.Selector{{}, {Kind: intake.SelectLabel, Value: "bug"}} {
		calls := trackerFakeGH(t, page, 0)
		got, err := GitHub{}.List(ctx, "acme/widgets", sel)
		if err != nil {
			t.Fatal(err)
		}
		gotCalls := calls()
		filter := ""
		if sel.Kind == intake.SelectLabel {
			filter = "&labels=bug"
		}
		wantCalls := [][]string{{"api", "--hostname", "github.com", "--method", "GET", "repos/acme/widgets/issues?state=open&sort=created&direction=asc&per_page=30" + filter + "&page=1"}}
		if !reflect.DeepEqual(gotCalls, wantCalls) {
			t.Fatalf("%v: gh calls = %q, want %q", sel, gotCalls, wantCalls)
		}
		want, err := intake.ReadSelectedOpenIssues(ctx, "acme/widgets", sel)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) || len(got.Issues) != 1 || got.Issues[0].Number != 3 || got.Truncated {
			t.Fatalf("%v: List = %+v, want %+v", sel, got, want)
		}
	}
}

func TestGitHubListFailures(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		reply string
		code  int
	}{"gh failure": {"", 1}, "malformed JSON": {`{"not":"a page"`, 0}} {
		trackerFakeGH(t, tc.reply, tc.code)
		got, err := GitHub{}.List(ctx, "acme/widgets", intake.Selector{})
		_, wantErr := intake.ReadSelectedOpenIssues(ctx, "acme/widgets", intake.Selector{})
		if err == nil || wantErr == nil || err.Error() != wantErr.Error() || len(got.Issues) != 0 {
			t.Fatalf("%s: List = %+v, %v; want error %v", name, got, err, wantErr)
		}
	}

	calls := trackerFakeGH(t, "[]", 0)
	_, err := GitHub{}.List(ctx, "", intake.Selector{})
	_, wantErr := intake.ReadSelectedOpenIssues(ctx, "", intake.Selector{})
	if err == nil || wantErr == nil || err.Error() != wantErr.Error() {
		t.Fatalf("empty repo: err = %v, want %v", err, wantErr)
	}
	if c := calls(); len(c) != 0 {
		t.Fatalf("empty repo started gh: %q", c)
	}
}

const trackerStatesAnswer = `{"data":{"repository":{"i7":{"number":7,"state":"OPEN","stateReason":null,"createdAt":"2026-01-02T03:04:05Z","updatedAt":"2026-01-02T03:04:05Z","title":"Seven","locked":false,"labels":{"nodes":[]},"milestone":null,"reopened":{"nodes":[]},"closedAt":null,"timelineItems":{"nodes":[]}}}}}`

func TestGitHubStatesMatchesGHStates(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{reply: process.Result{OK: true, Stdout: trackerStatesAnswer}}
	issues, truncated, err := GitHub{Run: rec.run}.States(ctx, "acme/widgets", []int64{7})
	if err != nil {
		t.Fatal(err)
	}
	direct := &recorder{reply: rec.reply}
	gh := forgesync.NewGH()
	gh.Run = direct.run
	want, err := gh.States(ctx, "acme/widgets", []int64{7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.calls, direct.calls) || len(rec.calls) != 1 {
		t.Fatalf("calls = %#v, want %#v", rec.calls, direct.calls)
	}
	o := rec.calls[0]
	if o.Timeout != 20*time.Second || o.MaxOutputBytes != 4_000_000 || o.Dir != "" || o.Stdin == nil || len(o.Stdin) != 0 {
		t.Fatalf("options = %#v", o)
	}
	if head := []string{"gh", "api", "graphql", "--hostname", "github.com", "-f"}; !reflect.DeepEqual(o.Argv[:6], head) || !reflect.DeepEqual(o.Argv[7:], []string{"-f", "name=widgets", "-f", "owner=acme"}) {
		t.Fatalf("argv = %q", o.Argv)
	}
	if !reflect.DeepEqual(issues, want.Issues) || truncated != want.Truncated || truncated {
		t.Fatalf("States = %+v, %v; want %+v, %v", issues, truncated, want.Issues, want.Truncated)
	}
	if i := issues[7]; !i.Found || i.Title != "Seven" || i.State != forgesync.IssueOpen {
		t.Fatalf("issue 7 = %+v", i)
	}
}

func TestGitHubStatesFailures(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		reply  process.Result
		reason string
	}{
		"gh failure":     {process.Result{Code: 1, Error: "exit status 1", Stderr: "HTTP 401: Bad credentials"}, forgesync.ReasonNotAuthenticated},
		"malformed JSON": {process.Result{OK: true, Stdout: `{"data":`}, forgesync.ReasonMalformed},
	} {
		rec := &recorder{reply: tc.reply}
		issues, truncated, err := GitHub{Run: rec.run}.States(ctx, "acme/widgets", []int64{7})
		if err == nil || err.Error() != tc.reason || forgesync.ReasonOf(err) != tc.reason || issues != nil || truncated {
			t.Fatalf("%s: States = %v, %v, %v; want %q", name, issues, truncated, err, tc.reason)
		}
	}

	rec := &recorder{}
	_, _, err := GitHub{Run: rec.run}.States(ctx, "", []int64{7})
	if err == nil || err.Error() != forgesync.ReasonRepository || len(rec.calls) != 0 {
		t.Fatalf("empty repo: err = %v, calls = %d", err, len(rec.calls))
	}
}
