package molecule

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func keyedIssueFixture() Issue {
	return Issue{Key: "ENG-1234", Repo: "acme/widgets", Title: "Change code", Body: "Write changed", URL: "https://tracker.example.test/browse/ENG-1234", State: "OPEN"}
}

// keyedFixture is fixture with the numeric reader removed and a keyed one that
// records each call.
func keyedFixture(t *testing.T, issue Issue) (Options, *[]string) {
	t.Helper()
	o, _ := fixture(t)
	o.Issue, o.ReadIssue = "", nil
	var calls []string
	o.ReadIssueKey = func(_ context.Context, repo, key, dir string) (Issue, error) {
		calls = append(calls, repo+" "+key+" "+filepath.Base(dir))
		return issue, nil
	}
	return o, &calls
}

func TestRunKeyedNamesMoleculeAndWorkspaceBySlug(t *testing.T) {
	t.Parallel()
	o, calls := keyedFixture(t, keyedIssueFixture())
	r, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil {
		t.Fatalf("RunKeyed: %v", err)
	}
	if !regexp.MustCompile(`^mol-eng-1234-[0-9a-f]{24}$`).MatchString(r.ID) {
		t.Fatalf("id = %q", r.ID)
	}
	if r.Status != "waiting" || r.CheckoutStatus != "available" {
		t.Fatalf("status = %q checkout = %q notes = %q", r.Status, r.CheckoutStatus, r.Notes)
	}
	if r.Issue.Key != "ENG-1234" || r.Issue.Number != 0 || !r.Issue.IsKeyed() {
		t.Fatalf("issue = %+v", r.Issue)
	}
	name := filepath.Base(r.Workspace.Path)
	if !regexp.MustCompile(`^issue-eng-1234-[0-9a-f]{24}$`).MatchString(name) || r.Workspace.Branch != "forgecell/"+name || r.Workspace.Path != filepath.Join(o.LabDir, "workspaces", name) {
		t.Fatalf("workspace = %+v", r.Workspace)
	}
	if len(*calls) != 1 || (*calls)[0] != "acme/widgets ENG-1234 repo" {
		t.Fatalf("reader calls = %q", *calls)
	}
	if r.Readiness == nil || len(r.Readiness.Plans) != 1 {
		t.Fatalf("readiness = %+v", r.Readiness)
	}
	p := r.Readiness.Plans[0].Plan
	if p.Inputs.Issue.Key != "ENG-1234" || p.Inputs.Issue.Number != 0 || p.MoleculeID != r.ID {
		t.Fatalf("plan issue = %+v", p.Inputs.Issue)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("keyed plan: %v", err)
	}
	if r.RemotePolicy != nil {
		t.Fatalf("keyed run read remote policy")
	}
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "ledgers", r.ID+".json"))
	if err != nil || !strings.Contains(string(raw), `"ENG-1234"`) {
		t.Fatalf("ledger: %v", err)
	}
	// The keyed ledger joins the queue under its key, not under any number.
	q := ReadQueueInventory(o.LabDir)
	if !q.Complete {
		t.Fatalf("inventory incomplete: %q", q.Diagnostics)
	}
	if s := q.SummarizeKey("acme/widgets", "ENG-1234"); s.State != "recorded" || len(s.Attempts) != 1 || s.Attempts[0].ID != r.ID {
		t.Fatalf("summary = %+v", s)
	}
	// A second keyed run links the first as a clarification predecessor.
	second, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil {
		t.Fatalf("second RunKeyed: %v", err)
	}
	if len(second.ClarificationPredecessors) != 1 || second.ClarificationPredecessors[0] != r.ID {
		t.Fatalf("predecessors = %q", second.ClarificationPredecessors)
	}
}

