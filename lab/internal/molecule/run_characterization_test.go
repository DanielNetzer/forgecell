package molecule

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

const moleculeCharacterizationRejected = "Issue is closed or does not match the requested repository and number."

// moleculeCharacterizationGH puts a fake gh first on PATH. It logs its working
// directory and each argument on its own line, marks non-empty stdin, then prints
// out to stdout, errText to stderr and exits with code.
func moleculeCharacterizationGH(t *testing.T, out, errText, code string) func() []string {
	t.Helper()
	dir := resolvedTempDir(t)
	for name, body := range map[string]string{"out": out, "err": errText, "code": code} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
dir='` + dir + `'
printf 'cwd=%s\n' "$(pwd -P)" >> "$dir/calls.log"
for arg; do printf '%s\n' "$arg" >> "$dir/calls.log"; done
if [ -n "$(cat)" ]; then echo '<stdin>' >> "$dir/calls.log"; fi
cat "$dir/out"
cat "$dir/err" >&2
exit "$(cat "$dir/code")"
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(dir, "calls.log"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadIssueArgvAndDecoding(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "characterization", "issue-view.json"))
	if err != nil {
		t.Fatal(err)
	}
	calls := moleculeCharacterizationGH(t, string(fixture), "", "0")
	cwd := resolvedTempDir(t)
	issue, err := readIssue(context.Background(), "acme/widgets", 12, cwd)
	if err != nil {
		t.Fatalf("readIssue: %v", err)
	}
	want := []string{"cwd=" + cwd, "issue", "view", "12", "--repo", "acme/widgets", "--json", "number,title,body,state,url,updatedAt"}
	if got := calls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", got, want)
	}
	// Repo is always the requested repository, never the response's.
	wantIssue := Issue{UpdatedAt: "2026-10-30T10:47:34Z", Number: 12, Repo: "acme/widgets", Title: "Change code", Body: "Write changed\n\n- [ ] keep `markdown`", URL: "https://github.com/acme/widgets/issues/12", State: "OPEN"}
	if issue != wantIssue {
		t.Fatalf("issue =\n%+v\nwant\n%+v", issue, wantIssue)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationReadIssueFailures(t *testing.T) {
	cases := []struct {
		name, out, errText, code, want string
	}{
		{name: "non-zero exit", errText: "HTTP 404: Not Found", code: "1", want: "GitHub intake failed: exit status 1 HTTP 404: Not Found"},
		{name: "malformed json", out: "{", code: "0", want: "unexpected end of JSON input"},
		{name: "output over the default cap", out: strings.Repeat(" ", 1_000_001), code: "0", want: "GitHub intake failed: command output exceeded the configured byte limit "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			moleculeCharacterizationGH(t, c.out, c.errText, c.code)
			issue, err := readIssue(context.Background(), "acme/widgets", 12, resolvedTempDir(t))
			if err == nil || err.Error() != c.want {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if issue != (Issue{}) {
				t.Fatalf("issue = %+v", issue)
			}
		})
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationWorkspaceNameAndBranch(t *testing.T) {
	lab := resolvedTempDir(t)
	w, err := planWorkspace(Options{LabDir: lab}, Workspace{Repo: "acme/widgets"}, 12)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(w.Path)
	if !regexp.MustCompile(`^issue-12-[0-9a-f]{24}$`).MatchString(name) || w.Branch != "forgecell/"+name || w.Path != filepath.Join(lab, "workspaces", name) || w.Repo != "acme/widgets" {
		t.Fatalf("workspace = %+v", w)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationRunIssueRefsAndMoleculeID(t *testing.T) {
	t.Parallel()
	id := regexp.MustCompile(`^mol-12-[0-9a-f]{24}$`)
	for _, ref := range []string{"12", "#12", "acme/widgets#12", "ACME/Widgets#12", "https://github.com/acme/widgets/issues/12", "https://github.com/acme/widgets/issues/12#issuecomment-1"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			o, _ := fixture(t)
			o.Issue = ref
			type call struct {
				repo   string
				number int64
				dir    string
			}
			var got []call
			o.ReadIssue = func(_ context.Context, repo string, number int64, dir string) (Issue, error) {
				got = append(got, call{repo, number, dir})
				return Issue{}, errors.New("reader stopped")
			}
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !id.MatchString(r.ID) || r.Status != "blocked" || r.Issue != (Issue{Number: 12, Repo: "acme/widgets"}) {
				t.Fatalf("record id=%q status=%q issue=%+v", r.ID, r.Status, r.Issue)
			}
			if len(got) != 1 || got[0].repo != "acme/widgets" || got[0].number != 12 || got[0].dir != o.SourceRoot {
				t.Fatalf("reader calls = %+v", got)
			}
			if _, err := os.Lstat(filepath.Join(o.LabDir, "ledgers", r.ID+".json")); err != nil {
				t.Fatalf("ledger: %v", err)
			}
			if len(r.Notes) == 0 || r.Notes[len(r.Notes)-1] != "reader stopped" {
				t.Fatalf("notes = %q", r.Notes)
			}
		})
	}
	refusals := map[string]string{
		"other/repo#12": "issue repository does not match the approved Formula",
		"https://github.com/other/repo/issues/12": "issue repository does not match the approved Formula",
		"0":     "issue number must be a positive safe integer",
		"12abc": "invalid issue reference: use 123, #123, owner/repo#123, or a GitHub issue URL",
		"https://github.com/acme/widgets/pull/12": "invalid issue reference: use 123, #123, owner/repo#123, or a GitHub issue URL",
	}
	for ref, want := range refusals {
		t.Run("refuse "+ref, func(t *testing.T) {
			t.Parallel()
			o, _ := fixture(t)
			o.Issue = ref
			o.ReadIssue = func(context.Context, string, int64, string) (Issue, error) {
				t.Fatal("reader called for a refused reference")
				return Issue{}, nil
			}
			if _, err := Run(context.Background(), o); err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if entries, _ := os.ReadDir(filepath.Join(o.LabDir, "ledgers")); len(entries) != 0 {
				t.Fatalf("ledger written for a refused reference: %d", len(entries))
			}
		})
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationRunIssueResponseURLChecks(t *testing.T) {
	t.Parallel()
	good := Issue{Number: 12, Repo: "acme/widgets", Title: "Change code", Body: "Write changed", URL: "https://github.com/acme/widgets/issues/12", State: "OPEN"}
	cases := map[string]func(*Issue){
		"other repository url": func(i *Issue) { i.URL = "https://github.com/other/widgets/issues/12" },
		"other number url":     func(i *Issue) { i.URL = "https://github.com/acme/widgets/issues/13" },
		"pull url":             func(i *Issue) { i.URL = "https://github.com/acme/widgets/pull/12" },
		"http url":             func(i *Issue) { i.URL = "http://github.com/acme/widgets/issues/12" },
		"trailing slash":       func(i *Issue) { i.URL = "https://github.com/acme/widgets/issues/12/" },
		"case differs":         func(i *Issue) { i.URL = "https://github.com/Acme/widgets/issues/12" },
		"enterprise host":      func(i *Issue) { i.URL = "https://github.example.com/acme/widgets/issues/12" },
		"other number":         func(i *Issue) { i.Number = 13 },
		"other repo":           func(i *Issue) { i.Repo = "acme/other" },
		"closed":               func(i *Issue) { i.State = "CLOSED" },
		"lower-case state":     func(i *Issue) { i.State = "open" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o, _ := fixture(t)
			issue := good
			mutate(&issue)
			o.ReadIssue = func(context.Context, string, int64, string) (Issue, error) { return issue, nil }
			r, err := Run(context.Background(), o)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if r.Status != "blocked" || len(r.Notes) == 0 || r.Notes[len(r.Notes)-1] != moleculeCharacterizationRejected {
				t.Fatalf("status = %q notes = %q", r.Status, r.Notes)
			}
			if r.Issue != (Issue{Number: 12, Repo: "acme/widgets"}) {
				t.Fatalf("rejected response kept: %+v", r.Issue)
			}
		})
	}
}
