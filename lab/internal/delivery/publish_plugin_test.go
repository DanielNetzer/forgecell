package delivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
)

const (
	pluginTestHost    = "forge.example"
	pluginTestRef     = "!7"
	pluginTestURL     = "https://forge.example/acme/widgets/-/merge_requests/7"
	pluginTestProject = "acme/widgets"
	pluginTestName    = "gitlab"
)

// fakeChanges serves forge.Changes in process. Every call appends its tool
// name to the shared order log and a record to the plugin call log, as the
// adapter's Observer would. Answers are functions of the pushed commit, which
// the git runner learns from the push.
type fakeChanges struct {
	mu        sync.Mutex
	order     *[]string
	calls     *PluginCallLog
	pushed    *string
	describe  func() (contract.ForgeDescription, error)
	policy    func() (contract.ForgePolicy, error)
	find      func(commit string) (contract.ForgeChanges, error)
	publish   func(commit string) (contract.ForgePublished, error)
	change    func(commit string) (contract.ForgeChange, error)
	checks    func(commit string) (contract.ForgeChecks, error)
	published []forge.PublishInput
	branches  []string
}

func (f *fakeChanges) note(tool string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	*f.order = append(*f.order, tool)
	f.calls.Observe(mcpadapter.CallRecord{Plugin: pluginTestName, Kind: "forge", Tool: tool})
}

func (f *fakeChanges) Describe(context.Context) (contract.ForgeDescription, error) {
	f.note("forge_describe")
	if f.describe != nil {
		return f.describe()
	}
	return contract.ForgeDescription{Identity: "forge.example/acme/widgets", Host: pluginTestHost, DisplayName: "Fake GitLab", ChangeNoun: "merge request", DefaultBranch: "main", CIKind: "gitlab-ci", WebURL: "https://forge.example/acme/widgets"}, nil
}

func (f *fakeChanges) Policy(_ context.Context, branch string) (contract.ForgePolicy, error) {
	f.note("forge_policy")
	f.branches = append(f.branches, branch)
	if f.policy != nil {
		return f.policy()
	}
	return contract.ForgePolicy{RequiredChecks: []string{"build"}, RequiredReviews: 1, Protected: true}, nil
}

func (f *fakeChanges) FindChange(_ context.Context, head string) (contract.ForgeChanges, error) {
	f.note("forge_find_change")
	f.branches = append(f.branches, head)
	if f.find != nil {
		return f.find(*f.pushed)
	}
	return contract.ForgeChanges{Changes: []contract.ForgeChangeRef{}}, nil
}

func (f *fakeChanges) PublishDraft(_ context.Context, in forge.PublishInput) (contract.ForgePublished, error) {
	f.note("forge_publish_draft")
	f.published = append(f.published, in)
	if f.publish != nil {
		return f.publish(in.Commit)
	}
	return contract.ForgePublished{Ref: pluginTestRef, URL: pluginTestURL, Draft: true, HeadCommit: in.Commit}, nil
}

func (f *fakeChanges) Change(_ context.Context, ref string) (contract.ForgeChange, error) {
	f.note("forge_change")
	if ref != pluginTestRef {
		return contract.ForgeChange{}, mcp.Fail(mcp.SchemaInvalid, "forge_change", "unknown ref")
	}
	if f.change != nil {
		return f.change(*f.pushed)
	}
	return contract.ForgeChange{State: "open", Draft: true, HeadCommit: *f.pushed, URL: pluginTestURL}, nil
}

func (f *fakeChanges) Checks(_ context.Context, ref, commit string) (contract.ForgeChecks, error) {
	f.note("forge_checks")
	if f.checks != nil {
		return f.checks(*f.pushed)
	}
	return contract.ForgeChecks{Checks: []contract.ForgeCheck{}, Complete: true}, nil
}

func (f *fakeChanges) Comments(context.Context, string, string) (contract.ForgeComments, error) {
	f.note("forge_comments")
	return contract.ForgeComments{Comments: []contract.ForgeComment{}}, nil
}

// pluginPublishFixture is the delivery fixture with a local bare remote: the
// runner runs real git with origin replaced by the bare repository, so fetch
// and push are real and nothing reaches the network.
type pluginPublishFixture struct {
	o      Options
	digest string
	bare   string
	order  []string
	pushed string
	fake   *fakeChanges
	calls  *PluginCallLog
}

