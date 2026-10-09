package delivery

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

func TestPublishResumesAfterPushFailure(t *testing.T) {
	o := fixture(t)
	p, err := Preview(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	failures := true
	pushes, creates := 0, 0
	runner := func(_ context.Context, cmd process.Options) process.Result {
		argv := strings.Join(cmd.Argv, " ")
		if len(cmd.Argv) > 1 && cmd.Argv[1] == "api" {
			return process.Result{Error: "advisory policy unavailable"}
		}
		if cmd.Argv[0] == "git" {
			if cmd.Argv[1] == "fetch" {
				return process.Result{OK: true}
			}
			pushes++
			if failures {
				return process.Result{Error: "offline"}
			}
			return process.Result{OK: true}
		}
		if strings.Contains(argv, "pr view") {
			return fixtureLifecycle(t, o, "OPEN")
		}
		if strings.Contains(argv, "pr list") {
			return process.Result{OK: true, Stdout: "[]"}
		}
		if strings.Contains(argv, "pr create") {
			creates++
			return process.Result{OK: true, Stdout: "https://github.com/owner/repo/pull/10\n"}
		}
		t.Fatalf("unexpected remote call %v", cmd.Argv)
		return process.Result{}
	}
	po := PublishOptions{Options: o, Approve: p.Digest, Title: "Extract hygiene guard", Runner: runner}
	first, err := Publish(context.Background(), po)
	if err == nil || first.Commit == "" {
		t.Fatalf("%+v %v", first, err)
	}
	failures = false
	done, err := Publish(context.Background(), po)
	if err != nil || done.URL == "" || done.Commit != first.Commit {
		t.Fatalf("%+v %v", done, err)
	}
	_, err = Publish(context.Background(), po)
	if err != nil || pushes != 2 || creates != 1 {
		t.Fatalf("%v pushes=%d creates=%d", err, pushes, creates)
	}
}
func TestPublicationIntentRecoversCommittedTree(t *testing.T) {
	o := fixture(t)
	p, err := Preview(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	w := filepath.Join(o.LabDir, "workspaces", "fixture")
	receipt := Receipt{Preview: p, Title: "Fixture", State: "prepared"}
	if err = writeReceipt(o, receipt); err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{{"add", "a.txt"}, {"commit", "-m", "Fixture"}} {
		cmd := exec.Command("git", argv...)
		cmd.Dir = w
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v", b, err)
		}
	}
	r, err := Publish(context.Background(), PublishOptions{Options: o, Approve: p.Digest, Title: "Fixture", Runner: func(_ context.Context, cmd process.Options) process.Result {
		if cmd.Argv[0] == "git" {
			return process.Result{OK: true}
		}
		if cmd.Argv[1] == "api" {
			return process.Result{Error: "advisory policy unavailable"}
		}
		if cmd.Argv[2] == "view" {
			return fixtureLifecycle(t, o, "OPEN")
		}
		if cmd.Argv[2] == "list" {
			return process.Result{OK: true, Stdout: "[]"}
		}
		return process.Result{OK: true, Stdout: "https://github.com/owner/repo/pull/10"}
	}})
	if err != nil || r.URL == "" {
		t.Fatalf("%+v %v", r, err)
	}
	raw, _ := os.ReadFile(filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json"))
	var persisted Receipt
	if json.Unmarshal(raw, &persisted) != nil || persisted.Commit != r.Commit {
		t.Fatal("receipt missing")
	}
}
func TestStaleApprovalDoesNotCommit(t *testing.T) {
	o := fixture(t)
	p, err := Preview(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(o.LabDir, "workspaces", "fixture", "a.txt"), []byte("different"), 0600)
	_, err = Publish(context.Background(), PublishOptions{Options: o, Approve: p.Digest, Title: "Fixture", Runner: func(context.Context, process.Options) process.Result {
		t.Fatal("unexpected remote write")
		return process.Result{}
	}})
	if err == nil {
		t.Fatal("stale approval accepted")
	}
}

func TestCommitHookCannotPublishUnreviewedTree(t *testing.T) {
	o := fixture(t)
	p, err := Preview(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	w := filepath.Join(o.LabDir, "workspaces", "fixture")
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = w
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	common := strings.TrimSpace(string(raw))
	if !filepath.IsAbs(common) {
		common = filepath.Join(w, common)
	}
	hook := []byte("#!/bin/sh\nprintf 'hook changed content\\n' > a.txt\ngit add a.txt\n")
	if err = os.WriteFile(filepath.Join(common, "hooks", "pre-commit"), hook, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = Publish(context.Background(), PublishOptions{Options: o, Approve: p.Digest, Title: "Fixture", Runner: func(context.Context, process.Options) process.Result {
		t.Fatal("unreviewed commit was pushed")
		return process.Result{}
	}})
	if err == nil || !strings.Contains(err.Error(), "reviewed tree") {
		t.Fatalf("expected tree refusal, got %v", err)
	}
}

func TestRelativeLabProducesAbsoluteBodyFile(t *testing.T) {
	o := fixture(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(cwd, o.LabDir)
	if err != nil {
		t.Fatal(err)
	}
	o.LabDir = relative
	p, err := Preview(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Publish(context.Background(), PublishOptions{Options: o, Approve: p.Digest, Title: "Relative fixture", Runner: func(_ context.Context, cmd process.Options) process.Result {
		if cmd.Argv[0] == "git" {
			return process.Result{OK: true}
		}
		if cmd.Argv[1] == "api" {
			return process.Result{Error: "advisory policy unavailable"}
		}
		if cmd.Argv[2] == "view" {
			return fixtureLifecycle(t, o, "OPEN")
		}
		if cmd.Argv[2] == "list" {
			return process.Result{OK: true, Stdout: "[]"}
		}
		body := cmd.Argv[len(cmd.Argv)-1]
		if !filepath.IsAbs(body) {
			t.Fatalf("body path is relative: %s", body)
		}
		if _, err := os.ReadFile(body); err != nil {
			t.Fatal(err)
		}
		return process.Result{OK: true, Stdout: "https://github.com/owner/repo/pull/10"}
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecordedPublicationRequiresFreshLifecycle(t *testing.T) {
	for _, state := range []string{"OPEN", "CLOSED", "MERGED", "UNKNOWN", "moved", "api", "overflow", "timeout", "malformed", "missing", "repository", "base", "branch", "draft", "null-draft", "target", "missing-head", "null-identity"} {
		t.Run(state, func(t *testing.T) {
			o := fixture(t)
			p, err := Preview(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			w := filepath.Join(o.LabDir, "workspaces", "fixture")
			for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-m", "Fixture"}} {
				c := exec.Command("git", args...)
				c.Dir = w
				if b, e := c.CombinedOutput(); e != nil {
					t.Fatalf("%s %v", b, e)
				}
			}
			head, err := git(context.Background(), w, nil, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			original := Receipt{Preview: p, Title: "Fixture", State: "published", Commit: strings.TrimSpace(head), URL: "https://github.com/owner/repo/pull/10", DraftAtPublication: true}
			if err = writeReceipt(o, original); err != nil {
				t.Fatal(err)
			}
			got, err := Publish(context.Background(), PublishOptions{Options: o, Approve: p.Digest, Title: "Fixture", Runner: func(_ context.Context, cmd process.Options) process.Result {
				if cmd.Argv[0] != "gh" || cmd.Argv[2] != "view" {
					t.Fatalf("unexpected write/query %v", cmd.Argv)
				}
				if cmd.Timeout != 60*time.Second || cmd.MaxOutputBytes != 1_000_000 {
					t.Fatal("unbounded query")
				}
				result := process.Result{OK: true, Stdout: lifecycleJSON(original, state)}
				switch state {
				case "api":
					return process.Result{Error: "API unavailable"}
				case "overflow":
					result.Overflow = true
				case "timeout":
					result.TimedOut = true
				case "malformed":
					result.Stdout = "{"
				case "missing":
					result.Stdout = "null"
				case "moved", "repository", "base", "branch", "draft", "null-draft", "target", "missing-head", "null-identity":
					var fields map[string]any
					json.Unmarshal([]byte(lifecycleJSON(original, "OPEN")), &fields)
					switch state {
					case "moved":
						fields["headRefOid"] = strings.Repeat("f", 40)
					case "repository":
						fields["headRepositoryOwner"] = map[string]string{"login": "other"}
					case "base":
						fields["baseRefName"] = "other"
					case "branch":
						fields["headRefName"] = "other"
					case "draft":
						fields["isDraft"] = false
					case "null-draft":
						fields["isDraft"] = nil
					case "target":
						fields["url"] = "https://github.com/other/repo/pull/10"
					case "missing-head":
						delete(fields, "headRefOid")
					case "null-identity":
						fields["headRepository"] = nil
					}
					raw, _ := json.Marshal(fields)
					result.Stdout = string(raw)
				}
				return result
			}})
			if state == "OPEN" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "reconciliation") {
				t.Fatalf("%s accepted or not reconciled: %+v %v", state, got, err)
			}
			if got.Lifecycle == nil || got.Lifecycle.ObservedAt == "" {
				t.Fatal("missing observation")
			}
			raw, _ := os.ReadFile(filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json"))
			var persisted Receipt
			if json.Unmarshal(raw, &persisted) != nil || !reflect.DeepEqual(got, persisted) {
				t.Fatal("observation not persisted")
			}
			if got.State != original.State || got.URL != original.URL || got.Commit != original.Commit || !got.DraftAtPublication {
				t.Fatal("historical publication rewritten")
			}
		})
	}
}

func lifecycleJSON(r Receipt, state string) string {
	raw, _ := json.Marshal(map[string]any{"url": r.URL, "state": state, "isDraft": true, "baseRefName": r.Preview.BaseBranch, "headRefName": r.Preview.Branch, "headRefOid": r.Commit, "headRepository": map[string]string{"name": "repo"}, "headRepositoryOwner": map[string]string{"login": "owner"}})
	return string(raw)
}

func fixtureLifecycle(t *testing.T, o Options, state string) process.Result {
	t.Helper()
	head, err := git(context.Background(), filepath.Join(o.LabDir, "workspaces", "fixture"), nil, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return process.Result{OK: true, Stdout: lifecycleJSON(Receipt{URL: "https://github.com/owner/repo/pull/10", Commit: strings.TrimSpace(head), Preview: PreviewResult{BaseBranch: o.BaseBranch, Branch: "forgecell/fixture"}}, state)}
}

func TestPublicationVerificationRetainsReturnedIdentity(t *testing.T) {
	for _, mode := range []string{"create", "list", "ambiguous", "list-api", "list-null"} {
		t.Run(mode, func(t *testing.T) {
			o := fixture(t)
			p, err := Preview(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			creates := 0
			reads := 0
			po := PublishOptions{Options: o, Approve: p.Digest, Title: "Fixture", Runner: func(_ context.Context, c process.Options) process.Result {
				if c.Argv[0] == "git" {
					return process.Result{OK: true}
				}
				if c.Argv[1] == "api" {
					return process.Result{Error: "advisory unavailable"}
				}
				switch c.Argv[2] {
				case "list":
					if !strings.Contains(strings.Join(c.Argv, " "), "--limit 2") {
						t.Fatal("unbounded list")
					}
					if mode == "list-api" {
						return process.Result{Error: "offline"}
					}
					if mode == "list-null" {
						return process.Result{OK: true, Stdout: "null"}
					}
					if mode == "create" {
						return process.Result{OK: true, Stdout: "[]"}
					}
					item := fixtureLifecycle(t, o, "OPEN").Stdout
					if mode == "ambiguous" {
						item += "," + item
					}
					return process.Result{OK: true, Stdout: "[" + item + "]"}
				case "create":
					creates++
					return process.Result{OK: true, Stdout: "https://github.com/owner/repo/pull/10"}
				case "view":
					reads++
					if reads == 1 {
						return process.Result{Error: "offline"}
					}
					return fixtureLifecycle(t, o, "OPEN")
				}
				t.Fatalf("unexpected call %v", c.Argv)
				return process.Result{}
			}}
			first, err := Publish(context.Background(), po)
			if err == nil || !strings.Contains(err.Error(), "reconciliation") || first.State != "committed" || first.DraftAtPublication {
				t.Fatalf("false publication %+v %v", first, err)
			}
			if mode == "ambiguous" || mode == "list-api" || mode == "list-null" {
				if creates != 0 || first.Lifecycle == nil {
					t.Fatal("uncertainty created PR")
				}
				return
			}
			if first.URL == "" {
				t.Fatal("lost returned identity")
			}
			second, err := Publish(context.Background(), po)
			wantCreates := 0
			if mode == "create" {
				wantCreates = 1
			}
			if err != nil || second.State != "published" || second.URL != first.URL || creates != wantCreates {
				t.Fatalf("retry %+v %v creates=%d", second, err, creates)
			}
		})
	}
}

func TestUncertainCreationNeverCreatesReplacement(t *testing.T) {
	for _, mode := range []string{"timeout", "error", "overflow", "malformed", "missing", "persist-failure"} {
		t.Run(mode, func(t *testing.T) {
			o := fixture(t)
			p, err := Preview(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			creates := 0
			recoverPR := false
			file := filepath.Join(o.LabDir, "deliveries", o.MoleculeID+".json")
			po := PublishOptions{Options: o, Approve: p.Digest, Title: "Fixture", Runner: func(_ context.Context, c process.Options) process.Result {
				if c.Argv[0] == "git" {
					return process.Result{OK: true}
				}
				if c.Argv[1] == "api" {
					return process.Result{Error: "advisory unavailable"}
				}
				switch c.Argv[2] {
				case "list":
					if recoverPR {
						return process.Result{OK: true, Stdout: "[" + fixtureLifecycle(t, o, "OPEN").Stdout + "]"}
					}
					if mode == "persist-failure" {
						if err := os.Remove(file); err != nil {
							t.Fatal(err)
						}
						if err := os.Mkdir(file, 0700); err != nil {
							t.Fatal(err)
						}
					}
					return process.Result{OK: true, Stdout: "[]"}
				case "create":
					creates++ // Remote creation may succeed before its response is lost.
					switch mode {
					case "timeout":
						return process.Result{TimedOut: true}
					case "error":
						return process.Result{Error: "response lost"}
					case "overflow":
						return process.Result{OK: true, Overflow: true}
					case "malformed":
						return process.Result{OK: true, Stdout: "not a URL"}
					default:
						return process.Result{OK: true}
					}
				case "view":
					return fixtureLifecycle(t, o, "OPEN")
				}
				t.Fatalf("unexpected call %v", c.Argv)
				return process.Result{}
			}}
			first, err := Publish(context.Background(), po)
			if err == nil {
				t.Fatal("uncertain creation accepted")
			}
			if mode == "persist-failure" {
				if creates != 0 {
					t.Fatalf("create invoked before durable intent: %d", creates)
				}
				return
			}
			if first.State == "published" {
				t.Fatal("false publication")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var persisted Receipt
			if err := json.Unmarshal(raw, &persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.Lifecycle == nil || persisted.Lifecycle.State != "UNKNOWN" || persisted.Lifecycle.Reconciliation == "" {
				t.Fatal("creation uncertainty not durable")
			}
			second, err := Publish(context.Background(), po)
			if err == nil || !strings.Contains(err.Error(), "reconciliation") || creates != 1 || second.State == "published" {
				t.Fatalf("empty-list retry %+v %v creates=%d", second, err, creates)
			}
			recoverPR = true
			third, err := Publish(context.Background(), po)
			if err != nil || third.State != "published" || creates != 1 {
				t.Fatalf("exact recovery %+v %v creates=%d", third, err, creates)
			}
		})
	}
}
