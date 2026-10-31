package delivery

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/checks"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

// publishedPluginFixture is a delivery published through the fake plugin forge.
func publishedPluginFixture(t *testing.T) (*pluginPublishFixture, Receipt) {
	t.Helper()
	fx := newPluginPublishFixture(t)
	r, err := fx.publish()
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	fx.order = nil
	return fx, r
}

func (fx *pluginPublishFixture) ciTarget(r Receipt, required ...string) PluginCITarget {
	return PluginCITarget{Project: pluginTestProject, Ref: r.Ref, Commit: r.Commit, Required: required, Changes: fx.fake, Calls: fx.calls}
}

func TestCollectCIPluginRecordsAPluginObservation(t *testing.T) {
	fx, r := publishedPluginFixture(t)
	fx.fake.checks = func(string) (contract.ForgeChecks, error) {
		url := "https://forge.example/acme/widgets/-/jobs/1"
		return contract.ForgeChecks{Checks: []contract.ForgeCheck{{Name: "build", Status: "completed", Conclusion: "success", URL: &url}}, Complete: true}, nil
	}
	a, err := CollectCIPlugin(context.Background(), fx.o, fx.ciTarget(r, "build"))
	if err != nil {
		t.Fatalf("CollectCIPlugin: %v", err)
	}
	want := CIIdentity{MoleculeID: "mol-fixture", PreviewDigest: r.Preview.Digest, Repo: pluginTestProject, PR: 0, URL: pluginTestURL, Commit: r.Commit, Ref: pluginTestRef}
	if a.Identity != want {
		t.Fatalf("identity = %+v, want %+v", a.Identity, want)
	}
	s := a.Snapshot
	if s.Status != "passed" || s.Repo != pluginTestProject || s.PR != 0 || s.ObservedHead != r.Commit || !reflect.DeepEqual(s.Checks, []checks.Check{{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS", URL: "https://forge.example/acme/widgets/-/jobs/1"}}) {
		t.Fatalf("snapshot = %+v", s)
	}
	if got := pluginTools(a.PluginCalls); !reflect.DeepEqual(got, []string{"forge_change", "forge_checks"}) || !reflect.DeepEqual(fx.order, got) {
		t.Fatalf("calls = %q, order %q", got, fx.order)
	}
	history, err := ReadCI(fx.o)
	if err != nil || len(history) != 1 || !reflect.DeepEqual(history[0], a) || CIClassification(history) != "passed" {
		t.Fatalf("ReadCI = %+v, %v", history, err)
	}
	// A second observation links to the first.
	fx.fake.checks = func(string) (contract.ForgeChecks, error) {
		return contract.ForgeChecks{Checks: []contract.ForgeCheck{{Name: "build", Status: "completed", Conclusion: "failure"}}, Complete: true}, nil
	}
	b, err := CollectCIPlugin(context.Background(), fx.o, fx.ciTarget(r, "build"))
	if err != nil || b.Sequence != 2 || b.Previous != a.SHA256 || b.Snapshot.Status != "failed" {
		t.Fatalf("second = %+v, %v", b, err)
	}
	if history, err = ReadCI(fx.o); err != nil || CIClassification(history) != "failure" {
		t.Fatalf("classification after failure: %v", err)
	}
}

func TestCollectCIPluginRefusesAnotherTargetBeforeLookup(t *testing.T) {
	fx, r := publishedPluginFixture(t)
	for name, target := range map[string]PluginCITarget{
		"ref":     {Project: pluginTestProject, Ref: "!8", Commit: r.Commit},
		"project": {Project: "acme/other", Ref: r.Ref, Commit: r.Commit},
		"commit":  {Project: pluginTestProject, Ref: r.Ref, Commit: strings.Repeat("f", 40)},
	} {
		target.Required, target.Changes = []string{"build"}, fx.fake
		if _, err := CollectCIPlugin(context.Background(), fx.o, target); err == nil || err.Error() != "CI target differs from delivery" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(fx.order) != 0 {
		t.Fatalf("plugin calls before linkage passed: %q", fx.order)
	}
	if _, err := CollectCIPlugin(context.Background(), fx.o, PluginCITarget{Project: pluginTestProject, Ref: r.Ref, Commit: r.Commit, Required: []string{"build"}}); err == nil {
		t.Fatal("a target without a forge was accepted")
	}
	// The GitHub collection refuses a plugin delivery: its identity has no PR.
	if err := ValidateCITarget(fx.o, "owner/repo", 7, r.Commit); err == nil {
		t.Fatal("GitHub target accepted for a plugin delivery")
	}
	// Even a GitHub target that names the plugin identity's own repo, PR 0 and
	// commit is refused, before any lookup.
	if err := ValidateCITarget(fx.o, pluginTestProject, 0, r.Commit); err == nil || !strings.Contains(err.Error(), "forge plugin") {
		t.Fatalf("GitHub target with the plugin identity: %v", err)
	}
	called := false
	_, err := CollectCI(context.Background(), fx.o, checks.Options{Repo: pluginTestProject, PR: 0, Commit: r.Commit, Required: []string{"build"}, Runner: func(context.Context, process.Options) process.Result {
		called = true
		return process.Result{OK: true}
	}})
	if err == nil || called {
		t.Fatalf("GitHub collection of a plugin delivery: %v, called %v", err, called)
	}
}

// githubStatus is the status checks.Collect gives the same checks on GitHub.
func githubStatus(t *testing.T, head, commit string, required []string, in []contract.ForgeCheck) string {
	t.Helper()
	entries := []map[string]string{}
	for _, c := range in {
		entries = append(entries, map[string]string{"__typename": "CheckRun", "name": c.Name, "status": pluginCheckStatus[c.Status], "conclusion": pluginCheckConclusion[c.Conclusion]})
	}
	raw, _ := json.Marshal(map[string]any{"headRefOid": head, "statusCheckRollup": entries})
	s, err := checks.Collect(context.Background(), checks.Options{Repo: "owner/repo", PR: 1, Commit: commit, Required: required, Runner: func(context.Context, process.Options) process.Result {
		return process.Result{OK: true, Stdout: string(raw)}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s.Status
}

func TestPluginCIEvaluationMatchesGitHub(t *testing.T) {
	commit, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	check := func(name, status, conclusion string) contract.ForgeCheck {
		return contract.ForgeCheck{Name: name, Status: status, Conclusion: conclusion}
	}
	cases := []struct {
		name     string
		head     string
		required []string
		checks   []contract.ForgeCheck
		want     string
	}{
		{"passed", commit, []string{"build"}, []contract.ForgeCheck{check("build", "completed", "success")}, "passed"},
		{"failed", commit, []string{"build"}, []contract.ForgeCheck{check("build", "completed", "failure")}, "failed"},
		{"neutral is not success", commit, []string{"build"}, []contract.ForgeCheck{check("build", "completed", "neutral")}, "failed"},
		{"skipped is not success", commit, []string{"build"}, []contract.ForgeCheck{check("build", "completed", "skipped")}, "failed"},
		{"an unrequired failure fails", commit, []string{"build"}, []contract.ForgeCheck{check("build", "completed", "success"), check("lint", "completed", "timed_out")}, "failed"},
		{"pending", commit, []string{"build"}, []contract.ForgeCheck{check("build", "in_progress", "")}, "pending"},
		{"queued", commit, []string{"build"}, []contract.ForgeCheck{check("build", "queued", "")}, "pending"},
		{"missing", commit, []string{"build"}, []contract.ForgeCheck{check("lint", "completed", "success")}, "missing"},
		{"missing beats failed", commit, []string{"build"}, []contract.ForgeCheck{check("lint", "completed", "failure")}, "missing"},
		{"stale", other, []string{"build"}, []contract.ForgeCheck{check("build", "completed", "success")}, "stale"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			head := c.head
			order := []string{}
			fake := &fakeChanges{order: &order, pushed: &head, checks: func(string) (contract.ForgeChecks, error) {
				return contract.ForgeChecks{Checks: c.checks, Complete: true}, nil
			}, change: func(h string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: "open", Draft: true, HeadCommit: h, URL: pluginTestURL}, nil
			}}
			s, err := pluginSnapshot(context.Background(), PluginCITarget{Project: pluginTestProject, Ref: pluginTestRef, Commit: commit, Required: c.required, Changes: fake})
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != c.want || s.Detail != "" {
				t.Fatalf("plugin status = %q (%s), want %q", s.Status, s.Detail, c.want)
			}
			if gh := githubStatus(t, head, commit, c.required, c.checks); gh != s.Status {
				t.Fatalf("plugin status %q differs from GitHub's %q", s.Status, gh)
			}
			id := CIIdentity{Repo: pluginTestProject, Commit: commit, Ref: pluginTestRef}
			if err := validatePluginSnapshot(id, s); err != nil {
				t.Fatalf("shared validation refused the snapshot: %v", err)
			}
			// A falsely passing snapshot fails the shared validation.
			if c.want != "passed" {
				forged := s
				forged.Status = "passed"
				if validatePluginSnapshot(id, forged) == nil {
					t.Fatal("a forged pass was accepted")
				}
			}
		})
	}
}

func TestPluginCIUncertaintyIsUnknown(t *testing.T) {
	commit := strings.Repeat("a", 40)
	success := []contract.ForgeCheck{{Name: "build", Status: "completed", Conclusion: "success"}}
	cases := []struct {
		name   string
		change error
		checks func() (contract.ForgeChecks, error)
		want   string
		detail string
	}{
		{"incomplete pass", nil, func() (contract.ForgeChecks, error) {
			return contract.ForgeChecks{Checks: success, Complete: false}, nil
		}, "unknown", "more checks may still be added"},
		{"incomplete missing", nil, func() (contract.ForgeChecks, error) {
			return contract.ForgeChecks{Checks: []contract.ForgeCheck{}, Complete: false}, nil
		}, "unknown", "more checks may still be added"},
		{"incomplete failure stays failed", nil, func() (contract.ForgeChecks, error) {
			return contract.ForgeChecks{Checks: []contract.ForgeCheck{{Name: "build", Status: "completed", Conclusion: "failure"}}, Complete: false}, nil
		}, "failed", ""},
		{"checks lookup failed", nil, func() (contract.ForgeChecks, error) {
			return contract.ForgeChecks{}, mcp.Fail(mcp.AuthMissing, "forge_checks", "Bearer secret")
		}, "unknown", "Forge plugin check lookup failed (auth-missing)"},
		{"change lookup failed", mcp.Fail(mcp.RateLimited, "forge_change", ""), nil, "unknown", "Forge plugin change lookup failed (rate-limited)"},
		{"null checks", nil, func() (contract.ForgeChecks, error) { return contract.ForgeChecks{Complete: true}, nil }, "unknown", "Check data unavailable."},
		{"completed without conclusion", nil, func() (contract.ForgeChecks, error) {
			return contract.ForgeChecks{Checks: []contract.ForgeCheck{{Name: "build", Status: "completed"}}, Complete: true}, nil
		}, "unknown", "Completed check without a conclusion."},
		{"unknown status", nil, func() (contract.ForgeChecks, error) {
			return contract.ForgeChecks{Checks: []contract.ForgeCheck{{Name: "build", Status: "waiting", Conclusion: "success"}}, Complete: true}, nil
		}, "unknown", "Unrecognized check status."},
		{"too many checks", nil, func() (contract.ForgeChecks, error) {
			many := make([]contract.ForgeCheck, checks.MaxChecks+1)
			for i := range many {
				many[i] = success[0]
			}
			return contract.ForgeChecks{Checks: many, Complete: true}, nil
		}, "unknown", "Check count exceeds bound."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			head := commit
			order := []string{}
			fake := &fakeChanges{order: &order, pushed: &head, change: func(h string) (contract.ForgeChange, error) {
				if c.change != nil {
					return contract.ForgeChange{}, c.change
				}
				return contract.ForgeChange{State: "open", Draft: true, HeadCommit: h, URL: pluginTestURL}, nil
			}}
			if c.checks != nil {
				fake.checks = func(string) (contract.ForgeChecks, error) { return c.checks() }
			}
			s, err := pluginSnapshot(context.Background(), PluginCITarget{Project: pluginTestProject, Ref: pluginTestRef, Commit: commit, Required: []string{"build"}, Changes: fake})
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != c.want || !strings.Contains(s.Detail, c.detail) || strings.Contains(s.Detail, "secret") {
				t.Fatalf("snapshot status %q detail %q, want %q containing %q", s.Status, s.Detail, c.want, c.detail)
			}
			if err := validatePluginSnapshot(CIIdentity{Repo: pluginTestProject, Commit: commit, Ref: pluginTestRef}, s); err != nil {
				t.Fatalf("validation: %v", err)
			}
		})
	}
	// Invalid required names are refused, as on GitHub.
	for _, required := range [][]string{nil, {" "}} {
		if _, err := pluginSnapshot(context.Background(), PluginCITarget{Project: pluginTestProject, Ref: pluginTestRef, Commit: commit, Required: required, Changes: &fakeChanges{}}); err == nil {
			t.Fatalf("required %q accepted", required)
		}
	}
}

func TestPluginPublishedEvidenceMatching(t *testing.T) {
	fx, r := publishedPluginFixture(t)
	record, err := molecule.ReadRecord(fx.o.LabDir, fx.o.MoleculeID)
	if err != nil {
		t.Fatal(err)
	}
	if !MatchesPluginPublishedEvidence(r, record) {
		t.Fatal("the published plugin receipt does not match")
	}
	if MatchesPublishedEvidence(r, record) {
		t.Fatal("a plugin receipt matched the GitHub predicate")
	}
	for name, mutate := range map[string]func(*Receipt){
		"foreign url":  func(r *Receipt) { r.URL = "https://other.example/acme/widgets/-/merge_requests/7" },
		"no ref":       func(r *Receipt) { r.Ref = "" },
		"short commit": func(r *Receipt) { r.Commit = r.Commit[:39] },
		"committed":    func(r *Receipt) { r.State = "committed" },
		"no forge":     func(r *Receipt) { r.Forge = "" },
	} {
		changed := r
		mutate(&changed)
		if MatchesPluginPublishedEvidence(changed, record) {
			t.Fatalf("%s: matched", name)
		}
	}
	if noun, err := ReadPluginChangeNoun(fx.o); err != nil || noun != "merge request" {
		t.Fatalf("ReadPluginChangeNoun = %q, %v", noun, err)
	}
}

func TestGitHubCIObservationBytesUnchanged(t *testing.T) {
	raw, err := json.Marshal(CIObservation{SchemaVersion: "v1", Identity: CIIdentity{Repo: "owner/repo", PR: 9}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"ref"`, `"pluginCalls"`, `"pluginCallsDropped"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("a GitHub observation writes %s: %s", key, raw)
		}
	}
	raw, err = json.Marshal(Receipt{State: "published", URL: "https://github.com/owner/repo/pull/9"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"forge"`, `"project"`, `"forgeHost"`, `"changeNoun"`, `"ref"`, `"pluginCalls"`, `"pluginCallsDropped"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("a GitHub receipt writes %s: %s", key, raw)
		}
	}
}