func newPluginPublishFixture(t *testing.T) *pluginPublishFixture {
	t.Helper()
	o := fixture(t)
	p, err := Preview(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(t.TempDir(), "remote.git")
	workspace := filepath.Join(o.LabDir, "workspaces", "fixture")
	for _, args := range [][]string{{"init", "--bare", "-b", "main", bare}, {"-C", workspace, "push", bare, p.BaseCommit + ":refs/heads/main"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %q: %v %s", args, err, out)
		}
	}
	fx := &pluginPublishFixture{o: o, digest: p.Digest, bare: bare, calls: &PluginCallLog{}}
	fx.fake = &fakeChanges{order: &fx.order, calls: fx.calls, pushed: &fx.pushed}
	return fx
}

// runner runs the engine's git calls against the bare remote, in order with
// the plugin calls.
func (fx *pluginPublishFixture) runner(ctx context.Context, c process.Options) process.Result {
	argv := append([]string(nil), c.Argv...)
	for i, a := range argv {
		if a == "origin" {
			argv[i] = fx.bare
		}
	}
	if len(argv) > 1 {
		fx.order = append(fx.order, argv[0]+" "+argv[1])
	}
	if len(argv) > 3 && argv[0] == "git" && argv[1] == "push" {
		fx.pushed, _, _ = strings.Cut(argv[3], ":")
	}
	c.Argv = argv
	return process.Run(ctx, c)
}

func (fx *pluginPublishFixture) publish() (Receipt, error) {
	fx.order = nil
	return Publish(context.Background(), PublishOptions{Options: fx.o, Approve: fx.digest, Title: "Fixture", Runner: fx.runner, Changes: fx.fake, Plugin: PluginForge{Name: pluginTestName, Project: pluginTestProject, Calls: fx.calls}})
}

// sameReceipt compares receipts as JSON: the persisted receipt holds its policy
// content re-indented, which compact marshaling makes equal again.
func sameReceipt(a, b Receipt) bool {
	x, errX := json.Marshal(a)
	y, errY := json.Marshal(b)
	return errX == nil && errY == nil && bytes.Equal(x, y)
}

func pluginTools(records []mcpadapter.CallRecord) []string {
	tools := []string{}
	for _, r := range records {
		tools = append(tools, r.Tool)
	}
	return tools
}

func TestPluginPublishCallOrderAndReceipt(t *testing.T) {
	fx := newPluginPublishFixture(t)
	r, err := fx.publish()
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	want := []string{"forge_describe", "git fetch", "forge_policy", "git push", "forge_find_change", "forge_publish_draft", "forge_change"}
	if !reflect.DeepEqual(fx.order, want) {
		t.Fatalf("order = %q, want %q", fx.order, want)
	}
	if len(r.Commit) != 40 || fx.pushed != r.Commit {
		t.Fatalf("commit %q, pushed %q", r.Commit, fx.pushed)
	}
	out, err := exec.Command("git", "--git-dir", fx.bare, "rev-parse", "refs/heads/forgecell/fixture").CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != r.Commit {
		t.Fatalf("bare remote branch = %q, %v", out, err)
	}
	if !reflect.DeepEqual(fx.fake.branches, []string{"main", "forgecell/fixture"}) {
		t.Fatalf("policy and find branches = %q", fx.fake.branches)
	}
	wantBody := "Implements issue #9.\n\nMolecule: `mol-fixture`\nFormula: `delivery-fixture`\nBase: `" + r.Preview.BaseCommit + "`\nDelivery: `" + r.Commit + "`\n\nAwaiting exact-commit CI and human review. Harness-reported checks are not independent CI evidence. No merge, deployment or issue closure is authorized.\n"
	if want := []forge.PublishInput{{HeadBranch: "forgecell/fixture", Base: "main", Title: "Fixture", Body: wantBody, Commit: r.Commit}}; !reflect.DeepEqual(fx.fake.published, want) {
		t.Fatalf("PublishDraft input = %+v, want %+v", fx.fake.published, want)
	}
	if r.State != "published" || !r.DraftAtPublication || r.Ref != pluginTestRef || r.URL != pluginTestURL || r.Forge != pluginTestName || r.Project != pluginTestProject || r.ForgeHost != pluginTestHost || r.ChangeNoun != "merge request" || r.CreationAttemptedAt == "" {
		t.Fatalf("receipt = %+v", r)
	}
	if r.Lifecycle == nil || r.Lifecycle.State != "OPEN" || r.Lifecycle.Reconciliation != "" {
		t.Fatalf("lifecycle = %+v", r.Lifecycle)
	}
	if r.Policy == nil || r.Policy.Rules.Source != "plugin:gitlab/forge_policy" || r.Policy.Rules.Status != "observed" || string(r.Policy.Rules.Content) != `{"requiredChecks":["build"],"requiredReviews":1,"protected":true}` || r.Policy.Protection.SHA256 != r.Policy.Rules.SHA256 {
		t.Fatalf("policy = %+v", r.Policy)
	}
	if got := pluginTools(r.PluginCalls); !reflect.DeepEqual(got, []string{"forge_describe", "forge_policy", "forge_find_change", "forge_publish_draft", "forge_change"}) {
		t.Fatalf("receipt plugin calls = %q", got)
	}
	if PluginPublishOutcome(r) != "created" {
		t.Fatalf("outcome = %q", PluginPublishOutcome(r))
	}
	if persisted := publishCharacterizationPersisted(t, fx.o); !sameReceipt(persisted, r) {
		t.Fatalf("persisted receipt differs:\n%+v\n%+v", persisted, r)
	}
	// A verified receipt is only verified again: no second draft.
	again, err := fx.publish()
	if err != nil || !reflect.DeepEqual(fx.order, []string{"forge_describe", "forge_change"}) || again.URL != pluginTestURL || len(fx.fake.published) != 1 {
		t.Fatalf("resume = %q, %v", fx.order, err)
	}
}

func TestPluginPublishExistingChangeSkipsPublishDraft(t *testing.T) {
	fx := newPluginPublishFixture(t)
	fx.fake.find = func(commit string) (contract.ForgeChanges, error) {
		return contract.ForgeChanges{Changes: []contract.ForgeChangeRef{{Ref: pluginTestRef, URL: pluginTestURL, State: "open", Draft: true, HeadCommit: commit}}}, nil
	}
	r, err := fx.publish()
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if want := []string{"forge_describe", "git fetch", "forge_policy", "git push", "forge_find_change", "forge_change"}; !reflect.DeepEqual(fx.order, want) {
		t.Fatalf("order = %q, want %q", fx.order, want)
	}
	if len(fx.fake.published) != 0 || r.CreationAttemptedAt != "" || r.State != "published" || r.Ref != pluginTestRef || PluginPublishOutcome(r) != "existing-draft" {
		t.Fatalf("receipt = %+v, published %d", r, len(fx.fake.published))
	}
}

func TestPluginPublishReconciliationStates(t *testing.T) {
	other := strings.Repeat("f", 40)
	listed := func(mutate func(*contract.ForgeChangeRef)) func(string) (contract.ForgeChanges, error) {
		return func(commit string) (contract.ForgeChanges, error) {
			c := contract.ForgeChangeRef{Ref: pluginTestRef, URL: pluginTestURL, State: "open", Draft: true, HeadCommit: commit}
			mutate(&c)
			return contract.ForgeChanges{Changes: []contract.ForgeChangeRef{c}}, nil
		}
	}
	cases := []struct {
		name      string
		set       func(*fakeChanges)
		lifecycle string
		reason    string
		ref       bool
	}{
		{"find failed", func(f *fakeChanges) {
			f.find = func(string) (contract.ForgeChanges, error) {
				return contract.ForgeChanges{}, mcp.Fail(mcp.Network, "forge_find_change", "dial tcp")
			}
		}, "UNKNOWN", "the forge plugin could not find the merge request for the branch: the plugin could not reach the forge (network)", false},
		{"ambiguous", func(f *fakeChanges) {
			f.find = func(commit string) (contract.ForgeChanges, error) {
				c := contract.ForgeChangeRef{Ref: pluginTestRef, URL: pluginTestURL, State: "open", Draft: true, HeadCommit: commit}
				return contract.ForgeChanges{Changes: []contract.ForgeChangeRef{c, c}}, nil
			}
		}, "AMBIGUOUS", "more than one merge request found", false},
		{"listed foreign host", func(f *fakeChanges) {
			f.find = listed(func(c *contract.ForgeChangeRef) { c.URL = "https://other.example/acme/widgets/-/merge_requests/7" })
		}, "UNKNOWN", "unexpected merge request ref or URL", false},
		{"listed closed", func(f *fakeChanges) { f.find = listed(func(c *contract.ForgeChangeRef) { c.State = "closed" }) }, "CLOSED", "listed merge request is not confirmed OPEN", true},
		{"listed not draft", func(f *fakeChanges) { f.find = listed(func(c *contract.ForgeChangeRef) { c.Draft = false }) }, "OPEN", "listed merge request is not a confirmed draft", true},
		{"listed moved head", func(f *fakeChanges) { f.find = listed(func(c *contract.ForgeChangeRef) { c.HeadCommit = other }) }, "OPEN", "head commit missing or moved", true},
		{"created on foreign host", func(f *fakeChanges) {
			f.publish = func(commit string) (contract.ForgePublished, error) {
				return contract.ForgePublished{Ref: pluginTestRef, URL: "https://forge.example.evil/acme/widgets/-/merge_requests/7", Draft: true, HeadCommit: commit}, nil
			}
		}, "UNKNOWN", "creation returned a missing or unexpected merge request ref or URL", false},
		{"created over http", func(f *fakeChanges) {
			f.publish = func(commit string) (contract.ForgePublished, error) {
				return contract.ForgePublished{Ref: pluginTestRef, URL: "http://forge.example/acme/widgets/-/merge_requests/7", Draft: true, HeadCommit: commit}, nil
			}
		}, "UNKNOWN", "creation returned a missing or unexpected merge request ref or URL", false},
		{"view closed", func(f *fakeChanges) {
			f.change = func(commit string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: "closed", Draft: true, HeadCommit: commit, URL: pluginTestURL}, nil
			}
		}, "CLOSED", "merge request is CLOSED", true},
		{"view url mismatch", func(f *fakeChanges) {
			f.change = func(commit string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: "open", Draft: true, HeadCommit: commit, URL: "https://forge.example/acme/widgets/-/merge_requests/8"}, nil
			}
		}, "OPEN", "target project or merge request URL mismatch", true},
		{"view not draft", func(f *fakeChanges) {
			f.change = func(commit string) (contract.ForgeChange, error) {
				return contract.ForgeChange{State: "open", Draft: false, HeadCommit: commit, URL: pluginTestURL}, nil
			}
		}, "OPEN", "merge request is not a confirmed draft", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newPluginPublishFixture(t)
			c.set(fx.fake)
			r, err := fx.publish()
			if err == nil || err.Error() != "merge request reconciliation required: "+c.reason {
				t.Fatalf("error = %v, want reason %q", err, c.reason)
			}
			persisted := publishCharacterizationPersisted(t, fx.o)
			if !sameReceipt(persisted, r) || r.Lifecycle == nil || r.Lifecycle.State != c.lifecycle || r.Lifecycle.Reconciliation != c.reason || r.State == "published" {
				t.Fatalf("receipt = %+v lifecycle %+v", r, r.Lifecycle)
			}
			if (r.Ref == pluginTestRef) != c.ref || (r.URL == pluginTestURL) != c.ref {
				t.Fatalf("ref %q url %q, want kept=%t", r.Ref, r.URL, c.ref)
			}
			if PluginPublishOutcome(r) != "mismatch" || len(r.PluginCalls) == 0 {
				t.Fatalf("outcome %q, calls %d", PluginPublishOutcome(r), len(r.PluginCalls))
			}
		})
	}
}

