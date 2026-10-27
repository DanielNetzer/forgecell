package forgesync

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode"
)

// fakeGH answers gh api graphql calls without running any process.
type fakeGH struct {
	mu    sync.Mutex
	calls []process.Options
	reply func(query string, vars map[string]string) process.Result
}

func (f *fakeGH) run(_ context.Context, o process.Options) process.Result {
	f.mu.Lock()
	f.calls = append(f.calls, o)
	f.mu.Unlock()
	vars := map[string]string{}
	query := ""
	for i := 0; i+1 < len(o.Argv); i++ {
		if o.Argv[i] != "-f" {
			continue
		}
		k, v, _ := strings.Cut(o.Argv[i+1], "=")
		if k == "query" {
			query = v
		} else {
			vars[k] = v
		}
	}
	return f.reply(query, vars)
}

func (f *fakeGH) gh() *GH { return &GH{Run: f.run} }

func ok(v any) process.Result {
	raw, _ := json.Marshal(v)
	return process.Result{OK: true, Code: 0, Reconciled: true, Stdout: string(raw)}
}

func issueNode(n int64, state string) map[string]any {
	return map[string]any{"number": n, "state": state, "stateReason": nil, "createdAt": "2026-10-01T00:00:00Z", "updatedAt": "2026-10-02T00:00:00Z", "title": fmt.Sprintf("Issue %d", n), "locked": false, "labels": map[string]any{"nodes": []any{map[string]any{"name": "bug"}}}, "milestone": nil}
}

var aliasPattern = regexp.MustCompile(`([ip])([0-9]+):(issue|pullRequest)\(number:([0-9]+)\)`)

// statesReply answers every alias in the query; missing names are returned as
// null nodes with a NOT_FOUND error and a non-zero exit, as gh does.
func statesReply(missing map[string]bool) func(string, map[string]string) process.Result {
	return func(query string, _ map[string]string) process.Result {
		repo := map[string]any{}
		var errs []any
		for _, m := range aliasPattern.FindAllStringSubmatch(query, -1) {
			alias := m[1] + m[2]
			var n int64
			fmt.Sscan(m[4], &n)
			switch {
			case missing[alias]:
				repo[alias] = nil
				errs = append(errs, map[string]any{"type": "NOT_FOUND", "message": "Could not resolve", "path": []any{"repository", alias}})
			case m[1] == "p":
				repo[alias] = map[string]any{"number": n, "state": "MERGED", "isDraft": false, "mergedAt": "2026-10-18T10:00:00Z", "mergeCommit": map[string]any{"oid": strings.Repeat("a", 40)}, "headRefOid": strings.Repeat("b", 40), "headRef": nil, "reviewDecision": "APPROVED", "commits": map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": "SUCCESS"}}}}}}
			default:
				node := issueNode(n, "CLOSED")
				node["stateReason"] = "COMPLETED"
				node["closedAt"] = "2026-10-18T11:00:00Z"
				node["timelineItems"] = map[string]any{"nodes": []any{map[string]any{"closer": map[string]any{"__typename": "PullRequest", "number": 5}}}}
				repo[alias] = node
			}
		}
		res := ok(map[string]any{"data": map[string]any{"repository": repo}, "errors": errs})
		if len(errs) > 0 {
			res.OK, res.Code, res.Stderr = false, 1, "gh: Could not resolve to an Issue"
		}
		return res
	}
}

func TestStatesBatchesAtMostFiftyNumbersPerQuery(t *testing.T) {
	f := &fakeGH{reply: statesReply(map[string]bool{"i3": true})}
	var issues, prs []int64
	for n := int64(1); n <= 120; n++ {
		issues = append(issues, n)
	}
	for n := int64(200); n < 230; n++ {
		prs = append(prs, n)
	}
	// Duplicates and invalid numbers are dropped.
	issues = append(issues, 1, 0, -4)
	got, err := f.gh().States(context.Background(), "o/r", issues, prs)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("%d calls for 150 numbers, want 3", len(f.calls))
	}
	for _, c := range f.calls {
		q := c.Argv[slicesIndex(c.Argv, "-f")+1]
		if n := len(aliasPattern.FindAllString(q, -1)); n > MaxBatch {
			t.Fatalf("a query carries %d numbers", n)
		}
		if c.Argv[0] != "gh" || c.Argv[1] != "api" || c.Argv[2] != "graphql" || c.Argv[4] != "github.com" || c.Timeout <= 0 || c.MaxOutputBytes <= 0 {
			t.Fatalf("call %v timeout %v cap %d", c.Argv[:5], c.Timeout, c.MaxOutputBytes)
		}
	}
	if len(got.Issues) != 120 || len(got.PRs) != 30 || got.Truncated {
		t.Fatalf("issues %d prs %d truncated %v", len(got.Issues), len(got.PRs), got.Truncated)
	}
	if i := got.Issues[3]; i.Found || i.Unavailable != UnavailableNotFound {
		t.Fatalf("missing Issue %+v", i)
	}
	i := got.Issues[7]
	if !i.Found || i.State != IssueClosed || i.StateReason != ReasonCompleted || i.ClosedBy == nil || i.ClosedBy.URL != "https://github.com/o/r/pull/5" || i.URL != "https://github.com/o/r/issues/7" || i.ClosedAt != "2026-10-18T11:00:00Z" {
		t.Fatalf("closed Issue %+v", i)
	}
	p := got.PRs[201]
	if !p.Found || p.State != PRMerged || p.MergeCommit != strings.Repeat("a", 40) || !p.HeadDeleted || p.Checks != "SUCCESS" || p.ReviewDecision != "APPROVED" || p.URL != "https://github.com/o/r/pull/201" {
		t.Fatalf("PR %+v", p)
	}
}

