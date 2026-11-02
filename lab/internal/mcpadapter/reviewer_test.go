package mcpadapter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
	"github.com/DanielNetzer/forgecell/lab/internal/reviewer"
)

const reviewCommit = "0123456789abcdef0123456789abcdef01234567"

// reviewerFixture is a reviewer fake whose received calls are appended to a
// record file the test reads, through the manifest's needs.env FAKE_RECORD.
type reviewerFixture struct {
	*fixture
	record string
}

func newReviewerFixture(t *testing.T) *reviewerFixture {
	t.Helper()
	server := plugintest.Server(t)
	lab := plugintest.TempDir(t)
	raw := plugintest.Manifest(t, server, "reviewer", "good", "FAKE_RECORD")
	if err := os.MkdirAll(filepath.Join(lab, "plugins", "fake"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lab, filepath.FromSlash(manifestRel)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(lab, "runs")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(plugintest.TempDir(t), "calls.jsonl")
	sum := sha256.Sum256(raw)
	f := &reviewerFixture{fixture: &fixture{lab: lab, root: root}, record: record}
	f.opts = Options{
		LabDir:  lab,
		Ref:     Ref{Manifest: manifestRel, SHA256: hex.EncodeToString(sum[:])},
		RunRoot: root,
		LookupEnv: func(name string) (string, bool) {
			if name == "FAKE_RECORD" {
				return record, true
			}
			return "", false
		},
		Observer: func(r CallRecord) { f.records = append(f.records, r) },
	}
	return f
}

// calls returns the arguments of every received call of tool, and resets the
// record.
func (f *reviewerFixture) calls(t *testing.T, tool string) []map[string]json.RawMessage {
	t.Helper()
	file, err := os.Open(f.record)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var out []map[string]json.RawMessage
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for scanner.Scan() {
		var line struct {
			Tool      string                     `json:"tool"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		if line.Tool == tool {
			out = append(out, line.Arguments)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.record); err != nil {
		t.Fatal(err)
	}
	return out
}

func describeScript(capabilities ...string) map[string]any {
	d := map[string]any{"displayName": "Scripted critic", "severities": []string{"high", "low"}}
	if capabilities != nil {
		d["capabilities"] = capabilities
	}
	return d
}

func reviewRequest() reviewer.Request {
	return reviewer.Request{
		Ref:      "7",
		Commit:   reviewCommit,
		Forge:    map[string]any{"kind": "github", "host": "github.com"},
		Comments: []forge.Comment{{ID: "c1", Author: "bot", IsBot: true, Body: "Looks risky", Path: "src/a.go", Line: 3, CreatedAt: "2026-11-01T10:00:00Z"}},
		Checks:   []reviewer.Check{{Name: "build", Status: "completed", Conclusion: "success"}},
	}
}

func TestReviewerCapabilityGate(t *testing.T) {
	f := newReviewerFixture(t)
	for _, c := range []struct {
		name         string
		capabilities []string
		want         []string
	}{
		{"neither", nil, []string{"commit", "config", "forge", "ref"}},
		{"unrelated", []string{"forge-diff"}, []string{"commit", "config", "forge", "ref"}},
		{"comments", []string{"forge-comments"}, []string{"comments", "commit", "config", "forge", "ref"}},
		{"checks", []string{"forge-checks"}, []string{"checks", "commit", "config", "forge", "ref"}},
		{"both", []string{"forge-checks", "forge-comments"}, []string{"checks", "comments", "commit", "config", "forge", "ref"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			config := map[string]any{"mode": "strict", "describe": describeScript(c.capabilities...)}
			r, err := NewReviewer(f.opts, formula.ReviewerRef{Plugin: "critic", AutoFix: "high", Config: config})
			if err != nil {
				t.Fatal(err)
			}
			res, err := r.Findings(context.Background(), reviewRequest())
			if err != nil || res.Status != reviewer.StatusComplete || res.InputDropped != 0 {
				t.Fatalf("Findings = %+v, %v", res, err)
			}
			got := f.calls(t, "reviewer_findings")
			if len(got) != 1 {
				t.Fatalf("received %d reviewer_findings calls", len(got))
			}
			if keys := slices.Sorted(maps.Keys(got[0])); !slices.Equal(keys, c.want) {
				t.Fatalf("arguments %v, want %v", keys, c.want)
			}
			if string(got[0]["ref"]) != `"7"` || string(got[0]["commit"]) != `"`+reviewCommit+`"` || !strings.Contains(string(got[0]["config"]), `"mode":"strict"`) {
				t.Fatalf("arguments = %v", got[0])
			}
			if raw, ok := got[0]["comments"]; ok && string(raw) != `[{"id":"c1","author":"bot","isBot":true,"body":"Looks risky","path":"src/a.go","line":3,"createdAt":"2026-11-01T10:00:00Z"}]` {
				t.Fatalf("comments = %s", raw)
			}
			if raw, ok := got[0]["checks"]; ok && string(raw) != `[{"name":"build","status":"completed","conclusion":"success"}]` {
				t.Fatalf("checks = %s", raw)
			}
		})
	}
	f.requireCleaned(t)

	// A capable reviewer with nothing to read still gets the empty lists.
	r, _ := NewReviewer(f.opts, formula.ReviewerRef{Plugin: "critic", Config: map[string]any{"describe": describeScript("forge-checks", "forge-comments")}})
	if _, err := r.Findings(context.Background(), reviewer.Request{Ref: "7", Commit: reviewCommit}); err != nil {
		t.Fatal(err)
	}
	got := f.calls(t, "reviewer_findings")
	if len(got) != 1 || string(got[0]["comments"]) != "[]" || string(got[0]["checks"]) != "[]" || string(got[0]["forge"]) != "{}" {
		t.Fatalf("arguments = %v", got)
	}
}

func TestReviewerDescribe(t *testing.T) {
	f := newReviewerFixture(t)
	r, err := NewReviewer(f.opts, formula.ReviewerRef{Plugin: "critic", Config: map[string]any{"describe": describeScript("forge-comments")}})
	if err != nil {
		t.Fatal(err)
	}
	if r.Plugin() != "critic" {
		t.Fatalf("Plugin = %q", r.Plugin())
	}
	d, err := r.Describe(context.Background())
	if err != nil || d.DisplayName != "Scripted critic" || !d.Has(reviewer.CapabilityComments) || d.Has(reviewer.CapabilityChecks) {
		t.Fatalf("Describe = %+v, %v", d, err)
	}
	// A config that is not a bounded object is refused before any start.
	f.records = nil
	if _, err := NewReviewer(f.opts, formula.ReviewerRef{Plugin: "critic", Config: map[string]any{"x": strings.Repeat("x", MaxConfigBytes)}}); mcp.ReasonOf(err) != mcp.SchemaInvalid {
		t.Fatalf("an oversized config: %v", err)
	}
	if len(f.records) != 0 {
		t.Fatalf("records = %+v", f.records)
	}
}

func TestReviewerFindingsAnswersAndFailures(t *testing.T) {
	f := newReviewerFixture(t)
	findings := func(script map[string]any) (reviewer.Result, error) {
		t.Helper()
		r, err := NewReviewer(f.opts, formula.ReviewerRef{Plugin: "critic", Config: map[string]any{"findings": script}})
		if err != nil {
			t.Fatal(err)
		}
		return r.Findings(context.Background(), reviewRequest())
	}

	// pending, with findings decoded and sanitized.
	res, err := findings(map[string]any{"status": "pending", "findings": []any{
		map[string]any{"id": "F-1", "severity": "high", "path": "src/a.go", "line": 12, "title": "Unchecked‮ error", "body": "Body\x07 text", "resolved": false},
	}})
	if err != nil || res.Status != reviewer.StatusPending || len(res.Findings) != 1 {
		t.Fatalf("pending = %+v, %v", res, err)
	}
	if got := res.Findings[0]; got.ID != "F-1" || got.Path != "src/a.go" || got.Line != 12 || got.Title != "Unchecked error" || got.Body != "Body  text" || got.Note != "" {
		t.Fatalf("finding = %+v", got)
	}

	// A plugin error is the fixed reason, never server text.
	r, _ := NewReviewer(f.opts, formula.ReviewerRef{Plugin: "critic", Config: map[string]any{"fail": "auth-missing"}})
	_, err = r.Findings(context.Background(), reviewRequest())
	f.requireReason(t, err, mcp.AuthMissing, "plugin fake-reviewer reviewer_describe: auth-missing")

	// A schema-invalid answer is refused and its record carries the reason.
	f.records = nil
	_, err = findings(map[string]any{"status": "done", "findings": []any{}})
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-reviewer reviewer_findings: schema-invalid")
	if n := len(f.records); n != 2 || f.records[1].Tool != "reviewer_findings" || f.records[1].Reason != mcp.SchemaInvalid {
		t.Fatalf("records = %+v", f.records)
	}
	f.requireCleaned(t)

	// A bad commit is refused before anything is sent.
	f.records = nil
	f.calls(t, "reviewer_findings")
	r, _ = NewReviewer(f.opts, formula.ReviewerRef{Plugin: "critic"})
	_, err = r.Findings(context.Background(), reviewer.Request{Ref: "7", Commit: "HEAD"})
	if mcp.ReasonOf(err) != mcp.SchemaInvalid || len(f.calls(t, "reviewer_findings")) != 0 {
		t.Fatalf("a bad commit: %v", err)
	}
}

func TestReviewerClearsUnsafeFindingPaths(t *testing.T) {
	f := newReviewerFixture(t)
	paths := []string{"src/ok.go", "../etc/passwd", "/etc/passwd", `src\a.go`, "./src/a.go", "src//a.go", "src/../../a.go", ".", "a/.."}
	list := []any{}
	for i, p := range paths {
		list = append(list, map[string]any{"id": fmt.Sprint("F-", i), "severity": "low", "path": p, "line": 1, "title": "T", "body": "", "resolved": false})
	}
	r, _ := NewReviewer(f.opts, formula.ReviewerRef{Plugin: "critic", Config: map[string]any{"findings": map[string]any{"status": "complete", "findings": list}}})
	res, err := r.Findings(context.Background(), reviewRequest())
	if err != nil || len(res.Findings) != len(paths) {
		t.Fatalf("Findings = %+v, %v", res, err)
	}
	for i, got := range res.Findings {
		if i == 0 {
			if got.Path != "src/ok.go" || got.Note != "" {
				t.Fatalf("a clean path was changed: %+v", got)
			}
			continue
		}
		if got.Path != "" || got.Note != PathClearedNote || got.Title != "T" {
			t.Fatalf("path %q was kept: %+v", paths[i], got)
		}
	}
}

func TestReviewerBoundsTheRequest(t *testing.T) {
	body := strings.Repeat("x", 65536)
	comments := make([]forge.Comment, 100)
	for i := range comments {
		comments[i] = forge.Comment{ID: fmt.Sprint("c", i), Author: "bot", Body: body, CreatedAt: "2026-11-01T10:00:00Z"}
	}
	args := map[string]any{"forge": map[string]any{}, "ref": "7", "commit": reviewCommit, "config": json.RawMessage(`{}`)}
	kept, dropped, err := fitComments(args, comments)
	if err != nil || dropped == 0 || len(kept)+dropped != len(comments) || kept[0].ID != "c0" {
		t.Fatalf("fitComments kept %d, dropped %d, %v", len(kept), dropped, err)
	}
	args["comments"] = kept
	if size, _ := argsBytes(args); size > MaxReviewerRequestBytes {
		t.Fatalf("the kept request is %d bytes", size)
	}
	// Arguments that cannot fit even without comments are refused.
	huge := map[string]any{"forge": map[string]any{"x": strings.Repeat("y", MaxReviewerRequestBytes)}}
	if _, _, err := fitComments(huge, comments); err == nil {
		t.Fatal("an oversized request was accepted")
	}

	// Through the plugin: the call succeeds with the comments that fit, and the
	// result counts the ones left out.
	f := newReviewerFixture(t)
	r, _ := NewReviewer(f.opts, formula.ReviewerRef{Plugin: "critic", Config: map[string]any{"describe": describeScript("forge-comments")}})
	res, err := r.Findings(context.Background(), reviewer.Request{Ref: "7", Commit: reviewCommit, Comments: comments})
	if err != nil || res.InputDropped != dropped {
		t.Fatalf("Findings = %+v, %v (want %d dropped)", res, err, dropped)
	}
	got := f.calls(t, "reviewer_findings")
	var sent []forge.Comment
	if len(got) != 1 || json.Unmarshal(got[0]["comments"], &sent) != nil || len(sent) != len(kept) {
		t.Fatalf("sent %d comments, want %d", len(sent), len(kept))
	}
}

func TestReviewerFindingsInputSchema(t *testing.T) {
	tool, ok := contract.FindTool("reviewer_findings")
	if !ok {
		t.Fatal("no reviewer_findings")
	}
	comment := func(edit func(map[string]any)) map[string]any {
		c := map[string]any{"id": "c1", "author": "bot", "isBot": true, "body": "b", "path": "src/a.go", "line": 2, "createdAt": "2026-11-01T10:00:00Z"}
		if edit != nil {
			edit(c)
		}
		return c
	}
	check := func(edit func(map[string]any)) map[string]any {
		c := map[string]any{"name": "build", "status": "completed", "conclusion": "success", "url": "https://ci.example/1"}
		if edit != nil {
			edit(c)
		}
		return c
	}
	args := func(extra map[string]any) map[string]any {
		a := map[string]any{"forge": map[string]any{}, "ref": "7", "commit": reviewCommit}
		maps.Copy(a, extra)
		return a
	}
	many := func(n int, item map[string]any) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = item
		}
		return out
	}
	props := map[string]any{}
	for i := range 65 {
		props[fmt.Sprint("k", i)] = i
	}
	accepted := map[string]map[string]any{
		"today's shape": args(nil),
		"config":        args(map[string]any{"config": map[string]any{"mode": "strict", "nested": map[string]any{"x": []any{1}}}}),
		"comments":      args(map[string]any{"comments": []any{comment(nil), comment(func(c map[string]any) { delete(c, "path"); delete(c, "line") })}}),
		"checks":        args(map[string]any{"checks": []any{check(nil), check(func(c map[string]any) { c["status"] = "queued"; c["conclusion"] = ""; delete(c, "url") })}}),
		"500 comments":  args(map[string]any{"comments": many(500, comment(nil))}),
		"200 checks":    args(map[string]any{"checks": many(200, check(nil))}),
		"empty lists":   args(map[string]any{"comments": []any{}, "checks": []any{}, "config": map[string]any{}}),
	}
	for name, a := range accepted {
		if err := tool.ValidateInput(a); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
	refused := map[string]map[string]any{
		"no ref":                {"forge": map[string]any{}, "commit": reviewCommit},
		"unknown key":           args(map[string]any{"diff": "x"}),
		"config not an object":  args(map[string]any{"config": []any{}}),
		"65 config properties":  args(map[string]any{"config": props}),
		"comments not an array": args(map[string]any{"comments": map[string]any{}}),
		"501 comments":          args(map[string]any{"comments": many(501, comment(nil))}),
		"comment without date":  args(map[string]any{"comments": []any{comment(func(c map[string]any) { delete(c, "createdAt") })}}),
		"comment unknown key":   args(map[string]any{"comments": []any{comment(func(c map[string]any) { c["html"] = "x" })}}),
		"comment line 0":        args(map[string]any{"comments": []any{comment(func(c map[string]any) { c["line"] = 0 })}}),
		"comment empty path":    args(map[string]any{"comments": []any{comment(func(c map[string]any) { c["path"] = "" })}}),
		"201 checks":            args(map[string]any{"checks": many(201, check(nil))}),
		"check bad status":      args(map[string]any{"checks": []any{check(func(c map[string]any) { c["status"] = "COMPLETED" })}}),
		"check bad conclusion":  args(map[string]any{"checks": []any{check(func(c map[string]any) { c["conclusion"] = "passed" })}}),
		"check ftp url":         args(map[string]any{"checks": []any{check(func(c map[string]any) { c["url"] = "ftp://ci.example/1" })}}),
		"check without name":    args(map[string]any{"checks": []any{check(func(c map[string]any) { delete(c, "name") })}}),
	}
	for name, a := range refused {
		if err := tool.ValidateInput(a); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A plugin advertising today's input schema stays compatible.
	old := `{"type":"object","required":["forge","ref","commit"],"properties":{"forge":{"type":"object"},"ref":{"type":"string"},"commit":{"type":"string"}}}`
	if err := tool.CheckAdvertised(json.RawMessage(old)); err != nil {
		t.Fatalf("today's advertised schema: %v", err)
	}
	if err := tool.CheckAdvertised(json.RawMessage(`{"type":"object","required":["forge","ref","commit","comments"],"properties":{"forge":{"type":"object"},"ref":{"type":"string"},"commit":{"type":"string"},"comments":{"type":"array"}}}`)); err == nil {
		t.Fatal("a plugin requiring comments was accepted")
	}
}

// reviewersFormula declares the fixture's manifest as two reviewer plugins, in
// the given reviewer order.
func reviewersFormula(t *testing.T, pin string, order ...string) formula.Formula {
	t.Helper()
	yaml := "schemaVersion: v0\nkind: formula\nid: sample\nintake:\n  repo: acme/widgets\natoms:\n  - id: intake\n    type: intake\n  - id: work\n    type: harness\n"
	yaml += "plugins:\n  - name: critic\n    manifest: " + manifestRel + "\n    sha256: " + pin + "\n  - name: lint\n    manifest: " + manifestRel + "\n    sha256: " + pin + "\n"
	if len(order) > 0 {
		yaml += "reviewers:\n"
	}
	for _, name := range order {
		yaml += "  - plugin: " + name + "\n    autoFix: high\n"
	}
	f, err := formula.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestReviewersFor(t *testing.T) {
	f := newReviewerFixture(t)
	// No reviewers: none, and no run root.
	list, err := ReviewersFor(f.lab, reviewersFormula(t, f.opts.Ref.SHA256), Options{})
	if list != nil || err != nil {
		t.Fatalf("ReviewersFor = %v, %v", list, err)
	}
	if _, err := os.Lstat(filepath.Join(f.lab, RunRootName)); !os.IsNotExist(err) {
		t.Fatalf("no reviewers made a run root: %v", err)
	}

	// Formula order, each with its own config.
	form := reviewersFormula(t, f.opts.Ref.SHA256, "lint", "critic")
	form.ReviewerPlugins[0].Config = map[string]any{"describe": describeScript("forge-checks")}
	list, err = ReviewersFor(f.lab, form, Options{LookupEnv: f.opts.LookupEnv})
	if err != nil || len(list) != 2 {
		t.Fatalf("ReviewersFor = %v, %v", list, err)
	}
	for i, want := range []string{"lint", "critic"} {
		if got := list[i].(*Reviewer).Plugin(); got != want {
			t.Fatalf("reviewer %d is %s, want %s", i, got, want)
		}
	}
	d, err := list[0].Describe(context.Background())
	if err != nil || !d.Has(reviewer.CapabilityChecks) {
		t.Fatalf("lint Describe = %+v, %v", d, err)
	}
	if res, err := list[1].Findings(context.Background(), reviewRequest()); err != nil || res.Status != reviewer.StatusComplete {
		t.Fatalf("critic Findings = %+v, %v", res, err)
	}
	if got := f.calls(t, "reviewer_findings"); len(got) != 1 || got[0]["checks"] != nil {
		t.Fatalf("critic got %v", got)
	}

	// A reviewer naming no declared plugin, or too many reviewers, is refused.
	bad := form
	bad.ReviewerPlugins = []formula.ReviewerRef{{Plugin: "missing", AutoFix: "high"}}
	if list, err := ReviewersFor(f.lab, bad, Options{}); list != nil || mcp.ReasonOf(err) != mcp.Failed {
		t.Fatalf("an undeclared reviewer: %v, %v", list, err)
	}
	bad.ReviewerPlugins = slices.Repeat([]formula.ReviewerRef{{Plugin: "critic", AutoFix: "high"}}, formula.MaxReviewers+1)
	if list, err := ReviewersFor(f.lab, bad, Options{}); list != nil || err == nil {
		t.Fatalf("too many reviewers: %v, %v", list, err)
	}
}