func TestPluginPublishHeadCommitMismatchKeepsIdentity(t *testing.T) {
	fx := newPluginPublishFixture(t)
	// As the adapter does: the decoded answer comes back with the refusal.
	fx.fake.publish = func(string) (contract.ForgePublished, error) {
		return contract.ForgePublished{Ref: pluginTestRef, URL: pluginTestURL, Draft: true, HeadCommit: strings.Repeat("e", 40)}, mcp.Fail(mcp.SchemaInvalid, "forge_publish_draft", "the published change's head is not the approved commit")
	}
	r, err := fx.publish()
	want := "merge request reconciliation required: the forge plugin could not publish the draft merge request: the plugin's answer did not match the forge contract (schema-invalid)"
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v", err)
	}
	persisted := publishCharacterizationPersisted(t, fx.o)
	if persisted.Ref != pluginTestRef || persisted.URL != pluginTestURL || persisted.Lifecycle == nil || persisted.Lifecycle.State != "UNKNOWN" || persisted.CreationAttemptedAt == "" || !sameReceipt(persisted, r) {
		t.Fatalf("persisted = %+v", persisted)
	}
	// A retry finds the kept change by its ref and never opens a replacement.
	fx.fake.change = func(string) (contract.ForgeChange, error) {
		return contract.ForgeChange{State: "open", Draft: true, HeadCommit: strings.Repeat("e", 40), URL: pluginTestURL}, nil
	}
	_, err = fx.publish()
	if err == nil || !strings.Contains(err.Error(), "head commit missing or moved") || !reflect.DeepEqual(fx.order, []string{"forge_describe", "forge_change"}) || len(fx.fake.published) != 1 {
		t.Fatalf("retry = %q, %v", fx.order, err)
	}
}

