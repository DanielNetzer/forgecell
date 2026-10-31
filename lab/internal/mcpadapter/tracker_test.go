package mcpadapter

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

func (f *fixture) tools() []string {
	var names []string
	for _, r := range f.records {
		names = append(names, r.Tool)
	}
	return names
}

func TestTrackerServesEveryTool(t *testing.T) {
	f := newFixture(t, "tracker", "good")
	tr := newTestTracker(t, f, map[string]any{"jql": "project = ENG"})
	ctx := context.Background()

	d, err := tr.Describe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, contract.TrackerDescription{DisplayName: "Fake tracker", KeyPattern: "^FAKE-[0-9]+$", States: []string{"open", "closed"}, Selectors: []string{"all"}}) {
		t.Fatalf("Describe = %+v", d)
	}

	page, err := tr.List(ctx, "all", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	want := contract.TrackerPage{Items: []contract.TrackerItem{{Key: "FAKE-1", Title: "First", Labels: []string{}, UpdatedAt: "2026-10-30T10:00:00Z", State: "open"}}}
	if !reflect.DeepEqual(page, want) {
		t.Fatalf("List = %+v", page)
	}
	if _, err := tr.List(ctx, "all", "cursor-1", 1); err != nil {
		t.Fatalf("List with a cursor: %v", err)
	}

	issue, err := tr.Get(ctx, "FAKE-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(issue, contract.TrackerIssue{Key: "FAKE-1", Title: "First", Body: "Body", State: "open", URL: "https://tracker.example/FAKE-1", UpdatedAt: "2026-10-30T10:00:00Z", Labels: []string{}}) {
		t.Fatalf("Get = %+v", issue)
	}

	states, err := tr.States(ctx, []string{"FAKE-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(states, contract.TrackerStates{States: map[string]contract.TrackerState{"FAKE-1": {State: "open"}}}) {
		t.Fatalf("States = %+v", states)
	}

	// Each operation reads keyPattern first; each record is a success.
	wantTools := []string{"tracker_describe", "tracker_describe", "tracker_list", "tracker_describe", "tracker_list", "tracker_describe", "tracker_get", "tracker_describe", "tracker_states"}
	if !reflect.DeepEqual(f.tools(), wantTools) {
		t.Fatalf("tools called = %v", f.tools())
	}
	for _, r := range f.records {
		if r.Reason != "" || !hex64.MatchString(r.ResultSHA256) || r.Plugin != "fake-tracker" || r.Kind != "tracker" {
			t.Fatalf("record = %+v", r)
		}
	}
	f.requireCleaned(t)
}

// TestTrackerCarriesConfigIntoEveryCall uses the fake server's {"fail":
// REASON} config: every tool fails with it, so config reached the first call,
// and the three reasons a server may name are kept while any other is failed.
func TestTrackerCarriesConfigIntoEveryCall(t *testing.T) {
	f := newFixture(t, "tracker", "good")
	for reason, want := range map[string]mcp.Reason{"auth-missing": mcp.AuthMissing, "rate-limited": mcp.RateLimited, "network": mcp.Network, "timeout": mcp.Failed, "Bearer secret text": mcp.Failed} {
		f.records = nil
		tr := newTestTracker(t, f, map[string]any{"fail": reason})
		_, err := tr.Get(context.Background(), "FAKE-1")
		e := f.requireReason(t, err, want, "plugin fake-tracker tracker_describe: "+string(want))
		if !e.Remote {
			t.Fatalf("%s: a reported failure is not marked remote", reason)
		}
		if strings.Contains(e.Error(), reason) && reason != string(want) {
			t.Fatalf("%s: Error() carries the server's reason text: %q", reason, e.Error())
		}
		if len(f.records) != 1 || f.records[0].Reason != want || f.records[0].ResultSHA256 != "" {
			t.Fatalf("%s: records = %+v", reason, f.records)
		}
	}
	f.requireCleaned(t)
}

func TestTrackerConfigBounds(t *testing.T) {
	f := newFixture(t, "tracker", "good")
	for name, config := range map[string]any{
		"too large":  map[string]any{"jql": strings.Repeat("x", MaxConfigBytes)},
		"not object": []string{"a"},
		"string":     "jql",
		"not JSON":   map[string]any{"f": func() {}},
	} {
		if _, err := NewTracker(f.opts, config); mcp.ReasonOf(err) != mcp.SchemaInvalid {
			t.Errorf("%s: NewTracker = %v", name, err)
		}
	}
	// The contract bounds config members; that refusal comes before sending.
	wide := map[string]any{}
	for i := 0; i < 65; i++ {
		wide[strings.Repeat("k", i+1)] = 1
	}
	tr := newTestTracker(t, f, wide)
	_, err := tr.Describe(context.Background())
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-tracker tracker_describe: schema-invalid")
	if len(f.records) != 1 || f.records[0].DurationMS != 0 || f.records[0].ResultSHA256 != "" {
		t.Fatalf("records = %+v", f.records)
	}
	f.requireCleaned(t)
}

// TestTrackerRefusesHostileKeys checks the key rules: keys that are not clean
// text are refused before a process starts; clean keys that do not match the
// anchored keyPattern are refused after describe, before tracker_get.
func TestTrackerRefusesHostileKeys(t *testing.T) {
	f := newFixture(t, "tracker", "good")
	tr := newTestTracker(t, f, nil)
	ctx := context.Background()
	unclean := []string{
		"",
		"FAKE-1\n",
		"FAKE-1\x00",
		"FAKE-\xe2\x80\xae1", // U+202E right-to-left override
		"FAKE-\xe2\x80\x8e1", // U+200E left-to-right mark
		"\xe2\x80\x8bFAKE-1", // U+200B zero-width space
		"FAKE-1\xe2\x80\xa8", // U+2028 line separator
		"FAKE-1\xe2\x80\xa9", // U+2029 paragraph separator
		"FAKE-\xe2\x81\xa61", // U+2066 left-to-right isolate
		"\xef\xbb\xbfFAKE-1", // U+FEFF byte order mark
		"FAKE-\xff",
		"FAKE-" + strings.Repeat("1", 124),
	}
	for _, key := range unclean {
		_, err := tr.Get(ctx, key)
		f.requireReason(t, err, mcp.SchemaInvalid, "plugin tracker_get: schema-invalid")
		if _, err := tr.States(ctx, []string{"FAKE-1", key}); mcp.ReasonOf(err) != mcp.SchemaInvalid {
			t.Fatalf("States accepted %q: %v", key, err)
		}
	}
	if len(f.records) != 0 {
		t.Fatalf("a process ran for an unclean key: %v", f.tools())
	}
	for _, key := range []string{"FAKE-1x", "xFAKE-1", "fake-1", "FAKE-1 OR 1=1", "FAKE-", "FAKE-1|x", strings.Repeat("FAKE-1", 2)} {
		f.records = nil
		_, err := tr.Get(ctx, key)
		f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-tracker tracker_get: schema-invalid")
		if !reflect.DeepEqual(f.tools(), []string{"tracker_describe"}) {
			t.Fatalf("%q: tools called = %v", key, f.tools())
		}
	}
	f.requireCleaned(t)
}

// TestTrackerRefusesAnswersItDidNotAskFor relies on the fake answering FAKE-1
// whatever was asked.
func TestTrackerRefusesAnswersItDidNotAskFor(t *testing.T) {
	f := newFixture(t, "tracker", "good")
	tr := newTestTracker(t, f, nil)
	ctx := context.Background()
	_, err := tr.Get(ctx, "FAKE-2")
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-tracker tracker_get: schema-invalid")
	_, err = tr.States(ctx, []string{"FAKE-2"})
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-tracker tracker_states: schema-invalid")
	f.requireCleaned(t)
}

func TestTrackerListAndStatesBounds(t *testing.T) {
	f := newFixture(t, "tracker", "good")
	tr := newTestTracker(t, f, nil)
	ctx := context.Background()
	for _, limit := range []int{0, -1, 101} {
		_, err := tr.List(ctx, "all", "", limit)
		f.requireReason(t, err, mcp.SchemaInvalid, "plugin tracker_list: schema-invalid")
	}
	for _, sel := range []string{"", "all\n", "a\xe2\x80\xae", strings.Repeat("a", 201)} {
		_, err := tr.List(ctx, sel, "", 1)
		f.requireReason(t, err, mcp.SchemaInvalid, "plugin tracker_list: schema-invalid")
	}
	_, err := tr.List(ctx, "all", "c\x00", 1)
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin tracker_list: schema-invalid")
	keys := make([]string, 51)
	for i := range keys {
		keys[i] = "FAKE-" + strings.Repeat("1", i+1)
	}
	_, err = tr.States(ctx, keys)
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin tracker_states: schema-invalid")
	_, err = tr.States(ctx, []string{"FAKE-1", "FAKE-1"})
	f.requireReason(t, err, mcp.SchemaInvalid, "plugin tracker_states: schema-invalid")
	if len(f.records) != 0 {
		t.Fatalf("a process ran for refused input: %v", f.tools())
	}
	if s, err := tr.States(ctx, nil); err != nil || s.States == nil || len(s.States) != 0 || len(f.records) != 0 {
		t.Fatalf("States(nil) = %+v, %v", s, err)
	}

	// A selector the plugin does not declare is refused after describe.
	for _, sel := range []string{"oldest", "label:x", "allx"} {
		f.records = nil
		_, err := tr.List(ctx, sel, "", 1)
		f.requireReason(t, err, mcp.SchemaInvalid, "plugin fake-tracker tracker_list: schema-invalid")
		if !reflect.DeepEqual(f.tools(), []string{"tracker_describe"}) {
			t.Fatalf("%q: tools called = %v", sel, f.tools())
		}
	}
	f.requireCleaned(t)
}

// TestTrackerListRefusesPagesThatCannotEnd covers the page checks the fake's
// fixed answer cannot reach: a next cursor equal to the request's, a page with
// no items but a next cursor, and a total below the items held. Each would let
// a caller page forever or trust a wrong count, so each is schema-invalid.
func TestTrackerListRefusesPagesThatCannotEnd(t *testing.T) {
	pattern, err := compileKeyPattern("FAKE-[0-9]+")
	if err != nil {
		t.Fatal(err)
	}
	str := func(s string) *string { return &s }
	num := func(n int64) *int64 { return &n }
	item := contract.TrackerItem{Key: "FAKE-1", Title: "First", Labels: []string{}, UpdatedAt: "2026-10-30T10:00:00Z", State: "open"}
	for name, c := range map[string]struct {
		page   contract.TrackerPage
		cursor string
	}{
		"same cursor":         {contract.TrackerPage{Items: []contract.TrackerItem{item}, NextCursor: str("c1")}, "c1"},
		"same empty cursor":   {contract.TrackerPage{Items: []contract.TrackerItem{item}, NextCursor: str("")}, ""},
		"empty with cursor":   {contract.TrackerPage{Items: []contract.TrackerItem{}, NextCursor: str("c2")}, "c1"},
		"total below items":   {contract.TrackerPage{Items: []contract.TrackerItem{item, item}, Total: num(1)}, ""},
		"more than the limit": {contract.TrackerPage{Items: []contract.TrackerItem{item, item, item}}, ""},
	} {
		if checkPage(c.page, pattern, c.cursor, 2) == nil {
			t.Errorf("%s: the page was accepted", name)
		}
	}
	for name, c := range map[string]struct {
		page   contract.TrackerPage
		cursor string
	}{
		"next page":      {contract.TrackerPage{Items: []contract.TrackerItem{item}, NextCursor: str("c2"), Total: num(5)}, "c1"},
		"empty last":     {contract.TrackerPage{Items: []contract.TrackerItem{}, Total: num(0)}, "c1"},
		"total equal":    {contract.TrackerPage{Items: []contract.TrackerItem{item}, Total: num(1)}, ""},
		"first, no more": {contract.TrackerPage{Items: []contract.TrackerItem{item}}, ""},
	} {
		if err := checkPage(c.page, pattern, c.cursor, 2); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	// List returns checkPage's refusal through refuseAnswer: schema-invalid,
	// with the call's record marked.
	s := &session{manifest: plugin.Manifest{Name: "fake-tracker"}, records: []CallRecord{{Tool: "tracker_list"}}}
	e := s.refuseAnswer("tracker_list", mcp.SchemaInvalid, "the next cursor is the request's own cursor")
	if mcp.ReasonOf(e) != mcp.SchemaInvalid || s.records[0].Reason != mcp.SchemaInvalid || e.Error() != "plugin fake-tracker tracker_list: schema-invalid" {
		t.Fatalf("refusal = %v, records = %+v", e, s.records)
	}
	f := newFixture(t, "tracker", "good")
	if _, err := newTestTracker(t, f, nil).List(context.Background(), "all", "", 1); err != nil {
		t.Fatalf("a valid page was refused: %v", err)
	}
	f.requireCleaned(t)
}

func TestKeyPatternRules(t *testing.T) {
	for _, pattern := range []string{"", "(", strings.Repeat("a", 257), "\xff", "^x)|(y$", "a)|(b", ")("} {
		if _, err := compileKeyPattern(pattern); err == nil {
			t.Errorf("pattern %q compiled", pattern)
		}
	}
	re, err := compileKeyPattern(strings.Repeat("a", 255) + "+")
	if err != nil || !re.MatchString(strings.Repeat("a", 256)) {
		t.Fatalf("a 256-character pattern: %v", err)
	}
	// Anchoring holds whatever the pattern declares itself.
	re, _ = compileKeyPattern("A-[0-9]+|B")
	for key, want := range map[string]bool{"A-1": true, "B": true, "xB": false, "A-1x": false, "BB": false} {
		if got := matchKey(re, key) == nil; got != want {
			t.Errorf("match %q = %t, want %t", key, got, want)
		}
	}
	if !admits([]string{"jql:", "all"}, "jql:x") || admits([]string{"jql:"}, "jql:") || admits([]string{"all"}, "all:x") {
		t.Fatal("selector admission is wrong")
	}
	if cleanText(strings.Repeat("\xc3\xa9", 129), maxKeyRunes) || !cleanText(strings.Repeat("\xc3\xa9", 128), maxKeyRunes) {
		t.Fatal("the key bound is not 128 runes")
	}
}
