package forge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// Pinned to the field list in delivery/publish_characterization_test.go.
const wantPRFields = "url,state,baseRefName,headRefName,headRefOid,isDraft,headRepository,headRepositoryOwner"

type recorder struct {
	calls  []process.Options
	result func(process.Options) process.Result
}

func (r *recorder) run(_ context.Context, o process.Options) process.Result {
	r.calls = append(r.calls, o)
	if r.result == nil {
		return process.Result{OK: true}
	}
	return r.result(o)
}

func assertGHOptions(t *testing.T, got process.Options, argv []string, dir string, timeout time.Duration, limit int) {
	t.Helper()
	if !reflect.DeepEqual(got.Argv, argv) {
		t.Fatalf("argv = %q, want %q", got.Argv, argv)
	}
	if got.Dir != dir || got.Timeout != timeout || got.MaxOutputBytes != limit || got.Stdin == nil || len(got.Stdin) != 0 || got.Input != nil || got.Env != nil || got.StdoutLines != nil {
		t.Fatalf("options = %+v", got)
	}
}

func TestPRFieldsMatchCharacterization(t *testing.T) {
	if PRFields != wantPRFields {
		t.Fatalf("PRFields = %q", PRFields)
	}
}

func TestGitHubPRCallsBuildExactArgv(t *testing.T) {
	url := "https://github.com/owner/repo/pull/7"
	cases := []struct {
		name string
		call func(GitHub) (string, error)
		argv []string
	}{
		{"find", func(g GitHub) (string, error) {
			return g.FindChange(context.Background(), "owner/repo", "forgecell/fixture", "work")
		}, []string{"gh", "pr", "list", "--repo", "owner/repo", "--head", "forgecell/fixture", "--state", "all", "--limit", "2", "--json", wantPRFields}},
		{"create", func(g GitHub) (string, error) {
			return g.PublishDraft(context.Background(), "owner/repo", "main", "forgecell/fixture", "Fixture", "body.md", "work")
		}, []string{"gh", "pr", "create", "--draft", "--repo", "owner/repo", "--base", "main", "--head", "forgecell/fixture", "--title", "Fixture", "--body-file", "body.md"}},
		{"view", func(g GitHub) (string, error) {
			return g.Change(context.Background(), "owner/repo", url, "work")
		}, []string{"gh", "pr", "view", url, "--repo", "owner/repo", "--json", wantPRFields}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &recorder{result: func(process.Options) process.Result {
				return process.Result{OK: true, Stdout: "  \n" + url + "\n"}
			}}
			out, err := c.call(GitHub{Run: r.run})
			if err != nil || out != url {
				t.Fatalf("out = %q, err = %v", out, err)
			}
			if len(r.calls) != 1 {
				t.Fatalf("calls = %d", len(r.calls))
			}
			assertGHOptions(t, r.calls[0], c.argv, "work", 60*time.Second, 1_000_000)
		})
	}
}

func TestGitHubPRCallsFailClosed(t *testing.T) {
	big := strings.Repeat("x", 600_000)
	cases := []struct {
		name   string
		result process.Result
		err    string
	}{
		{"gh failure", process.Result{Code: 1, Error: "exit status 1", Stderr: "HTTP 502"}, "gh failed or uncertain: exit status 1 HTTP 502"},
		{"timeout", process.Result{OK: true, TimedOut: true, Error: "timed out"}, "gh failed or uncertain: timed out "},
		{"interrupted", process.Result{OK: true, Interrupted: true, Error: "interrupted"}, "gh failed or uncertain: interrupted "},
		{"overflow", process.Result{OK: true, Overflow: true, Stdout: "partial"}, "gh failed or uncertain:  "},
		{"oversized output", process.Result{OK: true, Stdout: big, Stderr: big}, "gh failed or uncertain:  " + big},
		{"uncertain empty", process.Result{}, "gh failed or uncertain:  "},
	}
	calls := map[string]func(GitHub) (string, error){
		"find": func(g GitHub) (string, error) { return g.FindChange(context.Background(), "owner/repo", "b", "work") },
		"create": func(g GitHub) (string, error) {
			return g.PublishDraft(context.Background(), "owner/repo", "main", "b", "T", "body.md", "work")
		},
		"view": func(g GitHub) (string, error) {
			return g.Change(context.Background(), "owner/repo", "https://github.com/owner/repo/pull/1", "work")
		},
	}
	for _, c := range cases {
		for name, call := range calls {
			t.Run(c.name+"/"+name, func(t *testing.T) {
				r := &recorder{result: func(process.Options) process.Result { return c.result }}
				out, err := call(GitHub{Run: r.run})
				if err == nil || err.Error() != c.err || out != "" {
					t.Fatalf("out = %q, err = %v", out, err)
				}
			})
		}
	}
}

func TestGitHubPRCallsPassMalformedJSONThroughRaw(t *testing.T) {
	r := &recorder{result: func(process.Options) process.Result { return process.Result{OK: true, Stdout: "{not json\n"} }}
	g := GitHub{Run: r.run}
	if out, err := g.FindChange(context.Background(), "owner/repo", "b", "work"); err != nil || out != "{not json" {
		t.Fatalf("find out = %q, err = %v", out, err)
	}
	if out, err := g.Change(context.Background(), "owner/repo", "https://github.com/owner/repo/pull/1", "work"); err != nil || out != "{not json" {
		t.Fatalf("view out = %q, err = %v", out, err)
	}
}