func TestPluginPublishUnresolvedCreationIsMissing(t *testing.T) {
	fx := newPluginPublishFixture(t)
	fx.fake.publish = func(string) (contract.ForgePublished, error) {
		return contract.ForgePublished{}, mcp.Fail(mcp.Timeout, "forge_publish_draft", "")
	}
	if _, err := fx.publish(); err == nil || !strings.Contains(err.Error(), "did not answer in time (timeout)") {
		t.Fatalf("first attempt: %v", err)
	}
	fx.fake.publish = nil
	_, err := fx.publish()
	if err == nil || err.Error() != "merge request reconciliation required: no merge request found for unresolved creation attempt; replacement requires reconciliation" || len(fx.fake.published) != 1 {
		t.Fatalf("retry: %v, published %d", err, len(fx.fake.published))
	}
}

func TestPluginPublishBrokenPluginFailsBeforePush(t *testing.T) {
	for _, reason := range mcp.Reasons {
		t.Run(string(reason), func(t *testing.T) {
			fx := newPluginPublishFixture(t)
			fx.fake.describe = func() (contract.ForgeDescription, error) {
				return contract.ForgeDescription{}, mcp.Fail(reason, "forge_describe", "Authorization: Bearer fakefakefakefake0123456789 at /home/someone/lab")
			}
			_, err := fx.publish()
			want := "the forge plugin could not describe the forge: " + pluginReasonText[reason] + " (" + string(reason) + ")"
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			if mcp.ReasonOf(err) != reason {
				t.Fatalf("reason = %s", mcp.ReasonOf(err))
			}
			if !reflect.DeepEqual(fx.order, []string{"forge_describe"}) {
				t.Fatalf("calls after a broken describe: %q", fx.order)
			}
			// The failed call is still recorded on the receipt.
			if r := publishCharacterizationPersisted(t, fx.o); !reflect.DeepEqual(pluginTools(r.PluginCalls), []string{"forge_describe"}) || r.URL != "" {
				t.Fatalf("receipt = %+v", r)
			}
		})
	}
	// Every fixed reason has a plain account.
	for _, reason := range mcp.Reasons {
		if pluginReasonText[reason] == "" {
			t.Fatalf("no plain text for %s", reason)
		}
	}
	if err := pluginError("read", errors.New("raw server text")); err.Error() != "the forge plugin could not read: the plugin call failed (failed)" {
		t.Fatalf("other error = %q", err)
	}
}

