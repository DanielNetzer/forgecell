package molecule

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

func pluginCallRecord(tool string) mcpadapter.CallRecord {
	return mcpadapter.CallRecord{Plugin: "jira", Kind: "tracker", ManifestSHA256: strings.Repeat("a", 64), Tool: tool, ArgsSHA256: strings.Repeat("b", 64), ResultSHA256: strings.Repeat("c", 64), DurationMS: 3}
}

func TestPluginCallsAreCappedWithADroppedCount(t *testing.T) {
	t.Parallel()
	l := NewPluginCallLog()
	for i := 0; i < MaxPluginCalls+44; i++ {
		l.Observe(pluginCallRecord("tracker_get"))
	}
	var r Record
	if !absorbPluginCalls(&r, l) {
		t.Fatal("absorb reported no change")
	}
	if len(r.PluginCalls) != MaxPluginCalls || r.PluginCallsDropped != 44 {
		t.Fatalf("calls = %d, dropped = %d", len(r.PluginCalls), r.PluginCallsDropped)
	}
	// The log is drained, so a second absorb adds nothing twice; later calls past
	// the Record's own bound only count.
	if absorbPluginCalls(&r, l) {
		t.Fatal("a drained log changed the record")
	}
	for i := 0; i < 10; i++ {
		l.Observe(pluginCallRecord("tracker_describe"))
	}
	absorbPluginCalls(&r, l)
	if len(r.PluginCalls) != MaxPluginCalls || r.PluginCallsDropped != 54 {
		t.Fatalf("calls = %d, dropped = %d", len(r.PluginCalls), r.PluginCallsDropped)
	}
	if absorbPluginCalls(&r, nil) {
		t.Fatal("a nil log changed the record")
	}
}

