package delivery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

const (
	publishCharacterizationFields = "url,state,baseRefName,headRefName,headRefOid,isDraft,headRepository,headRepositoryOwner"
	publishCharacterizationURL    = "https://github.com/owner/repo/pull/10"
	publishCharacterizationBody   = "<body-file>"
)

// publishCharacterizationArgv is every remote call of a first publication, in order.
func publishCharacterizationArgv(commit string) [][]string {
	return [][]string{
		{"git", "fetch", "--no-tags", "origin", "+refs/heads/main:refs/remotes/origin/main"},
		{"gh", "api", "--method", "GET", "repos/owner/repo/rules/branches/main?per_page=100"},
		{"gh", "api", "--method", "GET", "repos/owner/repo/branches/main/protection"},
		{"git", "push", "origin", commit + ":refs/heads/forgecell/fixture"},
		{"gh", "pr", "list", "--repo", "owner/repo", "--head", "forgecell/fixture", "--state", "all", "--limit", "2", "--json", publishCharacterizationFields},
		{"gh", "pr", "create", "--draft", "--repo", "owner/repo", "--base", "main", "--head", "forgecell/fixture", "--title", "Fixture", "--body-file", publishCharacterizationBody},
		{"gh", "pr", "view", publishCharacterizationURL, "--repo", "owner/repo", "--json", publishCharacterizationFields},
	}
}

// publishCharacterizationPR is the fixture PR observation with optional field changes.
func publishCharacterizationPR(t *testing.T, o Options, mutate func(map[string]any)) string {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(fixtureLifecycle(t, o, "OPEN").Stdout), &fields); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(fields)
	}
	raw, _ := json.Marshal(fields)
	return string(raw)
}

type publishCharacterizationRun struct {
	receipt  Receipt
	err      error
	argv     [][]string
	options  []process.Options
	body     string
	bodyPath string
}

