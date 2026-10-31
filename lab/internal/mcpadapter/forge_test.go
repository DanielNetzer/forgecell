package mcpadapter

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

func TestForgeServesEveryTool(t *testing.T) {
	f := newFixture(t, "forge", "good")
	fg := newTestForge(t, f)
	ctx := context.Background()

	d, err := fg.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, contract.ForgeDescription{Identity: "forge.example/example/project", Host: "forge.example", DisplayName: "Fake forge", ChangeNoun: "change request", DefaultBranch: "main", CIKind: "none", WebURL: "https://forge.example/example/project"}) {
		t.Fatalf("Describe = %+v", d)
	}
	policy, err := fg.Policy(ctx, "main")
	if err != nil || !reflect.DeepEqual(policy, contract.ForgePolicy{RequiredChecks: []string{}}) {
		t.Fatalf("Policy = %+v, %v", policy, err)
	}
	changes, err := fg.FindChange(ctx, "forgecell/topic")
	if err != nil || !reflect.DeepEqual(changes, contract.ForgeChanges{Changes: []contract.ForgeChangeRef{}}) {
		t.Fatalf("FindChange = %+v, %v", changes, err)
	}
	published, err := fg.PublishDraft(ctx, forge.PublishInput{HeadBranch: "forgecell/topic", Base: "main", Title: "Title", Body: "Body", Commit: fakeCommit})
	if err != nil || published != (contract.ForgePublished{Ref: "!1", URL: "https://forge.example/1", Draft: true, HeadCommit: fakeCommit}) {
		t.Fatalf("PublishDraft = %+v, %v", published, err)
	}
	change, err := fg.Change(ctx, "!1")
	if err != nil || !reflect.DeepEqual(change, contract.ForgeChange{State: "open", Draft: true, HeadCommit: fakeCommit, URL: "https://forge.example/1"}) {
		t.Fatalf("Change = %+v, %v", change, err)
	}
	checks, err := fg.Checks(ctx, "!1", fakeCommit)
	if err != nil || !reflect.DeepEqual(checks, contract.ForgeChecks{Checks: []contract.ForgeCheck{}, Complete: true}) {
		t.Fatalf("Checks = %+v, %v", checks, err)
	}
	for _, since := range []string{"", "2026-10-30T10:00:00Z"} {
		comments, err := fg.Comments(ctx, "!1", since)
		if err != nil || !reflect.DeepEqual(comments, contract.ForgeComments{Comments: []contract.ForgeComment{}}) {
			t.Fatalf("Comments(%q) = %+v, %v", since, comments, err)
		}
	}

	want := []string{"forge_describe", "forge_policy", "forge_find_change", "forge_publish_draft", "forge_change", "forge_checks", "forge_comments", "forge_comments"}
	if !reflect.DeepEqual(f.tools(), want) {
		t.Fatalf("tools called = %v", f.tools())
	}
	for _, r := range f.records {
		if r.Reason != "" || !hex64.MatchString(r.ResultSHA256) || r.Plugin != "fake-forge" || r.Kind != "forge" || r.ManifestSHA256 != f.opts.Ref.SHA256 {
			t.Fatalf("record = %+v", r)
		}
	}
	f.requireCleaned(t)
}

// TestForgeCarriesProjectIntoEveryCall: every forge tool's input schema
// requires project, so the happy path above already shows it reaches each
// call; here a project with only the tool's own arguments is refused before
// sending, and NewForge refuses projects that are not clean text.
func TestForgeCarriesProjectIntoEveryCall(t *testing.T) {
	f := newFixture(t, "forge", "bad-input-crash")
	fg := newTestForge(t, f)
	for tool, own := range map[string]map[string]any{"forge_policy": {"branch": "main"}, "forge_change": {"ref": "!1"}} {
		if args := fg.args(own); args["project"] != "example/project" || len(args) != len(own)+1 {
			t.Fatalf("%s: args = %v", tool, args)
		}
	}
	for _, project := range []string{"", "a\nb", "a\xe2\x80\xaeb", "\xe2\x80\x8b", strings.Repeat("p", 201)} {
		if _, err := NewForge(f.opts, project); mcp.ReasonOf(err) != mcp.SchemaInvalid {
			t.Fatalf("NewForge(%q) = %v", project, err)
		}
	}
	if _, err := fg.Policy(context.Background(), "main"); err != nil {
		t.Fatalf("a valid call was refused or crashed the strict server: %v", err)
	}
	f.requireCleaned(t)
}

// TestForgePublishDraftKeepsAChangeOfAnotherCommit relies on the fake
// answering with its own fixed commit: the change was published, so its ref
// and url come back with the refusal, and the call's record carries it.
func TestForgePublishDraftKeepsAChangeOfAnotherCommit(t *testing.T) {
	f := newFixture(t, "forge", "good")
	other := strings.Repeat("a", 40)
	published, err := newTestForge(t, f).PublishDraft(context.Background(), forge.PublishInput{HeadBranch: "topic", Base: "main", Title: "T", Body: "", Commit: other})
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-forge forge_publish_draft: schema-invalid")
	if published.Ref != "!1" || published.URL != "https://forge.example/1" || published.HeadCommit != fakeCommit {
		t.Fatalf("the published change was dropped: %+v", published)
	}
	if len(f.records) != 1 || f.records[0].Reason != mcp.SchemaInvalid || !hex64.MatchString(f.records[0].ResultSHA256) || f.records[0].Cleanup != "" {
		t.Fatalf("records = %+v", f.records)
	}
	f.requireCleaned(t)
}

// TestForgePublishDraftSucceedsDespiteACleanupFailure uses the unmark mode, so
// the run directory is not removed after a successful publish: the change is
// returned with no error, and the cleanup failure is in the call's record.
func TestForgePublishDraftSucceedsDespiteACleanupFailure(t *testing.T) {
	f := newFixture(t, "forge", "unmark")
	published, err := newTestForge(t, f).PublishDraft(context.Background(), forge.PublishInput{HeadBranch: "topic", Base: "main", Title: "T", Body: "", Commit: fakeCommit})
	if err != nil {
		t.Fatalf("a cleanup failure after a successful publish failed it: %v", err)
	}
	if published != (contract.ForgePublished{Ref: "!1", URL: "https://forge.example/1", Draft: true, HeadCommit: fakeCommit}) {
		t.Fatalf("PublishDraft = %+v", published)
	}
	if len(f.records) != 1 {
		t.Fatalf("records = %+v", f.records)
	}
	r := f.records[0]
	if r.Reason != "" || r.Cleanup != mcp.Failed || r.CleanupDiagnostics != cleanupDiagnostics || strings.Contains(r.CleanupDiagnostics, f.lab) {
		t.Fatalf("record = %+v", r)
	}
}