func slicesIndex(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

func TestStatesCapsTheNumbersRead(t *testing.T) {
	f := &fakeGH{reply: statesReply(nil)}
	var issues []int64
	for n := int64(1); n <= MaxStateNumbers+20; n++ {
		issues = append(issues, n)
	}
	got, err := f.gh().States(context.Background(), "o/r", issues, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || len(got.Issues) != MaxStateNumbers || len(f.calls) != MaxStateNumbers/MaxBatch {
		t.Fatalf("truncated %v issues %d calls %d", got.Truncated, len(got.Issues), len(f.calls))
	}
	// The newest numbers are the ones read.
	if _, ok := got.Issues[MaxStateNumbers+20]; !ok {
		t.Fatal("the newest Issue was not read")
	}
}

func listReply(total int, pageSize func(page int) int) func(string, map[string]string) process.Result {
	return func(_ string, vars map[string]string) process.Result {
		page := 0
		if after := vars["after"]; after != "" {
			fmt.Sscanf(after, "cursor%d", &page)
		}
		size := pageSize(page)
		nodes := []any{}
		for i := 0; i < size; i++ {
			nodes = append(nodes, issueNode(int64(page*100+i+1), "OPEN"))
		}
		more := page*100+size < total
		return ok(map[string]any{"data": map[string]any{"repository": map[string]any{"issues": map[string]any{"totalCount": total, "pageInfo": map[string]any{"hasNextPage": more, "endCursor": fmt.Sprintf("cursor%d", page+1)}, "nodes": nodes}}}})
	}
}

func TestOpenIssuesPaginatesAndTruncatesAtFiveHundred(t *testing.T) {
	f := &fakeGH{reply: listReply(742, func(int) int { return 100 })}
	got, err := f.gh().OpenIssues(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != MaxListed/listPageSize || len(got.Issues) != MaxListed || got.Total != 742 || !got.Truncated {
		t.Fatalf("calls %d issues %d total %d truncated %v", len(f.calls), len(got.Issues), got.Total, got.Truncated)
	}
	if got.Issues[0].Number != 1 || got.Issues[499].Number != 500 || got.Issues[0].Labels[0] != "bug" {
		t.Fatalf("order %+v", got.Issues[0])
	}
	// The first page carries no cursor; later ones carry the previous end cursor.
	if v := f.calls[0].Argv; strings.Contains(strings.Join(v, " "), "after=") {
		t.Fatalf("first page with a cursor: %v", v[5:])
	}
	f = &fakeGH{reply: listReply(130, func(page int) int { return []int{100, 30}[page] })}
	got, err = f.gh().OpenIssues(context.Background(), "o/r")
	if err != nil || len(f.calls) != 2 || len(got.Issues) != 130 || got.Truncated {
		t.Fatalf("short listing: %v calls %d issues %d truncated %v", err, len(f.calls), len(got.Issues), got.Truncated)
	}
}

func TestOpenIssuesRefusesMalformedPages(t *testing.T) {
	for name, reply := range map[string]func(string, map[string]string) process.Result{
		"not json": func(string, map[string]string) process.Result { return process.Result{OK: true, Stdout: "<html>"} },
		"closed in open": func(string, map[string]string) process.Result {
			return ok(listPage([]any{issueNode(1, "CLOSED")}, false, ""))
		},
		"unknown state": func(string, map[string]string) process.Result {
			return ok(listPage([]any{issueNode(1, "WEIRD")}, false, ""))
		},
		"bad number": func(string, map[string]string) process.Result {
			return ok(listPage([]any{issueNode(0, "OPEN")}, false, ""))
		},
		"bad cursor":      func(string, map[string]string) process.Result { return ok(listPage([]any{}, true, "$(rm -rf /)")) },
		"repeated cursor": func(_ string, v map[string]string) process.Result { return ok(listPage([]any{}, true, "same")) },
	} {
		f := &fakeGH{reply: reply}
		if _, err := f.gh().OpenIssues(context.Background(), "o/r"); ReasonOf(err) != ReasonMalformed {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func listPage(nodes []any, more bool, cursor string) map[string]any {
	return map[string]any{"data": map[string]any{"repository": map[string]any{"issues": map[string]any{"totalCount": len(nodes), "pageInfo": map[string]any{"hasNextPage": more, "endCursor": cursor}, "nodes": nodes}}}}
}

// Untrusted Issue text is bounded and stripped of control characters; links are
// built from the validated number, never taken from GitHub.
func TestOpenIssuesBoundsUntrustedText(t *testing.T) {
	node := issueNode(9, "OPEN")
	node["title"] = "evil\x1b[31m\n" + strings.Repeat("x", 1000)
	node["url"] = "javascript:alert(1)"
	labels := []any{}
	for i := 0; i < 40; i++ {
		labels = append(labels, map[string]any{"name": strings.Repeat("l", 500)})
	}
	node["labels"] = map[string]any{"nodes": labels}
	node["milestone"] = map[string]any{"title": strings.Repeat("m", 1000)}
	node["updatedAt"] = "yesterday"
	f := &fakeGH{reply: func(string, map[string]string) process.Result { return ok(listPage([]any{node}, false, "")) }}
	got, err := f.gh().OpenIssues(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	i := got.Issues[0]
	if strings.ContainsAny(i.Title, "\x1b\n") || len([]rune(i.Title)) > maxTitleRunes+1 || len(i.Labels) != maxLabels || len([]rune(i.Labels[0])) > maxLabelRunes+1 || len([]rune(i.Milestone)) > maxMilestone+1 {
		t.Fatalf("unbounded: %q %d labels", i.Title, len(i.Labels))
	}
	if i.URL != "https://github.com/o/r/issues/9" || i.UpdatedAt != "" {
		t.Fatalf("url %q updated %q", i.URL, i.UpdatedAt)
	}
}

// Unicode format characters (unicode.Cf), such as bidi overrides and zero-width
// joiners, are stripped from titles, labels and milestones.
func TestOpenIssuesStripsFormatCharacters(t *testing.T) {
	rlo, pdf, zwsp, lri, pdi, bom, zwj, rli := string(rune(0x202e)), string(rune(0x202c)), string(rune(0x200b)), string(rune(0x2066)), string(rune(0x2069)), string(rune(0xfeff)), string(rune(0x200d)), string(rune(0x2067))
	node := issueNode(9, "OPEN")
	node["title"] = "fix " + rlo + "gnp.exe" + pdf + " login" + zwsp + lri + "x" + pdi + bom
	node["labels"] = map[string]any{"nodes": []any{map[string]any{"name": rlo + "bug"}, map[string]any{"name": zwj}}}
	node["milestone"] = map[string]any{"title": "v1" + rli}
	f := &fakeGH{reply: func(string, map[string]string) process.Result { return ok(listPage([]any{node}, false, "")) }}
	got, err := f.gh().OpenIssues(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	i := got.Issues[0]
	if i.Title != "fix gnp.exe loginx" || len(i.Labels) != 1 || i.Labels[0] != "bug" || i.Milestone != "v1" {
		t.Fatalf("format characters kept: %q %q %q", i.Title, i.Labels, i.Milestone)
	}
	for _, s := range append([]string{i.Title, i.Milestone}, i.Labels...) {
		for _, r := range s {
			if unicode.Is(unicode.Cf, r) {
				t.Fatalf("%q keeps %U", s, r)
			}
		}
	}
}

// The latest reopen is read with every Issue, for row 15.
func TestIssuesReadTheLatestReopen(t *testing.T) {
	node := issueNode(9, "OPEN")
	node["stateReason"] = "REOPENED"
	node["reopened"] = map[string]any{"nodes": []any{map[string]any{"createdAt": "2026-10-20T10:00:00Z"}}}
	bad := issueNode(10, "OPEN")
	bad["reopened"] = map[string]any{"nodes": []any{map[string]any{"createdAt": "soon"}}}
	f := &fakeGH{reply: func(string, map[string]string) process.Result { return ok(listPage([]any{node, bad}, false, "")) }}
	got, err := f.gh().OpenIssues(context.Background(), "o/r")
	if err != nil {
		t.Fatal(err)
	}
	if got.Issues[0].ReopenedAt != "2026-10-20T10:00:00Z" || got.Issues[1].ReopenedAt != "" {
		t.Fatalf("reopened %q %q", got.Issues[0].ReopenedAt, got.Issues[1].ReopenedAt)
	}
	if q := f.calls[0].Argv[slicesIndex(f.calls[0].Argv, "-f")+1]; !strings.Contains(q, "reopened:timelineItems(last:1,itemTypes:[REOPENED_EVENT])") {
		t.Fatalf("query lacks the reopen: %s", q)
	}
}

func TestFetcherRefusesAnInvalidRepositoryWithoutCallingGH(t *testing.T) {
	f := &fakeGH{reply: func(string, map[string]string) process.Result { t.Fatal("gh was called"); return process.Result{} }}
	for _, repo := range []string{"", "o", "../x", "o/r; rm -rf /", "-f/x y"} {
		if _, err := f.gh().OpenIssues(context.Background(), repo); ReasonOf(err) != ReasonRepository {
			t.Errorf("%q: %v", repo, err)
		}
		if _, err := f.gh().States(context.Background(), repo, []int64{1}, nil); ReasonOf(err) != ReasonRepository {
			t.Errorf("%q: %v", repo, err)
		}
	}
}

// Every failure kind maps to its one fixed reason, and no reason carries gh's
// own output.
func TestFetcherFailuresMapToFixedReasons(t *testing.T) {
	const secret = "/Users/someone/.config/gh/hosts.yml ghp_SECRETTOKEN"
	cases := []struct {
		name   string
		result process.Result
		reason string
	}{
		{"gh missing", process.Result{Code: -1, Error: `exec: "gh": executable file not found in $PATH ` + secret}, ReasonGHMissing},
		{"not authenticated", process.Result{Code: 4, Stderr: "To get started with GitHub CLI, please run:  gh auth login " + secret}, ReasonNotAuthenticated},
		{"bad credentials", process.Result{Code: 1, Stderr: "HTTP 401: Bad credentials " + secret}, ReasonNotAuthenticated},
		{"rate limited", process.Result{Code: 1, Stderr: "HTTP 403: API rate limit exceeded for user " + secret}, ReasonRateLimited},
		{"rate limited graphql", ok(map[string]any{"data": nil, "errors": []any{map[string]any{"type": "RATE_LIMITED", "message": secret}}}), ReasonRateLimited},
		{"offline", process.Result{Code: 1, Stderr: "error connecting to api.github.com: dial tcp: lookup api.github.com: no such host " + secret}, ReasonOffline},
		{"wrong host", process.Result{Code: 1, Stderr: "none of the git remotes configured for this repository point to a known GitHub host " + secret}, ReasonWrongHost},
		{"repository not found", func() process.Result {
			r := ok(map[string]any{"data": map[string]any{"repository": nil}, "errors": []any{map[string]any{"type": "NOT_FOUND", "path": []any{"repository"}, "message": secret}}})
			r.OK, r.Code = false, 1
			return r
		}(), ReasonWrongHost},
		{"timeout", process.Result{TimedOut: true, Error: "context deadline exceeded " + secret}, ReasonTimeout},
		{"parent deadline", process.Result{ParentDeadline: true}, ReasonTimeout},
		{"byte cap", process.Result{Overflow: true, Stdout: secret}, ReasonOversized},
		{"malformed", process.Result{OK: true, Stdout: "not json " + secret}, ReasonMalformed},
		{"other", process.Result{Code: 1, Stderr: "something odd " + secret}, ReasonFailed},
	}
	for _, c := range cases {
		f := &fakeGH{reply: func(string, map[string]string) process.Result { return c.result }}
		_, err := f.gh().OpenIssues(context.Background(), "o/r")
		if ReasonOf(err) != c.reason || err.Error() != c.reason {
			t.Errorf("%s: %v, want %q", c.name, err, c.reason)
		}
		_, err = f.gh().States(context.Background(), "o/r", []int64{1}, []int64{2})
		if ReasonOf(err) != c.reason || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "/Users/") {
			t.Errorf("%s states: %v", c.name, err)
		}
	}
	if ReasonOf(fmt.Errorf("raw %s", secret)) != ReasonFailed {
		t.Fatal("a foreign error leaks its text")
	}
}

// The real process runner enforces the byte cap: a gh stand-in that prints
// more than the cap fails closed as oversized.
func TestFetcherByteCapWithTheRealRunner(t *testing.T) {
	g := &GH{Run: func(ctx context.Context, o process.Options) process.Result {
		o.Argv = []string{"sh", "-c", "head -c 4096 /dev/zero"}
		o.MaxOutputBytes = 64
		return process.Run(ctx, o)
	}}
	if _, err := g.OpenIssues(context.Background(), "o/r"); ReasonOf(err) != ReasonOversized {
		t.Fatalf("byte cap: %v", err)
	}
}