// publishCharacterizationPublish runs a first publication with default remote
// answers; answer may override any call by returning ok.
func publishCharacterizationPublish(t *testing.T, answer func(o Options, argv []string) (process.Result, bool)) (Options, publishCharacterizationRun) {
	t.Helper()
	o := fixture(t)
	p, err := Preview(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var run publishCharacterizationRun
	runner := func(_ context.Context, c process.Options) process.Result {
		argv := append([]string(nil), c.Argv...)
		if len(argv) > 2 && argv[0] == "gh" && argv[2] == "create" {
			run.bodyPath = argv[len(argv)-1]
			raw, err := os.ReadFile(run.bodyPath)
			if err != nil {
				t.Fatalf("body file unreadable during create: %v", err)
			}
			run.body = string(raw)
			argv[len(argv)-1] = publishCharacterizationBody
		}
		run.argv = append(run.argv, argv)
		run.options = append(run.options, c)
		if answer != nil {
			if result, ok := answer(o, c.Argv); ok {
				return result
			}
		}
		switch {
		case argv[0] == "git":
			return process.Result{OK: true}
		case argv[1] == "api" && strings.Contains(argv[4], "/rules/"):
			return process.Result{OK: true, Stdout: "[]"}
		case argv[1] == "api":
			return process.Result{OK: true, Stdout: "{}"}
		case argv[2] == "list":
			return process.Result{OK: true, Stdout: "[]"}
		case argv[2] == "create":
			return process.Result{OK: true, Stdout: publishCharacterizationURL + "\n"}
		case argv[2] == "view":
			return fixtureLifecycle(t, o, "OPEN")
		}
		t.Fatalf("unexpected remote call %q", c.Argv)
		return process.Result{}
	}
	run.receipt, run.err = Publish(context.Background(), PublishOptions{Options: o, Approve: p.Digest, Title: "Fixture", Runner: runner})
	return o, run
}

func publishCharacterizationPersisted(t *testing.T, o Options) Receipt {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Receipt
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationPublishArgvSequenceAndReceipt(t *testing.T) {
	o, run := publishCharacterizationPublish(t, nil)
	if run.err != nil {
		t.Fatalf("Publish: %v", run.err)
	}
	r := run.receipt
	if len(r.Commit) != 40 {
		t.Fatalf("commit = %q", r.Commit)
	}
	if want := publishCharacterizationArgv(r.Commit); !reflect.DeepEqual(run.argv, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", run.argv, want)
	}
	workspace := filepath.Join(o.LabDir, "workspaces", "fixture")
	for i, c := range run.options {
		timeout, limit := 60*time.Second, 1_000_000
		if c.Argv[0] == "gh" && c.Argv[1] == "api" {
			timeout, limit = 15*time.Second, 256000
		}
		if c.Dir != workspace || c.Timeout != timeout || c.MaxOutputBytes != limit || c.Stdin == nil || len(c.Stdin) != 0 || c.Input != nil {
			t.Fatalf("call %d options = %+v", i, c)
		}
	}
	if !filepath.IsAbs(run.bodyPath) || filepath.Dir(run.bodyPath) != filepath.Join(o.LabDir, "deliveries") || !strings.HasPrefix(filepath.Base(run.bodyPath), ".pr-body-") {
		t.Fatalf("body file = %q", run.bodyPath)
	}
	if _, err := os.Lstat(run.bodyPath); !os.IsNotExist(err) {
		t.Fatalf("body file kept after publication: %v", err)
	}
	wantBody := "Implements issue #9.\n\nMolecule: `mol-fixture`\nFormula: `delivery-fixture`\nBase: `" + r.Preview.BaseCommit + "`\nDelivery: `" + r.Commit + "`\n\nAwaiting exact-commit CI and human review. Harness-reported checks are not independent CI evidence. No merge, deployment or issue closure is authorized.\n"
	if run.body != wantBody {
		t.Fatalf("body =\n%s\nwant\n%s", run.body, wantBody)
	}
	if r.State != "published" || r.URL != publishCharacterizationURL || !r.DraftAtPublication || r.Title != "Fixture" || r.AcceptanceReviewAcknowledged {
		t.Fatalf("receipt = %+v", r)
	}
	if _, err := time.Parse(time.RFC3339Nano, r.CreationAttemptedAt); err != nil {
		t.Fatalf("creationAttemptedAt %q: %v", r.CreationAttemptedAt, err)
	}
	if r.Lifecycle == nil || r.Lifecycle.State != "OPEN" || r.Lifecycle.Reconciliation != "" {
		t.Fatalf("lifecycle = %+v", r.Lifecycle)
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Lifecycle.ObservedAt); err != nil {
		t.Fatalf("observedAt %q: %v", r.Lifecycle.ObservedAt, err)
	}
	if r.Policy == nil || r.Policy.Rules.Status != "observed" || string(r.Policy.Rules.Content) != "[]" || r.Policy.Rules.Source != "https://api.github.com/repos/owner/repo/rules/branches/main?per_page=100" ||
		r.Policy.Protection.Status != "observed" || string(r.Policy.Protection.Content) != "{}" || r.Policy.Protection.Source != "https://api.github.com/repos/owner/repo/branches/main/protection" {
		t.Fatalf("policy = %+v", r.Policy)
	}
	if persisted := publishCharacterizationPersisted(t, o); !reflect.DeepEqual(persisted, r) {
		t.Fatalf("persisted receipt differs:\n%+v\n%+v", persisted, r)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationPublishExistingDraftSkipsCreate(t *testing.T) {
	o, run := publishCharacterizationPublish(t, func(o Options, argv []string) (process.Result, bool) {
		if argv[0] == "gh" && argv[2] == "list" {
			return process.Result{OK: true, Stdout: "[" + fixtureLifecycle(t, o, "OPEN").Stdout + "]"}, true
		}
		return process.Result{}, false
	})
	if run.err != nil {
		t.Fatalf("Publish: %v", run.err)
	}
	all := publishCharacterizationArgv(run.receipt.Commit)
	want := append(append([][]string{}, all[:5]...), all[6])
	if !reflect.DeepEqual(run.argv, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", run.argv, want)
	}
	r := run.receipt
	if r.State != "published" || r.URL != publishCharacterizationURL || !r.DraftAtPublication || r.CreationAttemptedAt != "" || r.Lifecycle == nil || r.Lifecycle.State != "OPEN" {
		t.Fatalf("receipt = %+v", r)
	}
	if persisted := publishCharacterizationPersisted(t, o); !reflect.DeepEqual(persisted, r) {
		t.Fatal("receipt not persisted")
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationPublishReconciliationStates(t *testing.T) {
	state := func(s string) func(map[string]any) { return func(f map[string]any) { f["state"] = s } }
	cases := []struct {
		name      string
		verb      string // the gh pr call to override
		result    func(o Options) process.Result
		calls     int
		lifecycle string
		reason    string
		created   bool
	}{
		{name: "list non-zero exit", verb: "list", result: func(Options) process.Result {
			return process.Result{Code: 1, Error: "exit status 1", Stderr: "HTTP 502"}
		}, calls: 5, lifecycle: "UNKNOWN", reason: "gh failed or uncertain: exit status 1 HTTP 502"},
		{name: "list overflow", verb: "list", result: func(Options) process.Result {
			return process.Result{OK: true, Overflow: true, Stdout: "[]"}
		}, calls: 5, lifecycle: "UNKNOWN", reason: "gh failed or uncertain:  "},
		{name: "list malformed json", verb: "list", result: func(Options) process.Result {
			return process.Result{OK: true, Stdout: "[{"}
		}, calls: 5, lifecycle: "UNKNOWN", reason: "malformed PR list"},
		{name: "list null", verb: "list", result: func(Options) process.Result {
			return process.Result{OK: true, Stdout: "null"}
		}, calls: 5, lifecycle: "UNKNOWN", reason: "malformed PR list"},
		{name: "list ambiguous", verb: "list", result: func(o Options) process.Result {
			item := publishCharacterizationPR(t, o, nil)
			return process.Result{OK: true, Stdout: "[" + item + "," + item + "]"}
		}, calls: 5, lifecycle: "AMBIGUOUS", reason: "multiple PRs found"},
		{name: "list unexpected url", verb: "list", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: "[" + publishCharacterizationPR(t, o, func(f map[string]any) { f["url"] = "https://github.com/other/repo/pull/10" }) + "]"}
		}, calls: 5, lifecycle: "UNKNOWN", reason: "unexpected PR URL"},
		{name: "list closed", verb: "list", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: "[" + publishCharacterizationPR(t, o, state("CLOSED")) + "]"}
		}, calls: 5, lifecycle: "CLOSED", reason: "listed PR is not confirmed OPEN"},
		{name: "list merged", verb: "list", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: "[" + publishCharacterizationPR(t, o, state("MERGED")) + "]"}
		}, calls: 5, lifecycle: "MERGED", reason: "listed PR is not confirmed OPEN"},
		{name: "list unknown state", verb: "list", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: "[" + publishCharacterizationPR(t, o, state("DRAFT")) + "]"}
		}, calls: 5, lifecycle: "UNKNOWN", reason: "listed PR is not confirmed OPEN"},
		{name: "list not draft", verb: "list", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: "[" + publishCharacterizationPR(t, o, func(f map[string]any) { f["isDraft"] = false }) + "]"}
		}, calls: 5, lifecycle: "OPEN", reason: "listed PR is not a confirmed draft"},
		{name: "list moved head", verb: "list", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: "[" + publishCharacterizationPR(t, o, func(f map[string]any) { f["headRefOid"] = strings.Repeat("f", 40) }) + "]"}
		}, calls: 5, lifecycle: "OPEN", reason: "head commit missing or moved"},
		{name: "create non-zero exit", verb: "create", result: func(Options) process.Result {
			return process.Result{Code: 1, Error: "response lost"}
		}, calls: 6, lifecycle: "UNKNOWN", reason: "gh failed or uncertain: response lost ", created: true},
		{name: "create unexpected url", verb: "create", result: func(Options) process.Result {
			return process.Result{OK: true, Stdout: "https://github.com/other/repo/pull/1"}
		}, calls: 6, lifecycle: "UNKNOWN", reason: "creation returned a missing or unexpected GitHub PR URL", created: true},
		{name: "view non-zero exit", verb: "view", result: func(Options) process.Result {
			return process.Result{Code: 1, Error: "offline"}
		}, calls: 7, lifecycle: "UNKNOWN", reason: "gh failed or uncertain: offline ", created: true},
		{name: "view malformed json", verb: "view", result: func(Options) process.Result {
			return process.Result{OK: true, Stdout: "{"}
		}, calls: 7, lifecycle: "UNKNOWN", reason: "malformed PR observation", created: true},
		{name: "view missing", verb: "view", result: func(Options) process.Result {
			return process.Result{OK: true, Stdout: "null"}
		}, calls: 7, lifecycle: "MISSING", reason: "PR not found", created: true},
		{name: "view unknown state", verb: "view", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: publishCharacterizationPR(t, o, state("DRAFT"))}
		}, calls: 7, lifecycle: "UNKNOWN", reason: "unknown or missing PR state", created: true},
		{name: "view closed", verb: "view", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: publishCharacterizationPR(t, o, state("CLOSED"))}
		}, calls: 7, lifecycle: "CLOSED", reason: "PR is CLOSED", created: true},
		{name: "view merged", verb: "view", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: publishCharacterizationPR(t, o, state("MERGED"))}
		}, calls: 7, lifecycle: "MERGED", reason: "PR is MERGED", created: true},
		{name: "view not draft", verb: "view", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: publishCharacterizationPR(t, o, func(f map[string]any) { f["isDraft"] = nil })}
		}, calls: 7, lifecycle: "OPEN", reason: "PR is not a confirmed draft", created: true},
		{name: "view other repository", verb: "view", result: func(o Options) process.Result {
			return process.Result{OK: true, Stdout: publishCharacterizationPR(t, o, func(f map[string]any) { f["headRepositoryOwner"] = map[string]string{"login": "other"} })}
		}, calls: 7, lifecycle: "OPEN", reason: "head repository missing or mismatched", created: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, run := publishCharacterizationPublish(t, func(o Options, argv []string) (process.Result, bool) {
				if argv[0] == "gh" && argv[1] == "pr" && argv[2] == c.verb {
					return c.result(o), true
				}
				return process.Result{}, false
			})
			r := run.receipt
			if run.err == nil || run.err.Error() != "PR reconciliation required: "+c.reason {
				t.Fatalf("err = %v, want reason %q", run.err, c.reason)
			}
			if want := publishCharacterizationArgv(r.Commit)[:c.calls]; !reflect.DeepEqual(run.argv, want) {
				t.Fatalf("argv =\n%q\nwant\n%q", run.argv, want)
			}
			if r.Lifecycle == nil || r.Lifecycle.State != c.lifecycle || r.Lifecycle.Reconciliation != c.reason {
				t.Fatalf("lifecycle = %+v", r.Lifecycle)
			}
			if r.State != "committed" || r.DraftAtPublication || len(r.Commit) != 40 || r.Policy == nil {
				t.Fatalf("receipt = %+v", r)
			}
			if (r.CreationAttemptedAt != "") != c.created {
				t.Fatalf("creationAttemptedAt = %q, created = %v", r.CreationAttemptedAt, c.created)
			}
			if persisted := publishCharacterizationPersisted(t, o); !reflect.DeepEqual(persisted, r) {
				t.Fatalf("reconciliation not persisted:\n%+v\n%+v", persisted, r)
			}
		})
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationPublishRetryAfterUnresolvedCreationIsMissing(t *testing.T) {
	o, run := publishCharacterizationPublish(t, func(_ Options, argv []string) (process.Result, bool) {
		if argv[0] == "gh" && argv[1] == "pr" && argv[2] == "create" {
			return process.Result{TimedOut: true}, true
		}
		return process.Result{}, false
	})
	if run.err == nil || run.receipt.CreationAttemptedAt == "" {
		t.Fatalf("first attempt = %+v %v", run.receipt, run.err)
	}
	// The retry resumes the recorded receipt with the same approval.
	var argv [][]string
	r, err := Publish(context.Background(), PublishOptions{Options: o, Approve: run.receipt.Preview.Digest, Title: "Fixture", Runner: func(_ context.Context, c process.Options) process.Result {
		argv = append(argv, c.Argv)
		switch {
		case c.Argv[0] == "git":
			return process.Result{OK: true}
		case c.Argv[1] == "api":
			return process.Result{Code: 1, Error: "HTTP 404"}
		case c.Argv[2] == "list":
			return process.Result{OK: true, Stdout: "[]"}
		}
		t.Fatalf("unexpected remote call %q", c.Argv)
		return process.Result{}
	}})
	const reason = "no PR found for unresolved creation attempt; replacement requires reconciliation"
	if err == nil || err.Error() != "PR reconciliation required: "+reason {
		t.Fatalf("err = %v", err)
	}
	if want := publishCharacterizationArgv(r.Commit)[:5]; !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv =\n%q\nwant\n%q", argv, want)
	}
	if r.Lifecycle == nil || r.Lifecycle.State != "MISSING" || r.Lifecycle.Reconciliation != reason || r.State != "committed" {
		t.Fatalf("receipt = %+v", r)
	}
	if r.Policy == nil || r.Policy.Rules.Status != "unknown" || r.Policy.Protection.Status != "unknown" {
		t.Fatalf("policy = %+v", r.Policy)
	}
}

