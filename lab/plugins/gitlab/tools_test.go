package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
	"github.com/DanielNetzer/forgecell/lab/plugins/internal/mcpserver"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
}

func forgeTool(t *testing.T, name string) *contract.Tool {
	t.Helper()
	c, _ := contract.Lookup("forge")
	tool, ok := c.Tool(name)
	if !ok {
		t.Fatalf("no contract tool %s", name)
	}
	return tool
}

// harness is the plugin's forge pointed at a fake GitLab.
type harness struct {
	fake *fakeGitLab
	f    *forge
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeGitLab(t)
	f := newForge(lookupFrom(fake.env(t)))
	if f.configErr != nil {
		t.Fatalf("config: %v", f.configErr)
	}
	return &harness{fake: fake, f: f}
}

// call runs a tool's bound handler, with schema validation and the contract
// timeout, as the server does.
func (h *harness) call(t *testing.T, name string, args map[string]any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return h.f.bind(forgeTool(t, name))(context.Background(), raw)
}

// decoded calls a tool, requires success and decodes the output as the engine
// does, so every output is proved valid under forge.v1.json.
func decoded[T any](t *testing.T, h *harness, name string, args map[string]any) T {
	t.Helper()
	out, err := h.call(t, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	v, err := forgeTool(t, name).Decode(raw)
	if err != nil {
		t.Fatalf("%s output %s is invalid: %v", name, raw, err)
	}
	return v.(T)
}

// failure requires a tool failure with the given reason and returns its message.
func failure(t *testing.T, err error, want mcpserver.Reason) string {
	t.Helper()
	var e *mcpserver.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not a tool failure", err)
	}
	if e.Reason() != want {
		t.Fatalf("reason %s (%v), want %s", e.Reason(), err, want)
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Fatalf("the error names the token: %v", err)
	}
	return err.Error()
}

func TestDescribeMapsTheProject(t *testing.T) {
	h := newHarness(t)
	for _, project := range []string{"example/project", projectID} {
		got := decoded[contract.ForgeDescription](t, h, "forge_describe", map[string]any{"project": project})
		host := strings.TrimPrefix(h.fake.srv.URL, "https://")
		want := contract.ForgeDescription{Identity: host + "/example/project", Host: host, DisplayName: "GitLab", ChangeNoun: "merge request", DefaultBranch: "main", CIKind: "gitlab-ci", WebURL: "https://gitlab.example/example/project"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %+v, want %+v", project, got, want)
		}
	}
	seen := h.fake.seen()
	if len(seen) != 2 || seen[0].Path != "/api/v4/projects/"+projectKey || seen[1].Path != "/api/v4/projects/"+projectID {
		t.Fatalf("requests %+v", seen)
	}
	for _, s := range seen {
		if s.Token != fakeToken || s.Authorization != "" {
			t.Fatalf("auth headers %+v, want only PRIVATE-TOKEN", s)
		}
	}
}

func TestDescribeRefusesAProjectWithNoDefaultBranch(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) { f.project["default_branch"] = nil })
	_, err := h.call(t, "forge_describe", map[string]any{"project": "example/project"})
	failure(t, err, mcpserver.Failed)
}

func TestPolicyMapsProtectionPipelineAndApprovals(t *testing.T) {
	h := newHarness(t)
	got := decoded[contract.ForgePolicy](t, h, "forge_policy", map[string]any{"project": "example/project", "branch": "main"})
	if !reflect.DeepEqual(got, contract.ForgePolicy{RequiredChecks: []string{"pipeline"}, RequiredReviews: 2, Protected: true}) {
		t.Fatalf("policy %+v", got)
	}
	h.fake.set(func(f *fakeGitLab) {
		f.project["only_allow_merge_if_pipeline_succeeds"] = false
		delete(f.project, "approvals_before_merge")
	})
	got = decoded[contract.ForgePolicy](t, h, "forge_policy", map[string]any{"project": "example/project", "branch": "release/1.x"})
	if !reflect.DeepEqual(got, contract.ForgePolicy{RequiredChecks: []string{}, RequiredReviews: 0, Protected: false}) {
		t.Fatalf("policy %+v, want an unprotected branch with no requirements", got)
	}
	if p := h.fake.seen()[3].Path; p != "/api/v4/projects/"+projectKey+"/protected_branches/release%2F1.x" {
		t.Fatalf("protected branch lookup %s", p)
	}
}

func TestPolicyFailsOnOtherProtectionErrorsAndAMissingProject(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) { f.status, f.statusPath = 500, "/protected_branches/main" })
	_, err := h.call(t, "forge_policy", map[string]any{"project": "example/project", "branch": "main"})
	failure(t, err, mcpserver.Failed)
	h.fake.set(func(f *fakeGitLab) { f.status = 0 })
	_, err = h.call(t, "forge_policy", map[string]any{"project": "missing/project", "branch": "main"})
	if msg := failure(t, err, mcpserver.Failed); !strings.Contains(msg, "HTTP 404") {
		t.Fatalf("message %q", msg)
	}
	h.fake.set(func(f *fakeGitLab) { f.project["approvals_before_merge"] = 101 })
	_, err = h.call(t, "forge_policy", map[string]any{"project": "example/project", "branch": "main"})
	failure(t, err, mcpserver.Failed)
}