func TestGitHubPolicyDelegatesToReadRemotePolicy(t *testing.T) {
	r := &recorder{result: func(o process.Options) process.Result {
		if strings.Contains(o.Argv[4], "/rules/") {
			return process.Result{OK: true, Stdout: "[]"}
		}
		return process.Result{OK: true, Stdout: "{not json"}
	}}
	p := GitHub{Run: r.run}.Policy(context.Background(), "owner/repo", "release/1", "work")
	want := [][]string{
		{"gh", "api", "--method", "GET", "repos/owner/repo/rules/branches/release%2F1?per_page=100"},
		{"gh", "api", "--method", "GET", "repos/owner/repo/branches/release%2F1/protection"},
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls = %d", len(r.calls))
	}
	for i, c := range r.calls {
		assertGHOptions(t, c, want[i], "work", 15*time.Second, 256000)
	}
	if p.Rules.Status != "observed" || p.Rules.Source != "https://api.github.com/"+want[0][4] {
		t.Fatalf("rules = %+v", p.Rules)
	}
	if p.Protection.Status != "unknown" || p.Protection.Unknown != "Invalid policy response" {
		t.Fatalf("protection = %+v", p.Protection)
	}
}

func TestGitHubPolicyFailureStaysUnknown(t *testing.T) {
	r := &recorder{result: func(process.Options) process.Result { return process.Result{OK: true, TimedOut: true} }}
	p := GitHub{Run: r.run}.Policy(context.Background(), "owner/repo", "main", "work")
	text := "Policy unavailable, unauthorized, timed out or exceeded bounds. No absence of protection is inferred."
	if p.Rules.Status != "unknown" || p.Rules.Unknown != text || p.Protection.Status != "unknown" || p.Protection.Unknown != text {
		t.Fatalf("policy = %+v", p)
	}
}

func checksTarget() checks.Options {
	return checks.Options{Repo: "owner/repo", PR: 7, Commit: strings.Repeat("a", 40), Required: []string{"go"}, Dir: "work"}
}

func TestGitHubChecksUsesRunWhenRunnerUnset(t *testing.T) {
	head := strings.Repeat("a", 40)
	r := &recorder{result: func(process.Options) process.Result {
		return process.Result{OK: true, Stdout: `{"headRefOid":"` + head + `","statusCheckRollup":[{"__typename":"CheckRun","name":"go","status":"COMPLETED","conclusion":"SUCCESS"}]}`}
	}}
	s, err := GitHub{Run: r.run}.Checks(context.Background(), checksTarget())
	if err != nil || s.Status != "passed" || s.ObservedHead != head {
		t.Fatalf("snapshot = %+v, err = %v", s, err)
	}
	if len(r.calls) != 1 {
		t.Fatalf("calls = %d", len(r.calls))
	}
	assertGHOptions(t, r.calls[0], []string{"gh", "pr", "view", "7", "--repo", "owner/repo", "--json", "headRefOid,statusCheckRollup"}, "work", 30*time.Second, 1_000_000)
}

func TestGitHubChecksKeepsExplicitRunner(t *testing.T) {
	g := &recorder{}
	own := &recorder{result: func(process.Options) process.Result { return process.Result{OK: true, Stdout: "{not json"} }}
	target := checksTarget()
	target.Runner = own.run
	s, err := GitHub{Run: g.run}.Checks(context.Background(), target)
	if err != nil || s.Status != "unknown" || s.Detail != "Invalid GitHub check response." {
		t.Fatalf("snapshot = %+v, err = %v", s, err)
	}
	if len(g.calls) != 0 || len(own.calls) != 1 {
		t.Fatalf("forge calls = %d, target calls = %d", len(g.calls), len(own.calls))
	}
}

func TestGitHubChecksFailuresRecordNoPassingEvidence(t *testing.T) {
	big := strings.Repeat("x", 600_000)
	for name, result := range map[string]process.Result{
		"gh failure":       {Code: 1, Error: "exit status 1"},
		"timeout":          {OK: true, TimedOut: true},
		"overflow":         {OK: true, Overflow: true},
		"oversized output": {OK: true, Stdout: big, Stderr: big},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{result: func(process.Options) process.Result { return result }}
			s, err := GitHub{Run: r.run}.Checks(context.Background(), checksTarget())
			if err != nil || s.Status != "unknown" || s.Detail != "GitHub check lookup failed. No passing evidence recorded." {
				t.Fatalf("snapshot = %+v, err = %v", s, err)
			}
		})
	}
}

func TestGitHubChecksInvalidTargetMakesNoCall(t *testing.T) {
	r := &recorder{}
	target := checksTarget()
	target.Commit = "short"
	if _, err := (GitHub{Run: r.run}).Checks(context.Background(), target); err == nil || len(r.calls) != 0 {
		t.Fatalf("err = %v, calls = %d", err, len(r.calls))
	}
}

func TestGitHubDescribeIsStatic(t *testing.T) {
	d, err := GitHub{}.Describe(context.Background(), "owner/repo")
	want := Description{Identity: "owner/repo", Host: "github.com", DisplayName: "GitHub", ChangeNoun: "pull request", CIKind: "github-actions"}
	if err != nil || d != want {
		t.Fatalf("describe = %+v, err = %v", d, err)
	}
}

func TestGitHubCommentsUnsupported(t *testing.T) {
	r := &recorder{}
	c, err := GitHub{Run: r.run}.Comments(context.Background(), "owner/repo", "7", "work")
	if !errors.Is(err, ErrUnsupported) || c != nil || len(r.calls) != 0 {
		t.Fatalf("comments = %v, err = %v, calls = %d", c, err, len(r.calls))
	}
}