func TestPluginPublishRefusesAnInvalidDescribedHost(t *testing.T) {
	fx := newPluginPublishFixture(t)
	fx.fake.describe = func() (contract.ForgeDescription, error) {
		return contract.ForgeDescription{Host: "Forge.Example/evil", ChangeNoun: "merge request"}, nil
	}
	if _, err := fx.publish(); err == nil || err.Error() != "the forge plugin described an invalid host" || len(fx.order) != 1 {
		t.Fatalf("Publish = %v, calls %q", err, fx.order)
	}
}

func TestPluginURLCheck(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://forge.example/acme/widgets/-/merge_requests/7", true},
		{"https://forge.example/1", true},
		{"http://forge.example/1", false},
		{"https://forge.example.evil/1", false},
		{"https://evil.example/forge.example/1", false},
		{"https://user@forge.example/1", false},
		{"https://forge.example:443/1", false},
		{"https://forge.example", false},
		{"https://forge.example/\x1b[31m", false},
		{"https://forge.example/" + strings.Repeat("a", 2048), false},
	}
	for _, c := range cases {
		if got := validPluginURL(pluginTestHost, c.raw); got != c.want {
			t.Errorf("validPluginURL(%q) = %t, want %t", c.raw, got, c.want)
		}
	}
	if validPluginURL("", "https:///1") || !validPluginURL("forge.example:8443", "https://forge.example:8443/1") {
		t.Fatal("host forms")
	}
	// GitHub keeps its exact check.
	if !validURL("owner/repo", "https://github.com/owner/repo/pull/10") || validURL("owner/repo", pluginTestURL) {
		t.Fatal("GitHub URL check changed")
	}
}