func TestFindChangeListsOpenMergeRequestsFromTheProject(t *testing.T) {
	h := newHarness(t)
	got := decoded[contract.ForgeChanges](t, h, "forge_find_change", map[string]any{"project": "example/project", "headBranch": "forgecell/x"})
	want := []contract.ForgeChangeRef{{Ref: "!7", URL: "https://gitlab.example/example/project/-/merge_requests/7", State: "open", Draft: true, HeadCommit: headCommit}}
	if !reflect.DeepEqual(got.Changes, want) {
		t.Fatalf("changes %+v, want only the project's own merge request", got.Changes)
	}
	q := h.fake.seen()[0].Query
	if !strings.Contains(q, "source_branch=forgecell%2Fx") || !strings.Contains(q, "state=opened") {
		t.Fatalf("query %s", q)
	}
	none := decoded[contract.ForgeChanges](t, h, "forge_find_change", map[string]any{"project": "example/project", "headBranch": "nothing"})
	if none.Changes == nil || len(none.Changes) != 0 {
		t.Fatalf("changes %+v", none.Changes)
	}
}

func TestFindChangeFollowsPagesWithinTheBound(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) {
		f.pageSizeCap = 1
		f.mrs = append(f.mrs, map[string]any{"iid": 10, "state": "opened", "draft": false, "sha": otherSHA, "web_url": "https://gitlab.example/10", "source_branch": "forgecell/x", "source_project_id": 42, "target_project_id": 42})
	})
	got := decoded[contract.ForgeChanges](t, h, "forge_find_change", map[string]any{"project": "example/project", "headBranch": "forgecell/x"})
	if len(got.Changes) != 2 || got.Changes[0].Ref != "!7" || got.Changes[1].Ref != "!10" {
		t.Fatalf("changes %+v", got.Changes)
	}
	if n := len(h.fake.seen()); n != 3 {
		t.Fatalf("%d requests, want 3 pages", n)
	}
}

func TestListingsStopAtFivePagesAndFailSafe(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) { f.endless = true })
	_, err := h.call(t, "forge_find_change", map[string]any{"project": "example/project", "headBranch": "forgecell/x"})
	failure(t, err, mcpserver.Failed)
	if n := len(h.fake.seen()); n != maxPages {
		t.Fatalf("%d requests, want %d", n, maxPages)
	}
	_, err = h.call(t, "forge_comments", map[string]any{"project": "example/project", "ref": "!7"})
	failure(t, err, mcpserver.Failed)
	got := decoded[contract.ForgeChecks](t, h, "forge_checks", map[string]any{"project": "example/project", "ref": "!7", "commit": headCommit})
	if got.Complete || len(got.Checks) == 0 {
		t.Fatalf("checks %+v, want reported checks that are not complete", got)
	}
}

func TestAPageLargerThanAskedForFails(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) { f.bigPage = true })
	_, err := h.call(t, "forge_comments", map[string]any{"project": "example/project", "ref": "!7"})
	failure(t, err, mcpserver.Failed)
}

func TestPublishDraftPrefixesTheTitleOnce(t *testing.T) {
	cases := map[string]string{
		"Add the thing":        "Draft: Add the thing",
		"Draft: Add the thing": "Draft: Add the thing",
		"draft: lower case":    "draft: lower case",
		"[Draft] bracketed":    "[Draft] bracketed",
		"  Spaced title \t":    "Draft: Spaced title",
		"  Draft: indented":    "Draft: indented",
	}
	for title, want := range cases {
		h := newHarness(t)
		got := decoded[contract.ForgePublished](t, h, "forge_publish_draft", map[string]any{"project": "example/project", "headBranch": "forgecell/x", "base": "main", "title": title, "body": "Body text", "commit": headCommit})
		if !reflect.DeepEqual(got, contract.ForgePublished{Ref: "!12", URL: "https://gitlab.example/example/project/-/merge_requests/12", Draft: true, HeadCommit: headCommit}) {
			t.Fatalf("%q: %+v", title, got)
		}
		seen := h.fake.seen()
		if len(seen) != 1 || seen[0].Method != http.MethodPost || seen[0].Path != "/api/v4/projects/"+projectKey+"/merge_requests" {
			t.Fatalf("requests %+v", seen)
		}
		wantBody := map[string]any{"source_branch": "forgecell/x", "target_branch": "main", "title": want, "description": "Body text"}
		if !reflect.DeepEqual(seen[0].Body, wantBody) {
			t.Fatalf("%q: body %+v, want %+v", title, seen[0].Body, wantBody)
		}
	}
}

func TestPublishDraftNeutralizesQuickActions(t *testing.T) {
	h := newHarness(t)
	body := "/ready\nSummary line\n  /merge\n\t/assign @me\nnot /a command\n\n/approve"
	decoded[contract.ForgePublished](t, h, "forge_publish_draft", map[string]any{"project": "example/project", "headBranch": "forgecell/x", "base": "main", "title": "T", "body": body, "commit": headCommit})
	want := "\\/ready\nSummary line\n  \\/merge\n\t\\/assign @me\nnot /a command\n\n\\/approve"
	if got := h.fake.seen()[0].Body["description"]; got != want {
		t.Fatalf("description %q, want %q", got, want)
	}
	if got := neutralizeQuickActions(""); got != "" {
		t.Fatalf("empty body became %q", got)
	}
}

