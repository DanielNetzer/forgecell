package checks

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

func TestExactCommitEvidence(t *testing.T) {
	sha := strings.Repeat("a", 40)
	success := `{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}`
	cases := []struct{ name, head, entries, want string }{
		{"pass", sha, success, "passed"},
		{"stale", strings.Repeat("b", 40), success, "stale"},
		{"missing", sha, "", "missing"},
		{"pending", sha, `{"__typename":"CheckRun","name":"test","status":"IN_PROGRESS","conclusion":""}`, "pending"},
		{"skip is not proof", sha, `{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SKIPPED"}`, "failed"},
		{"other failing check", sha, success + `,{"__typename":"StatusContext","context":"security","state":"FAILURE"}`, "failed"},
		{"duplicate cannot mask failure", sha, success + `,{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"FAILURE"}`, "failed"},
		{"unknown schema", sha, `{"name":"test"}`, "unknown"},
		{"malformed completion", sha, `{"__typename":"CheckRun","name":"test","status":"BROKEN","conclusion":"SUCCESS"}`, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runner := func(_ context.Context, o process.Options) process.Result {
				if strings.Join(o.Argv, " ") != "gh pr view 9 --repo owner/repo --json headRefOid,statusCheckRollup" {
					t.Fatalf("unexpected argv: %v", o.Argv)
				}
				return process.Result{OK: true, Stdout: fmt.Sprintf(`{"headRefOid":%q,"statusCheckRollup":[%s]}`, c.head, c.entries)}
			}
			got, err := Collect(context.Background(), Options{Repo: "owner/repo", PR: 9, Commit: sha, Required: []string{"test"}, Runner: runner})
			if err != nil || got.Status != c.want {
				t.Fatalf("got %+v %v", got, err)
			}
		})
	}
}
func TestChecksFailClosed(t *testing.T) {
	for _, raw := range []string{"not json", `{"headRefOid":"` + strings.Repeat("a", 40) + `","statusCheckRollup":null}`} {
		r, err := Collect(context.Background(), Options{Repo: "owner/repo", PR: 1, Commit: strings.Repeat("a", 40), Required: []string{"test"}, Runner: func(context.Context, process.Options) process.Result { return process.Result{OK: true, Stdout: raw} }})
		if err != nil || r.Status != "unknown" {
			t.Fatalf("%+v %v", r, err)
		}
	}
	called := false
	_, err := Collect(context.Background(), Options{Repo: "owner/repo", PR: 1, Commit: strings.Repeat("a", 40), Runner: func(context.Context, process.Options) process.Result { called = true; return process.Result{} }})
	if err == nil || called {
		t.Fatal("missing required checks accepted")
	}
}