func TestPublishKeepsEachReceiptOnItsForge(t *testing.T) {
	// A GitHub-published receipt is never continued through a plugin forge.
	o, run := publishCharacterizationPublish(t, nil)
	if run.err != nil {
		t.Fatal(run.err)
	}
	var order []string
	calls := &PluginCallLog{}
	pushed := ""
	fake := &fakeChanges{order: &order, calls: calls, pushed: &pushed}
	_, err := Publish(context.Background(), PublishOptions{Options: o, Approve: run.receipt.Preview.Digest, Title: "Fixture", Runner: func(context.Context, process.Options) process.Result {
		t.Fatal("a remote call ran")
		return process.Result{}
	}, Changes: fake, Plugin: PluginForge{Name: pluginTestName, Project: pluginTestProject, Calls: calls}})
	if err == nil || !strings.Contains(err.Error(), "published through GitHub") || len(order) != 0 {
		t.Fatalf("Publish = %v, calls %q", err, order)
	}
	// A plugin-published receipt is never continued through GitHub.
	fx := newPluginPublishFixture(t)
	if _, err := fx.publish(); err != nil {
		t.Fatal(err)
	}
	_, err = Publish(context.Background(), PublishOptions{Options: fx.o, Approve: fx.digest, Title: "Fixture", Runner: func(context.Context, process.Options) process.Result {
		t.Fatal("a remote call ran")
		return process.Result{}
	}})
	if err == nil || !strings.Contains(err.Error(), "published through a forge plugin") {
		t.Fatalf("GitHub resume = %v", err)
	}
	// Nor through another plugin or project.
	_, err = Publish(context.Background(), PublishOptions{Options: fx.o, Approve: fx.digest, Title: "Fixture", Runner: fx.runner, Changes: fx.fake, Plugin: PluginForge{Name: "other", Project: pluginTestProject}})
	if err == nil || !strings.Contains(err.Error(), "another forge plugin or project") {
		t.Fatalf("other plugin = %v", err)
	}
}