func TestPublishDraftRefusesABlankTitle(t *testing.T) {
	h := newHarness(t)
	_, err := h.call(t, "forge_publish_draft", map[string]any{"project": "example/project", "headBranch": "forgecell/x", "base": "main", "title": "   ", "body": "", "commit": headCommit})
	failure(t, err, mcpserver.Failed)
	if n := len(h.fake.seen()); n != 0 {
		t.Fatalf("%d requests, want none", n)
	}
}

// TestPublishDraftRecoversALostAnswer proves a merge request GitLab created is
// reported even when the create answer is lost.
func TestPublishDraftRecoversALostAnswer(t *testing.T) {
	args := map[string]any{"project": "example/project", "headBranch": "forgecell/new", "base": "main", "title": "T", "body": "", "commit": headCommit}
	for _, fault := range []string{"hang", "malformed", "oversized"} {
		fake := newFakeGitLab(t)
		cfg, err := loadConfig(lookupFrom(fake.env(t)))
		if err != nil {
			t.Fatal(err)
		}
		if fault == "hang" {
			cfg.timeout = 50 * time.Millisecond
		}
		h := &harness{fake: fake, f: &forge{c: newClient(cfg)}}
		fake.set(func(f *fakeGitLab) { f.postFault = fault })
		got := decoded[contract.ForgePublished](t, h, "forge_publish_draft", args)
		if !reflect.DeepEqual(got, contract.ForgePublished{Ref: "!12", URL: "https://gitlab.example/example/project/-/merge_requests/12", Draft: true, HeadCommit: headCommit}) {
			t.Fatalf("%s: %+v", fault, got)
		}
		seen := fake.seen()
		if len(seen) != 2 || seen[1].Method != http.MethodGet || !strings.Contains(seen[1].Query, "source_branch=forgecell%2Fnew") || !strings.Contains(seen[1].Query, "state=opened") {
			t.Fatalf("%s: requests %+v, want the create and one lookup", fault, seen)
		}
	}
}

func TestPublishDraftRecoveryFailsUnlessExactlyOneDraftAtTheCommit(t *testing.T) {
	cases := map[string]struct {
		head, commit, title string
		change              func(f *fakeGitLab)
	}{
		// !7 is already open on forgecell/x, so two match.
		"two open": {"forgecell/x", headCommit, "T", func(f *fakeGitLab) { f.postFault = "malformed" }},
		// Nothing was created, and the lookup answer is malformed too.
		"none": {"forgecell/new", headCommit, "T", func(f *fakeGitLab) { f.badJSON = true }},
		// GitLab reports a head other than the approved commit.
		"other commit": {"forgecell/new", otherSHA, "T", func(f *fakeGitLab) { f.postFault = "malformed" }},
		// GitLab answered and lists a ready merge request, not a draft.
		"not a draft": {"forgecell/new", headCommit, "T", func(f *fakeGitLab) {
			f.created = map[string]any{"iid": 12, "draft": false, "sha": headCommit, "web_url": "https://gitlab.example/12"}
			f.mrs = append(f.mrs, map[string]any{"iid": 12, "state": "opened", "draft": false, "sha": headCommit, "web_url": "https://gitlab.example/12", "source_branch": "forgecell/new", "target_branch": "main", "source_project_id": 42, "target_project_id": 42})
		}},
		"lookup refused": {"forgecell/new", headCommit, "T", func(f *fakeGitLab) {
			f.postFault, f.status, f.statusPath = "malformed", 429, "/merge_requests"
		}},
	}
	for name, c := range cases {
		h := newHarness(t)
		h.fake.set(c.change)
		_, err := h.call(t, "forge_publish_draft", map[string]any{"project": "example/project", "headBranch": c.head, "base": "main", "title": c.title, "body": "", "commit": c.commit})
		// The original failure is reported, never the lookup's.
		failure(t, err, mcpserver.Failed)
		if n := len(h.fake.seen()); n != 2 {
			t.Fatalf("%s: %d requests, want the create and one lookup", name, n)
		}
	}
	// A create refused by status is never looked up.
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) { f.status = 409 })
	_, err := h.call(t, "forge_publish_draft", map[string]any{"project": "example/project", "headBranch": "forgecell/new", "base": "main", "title": "T", "body": "", "commit": headCommit})
	failure(t, err, mcpserver.Failed)
	if n := len(h.fake.seen()); n != 1 {
		t.Fatalf("%d requests, want only the refused create", n)
	}
}

func TestPublishDraftReturnsTheShaGitLabReports(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) {
		f.created = map[string]any{"iid": 12, "draft": true, "sha": otherSHA, "web_url": "https://gitlab.example/12"}
	})
	got := decoded[contract.ForgePublished](t, h, "forge_publish_draft", map[string]any{"project": "example/project", "headBranch": "forgecell/x", "base": "main", "title": "T", "body": "", "commit": headCommit})
	if got.HeadCommit != otherSHA {
		t.Fatalf("head commit %s, want GitLab's %s", got.HeadCommit, otherSHA)
	}
}

