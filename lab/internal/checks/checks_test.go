package checks

import (
	"context"
	"encoding/json"
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

func TestCollectionIdentityAndBounds(t *testing.T) {
	for _, response := range []process.Result{{OK: true, Overflow: true}, {OK: true, TimedOut: true}, {OK: true, Stdout: strings.Repeat("x", 1_000_001)}} {
		s, err := Collect(context.Background(), Options{Repo: "owner/repo", PR: 9, Commit: strings.Repeat("a", 40), Required: []string{"test", "test"}, Runner: func(context.Context, process.Options) process.Result { return response }})
		raw, _ := json.Marshal(s)
		if err != nil || s.Status != "unknown" || !strings.Contains(string(raw), `"repo":"owner/repo"`) || !strings.Contains(string(raw), `"pr":9`) || len(s.Required) != 1 {
			t.Fatalf("%s %v", raw, err)
		}
	}
}

func TestIncompleteAndUnknownSuitesRetainEvidence(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tc := range []struct {
		entries, status string
		count           int
	}{
		{`{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"CheckRun","name":"suite","status":"QUEUED"}`, "pending", 2},
		{`{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"CheckRun","name":"future","status":"FUTURE","conclusion":"CUSTOM"}`, "unknown", 2},
		{`{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"},{"__typename":"FutureCheck","name":"future","status":"CUSTOM","conclusion":"CUSTOM"}`, "unknown", 2},
	} {
		s, err := Collect(context.Background(), Options{Repo: "owner/repo", PR: 9, Commit: sha, Required: []string{"test"}, Runner: func(context.Context, process.Options) process.Result {
			return process.Result{OK: true, Stdout: fmt.Sprintf(`{"headRefOid":%q,"statusCheckRollup":[%s]}`, sha, tc.entries)}
		}})
		if err != nil || s.Status != tc.status || len(s.Checks) != tc.count {
			t.Fatalf("%+v %v", s, err)
		}
	}
}

func TestOversizedPartialObservationRemainsPersistableUnknown(t *testing.T) {
	head := strings.Repeat("a", 40)
	entries := []string{}
	for i := 0; i < 140; i++ {
		entries = append(entries, fmt.Sprintf(`{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":%q}`, strings.Repeat("x", 2000)))
	}
	entries = append(entries, `{"__typename":"CheckRun","status":"COMPLETED","conclusion":"SUCCESS"}`)
	s, err := Collect(context.Background(), Options{Repo: "owner/repo", PR: 9, Commit: head, Required: []string{"test"}, Runner: func(context.Context, process.Options) process.Result {
		return process.Result{OK: true, Stdout: fmt.Sprintf(`{"headRefOid":%q,"statusCheckRollup":[%s]}`, head, strings.Join(entries, ","))}
	}})
	if err != nil || s.Status != "unknown" {
		t.Fatalf("%+v %v", s, err)
	}
	if err := Validate(s); err != nil {
		t.Fatalf("valid collection could not be persisted: %v", err)
	}
}

func TestValidateRejectsUncertainPassingEvidence(t *testing.T) {
	s := Snapshot{Repo: "owner/repo", PR: 9, Commit: strings.Repeat("a", 40), ObservedHead: strings.Repeat("a", 40), CheckedAt: "2026-10-10T00:00:00Z", Required: []string{"test"}, Status: "passed", Checks: []Check{{Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"}}}
	for _, tc := range []struct{ name, repo, detail string }{
		{"collection uncertainty", "owner/repo", "Unrecognized check data."},
		{"dot repository", "owner/.", ""},
		{"parent repository", "owner/..", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := s
			current.Repo, current.Detail = tc.repo, tc.detail
			if err := Validate(current); err == nil {
				t.Fatal("invalid passing evidence accepted")
			}
		})
	}
}