func TestRunKeyedRefusesBeforeAnyLedger(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		key    string
		mutate func(*Options)
		want   string
	}{
		"no usable slug": {key: "###", want: errUnusableKey.Error()},
		"unicode only":   {key: "ÉÜ", want: errUnusableKey.Error()},
		"empty key":      {key: "", want: "issue key is empty"},
		"bidi key":       {key: "ENG-1\xe2\x80\xae", want: "issue key contains a control, format, bidi or line separator character"},
		"no reader":      {key: "ENG-1", mutate: func(o *Options) { o.ReadIssueKey = nil }, want: "a keyed run needs an Issue key reader"},
		"approve":        {key: "ENG-1", mutate: func(o *Options) { o.Approve = strings.Repeat("a", 64) }, want: "approving a keyed Molecule is not supported yet"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o, calls := keyedFixture(t, keyedIssueFixture())
			if c.mutate != nil {
				c.mutate(&o)
			}
			if _, err := RunKeyed(context.Background(), o, c.key); err == nil || err.Error() != c.want {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if len(*calls) != 0 {
				t.Fatalf("reader called: %q", *calls)
			}
			if entries, _ := os.ReadDir(filepath.Join(o.LabDir, "ledgers")); len(entries) != 0 {
				t.Fatalf("ledger written for a refused key: %d", len(entries))
			}
			if entries, _ := os.ReadDir(filepath.Join(o.LabDir, "workspaces")); len(entries) != 0 {
				t.Fatalf("workspace created for a refused key: %d", len(entries))
			}
		})
	}
}

func TestRunKeyedRejectsMismatchedResponses(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Issue){
		"other key":        func(i *Issue) { i.Key = "ENG-1235" },
		"key case differs": func(i *Issue) { i.Key = "eng-1234" },
		"no key":           func(i *Issue) { i.Key = "" },
		"non-zero number":  func(i *Issue) { i.Number = 1234 },
		"other repo":       func(i *Issue) { i.Repo = "acme/other" },
		"closed":           func(i *Issue) { i.State = "CLOSED" },
		"http url":         func(i *Issue) { i.URL = "http://tracker.example.test/browse/ENG-1234" },
		"relative url":     func(i *Issue) { i.URL = "/browse/ENG-1234" },
		"empty url":        func(i *Issue) { i.URL = "" },
		"userinfo url":     func(i *Issue) { i.URL = "https://user:pass@tracker.example.test/browse/ENG-1234" },
		"overlong url":     func(i *Issue) { i.URL = "https://tracker.example.test/" + strings.Repeat("a", 4096) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			issue := keyedIssueFixture()
			mutate(&issue)
			o, _ := keyedFixture(t, issue)
			r, err := RunKeyed(context.Background(), o, "ENG-1234")
			if err != nil {
				t.Fatalf("RunKeyed: %v", err)
			}
			if r.Status != "blocked" || len(r.Notes) == 0 || r.Notes[len(r.Notes)-1] != "Issue is closed or does not match the requested repository and key." {
				t.Fatalf("status = %q notes = %q", r.Status, r.Notes)
			}
			if r.Issue != (Issue{Key: "ENG-1234", Repo: "acme/widgets"}) {
				t.Fatalf("rejected response kept: %+v", r.Issue)
			}
			if r.Workspace.Path != "" {
				t.Fatalf("workspace planned for a rejected response: %+v", r.Workspace)
			}
		})
	}
}

func TestRunNumericRejectsResponseWithKey(t *testing.T) {
	t.Parallel()
	o, _ := fixture(t)
	o.ReadIssue = func(context.Context, string, int64, string) (Issue, error) {
		return Issue{Number: 12, Key: "ENG-12", Repo: "acme/widgets", Title: "Change code", Body: "Write changed", URL: "https://github.com/acme/widgets/issues/12", State: "OPEN"}, nil
	}
	r, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Status != "blocked" || r.Issue != (Issue{Number: 12, Repo: "acme/widgets"}) {
		t.Fatalf("status = %q issue = %+v", r.Status, r.Issue)
	}
}

func TestPlanWorkspaceKey(t *testing.T) {
	lab := resolvedTempDir(t)
	w, err := planWorkspaceKey(Options{LabDir: lab}, Workspace{Repo: "acme/widgets"}, Issue{Key: "ENG-1234"})
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(w.Path)
	if !regexp.MustCompile(`^issue-eng-1234-[0-9a-f]{24}$`).MatchString(name) || w.Branch != "forgecell/"+name || w.Path != filepath.Join(lab, "workspaces", name) {
		t.Fatalf("workspace = %+v", w)
	}
	if _, err := planWorkspaceKey(Options{LabDir: lab}, Workspace{}, Issue{Key: "../.."}); err != errUnusableKey {
		t.Fatalf("err = %v", err)
	}
	// A numeric Issue still plans issue-<n>-<nonce>.
	w, err = planIssueWorkspace(Options{LabDir: lab}, Workspace{}, Issue{Number: 12})
	if err != nil || !regexp.MustCompile(`^issue-12-[0-9a-f]{24}$`).MatchString(filepath.Base(w.Path)) {
		t.Fatalf("numeric workspace = %+v, %v", w, err)
	}
}