func TestPublishDraftRefusesALongTitleAndANonDraft(t *testing.T) {
	h := newHarness(t)
	_, err := h.call(t, "forge_publish_draft", map[string]any{"project": "example/project", "headBranch": "forgecell/x", "base": "main", "title": strings.Repeat("é", 249), "body": "", "commit": headCommit})
	failure(t, err, mcpserver.Failed)
	if n := len(h.fake.seen()); n != 0 {
		t.Fatalf("%d requests, want none for a title over 255 characters", n)
	}
	// "Draft: " plus 248 characters is exactly 255.
	decoded[contract.ForgePublished](t, h, "forge_publish_draft", map[string]any{"project": "example/project", "headBranch": "forgecell/x", "base": "main", "title": strings.Repeat("é", 248), "body": "", "commit": headCommit})
	h.fake.set(func(f *fakeGitLab) {
		f.created = map[string]any{"iid": 12, "draft": false, "sha": headCommit, "web_url": "https://gitlab.example/12"}
	})
	_, err = h.call(t, "forge_publish_draft", map[string]any{"project": "example/project", "headBranch": "forgecell/x", "base": "main", "title": "T", "body": "", "commit": headCommit})
	failure(t, err, mcpserver.Failed)
}

func TestChangeMapsStatesAndMergeDetails(t *testing.T) {
	h := newHarness(t)
	got := decoded[contract.ForgeChange](t, h, "forge_change", map[string]any{"project": "example/project", "ref": "!9"})
	if got.State != "merged" || got.MergedAt == nil || *got.MergedAt != "2026-10-30T12:34:56.789Z" || got.MergeCommit == nil || *got.MergeCommit != mergeSHA || got.HeadCommit != headCommit {
		t.Fatalf("merged change %+v", got)
	}
	for state, want := range map[string]string{"opened": "open", "locked": "open", "closed": "closed"} {
		h.fake.set(func(f *fakeGitLab) { f.mrs[2]["state"] = state })
		got := decoded[contract.ForgeChange](t, h, "forge_change", map[string]any{"project": "example/project", "ref": "!9"})
		if got.State != want || got.MergedAt != nil || got.MergeCommit != nil {
			t.Fatalf("%s: %+v, want %s with no merge details", state, got, want)
		}
	}
	h.fake.set(func(f *fakeGitLab) { f.mrs[2]["state"] = "reopened-ish" })
	_, err := h.call(t, "forge_change", map[string]any{"project": "example/project", "ref": "!9"})
	failure(t, err, mcpserver.Failed)
	_, err = h.call(t, "forge_change", map[string]any{"project": "example/project", "ref": "!404"})
	if msg := failure(t, err, mcpserver.Failed); !strings.Contains(msg, "HTTP 404") {
		t.Fatalf("message %q", msg)
	}
}

func TestChecksReportTheNewestPipelineForTheCommit(t *testing.T) {
	h := newHarness(t)
	got := decoded[contract.ForgeChecks](t, h, "forge_checks", map[string]any{"project": "example/project", "ref": "!7", "commit": headCommit})
	url := func(s string) *string { return &s }
	want := contract.ForgeChecks{Complete: true, Checks: []contract.ForgeCheck{
		{Name: "build", Status: "completed", Conclusion: "success", URL: url("https://gitlab.example/example/project/-/jobs/1")},
		{Name: "test", Status: "completed", Conclusion: "failure", URL: url("https://gitlab.example/example/project/-/jobs/3")},
		{Name: "deploy", Status: "queued", Conclusion: ""},
		{Name: "pipeline", Status: "completed", Conclusion: "failure", URL: url("https://gitlab.example/example/project/-/pipelines/103")},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checks %+v, want %+v", got, want)
	}
	if p := h.fake.seen()[1].Path; p != "/api/v4/projects/"+projectKey+"/pipelines/103/jobs" {
		t.Fatalf("jobs path %s", p)
	}
}

func TestChecksWithNoPipelineForTheCommitAreIncomplete(t *testing.T) {
	h := newHarness(t)
	got := decoded[contract.ForgeChecks](t, h, "forge_checks", map[string]any{"project": "example/project", "ref": "!7", "commit": mergeSHA})
	if got.Complete || got.Checks == nil || len(got.Checks) != 0 {
		t.Fatalf("checks %+v, want none and not complete", got)
	}
}

func TestChecksAreIncompleteWhileThePipelineRuns(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) {
		f.pipelines["7"][1]["status"] = "manual"
		f.jobs["103"] = []map[string]any{{"id": 1, "name": "pipeline", "status": "success"}}
	})
	got := decoded[contract.ForgeChecks](t, h, "forge_checks", map[string]any{"project": "example/project", "ref": "!7", "commit": headCommit})
	if got.Complete || len(got.Checks) != 2 || got.Checks[0].Name != "pipeline (job)" || got.Checks[1].Name != "pipeline" || got.Checks[1].Status != "queued" || got.Checks[1].Conclusion != "" {
		t.Fatalf("checks %+v", got)
	}
}

func TestChecksCutAtTheOutputBoundAreIncomplete(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) {
		jobs := []map[string]any{}
		for i := 1; i <= 250; i++ {
			jobs = append(jobs, map[string]any{"id": i, "name": "job", "status": "success"})
		}
		f.jobs["103"] = jobs
	})
	got := decoded[contract.ForgeChecks](t, h, "forge_checks", map[string]any{"project": "example/project", "ref": "!7", "commit": headCommit})
	if got.Complete || len(got.Checks) != maxChecks || got.Checks[maxChecks-1].Name != "pipeline" {
		t.Fatalf("%d checks, complete %v", len(got.Checks), got.Complete)
	}
}

