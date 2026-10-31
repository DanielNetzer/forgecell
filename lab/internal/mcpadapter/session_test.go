package mcpadapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/forge"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/plugintest"
)

const fakeCommit = "0123456789abcdef0123456789abcdef01234567"

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func newTestTracker(t *testing.T, f *fixture, config any) *Tracker {
	t.Helper()
	tr, err := NewTracker(f.opts, config)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func newTestForge(t *testing.T, f *fixture) *Forge {
	t.Helper()
	fg, err := NewForge(f.opts, "example/project")
	if err != nil {
		t.Fatal(err)
	}
	return fg
}

// TestFailureModesMapToFixedReasons runs each fake server misbehaviour through
// both kinds: each maps to one fixed reason, the error names only the plugin
// and the tool or step, the server's stderr goes only to Diagnostics, and
// Shutdown removes the run directory every time.
func TestFailureModesMapToFixedReasons(t *testing.T) {
	cases := []struct {
		mode    string
		reason  mcp.Reason
		step    string // "" means the kind's describe tool
		records int
	}{
		{"wrong-contract", mcp.ContractMismatch, "initialize", 0},
		{"wrong-id", mcp.SchemaInvalid, "tools/list", 0},
		{"missing-tool", mcp.ContractMismatch, "last", 0},
		{"crash", mcp.Failed, "", 1},
		{"slow", mcp.Timeout, "", 1},
		{"oversized", mcp.OutputTooLarge, "", 1},
	}
	for _, kind := range []string{"tracker", "forge"} {
		for _, c := range cases {
			t.Run(kind+"/"+c.mode, func(t *testing.T) {
				f := newFixture(t, kind, c.mode)
				if c.mode == "slow" {
					// The slow server never answers and ignores stdin EOF, so
					// any timeout fires and Shutdown must kill it.
					f.opts.Timeout = 20 * time.Millisecond
					f.opts.Grace = fired
				}
				var err error
				var lastTool string
				if kind == "tracker" {
					_, err = newTestTracker(t, f, nil).Describe(context.Background())
					lastTool = "tracker_states"
				} else {
					_, err = newTestForge(t, f).Describe(context.Background())
					lastTool = "forge_comments"
				}
				step := c.step
				switch step {
				case "":
					step = kind + "_describe"
				case "last":
					step = lastTool
				}
				e := f.requireReason(t, err, c.reason, "plugin fake-"+kind+" "+step+": "+string(c.reason))
				if e.Remote {
					t.Fatal("a local failure is marked remote")
				}
				// A killed server's stderr may be cut short, so only a server
				// that exits on its own is required to have reached Diagnostics.
				if c.mode != "slow" && (!strings.Contains(e.Diagnostics, plugintest.ReadyMarker) || !strings.Contains(e.Diagnostics, "[redacted]")) {
					t.Fatalf("diagnostics lack the plugin's redacted stderr:\n%s", e.Diagnostics)
				}
				if len(f.records) != c.records {
					t.Fatalf("records = %+v, want %d", f.records, c.records)
				}
				for _, r := range f.records {
					if r.Reason != c.reason || r.ResultSHA256 != "" || !hex64.MatchString(r.ArgsSHA256) {
						t.Fatalf("record = %+v", r)
					}
				}
				f.requireCleaned(t)
			})
		}
	}
}

func TestMissingRequiredToolIsRefusedBeforeAnyCall(t *testing.T) {
	f := newFixture(t, "tracker", "missing-tool")
	_, err := newTestTracker(t, f, nil).Get(context.Background(), "FAKE-1")
	f.requireReason(t, err, mcp.ContractMismatch, "plugin fake-tracker tracker_states: contract-mismatch")
	if len(f.records) != 0 {
		t.Fatalf("a tool was called before tools/list was checked: %+v", f.records)
	}
	f.requireCleaned(t)
}

func TestWrongKindAndChangedManifestAreRefusedBeforeStart(t *testing.T) {
	f := newFixture(t, "forge", "good")
	_, err := newTestTracker(t, f, nil).Describe(context.Background())
	f.requireReason(t, err, mcp.ContractMismatch, "plugin fake-forge load: contract-mismatch")

	f.opts.Ref.SHA256 = strings.Repeat("0", 64)
	_, err = newTestForge(t, f).Describe(context.Background())
	f.requireReason(t, err, mcp.ManifestChanged, "plugin load: manifest-changed")

	f.opts.Ref = Ref{Manifest: "../outside.json", SHA256: strings.Repeat("0", 64)}
	_, err = newTestForge(t, f).Describe(context.Background())
	f.requireReason(t, err, mcp.Failed, "plugin load: failed")
	if len(f.records) != 0 {
		t.Fatalf("records = %+v", f.records)
	}
	f.requireCleaned(t)
}

func TestRunRootMustBeAPrivateDirectoryUnderTheLab(t *testing.T) {
	f := newFixture(t, "forge", "good")
	open := filepath.Join(f.lab, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.lab, "link")
	if err := os.Symlink(f.root, link); err != nil {
		t.Fatal(err)
	}
	outside := plugintest.TempDir(t)
	for name, root := range map[string]string{
		"missing":         filepath.Join(f.lab, "missing"),
		"open":            open,
		"symlink":         link,
		"under a symlink": filepath.Join(link, "x"),
		"outside":         outside,
		"the Lab":         f.lab,
		"relative":        "runs",
		"unclean":         f.root + "/.",
		"traversal":       filepath.Join(f.lab, "..", filepath.Base(outside)),
	} {
		opts := f.opts
		opts.RunRoot = root
		fg, err := NewForge(opts, "example/project")
		if err != nil {
			t.Fatal(err)
		}
		_, err = fg.Describe(context.Background())
		f.requireReason(t, err, mcp.Failed, "plugin options: failed")
		if left, _ := os.ReadDir(outside); len(left) != 0 {
			t.Fatalf("%s: a run directory was made outside the Lab", name)
		}
	}
	if len(f.records) != 0 {
		t.Fatalf("records = %+v", f.records)
	}
}

func TestShutdownRunsAfterAPanic(t *testing.T) {
	f := newFixture(t, "forge", "good")
	f.opts.Observer = func(CallRecord) { panic("observer") }
	func() {
		defer func() {
			if r := recover(); r != "observer" {
				t.Fatalf("recovered %v, want the observer's panic", r)
			}
		}()
		newTestForge(t, f).Describe(context.Background())
	}()
	f.requireCleaned(t)
}

func TestCleanupFailureIsReportedWithTheOutput(t *testing.T) {
	// The unmark mode removes the run marker, so the run directory is not
	// recognised as Forgecell's and is left in place.
	f := newFixture(t, "forge", "unmark")
	d, err := newTestForge(t, f).Describe(context.Background())
	e := f.requireReason(t, err, mcp.Failed, "plugin fake-forge shutdown: failed")
	if d.Identity != "forge.example/example/project" {
		t.Fatalf("the output of a successful call was dropped: %+v", d)
	}
	if !strings.Contains(e.Diagnostics, cleanupDiagnostics) || strings.Contains(e.Diagnostics, f.lab) {
		t.Fatalf("diagnostics = %q", e.Diagnostics)
	}
	if len(f.records) != 1 || f.records[0].Reason != "" || f.records[0].Cleanup != mcp.Failed {
		t.Fatalf("records = %+v", f.records)
	}
}

// TestCallRecordsCarryDigestsOnly checks the record of a write call: the
// digests are of exactly the arguments sent and the structured result
// received, and nothing of either appears in the record.
func TestCallRecordsCarryDigestsOnly(t *testing.T) {
	f := newFixture(t, "forge", "good")
	in := forge.PublishInput{HeadBranch: "forgecell/topic", Base: "main", Title: "TITLE-SENTINEL", Body: "BODY-SENTINEL", Commit: fakeCommit}
	if _, err := newTestForge(t, f).PublishDraft(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 1 {
		t.Fatalf("records = %+v", f.records)
	}
	r := f.records[0]
	args, _ := json.Marshal(map[string]any{"project": "example/project", "headBranch": in.HeadBranch, "base": in.Base, "title": in.Title, "body": in.Body, "commit": in.Commit})
	result, _ := json.Marshal(map[string]any{"ref": "!1", "url": "https://forge.example/1", "draft": true, "headCommit": fakeCommit})
	want := CallRecord{Plugin: "fake-forge", Kind: "forge", ManifestSHA256: f.opts.Ref.SHA256, Tool: "forge_publish_draft", ArgsSHA256: digest(args), ResultSHA256: digest(result), DurationMS: r.DurationMS}
	if r != want || r.DurationMS < 0 {
		t.Fatalf("record = %+v, want %+v", r, want)
	}
	raw, _ := json.Marshal(r)
	for _, content := range []string{"SENTINEL", "forgecell/topic", "example/project", "forge.example", fakeCommit, f.lab} {
		if strings.Contains(string(raw), content) {
			t.Fatalf("the record holds content %q: %s", content, raw)
		}
	}
	f.requireCleaned(t)
}

// TestMalformedInputIsNeverSent uses the server that exits on input its schema
// refuses: the call fails as schema-invalid, not as a crash, so it was never
// sent.
func TestMalformedInputIsNeverSent(t *testing.T) {
	f := newFixture(t, "forge", "bad-input-crash")
	fg := newTestForge(t, f)
	_, err := fg.Checks(context.Background(), "!1", "not-a-commit")
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-forge forge_checks: schema-invalid")
	_, err = fg.Comments(context.Background(), "!1", "yesterday")
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-forge forge_comments: schema-invalid")
	_, err = fg.PublishDraft(context.Background(), forge.PublishInput{HeadBranch: "topic", Base: "main", Title: "", Body: "x", Commit: fakeCommit})
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-forge forge_publish_draft: schema-invalid")
	if len(f.records) != 3 {
		t.Fatalf("records = %+v", f.records)
	}
	for _, r := range f.records {
		if r.Reason != mcp.SchemaInvalid || r.ResultSHA256 != "" || r.DurationMS != 0 || !hex64.MatchString(r.ArgsSHA256) {
			t.Fatalf("record = %+v", r)
		}
	}
	f.requireCleaned(t)
}

func TestErrorUnwrapsToTheFixedReasonWithoutDiagnostics(t *testing.T) {
	e := &Error{Plugin: "p", Tool: "t", Reason: mcp.RateLimited, Remote: true, Diagnostics: "server text"}
	inner, ok := e.Unwrap().(*mcp.Error)
	if !ok || inner.Reason != mcp.RateLimited || !inner.Remote || inner.Diagnostics != "" {
		t.Fatalf("Unwrap = %+v", e.Unwrap())
	}
	if e.Error() != "plugin p t: rate-limited" || (&Error{Reason: mcp.Failed}).Error() != "plugin: failed" {
		t.Fatalf("Error() = %q", e.Error())
	}
}