func TestPluginPublishRefusesAnUnresolvedGitHubCreation(t *testing.T) {
	// A GitHub receipt whose PR creation timed out has no URL but may still
	// have opened a pull request; a plugin forge never adopts it.
	o, run := publishCharacterizationPublish(t, func(_ Options, argv []string) (process.Result, bool) {
		if argv[0] == "gh" && argv[1] == "pr" && argv[2] == "create" {
			return process.Result{TimedOut: true}, true
		}
		return process.Result{}, false
	})
	if run.err == nil || run.receipt.CreationAttemptedAt == "" || run.receipt.URL != "" || run.receipt.Forge != "" {
		t.Fatalf("first attempt = %+v %v", run.receipt, run.err)
	}
	before := publishCharacterizationPersisted(t, o)
	var order []string
	calls := &PluginCallLog{}
	pushed := ""
	fake := &fakeChanges{order: &order, calls: calls, pushed: &pushed}
	_, err := Publish(context.Background(), PublishOptions{Options: o, Approve: run.receipt.Preview.Digest, Title: "Fixture", Runner: func(context.Context, process.Options) process.Result {
		t.Fatal("a remote call ran")
		return process.Result{}
	}, Changes: fake, Plugin: PluginForge{Name: pluginTestName, Project: pluginTestProject, Calls: calls}})
	if err == nil || !strings.Contains(err.Error(), "GitHub pull request creation attempt") || len(order) != 0 {
		t.Fatalf("Publish = %v, calls %q", err, order)
	}
	if after := publishCharacterizationPersisted(t, o); !reflect.DeepEqual(after, before) {
		t.Fatalf("the GitHub receipt changed:\n%+v\n%+v", after, before)
	}
}

// TestPluginPublishThroughTheFakeServer runs the real adapter against the fake
// MCP server. Its canned draft's head is never the approved commit, so the
// adapter refuses it, and the receipt keeps the change's ref and URL with the
// refusal and the plugin's call records.
func TestPluginPublishThroughTheFakeServer(t *testing.T) {
	fx := newPluginPublishFixture(t)
	server := plugintest.Server(t)
	lab, err := filepath.EvalSymlinks(fx.o.LabDir)
	if err != nil {
		t.Fatal(err)
	}
	raw := plugintest.Manifest(t, server, "forge", "good")
	if err := os.MkdirAll(filepath.Join(lab, "plugins", "fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, "plugins", "fake", "plugin.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	f, err := formula.Parse([]byte("schemaVersion: v0\nkind: formula\nid: sample\nintake:\n  source: github-issues\n  repo: owner/repo\natoms:\n  - id: intake\n    type: intake\n  - id: work\n    type: harness\nplugins:\n  - name: gitlab\n    manifest: plugins/fake/plugin.json\n    sha256: " + hex.EncodeToString(sum[:]) + "\nforge:\n  plugin: gitlab\n  project: acme/widgets\n"))
	if err != nil {
		t.Fatal(err)
	}
	calls := &PluginCallLog{}
	changes, ok, err := mcpadapter.ForgeFor(lab, f, mcpadapter.Options{LookupEnv: func(string) (string, bool) { return "", false }, Observer: calls.Observe})
	if err != nil || !ok {
		t.Fatalf("ForgeFor = %t, %v", ok, err)
	}
	r, err := Publish(context.Background(), PublishOptions{Options: fx.o, Approve: fx.digest, Title: "Fixture", Runner: fx.runner, Changes: changes, Plugin: PluginForge{Name: pluginTestName, Project: pluginTestProject, Calls: calls}})
	if want := "change request reconciliation required: the forge plugin could not publish the draft change request: the plugin's answer did not match the forge contract (schema-invalid)"; err == nil || err.Error() != want {
		t.Fatalf("Publish = %v", err)
	}
	if strings.Contains(err.Error(), plugintest.PlantedSecret) || strings.Contains(err.Error(), lab) {
		t.Fatalf("the error leaks: %q", err)
	}
	if r.Ref != "!1" || r.URL != "https://forge.example/1" || r.ForgeHost != "forge.example" || r.ChangeNoun != "change request" {
		t.Fatalf("receipt = %+v", r)
	}
	tools := pluginTools(r.PluginCalls)
	if !reflect.DeepEqual(tools, []string{"forge_describe", "forge_policy", "forge_find_change", "forge_publish_draft"}) {
		t.Fatalf("plugin calls = %q", tools)
	}
	if last := r.PluginCalls[len(r.PluginCalls)-1]; last.Reason != mcp.SchemaInvalid || last.Plugin != "fake-forge" || last.ResultSHA256 == "" {
		t.Fatalf("publish record = %+v", last)
	}
	if persisted := publishCharacterizationPersisted(t, fx.o); !sameReceipt(persisted, r) {
		t.Fatal("receipt not persisted")
	}
	if left, _ := os.ReadDir(filepath.Join(lab, mcpadapter.RunRootName)); len(left) != 0 {
		t.Fatalf("%d plugin run directories left", len(left))
	}
}