func TestChecksReportAnAllowedFailureAsNeutralAndCutNamesOnOneLine(t *testing.T) {
	h := newHarness(t)
	long := strings.Repeat("n", maxNameRunes+5)
	h.fake.set(func(f *fakeGitLab) {
		f.jobs["103"] = []map[string]any{
			{"id": 1, "name": "lint", "status": "failed", "allow_failure": true},
			{"id": 2, "name": "test", "status": "failed", "allow_failure": false},
			{"id": 3, "name": "flaky", "status": "success", "allow_failure": true},
			{"id": 4, "name": long, "status": "success"},
		}
	})
	got := decoded[contract.ForgeChecks](t, h, "forge_checks", map[string]any{"project": "example/project", "ref": "!7", "commit": headCommit})
	conclusions := []string{}
	for _, c := range got.Checks {
		conclusions = append(conclusions, c.Conclusion)
	}
	if !reflect.DeepEqual(conclusions, []string{"neutral", "failure", "success", "success", "failure"}) {
		t.Fatalf("conclusions %v", conclusions)
	}
	name := got.Checks[3].Name
	if utf8.RuneCountInString(name) != maxNameRunes || !strings.HasSuffix(name, "…") || strings.Contains(name, "\n") {
		t.Fatalf("cut name %q", name)
	}
}

func TestJobStatusMapping(t *testing.T) {
	cases := []struct{ gitlab, status, conclusion string }{
		{"created", "queued", ""},
		{"pending", "queued", ""},
		{"preparing", "queued", ""},
		{"waiting_for_resource", "queued", ""},
		{"scheduled", "queued", ""},
		{"manual", "queued", ""},
		{"running", "in_progress", ""},
		{"success", "completed", "success"},
		{"failed", "completed", "failure"},
		{"canceled", "completed", "cancelled"},
		{"skipped", "completed", "skipped"},
		{"canceling", "queued", ""},
		{"", "queued", ""},
	}
	for _, c := range cases {
		status, conclusion := jobStatus(c.gitlab)
		if status != c.status || conclusion != c.conclusion {
			t.Errorf("%q: %s/%q, want %s/%q", c.gitlab, status, conclusion, c.status, c.conclusion)
		}
		if terminal(c.gitlab) != (c.status == "completed") {
			t.Errorf("%q: terminal %v", c.gitlab, terminal(c.gitlab))
		}
	}
}

func TestCommentsSkipSystemNotesAndApplySince(t *testing.T) {
	h := newHarness(t)
	got := decoded[contract.ForgeComments](t, h, "forge_comments", map[string]any{"project": "example/project", "ref": "!7"})
	path, line := "src/main.go", int64(12)
	want := []contract.ForgeComment{
		{ID: "2", Author: "alice", IsBot: false, Body: "Looks good", CreatedAt: "2026-10-30T11:00:00Z"},
		{ID: "3", Author: "review-bot", IsBot: true, Body: "Nit here", Path: &path, Line: &line, CreatedAt: "2026-10-30T12:00:00Z"},
	}
	if !reflect.DeepEqual(got.Comments, want) {
		t.Fatalf("comments %+v, want %+v", got.Comments, want)
	}
	if q := h.fake.seen()[0].Query; !strings.Contains(q, "sort=asc") || !strings.Contains(q, "per_page=20") {
		t.Fatalf("query %s", q)
	}
	got = decoded[contract.ForgeComments](t, h, "forge_comments", map[string]any{"project": "example/project", "ref": "!7", "since": "2026-10-30T11:30:00Z"})
	if len(got.Comments) != 1 || got.Comments[0].ID != "3" {
		t.Fatalf("comments since %+v", got.Comments)
	}
}

func TestCommentsRecognizeBotAccounts(t *testing.T) {
	h := newHarness(t)
	users := []string{"project_42_bot_0a1b2c", "group_7_bot", "project_3_bot", "ghost", "deploy-bot", "ci_bot", "alice", "botanist", "project_x_bot_zz", "robot"}
	h.fake.set(func(f *fakeGitLab) {
		notes := []map[string]any{}
		for i, u := range users {
			// No bot key: the username alone decides.
			notes = append(notes, map[string]any{"id": i + 1, "body": "b", "system": false, "created_at": "2026-10-30T10:00:00Z", "author": map[string]any{"username": u}})
		}
		f.notes["7"] = notes
	})
	got := decoded[contract.ForgeComments](t, h, "forge_comments", map[string]any{"project": "example/project", "ref": "!7"})
	bots := map[string]bool{}
	for _, c := range got.Comments {
		bots[c.Author] = c.IsBot
	}
	want := map[string]bool{"project_42_bot_0a1b2c": true, "group_7_bot": true, "project_3_bot": true, "ghost": true, "deploy-bot": true, "ci_bot": true, "alice": false, "botanist": false, "project_x_bot_zz": false, "robot": false}
	if !reflect.DeepEqual(bots, want) {
		t.Fatalf("bots %v, want %v", bots, want)
	}
}