// Characterization (plugins slice 0): pins GitHub behaviour slice 1 must keep.
func TestCharacterizationPublishGitFailuresStopBeforeGitHub(t *testing.T) {
	cases := []struct {
		name, verb, err string
		result          process.Result
		calls           int
		policy          bool
	}{
		{name: "fetch non-zero exit", verb: "fetch", result: process.Result{Code: 128, Error: "exit status 128", Stderr: "fatal: offline"}, err: "git failed or uncertain: exit status 128 fatal: offline", calls: 1},
		{name: "push non-zero exit", verb: "push", result: process.Result{Code: 1, Error: "exit status 1", Stderr: "rejected"}, err: "git failed or uncertain: exit status 1 rejected", calls: 4, policy: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, run := publishCharacterizationPublish(t, func(_ Options, argv []string) (process.Result, bool) {
				if argv[0] == "git" && argv[1] == c.verb {
					return c.result, true
				}
				return process.Result{}, false
			})
			r := run.receipt
			if run.err == nil || run.err.Error() != c.err {
				t.Fatalf("err = %v, want %q", run.err, c.err)
			}
			if want := publishCharacterizationArgv(r.Commit)[:c.calls]; !reflect.DeepEqual(run.argv, want) {
				t.Fatalf("argv =\n%q\nwant\n%q", run.argv, want)
			}
			if r.State != "committed" || r.Lifecycle != nil || r.URL != "" || (r.Policy != nil) != c.policy {
				t.Fatalf("receipt = %+v", r)
			}
			if persisted := publishCharacterizationPersisted(t, o); !reflect.DeepEqual(persisted, r) {
				t.Fatal("receipt not persisted")
			}
		})
	}
}