func TestPluginCallsHoldNoContent(t *testing.T) {
	t.Parallel()
	r := Record{PluginCalls: []mcpadapter.CallRecord{pluginCallRecord("tracker_get")}, PluginCallsDropped: 1}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		PluginCalls        []map[string]any `json:"pluginCalls"`
		PluginCallsDropped int              `json:"pluginCallsDropped"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.PluginCalls) != 1 || doc.PluginCallsDropped != 1 {
		t.Fatalf("ledger form: %v %s", err, raw)
	}
	allowed := []string{"plugin", "kind", "manifestSha256", "tool", "argsSha256", "resultSha256", "durationMs", "reason", "cleanup", "cleanupDiagnostics"}
	for key := range doc.PluginCalls[0] {
		if !slices.Contains(allowed, key) {
			t.Fatalf("a call record carries %q", key)
		}
	}
	// A GitHub Molecule writes neither field.
	raw, err = json.Marshal(Record{})
	if err != nil || strings.Contains(string(raw), "pluginCalls") {
		t.Fatalf("an empty record: %v %s", err, raw)
	}
}

func TestPluginDiagnosticsAreBoundedRedactedAndLedgerOnly(t *testing.T) {
	t.Parallel()
	lab := resolvedTempDir(t)
	l := NewPluginCallLog(lab)
	// Errors that are not the adapter's, or carry no diagnostics, add nothing.
	l.Fail(errors.New("plain " + lab))
	l.Fail(&mcpadapter.Error{Plugin: "jira", Tool: "tracker_get", Reason: mcp.Failed})
	l.Fail(&mcpadapter.Error{Plugin: "jira", Tool: "tracker_get", Reason: mcp.Failed, Diagnostics: "stderr at " + lab + "/plugin-runs/x\x1b[31m " + strings.Repeat("é", 4000)})
	var r Record
	absorbPluginCalls(&r, l)
	if len(r.Notes) != 1 {
		t.Fatalf("notes = %q", r.Notes)
	}
	note := r.Notes[0]
	if !strings.HasPrefix(note, PluginDiagnosticsNote+"plugin jira tracker_get: failed: stderr at <lab>/plugin-runs/x") || strings.Contains(note, lab) || strings.ContainsRune(note, 0x1b) || strings.Contains(note, "\n") {
		t.Fatalf("note = %.200q", note)
	}
	if !strings.HasSuffix(note, " [truncated]") || len(note) > len(PluginDiagnosticsNote)+100+maxPluginDiagnosticBytes {
		t.Fatalf("note is %d bytes", len(note))
	}

	// At most maxPluginDiagnosticNotes notes, then one fixed note.
	for i := 0; i < 3*maxPluginDiagnosticNotes; i++ {
		l.Fail(&mcpadapter.Error{Plugin: "jira", Tool: "tracker_get", Reason: mcp.Network, Diagnostics: "down"})
		absorbPluginCalls(&r, l)
	}
	kept := 0
	for _, n := range r.Notes {
		if strings.HasPrefix(n, PluginDiagnosticsNote) {
			kept++
		}
	}
	if kept != maxPluginDiagnosticNotes+1 || r.Notes[len(r.Notes)-1] != pluginDiagnosticsDropped {
		t.Fatalf("kept %d notes: %q", kept, r.Notes)
	}

	// Run output leaves them out; a record without them is unchanged.
	r.Notes = append([]string{"Intake reads the Issue body."}, r.Notes...)
	if out := WithoutPluginDiagnostics(r); !slices.Equal(out.Notes, []string{"Intake reads the Issue body."}) {
		t.Fatalf("output notes = %q", out.Notes)
	}
	plain := Record{Notes: []string{"a"}}
	if out := WithoutPluginDiagnostics(plain); !slices.Equal(out.Notes, plain.Notes) {
		t.Fatalf("plain notes = %q", out.Notes)
	}
}

// TestKeyedRunRecordsPluginCalls runs intake with a reader that makes plugin
// calls: their records reach the ledger, and a failed read's diagnostics reach
// only its notes, never the blocked Atom's detail.
func TestKeyedRunRecordsPluginCalls(t *testing.T) {
	t.Parallel()
	o, _ := keyedFixture(t, keyedIssueFixture())
	l := NewPluginCallLog(o.LabDir)
	o.PluginCalls = l
	read := o.ReadIssueKey
	o.ReadIssueKey = func(ctx context.Context, repo, key, dir string) (Issue, error) {
		l.Observe(pluginCallRecord("tracker_describe"))
		l.Observe(pluginCallRecord("tracker_get"))
		return read(ctx, repo, key, dir)
	}
	r, err := RunKeyed(context.Background(), o, "ENG-1234")
	if err != nil || r.Status != "waiting" {
		t.Fatalf("RunKeyed = %v, %v", r.Status, err)
	}
	saved := readLedgerRecord(t, o.LabDir, r.ID)
	if len(saved.PluginCalls) != 2 || saved.PluginCalls[0].Tool != "tracker_describe" || saved.PluginCalls[1].Tool != "tracker_get" || saved.PluginCallsDropped != 0 {
		t.Fatalf("ledger calls = %+v", saved.PluginCalls)
	}

	failed, _ := keyedFixture(t, keyedIssueFixture())
	fl := NewPluginCallLog(failed.LabDir)
	failed.PluginCalls = fl
	cause := &mcpadapter.Error{Plugin: "jira", Tool: "tracker_get", Reason: mcp.AuthMissing, Diagnostics: "token rejected for " + failed.LabDir}
	failed.ReadIssueKey = func(context.Context, string, string, string) (Issue, error) {
		fl.Observe(mcpadapter.CallRecord{Plugin: "jira", Kind: "tracker", Tool: "tracker_get", ArgsSHA256: strings.Repeat("b", 64), Reason: mcp.AuthMissing})
		fl.Fail(cause)
		return Issue{}, cause
	}
	r, err = RunKeyed(context.Background(), failed, "ENG-1234")
	if err != nil || r.Status != "blocked" {
		t.Fatalf("RunKeyed = %v, %v", r.Status, err)
	}
	saved = readLedgerRecord(t, failed.LabDir, r.ID)
	if len(saved.PluginCalls) != 1 || saved.PluginCalls[0].Reason != mcp.AuthMissing {
		t.Fatalf("ledger calls = %+v", saved.PluginCalls)
	}
	found := false
	for _, n := range saved.Notes {
		if strings.HasPrefix(n, PluginDiagnosticsNote) {
			found = true
			if !strings.Contains(n, "token rejected for <lab>") || strings.Contains(n, failed.LabDir) {
				t.Fatalf("note = %q", n)
			}
		}
	}
	if !found {
		t.Fatalf("no diagnostics note: %q", saved.Notes)
	}
	for _, a := range saved.Atoms {
		if strings.Contains(a.Detail, "token rejected") {
			t.Fatalf("diagnostics reached an Atom detail: %q", a.Detail)
		}
		if a.ID == "intake" && a.Detail != "plugin jira tracker_get: auth-missing" {
			t.Fatalf("intake detail = %q", a.Detail)
		}
	}
	if out := WithoutPluginDiagnostics(saved); slices.ContainsFunc(out.Notes, func(n string) bool { return strings.Contains(n, "token rejected") }) {
		t.Fatalf("run output keeps diagnostics: %q", out.Notes)
	}
}

// TestReviewLedgerLeavesOutPluginDiagnostics: plugin stderr in a diagnostics
// note never reaches the review provider, even as the latest note, and the
// other notes still do.
func TestReviewLedgerLeavesOutPluginDiagnostics(t *testing.T) {
	t.Parallel()
	notes := []string{"Intake reads the Issue body, not comments."}
	for i := 0; i < reviewLedgerNotes+3; i++ {
		notes = append(notes, PluginDiagnosticsNote+"plugin jira tracker_get: failed: plugin stderr secret-token-xyz")
	}
	r := Record{ID: "mol-eng-1-a", Status: "waiting", Notes: notes}
	view := reviewLedger(r, "")
	got, ok := view["notes"].([]string)
	if !ok || len(got) != 1 || got[0] != "Intake reads the Issue body, not comments." {
		t.Fatalf("review notes = %q", view["notes"])
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-token-xyz") || strings.Contains(string(raw), PluginDiagnosticsNote) {
		t.Fatalf("review ledger carries plugin diagnostics: %s", raw)
	}
	// A record without diagnostics keeps its notes as before.
	plain := reviewLedger(Record{ID: "mol-1-a", Notes: []string{"a", "b"}}, "")
	if got := plain["notes"].([]string); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("plain notes = %q", got)
	}
}

func readLedgerRecord(t *testing.T, lab, id string) Record {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(lab, "ledgers", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