func TestCommentsReadUpTo500NotesInPagesOf20(t *testing.T) {
	h := newHarness(t)
	notes := func(n int) []map[string]any {
		out := []map[string]any{}
		for i := 1; i <= n; i++ {
			out = append(out, map[string]any{"id": i, "body": "b", "system": false, "created_at": "2026-10-30T10:00:00Z", "author": map[string]any{"username": "alice"}})
		}
		return out
	}
	h.fake.set(func(f *fakeGitLab) { f.notes["7"] = notes(500) })
	got := decoded[contract.ForgeComments](t, h, "forge_comments", map[string]any{"project": "example/project", "ref": "!7"})
	if len(got.Comments) != 500 || len(h.fake.seen()) != maxNotePages {
		t.Fatalf("%d comments in %d requests", len(got.Comments), len(h.fake.seen()))
	}
	h.fake.set(func(f *fakeGitLab) { f.notes["7"], f.requests = notes(501), nil })
	_, err := h.call(t, "forge_comments", map[string]any{"project": "example/project", "ref": "!7"})
	failure(t, err, mcpserver.Failed)
	if n := len(h.fake.seen()); n != maxNotePages {
		t.Fatalf("%d requests, want %d", n, maxNotePages)
	}
}

func TestCommentsCutALongAuthorOnOneLine(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) {
		f.notes["7"][1]["author"] = map[string]any{"username": strings.Repeat("u", maxAuthorRunes+3)}
	})
	got := decoded[contract.ForgeComments](t, h, "forge_comments", map[string]any{"project": "example/project", "ref": "!7"})
	a := got.Comments[0].Author
	if utf8.RuneCountInString(a) != maxAuthorRunes || !strings.HasSuffix(a, "…") || strings.Contains(a, "\n") {
		t.Fatalf("cut author %q", a)
	}
}

func TestCommentsMarkACutBody(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeGitLab) { f.notes["7"][1]["body"] = strings.Repeat("a", maxBodyRunes+10) })
	got := decoded[contract.ForgeComments](t, h, "forge_comments", map[string]any{"project": "example/project", "ref": "!7"})
	if b := got.Comments[0].Body; len(b) != maxBodyRunes || !strings.HasSuffix(b, "[truncated]") {
		t.Fatalf("body of %d bytes", len(b))
	}
}

func TestRefsOutsideThePatternAreRefusedBeforeAnyRequest(t *testing.T) {
	h := newHarness(t)
	for _, ref := range []string{"7", "!0", "!07", "!12345678901", "#7", "!7 ", "!-7", "!7a", "!"} {
		for _, name := range []string{"forge_change", "forge_checks", "forge_comments"} {
			args := map[string]any{"project": "example/project", "ref": ref}
			if name == "forge_checks" {
				args["commit"] = headCommit
			}
			_, err := h.call(t, name, args)
			failure(t, err, mcpserver.Failed)
		}
	}
	decoded[contract.ForgeChange](t, h, "forge_change", map[string]any{"project": "example/project", "ref": "!9"})
	if n := len(h.fake.seen()); n != 1 {
		t.Fatalf("%d requests, want only the valid ref's", n)
	}
}

func TestInvalidProjectsAndBranchesAreRefusedBeforeAnyRequest(t *testing.T) {
	h := newHarness(t)
	for _, project := range []string{"../etc", "example/..", "example", "a//b", "/a/b", "a/b?x", "0", "a/b%2F"} {
		_, err := h.call(t, "forge_describe", map[string]any{"project": project})
		failure(t, err, mcpserver.Failed)
	}
	for _, branch := range []string{"..", "a..b", "-x", "a b", "x.lock", "a/", "@", "a@{1}", "a:b", ".hidden", "a/.b"} {
		_, err := h.call(t, "forge_policy", map[string]any{"project": "example/project", "branch": branch})
		failure(t, err, mcpserver.Failed)
	}
	if _, err := h.call(t, "forge_describe", map[string]any{"project": 12345}); err == nil {
		t.Fatal("a malformed argument was accepted")
	}
	if n := len(h.fake.seen()); n != 0 {
		t.Fatalf("%d requests, want none", n)
	}
}

func TestHTTPErrorsMapToFixedReasons(t *testing.T) {
	cases := []struct {
		status int
		token  bool
		want   mcpserver.Reason
	}{
		{401, true, mcpserver.AuthMissing},
		{401, false, mcpserver.AuthMissing},
		{403, false, mcpserver.AuthMissing},
		{403, true, mcpserver.Failed},
		{429, true, mcpserver.RateLimited},
		{404, true, mcpserver.Failed},
		{500, true, mcpserver.Failed},
		{502, true, mcpserver.Failed},
		{409, true, mcpserver.Failed},
	}
	for _, c := range cases {
		fake := newFakeGitLab(t)
		env := fake.env(t)
		if !c.token {
			delete(env, tokenEnv)
		}
		h := &harness{fake: fake, f: newForge(lookupFrom(env))}
		fake.set(func(f *fakeGitLab) { f.status = c.status })
		_, err := h.call(t, "forge_describe", map[string]any{"project": "example/project"})
		msg := failure(t, err, c.want)
		if !strings.Contains(msg, "HTTP "+strconv.Itoa(c.status)) || strings.Contains(msg, "denied") {
			t.Fatalf("%d: message %q, want the status and never the body", c.status, msg)
		}
		if seen := fake.seen(); !c.token && seen[0].Token != "" {
			t.Fatalf("a token was sent with none configured")
		}
	}
}

func TestResponseFailuresFailSafe(t *testing.T) {
	switches := map[string]func(f *fakeGitLab){
		"redirect":  func(f *fakeGitLab) { f.redirect = true },
		"oversized": func(f *fakeGitLab) { f.oversized = true },
		"bad json":  func(f *fakeGitLab) { f.badJSON = true },
		"duplicate": func(f *fakeGitLab) { f.duplicate = true },
		"not json":  func(f *fakeGitLab) { f.wrongType = true },
	}
	for name, change := range switches {
		h := newHarness(t)
		h.fake.set(change)
		_, err := h.call(t, "forge_describe", map[string]any{"project": "example/project"})
		failure(t, err, mcpserver.Failed)
		if name == "redirect" {
			if n := len(h.fake.seen()); n != 1 {
				t.Fatalf("redirect: %d requests, want the redirect left unfollowed", n)
			}
		}
	}
}

func TestTransportErrorsAndTimeoutsAreNetwork(t *testing.T) {
	closed := newFakeGitLab(t)
	env := closed.env(t)
	closed.srv.Close()
	f := newForge(lookupFrom(env))
	_, err := (&harness{fake: closed, f: f}).call(t, "forge_describe", map[string]any{"project": "example/project"})
	failure(t, err, mcpserver.Network)

	hung := newFakeGitLab(t)
	cfg, err := loadConfig(lookupFrom(hung.env(t)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.timeout = 50 * time.Millisecond
	hung.set(func(f *fakeGitLab) { f.hang = true })
	_, err = (&harness{fake: hung, f: &forge{c: newClient(cfg)}}).call(t, "forge_describe", map[string]any{"project": "example/project"})
	failure(t, err, mcpserver.Network)
}

func TestTLSVerificationIsNeverSkipped(t *testing.T) {
	fake := newFakeGitLab(t)
	env := fake.env(t)
	delete(env, caFileEnv)
	h := &harness{fake: fake, f: newForge(lookupFrom(env))}
	_, err := h.call(t, "forge_describe", map[string]any{"project": "example/project"})
	failure(t, err, mcpserver.Network)
	if n := len(fake.seen()); n != 0 {
		t.Fatalf("%d requests reached an untrusted server", n)
	}
}

func TestBaseURLRules(t *testing.T) {
	for _, raw := range []string{
		"http://gitlab.com", "http://127.0.0.1:8080", "https://gitlab.com/gitlab", "https://gitlab.com?x=1", "https://gitlab.com/?",
		"https://gitlab.com#f", "https://user:pw@gitlab.com", "https://GitLab.com", "https:gitlab.com", "https://", "ftp://gitlab.com",
		"https://gitlab.com:0", "https://gitlab.com:99999", "https://[::1]:443", "gitlab.com", "https://gitlab.com/%2e",
	} {
		if _, _, err := parseBaseURL(raw); err == nil {
			t.Errorf("%q was accepted", raw)
		} else if strings.Contains(err.Error(), raw) {
			t.Errorf("the error names the value: %v", err)
		}
	}
	for raw, want := range map[string]string{"https://gitlab.com": "gitlab.com", "https://gitlab.com/": "gitlab.com", "https://git.internal.example:8443": "git.internal.example:8443", "https://127.0.0.1:4443": "127.0.0.1:4443"} {
		base, host, err := parseBaseURL(raw)
		if err != nil || host != want || base != "https://"+want {
			t.Errorf("%q: %q %q %v", raw, base, host, err)
		}
	}
	cfg, err := loadConfig(lookupFrom(map[string]string{}))
	if err != nil || cfg.base != "https://gitlab.com" || cfg.host != "gitlab.com" || cfg.token != "" {
		t.Fatalf("default config %+v %v", cfg, err)
	}
	if _, err := loadConfig(lookupFrom(map[string]string{tokenEnv: "bad token\n"})); err == nil {
		t.Fatal("a token with control characters was accepted")
	}
}

func TestCAFileRules(t *testing.T) {
	fake := newFakeGitLab(t)
	good := fake.caFile(t)
	dir := plugintest.TempDir(t)
	write := func(name string, raw []byte, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	goodPEM, _ := os.ReadFile(good)
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		"missing":        filepath.Join(dir, "missing.pem"),
		"relative":       "gitlab-ca.pem",
		"unclean":        dir + "/./x/../gitlab-ca.pem",
		"symlink":        link,
		"empty":          write("empty.pem", nil, 0o600),
		"no certificate": write("text.pem", []byte("not a pem bundle\n"), 0o600),
		"a key":          write("key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2, 3}}), 0o600),
		"bad cert":       write("badcert.pem", append(goodPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}})...), 0o600),
		"too large":      write("large.pem", append(bytes.Repeat([]byte("#"), maxCAFileBytes), goodPEM...), 0o600),
		"group writable": write("open.pem", goodPEM, 0o620),
	}
	for name, path := range bad {
		_, err := loadConfig(lookupFrom(map[string]string{caFileEnv: path}))
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), dir) {
			t.Errorf("%s: the error names a local path: %v", name, err)
		}
	}
	bundle := write("bundle.pem", append([]byte("# a comment\n"), goodPEM...), 0o600)
	cfg, err := loadConfig(lookupFrom(map[string]string{caFileEnv: bundle, urlEnv: fake.srv.URL}))
	if err != nil {
		t.Fatalf("a valid bundle was refused: %v", err)
	}
	resp, err := newClient(cfg).http.Get(fake.srv.URL + "/api/v4/projects/" + projectID)
	if err != nil {
		t.Fatalf("the added certificate is not trusted: %v", err)
	}
	resp.Body.Close()
}

func TestAnInvalidConfigurationFailsEveryCallBeforeAnyRequest(t *testing.T) {
	fake := newFakeGitLab(t)
	env := fake.env(t)
	env[urlEnv] = strings.Replace(fake.srv.URL, "https://", "http://", 1)
	f := newForge(lookupFrom(env))
	if f.configErr == nil {
		t.Fatal("plain http was accepted")
	}
	c, _ := contract.Lookup("forge")
	for i := range c.Tools {
		tool := &c.Tools[i]
		args := map[string]any{"project": "example/project", "ref": "!7", "branch": "main", "headBranch": "x", "base": "main", "title": "T", "body": "", "commit": headCommit}
		in := map[string]any{}
		for name := range tool.Input.Properties {
			if v, ok := args[name]; ok {
				in[name] = v
			}
		}
		raw, _ := json.Marshal(in)
		_, err := f.bind(tool)(context.Background(), raw)
		if msg := failure(t, err, mcpserver.Failed); !strings.Contains(msg, "GITLAB_URL") {
			t.Fatalf("%s: message %q", tool.Name, msg)
		}
	}
	if n := len(fake.seen()); n != 0 {
		t.Fatalf("%d requests, want none", n)
	}
}

// TestTheTokenIsRedactedFromLogsAndErrors serves real MCP requests in process
// while GitLab answers with a body that carries the token.
func TestTheTokenIsRedactedFromLogsAndErrors(t *testing.T) {
	fake := newFakeGitLab(t)
	env := fake.env(t)
	t.Setenv(tokenEnv, fakeToken)
	t.Setenv(urlEnv, env[urlEnv])
	t.Setenv(caFileEnv, env[caFileEnv])
	var stderr, out bytes.Buffer
	s := newServer(mcpserver.Env, &stderr)
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"forge_describe","arguments":{"project":"example/project"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"forge_change","arguments":{"project":"example/project","ref":"` + fakeToken + `"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"forge_describe","arguments":{"project":"example/project"}}}`,
	}
	in := strings.NewReader(strings.Join(lines[:4], "\n") + "\n")
	fake.set(func(f *fakeGitLab) { f.status = 500 })
	if err := s.Serve(context.Background(), in, &out); err != nil {
		t.Fatal(err)
	}
	fake.set(func(f *fakeGitLab) { f.status = 401 })
	if err := s.Serve(context.Background(), strings.NewReader(lines[4]+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	for label, text := range map[string]string{"stdout": out.String(), "stderr": stderr.String()} {
		if strings.Contains(text, fakeToken) || strings.Contains(text, "denied for token") {
			t.Fatalf("%s carries the token or the body: %s", label, text)
		}
	}
	var replies []map[string]any
	sc := bufio.NewScanner(&out)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		replies = append(replies, m)
	}
	if len(replies) != 4 {
		t.Fatalf("%d replies: %s", len(replies), out.String())
	}
	reasons := []string{}
	for _, r := range replies[1:] {
		result, _ := r["result"].(map[string]any)
		structured, _ := result["structuredContent"].(map[string]any)
		reasons = append(reasons, structured["reason"].(string))
	}
	if !reflect.DeepEqual(reasons, []string{"failed", "failed", "auth-missing"}) {
		t.Fatalf("reasons %v", reasons)
	}
	for _, seen := range fake.seen() {
		if seen.Token != fakeToken || seen.Authorization != "" {
			t.Fatalf("headers %+v", seen)
		}
	}
	if !strings.Contains(stderr.String(), "HTTP 500") {
		t.Fatalf("stderr %q does not name the status", stderr.String())
	}
}

// renderManifest fills plugin.json.tmpl for a server binary and checks it
// decodes as a plugin manifest.
func renderManifest(t *testing.T, server string, network ...string) []byte {
	t.Helper()
	tmpl, err := os.ReadFile("plugin.json.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(server)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	quoted, _ := json.Marshal(server)
	out := strings.ReplaceAll(string(tmpl), `"{{SERVER_PATH}}"`, string(quoted))
	out = strings.ReplaceAll(out, "{{SERVER_SHA256}}", hex.EncodeToString(sum[:]))
	if len(network) > 0 {
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatal(err)
		}
		m["needs"].(map[string]any)["network"] = network
		b, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		out = string(b)
	}
	return []byte(out)
}

func TestTheManifestTemplateDeclaresTheContract(t *testing.T) {
	server := filepath.Join(plugintest.TempDir(t), "forgecell-gitlab")
	if err := os.WriteFile(server, []byte("stand-in"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := plugin.Decode(renderManifest(t, server))
	if err != nil {
		t.Fatalf("the rendered template is not a valid manifest: %v", err)
	}
	if m.Kind != "forge" || m.Contract != "forgecell.forge/v1" || m.Name != "gitlab" || len(m.Server.Args) != 0 {
		t.Fatalf("manifest %+v", m)
	}
	if !reflect.DeepEqual(m.Needs.Env, []string{tokenEnv, urlEnv, caFileEnv}) || !reflect.DeepEqual(m.Needs.Network, []string{"gitlab.com"}) {
		t.Fatalf("needs %+v", m.Needs)
	}
	c, _ := contract.Lookup("forge")
	if len(m.Tools) != len(c.Tools) {
		t.Fatalf("tools %+v", m.Tools)
	}
	for i, tool := range c.Tools {
		if m.Tools[i].Name != tool.Name || m.Tools[i].Access != plugin.AccessFor(tool.Access) {
			t.Fatalf("tool %+v, want %s %s", m.Tools[i], tool.Name, plugin.AccessFor(tool.Access))
		}
	}
}
