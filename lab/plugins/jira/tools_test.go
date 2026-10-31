package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

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

func trackerTool(t *testing.T, name string) *contract.Tool {
	t.Helper()
	c, _ := contract.Lookup("tracker")
	tool, ok := c.Tool(name)
	if !ok {
		t.Fatalf("no contract tool %s", name)
	}
	return tool
}

// siteCfg is the reviewed config the tests use.
func siteCfg() map[string]any {
	return map[string]any{"site": fakeSite, "project": "ENG"}
}

// harness is the plugin's tracker pointed at a fake Jira through the
// unexported origin seam.
type harness struct {
	fake *fakeJira
	tr   *tracker
}

func newHarnessWith(t *testing.T, fake *fakeJira, env map[string]string) *harness {
	t.Helper()
	tr := newTracker(lookupFrom(env))
	if tr.configErr != nil {
		t.Fatalf("config: %v", tr.configErr)
	}
	tr.c.origin = fake.srv.URL
	return &harness{fake: fake, tr: tr}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := newFakeJira(t)
	return newHarnessWith(t, fake, fake.env(t))
}

// call runs a tool's bound handler, with schema validation and the contract
// timeout, as the server does.
func (h *harness) call(t *testing.T, name string, args map[string]any) (any, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return h.tr.bind(trackerTool(t, name))(context.Background(), raw)
}

// decoded calls a tool, requires success and decodes the output as the engine
// does, so every output is proved valid under tracker.v1.json.
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
	v, err := trackerTool(t, name).Decode(raw)
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

func ptr(s string) *string { return &s }

func TestDescribeMakesNoRequestAndIsDeterministic(t *testing.T) {
	h := newHarness(t)
	want := contract.TrackerDescription{DisplayName: "Jira", KeyPattern: "^ENG-[1-9][0-9]{0,9}$", States: []string{"open", "closed"}, Selectors: []string{"oldest", "label:", "filter:"}}
	for range 2 {
		got := decoded[contract.TrackerDescription](t, h, "tracker_describe", map[string]any{"config": siteCfg()})
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("describe %+v, want %+v", got, want)
		}
	}
	generic := want
	generic.KeyPattern = `^[A-Z][A-Z0-9_]{1,9}-[1-9][0-9]{0,9}$`
	for _, cfg := range []map[string]any{{}, {"jql": "priority = High"}} {
		got := decoded[contract.TrackerDescription](t, h, "tracker_describe", map[string]any{"config": cfg})
		if !reflect.DeepEqual(got, generic) {
			t.Fatalf("describe %+v, want %+v", got, generic)
		}
	}
	// Describe needs neither credentials nor a site.
	bare := &harness{fake: h.fake, tr: newTracker(lookupFrom(map[string]string{}))}
	got := decoded[contract.TrackerDescription](t, bare, "tracker_describe", map[string]any{"config": map[string]any{"project": "ENG"}})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("describe %+v", got)
	}
	if n := len(h.fake.seen()); n != 0 {
		t.Fatalf("describe made %d requests", n)
	}
}

func TestConfigIsCheckedBeforeAnyRequest(t *testing.T) {
	h := newHarness(t)
	bad := []map[string]any{
		{},
		{"site": fakeSite},
		{"project": "ENG"},
		{"site": "https://" + fakeSite, "project": "ENG"},
		{"site": fakeSite + ":443", "project": "ENG"},
		{"site": fakeSite + "/jira", "project": "ENG"},
		{"site": "Example.atlassian.net", "project": "ENG"},
		{"site": "localhost", "project": "ENG"},
		{"site": fakeSite, "project": "eng"},
		{"site": fakeSite, "project": "E"},
		{"site": fakeSite, "project": "ENGINEERING1"},
		{"site": fakeSite, "project": 7},
		{"site": fakeSite, "project": "ENG", "unknown": true},
		{"site": fakeSite, "project": "ENG", "jql": strings.Repeat("a", maxJQLRunes+1)},
		{"site": fakeSite, "project": "ENG", "jql": "a = b) OR (project = OTHER"},
		{"site": fakeSite, "project": "ENG", "jql": `summary ~ "open`},
		{"site": fakeSite, "project": "ENG", "jql": "a = b\nORDER BY x"},
	}
	for _, cfg := range bad {
		for _, call := range []struct {
			name string
			args map[string]any
		}{
			{"tracker_list", map[string]any{"selector": "oldest", "limit": 10}},
			{"tracker_get", map[string]any{"key": "ENG-1"}},
			{"tracker_states", map[string]any{"keys": []string{"ENG-1"}}},
		} {
			call.args["config"] = cfg
			_, err := h.call(t, call.name, call.args)
			failure(t, err, mcpserver.Failed)
		}
	}
	if n := len(h.fake.seen()); n != 0 {
		t.Fatalf("%d requests, want none", n)
	}
	_, err := h.call(t, "tracker_describe", map[string]any{"config": map[string]any{"project": "eng"}})
	failure(t, err, mcpserver.Failed)
}

func TestSiteRules(t *testing.T) {
	cloud := &tracker{}
	custom := &tracker{customCA: true}
	for _, s := range []string{fakeSite, "a.atlassian.net", "my-team.atlassian.net"} {
		if !cloud.validSite(s) || !custom.validSite(s) {
			t.Errorf("%q was refused", s)
		}
	}
	for _, s := range []string{"atlassian.net", "jira.example.com", "example.atlassian.net.evil.example", "-x.atlassian.net", "x..atlassian.net", "x.atlassian.net.", ""} {
		if cloud.validSite(s) {
			t.Errorf("%q was accepted without JIRA_CA_FILE", s)
		}
	}
	if !custom.validSite("jira.example.com") || custom.validSite("localhost") || custom.validSite("127.0.0.1:443") {
		t.Error("the JIRA_CA_FILE site rules are wrong")
	}
}

func TestListMapsIssuesAndBuildsTheQuery(t *testing.T) {
	h := newHarness(t)
	got := decoded[contract.TrackerPage](t, h, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 10})
	want := []contract.TrackerItem{
		{Key: "ENG-1", Title: "First issue", Labels: []string{"forgecell", "backend"}, Milestone: ptr("1.0"), UpdatedAt: "2026-10-30T10:00:00Z", State: "open"},
		{Key: "ENG-2", Title: "Second issue", Labels: []string{"forgecell"}, UpdatedAt: "2026-10-30T13:00:00.123Z", State: "open"},
	}
	if !reflect.DeepEqual(got.Items, want) || got.NextCursor != nil {
		t.Fatalf("page %+v", got)
	}
	seen := h.fake.seen()
	if len(seen) != 1 || seen[0].Path != "/rest/api/3/search/jql" {
		t.Fatalf("requests %+v", seen)
	}
	q := seen[0].Query
	if q.Get("jql") != `project = "ENG" AND statusCategory != Done ORDER BY created ASC` || q.Get("fields") != listFields || q.Get("maxResults") != "10" || q.Has("nextPageToken") {
		t.Fatalf("query %v", q)
	}
	if !seen[0].HasBasic || seen[0].User != fakeEmail || seen[0].Pass != fakeToken {
		t.Fatalf("auth %+v, want Basic auth", seen[0])
	}
}

func TestListJQLQuotesAndRefusesSelectors(t *testing.T) {
	c := siteConfig{Site: fakeSite, Project: "ENG", JQL: `component = "Back end" AND summary ~ "a)b"`}
	cases := map[string]string{
		"oldest":         `project = "ENG" AND statusCategory != Done AND (component = "Back end" AND summary ~ "a)b") ORDER BY created ASC`,
		"label:ready.v2": `project = "ENG" AND statusCategory != Done AND (component = "Back end" AND summary ~ "a)b") AND labels = "ready.v2" ORDER BY created ASC`,
		"filter:10042":   `project = "ENG" AND statusCategory != Done AND (component = "Back end" AND summary ~ "a)b") AND filter = 10042 ORDER BY created ASC`,
	}
	for selector, want := range cases {
		got, err := listJQL(c, selector)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", selector, got, err, want)
		}
	}
	for _, selector := range []string{
		`label:x" OR project = OTHER OR labels = "y`, "label:a b", "label:", "label:" + strings.Repeat("a", 65), `label:x\`,
		"filter:1 OR project = OTHER", "filter:0", "filter:-1", "filter:", "filter:12a",
		"all", "jql:project = OTHER", "oldest ",
	} {
		if got, err := listJQL(c, selector); err == nil {
			t.Errorf("%q was accepted as %q", selector, got)
		}
	}
	if got := jqlString(`a"b\c`); got != `"a\"b\\c"` {
		t.Fatalf("jqlString %s", got)
	}
}

func TestListSendsTheSelectorQuery(t *testing.T) {
	h := newHarness(t)
	cfg := siteCfg()
	cfg["jql"] = "priority = High"
	decoded[contract.TrackerPage](t, h, "tracker_list", map[string]any{"config": cfg, "selector": "label:forgecell", "limit": 5})
	if got := h.fake.seen()[0].Query.Get("jql"); got != `project = "ENG" AND statusCategory != Done AND (priority = High) AND labels = "forgecell" ORDER BY created ASC` {
		t.Fatalf("jql %q", got)
	}
	_, err := h.call(t, "tracker_list", map[string]any{"config": siteCfg(), "selector": `label:x" OR key = "OTHER-1`, "limit": 5})
	failure(t, err, mcpserver.Failed)
	if n := len(h.fake.seen()); n != 1 {
		t.Fatalf("%d requests, want the injection refused before a request", n)
	}
}

func TestListPagesWithNextPageToken(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeJira) {
		f.pageSize = 1
		for i := 5; i <= 12; i++ {
			f.issues = append(f.issues, issue("ENG-"+strconv.Itoa(i), "Issue", "new", "", "2026-10-30T10:00:00.000+0000", nil))
		}
	})
	// Three items fill the limit within the page bound.
	got := decoded[contract.TrackerPage](t, h, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 3})
	if len(got.Items) != 3 || got.Items[2].Key != "ENG-5" || got.NextCursor == nil || *got.NextCursor != "tok-3" {
		t.Fatalf("page %+v", got)
	}
	seen := h.fake.seen()
	if len(seen) != 3 || seen[1].Query.Get("nextPageToken") != "tok-1" || seen[1].Query.Get("maxResults") != "2" || seen[2].Query.Get("maxResults") != "1" {
		t.Fatalf("requests %+v", seen)
	}
	// The cursor resumes where the page ended.
	next := decoded[contract.TrackerPage](t, h, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 1, "cursor": *got.NextCursor})
	if len(next.Items) != 1 || next.Items[0].Key != "ENG-6" || *next.NextCursor != "tok-4" {
		t.Fatalf("page %+v", next)
	}
	// Five pages is the bound: the page is cut there and says where to resume.
	h.fake.set(func(f *fakeJira) { f.requests = nil })
	bounded := decoded[contract.TrackerPage](t, h, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 10})
	if len(bounded.Items) != maxPages || bounded.NextCursor == nil || *bounded.NextCursor != "tok-5" {
		t.Fatalf("page %+v", bounded)
	}
	if n := len(h.fake.seen()); n != maxPages {
		t.Fatalf("%d requests, want %d", n, maxPages)
	}
	// The last page has no cursor.
	last := decoded[contract.TrackerPage](t, h, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 10, "cursor": "tok-8"})
	if len(last.Items) != 2 || last.NextCursor != nil {
		t.Fatalf("page %+v", last)
	}
}

func TestPagingFailsSafe(t *testing.T) {
	switches := map[string]func(f *fakeJira){
		"repeated token": func(f *fakeJira) { f.repeat = true },
		"bad token":      func(f *fakeJira) { f.badToken = true },
		"empty pages":    func(f *fakeJira) { f.endless = true },
		"big page":       func(f *fakeJira) { f.bigPage = true },
	}
	for name, change := range switches {
		h := newHarness(t)
		h.fake.set(change)
		_, err := h.call(t, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 10})
		failure(t, err, mcpserver.Failed)
		if n := len(h.fake.seen()); n > maxPages {
			t.Fatalf("%s: %d requests", name, n)
		}
	}
	// The request's own cursor coming back is a repeat too.
	h := newHarness(t)
	h.fake.set(func(f *fakeJira) { f.repeat = true })
	_, err := h.call(t, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 1, "cursor": "tok-same"})
	failure(t, err, mcpserver.Failed)
	if n := len(h.fake.seen()); n != 1 {
		t.Fatalf("%d requests", n)
	}
}

func TestListRefusesMalformedIssues(t *testing.T) {
	changes := map[string]func(is map[string]any){
		"other project": func(is map[string]any) { is["key"] = "OPS-1" },
		"bad key":       func(is map[string]any) { is["key"] = "ENG-01" },
		"bad time":      func(is map[string]any) { is["fields"].(map[string]any)["updated"] = "yesterday" },
		"no status":     func(is map[string]any) { delete(is["fields"].(map[string]any), "status") },
	}
	for name, change := range changes {
		h := newHarness(t)
		h.fake.set(func(f *fakeJira) { change(f.issues[0]) })
		_, err := h.call(t, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 10})
		if msg := failure(t, err, mcpserver.Failed); !strings.Contains(msg, "Jira sent") {
			t.Fatalf("%s: %q", name, msg)
		}
	}
}

func TestListBoundsTitlesLabelsAndMilestones(t *testing.T) {
	h := newHarness(t)
	labels := []string{strings.Repeat("l", 150)}
	for i := range 30 {
		labels = append(labels, "label"+strconv.Itoa(i))
	}
	h.fake.set(func(f *fakeJira) {
		f.issues = []map[string]any{issue("ENG-1", strings.Repeat("t", 400), "new", "", "2026-10-30T10:00:00Z", labels, strings.Repeat("m", 300))}
	})
	got := decoded[contract.TrackerPage](t, h, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 10})
	item := got.Items[0]
	if !strings.HasSuffix(item.Title, "…") || len([]rune(item.Title)) != maxTitleRunes {
		t.Fatalf("title %q", item.Title)
	}
	if len(item.Labels) != maxLabels || len([]rune(item.Labels[0])) != maxLabelRunes || !strings.HasSuffix(item.Labels[0], "…") {
		t.Fatalf("labels %v", item.Labels)
	}
	if item.Milestone == nil || len([]rune(*item.Milestone)) != maxMilestoneRunes {
		t.Fatalf("milestone %v", item.Milestone)
	}
}

func TestGetMapsTheIssue(t *testing.T) {
	h := newHarness(t)
	got := decoded[contract.TrackerIssue](t, h, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-1"})
	want := contract.TrackerIssue{Key: "ENG-1", Title: "First issue", Body: "Body of ENG-1", State: "open", URL: "https://" + fakeSite + "/browse/ENG-1", UpdatedAt: "2026-10-30T10:00:00Z", Labels: []string{"forgecell", "backend"}, Milestone: ptr("1.0")}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("issue %+v, want %+v", got, want)
	}
	seen := h.fake.seen()
	if len(seen) != 1 || seen[0].Path != "/rest/api/3/issue/ENG-1" || seen[0].Query.Get("fields") != getFields {
		t.Fatalf("requests %+v", seen)
	}
	closed := decoded[contract.TrackerIssue](t, h, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-4"})
	if closed.State != "closed" || closed.StateReason == nil || *closed.StateReason != "not_planned" {
		t.Fatalf("issue %+v", closed)
	}
	done := decoded[contract.TrackerIssue](t, h, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-3"})
	if done.State != "closed" || *done.StateReason != "completed" {
		t.Fatalf("issue %+v", done)
	}
}

func TestGetHandlesDescriptionsAndRefusesOtherIssues(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeJira) { f.issues[0]["fields"].(map[string]any)["description"] = nil })
	if got := decoded[contract.TrackerIssue](t, h, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-1"}); got.Body != "" {
		t.Fatalf("body %q", got.Body)
	}
	h.fake.set(func(f *fakeJira) { f.issues[0]["fields"].(map[string]any)["description"] = 7 })
	_, err := h.call(t, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-1"})
	failure(t, err, mcpserver.Failed)
	// A moved issue answers with its new key, which is not the Issue asked for.
	h.fake.set(func(f *fakeJira) { f.movedFrom = "ENG-50" })
	_, err = h.call(t, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-50"})
	if msg := failure(t, err, mcpserver.Failed); !strings.Contains(msg, "different issue") {
		t.Fatalf("message %q", msg)
	}
	_, err = h.call(t, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-77"})
	if msg := failure(t, err, mcpserver.Failed); !strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "permission") {
		t.Fatalf("message %q", msg)
	}
}

func TestKeysOutsideThePatternAreRefusedBeforeAnyRequest(t *testing.T) {
	h := newHarness(t)
	for _, key := range []string{"ENG-0", "ENG-01", "OPS-1", "eng-1", "ENG-12345678901", "ENG-1 OR key = OPS-1", "ENG-1/../../x", `ENG-1"`, "ENG", "ENG-", "ENGX-1"} {
		_, err := h.call(t, "tracker_get", map[string]any{"config": siteCfg(), "key": key})
		failure(t, err, mcpserver.Failed)
		_, err = h.call(t, "tracker_states", map[string]any{"config": siteCfg(), "keys": []string{"ENG-1", key}})
		failure(t, err, mcpserver.Failed)
	}
	if n := len(h.fake.seen()); n != 0 {
		t.Fatalf("%d requests, want none", n)
	}
	for _, key := range []string{"ENG-1", "ENG-9999999999"} {
		if !validKey("ENG", key) {
			t.Errorf("%s was refused", key)
		}
	}
}

func TestStatesReturnOnlyRequestedKeys(t *testing.T) {
	h := newHarness(t)
	h.fake.set(func(f *fakeJira) {
		f.issues[2]["fields"].(map[string]any)["resolutiondate"] = "2026-10-29T08:00:00.000+0000"
	})
	got := decoded[contract.TrackerStates](t, h, "tracker_states", map[string]any{"config": siteCfg(), "keys": []string{"ENG-1", "ENG-3", "ENG-4", "ENG-3", "ENG-404"}})
	want := map[string]contract.TrackerState{
		"ENG-1": {State: "open"},
		"ENG-3": {State: "closed", StateReason: ptr("completed"), ClosedAt: ptr("2026-10-29T08:00:00Z")},
		"ENG-4": {State: "closed", StateReason: ptr("not_planned"), ClosedAt: ptr("2026-10-29T06:00:00Z")},
	}
	if !reflect.DeepEqual(got.States, want) {
		t.Fatalf("states %+v, want %+v", got.States, want)
	}
	seen := h.fake.seen()
	if len(seen) != 1 || seen[0].Query.Get("jql") != `key in ("ENG-1", "ENG-3", "ENG-4", "ENG-404")` || seen[0].Query.Get("fields") != statesFields || seen[0].Query.Get("maxResults") != "50" {
		t.Fatalf("requests %+v", seen)
	}
	none := decoded[contract.TrackerStates](t, h, "tracker_states", map[string]any{"config": siteCfg(), "keys": []string{}})
	if none.States == nil || len(none.States) != 0 {
		t.Fatalf("states %+v", none)
	}
	if n := len(h.fake.seen()); n != 1 {
		t.Fatalf("no keys made %d requests", n-1)
	}
}

func TestStatesLeaveOutKeysJiraAddsAndFailOnAnUnfinishedListing(t *testing.T) {
	h := newHarness(t)
	// Jira answers for keys that were not asked for as well; they are left out.
	h.fake.set(func(f *fakeJira) { f.extraKeys = true })
	got := decoded[contract.TrackerStates](t, h, "tracker_states", map[string]any{"config": siteCfg(), "keys": []string{"ENG-2"}})
	if !reflect.DeepEqual(got.States, map[string]contract.TrackerState{"ENG-2": {State: "open"}}) {
		t.Fatalf("states %+v", got)
	}
	h = newHarness(t)
	h.fake.set(func(f *fakeJira) { f.endless = true })
	_, err := h.call(t, "tracker_states", map[string]any{"config": siteCfg(), "keys": []string{"ENG-1"}})
	failure(t, err, mcpserver.Failed)
	h = newHarness(t)
	h.fake.set(func(f *fakeJira) { f.issues[2]["fields"].(map[string]any)["resolutiondate"] = "soon" })
	_, err = h.call(t, "tracker_states", map[string]any{"config": siteCfg(), "keys": []string{"ENG-3"}})
	failure(t, err, mcpserver.Failed)
}

func TestStateAndReasonMapping(t *testing.T) {
	cases := []struct {
		category, resolution string
		state, reason        string
	}{
		{"new", "", "open", ""},
		{"indeterminate", "", "open", ""},
		{"undefined", "", "open", ""},
		{"", "", "open", ""},
		{"new", "Won't Do", "open", ""},
		{"done", "", "closed", "completed"},
		{"done", "Done", "closed", "completed"},
		{"done", "Fixed", "closed", "completed"},
		{"done", "Won't Do", "closed", "not_planned"},
		{"done", "won't fix", "closed", "not_planned"},
		{"done", "WON’T FIX", "closed", "not_planned"},
		{"done", "Duplicate", "closed", "not_planned"},
		{"done", "cannot reproduce", "closed", "not_planned"},
		{"done", "Declined", "closed", "not_planned"},
		{"done", "Declined later", "closed", "completed"},
		{"Done", "Won't Do", "open", ""},
	}
	for _, c := range cases {
		var res *jiraNamed
		if c.resolution != "" {
			res = &jiraNamed{Name: c.resolution}
		}
		state, reason := stateOf(c.category, res)
		got := ""
		if reason != nil {
			got = *reason
		}
		if state != c.state || got != c.reason {
			t.Errorf("%q %q: %s %s, want %s %s", c.category, c.resolution, state, got, c.state, c.reason)
		}
	}
}

func TestJiraTimes(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-30T10:00:00.000+0000": "2026-10-30T10:00:00Z",
		"2026-10-30T10:00:00.5+0200":   "2026-10-30T08:00:00.5Z",
		"2026-10-30T10:00:00+0200":     "2026-10-30T08:00:00Z",
		"2026-10-30T10:00:00.250Z":     "2026-10-30T10:00:00.25Z",
		"2026-10-30":                   "",
		"":                             "",
		strings.Repeat("9", 50):        "",
	} {
		got, ok := jiraTime(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v, want %q", in, got, ok, want)
		}
	}
}

func TestHTTPErrorsMapToFixedReasons(t *testing.T) {
	cases := []struct {
		status int
		want   mcpserver.Reason
	}{
		{401, mcpserver.AuthMissing},
		{403, mcpserver.Failed},
		{429, mcpserver.RateLimited},
		{404, mcpserver.Failed},
		{400, mcpserver.Failed},
		{500, mcpserver.Failed},
		{503, mcpserver.Failed},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.fake.set(func(f *fakeJira) { f.status = c.status })
		for _, call := range []struct {
			name string
			args map[string]any
		}{
			{"tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 10}},
			{"tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-1"}},
			{"tracker_states", map[string]any{"config": siteCfg(), "keys": []string{"ENG-1"}}},
		} {
			_, err := h.call(t, call.name, call.args)
			msg := failure(t, err, c.want)
			if !strings.Contains(msg, "HTTP "+strconv.Itoa(c.status)) || strings.Contains(msg, "denied") {
				t.Fatalf("%d: message %q, want the status and never the body", c.status, msg)
			}
		}
	}
}

func TestResponseFailuresFailSafe(t *testing.T) {
	switches := map[string]func(f *fakeJira){
		"redirect":  func(f *fakeJira) { f.redirect = true },
		"oversized": func(f *fakeJira) { f.oversized = true },
		"bad json":  func(f *fakeJira) { f.badJSON = true },
		"duplicate": func(f *fakeJira) { f.duplicate = true },
		"not json":  func(f *fakeJira) { f.wrongType = true },
	}
	for name, change := range switches {
		h := newHarness(t)
		h.fake.set(change)
		_, err := h.call(t, "tracker_list", map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 10})
		failure(t, err, mcpserver.Failed)
		if n := len(h.fake.seen()); n != 1 {
			t.Fatalf("%s: %d requests, want one and the redirect left unfollowed", name, n)
		}
	}
}

func TestTransportErrorsAndTimeoutsAreNetwork(t *testing.T) {
	closed := newFakeJira(t)
	h := newHarnessWith(t, closed, closed.env(t))
	closed.srv.Close()
	_, err := h.call(t, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-1"})
	failure(t, err, mcpserver.Network)

	hung := newFakeJira(t)
	cfg, err := loadConfig(lookupFrom(hung.env(t)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.timeout = 50 * time.Millisecond
	hung.set(func(f *fakeJira) { f.hang = true })
	c := newClient(cfg)
	c.origin = hung.srv.URL
	_, err = (&harness{fake: hung, tr: &tracker{c: c, customCA: true}}).call(t, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-1"})
	failure(t, err, mcpserver.Network)
}

func TestTLSVerificationIsNeverSkipped(t *testing.T) {
	fake := newFakeJira(t)
	env := fake.env(t)
	delete(env, caFileEnv)
	h := newHarnessWith(t, fake, env)
	_, err := h.call(t, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-1"})
	failure(t, err, mcpserver.Network)
	if n := len(fake.seen()); n != 0 {
		t.Fatalf("%d requests reached an untrusted server", n)
	}
	// The seam never turns plain http on.
	h.tr.c.origin = strings.Replace(fake.srv.URL, "https://", "http://", 1)
	_, err = h.call(t, "tracker_get", map[string]any{"config": siteCfg(), "key": "ENG-1"})
	failure(t, err, mcpserver.Failed)
}

func TestCredentialRules(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"token alone":    {tokenEnv: fakeToken},
		"email alone":    {emailEnv: fakeEmail},
		"spaced token":   {emailEnv: fakeEmail, tokenEnv: "bad token"},
		"newline token":  {emailEnv: fakeEmail, tokenEnv: "bad\ntoken"},
		"long token":     {emailEnv: fakeEmail, tokenEnv: strings.Repeat("a", maxTokenBytes+1)},
		"not an email":   {emailEnv: "bot", tokenEnv: fakeToken},
		"spaced email":   {emailEnv: "bot @example.com", tokenEnv: fakeToken},
		"relative CA":    {caFileEnv: "jira-ca.pem"},
		"missing CA":     {caFileEnv: filepath.Join(plugintest.TempDir(t), "missing.pem")},
		"unclean CA":     {caFileEnv: plugintest.TempDir(t) + "/./x/../ca.pem"},
		"long email":     {emailEnv: strings.Repeat("a", maxEmailBytes) + "@x", tokenEnv: fakeToken},
		"token in email": {emailEnv: fakeToken + "@\x7f", tokenEnv: fakeToken},
	} {
		_, err := loadConfig(lookupFrom(env))
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), "/") {
			t.Errorf("%s: the error names a value: %v", name, err)
		}
	}
	cfg, err := loadConfig(lookupFrom(map[string]string{}))
	if err != nil || cfg.token != "" || cfg.customCA {
		t.Fatalf("anonymous config %+v %v", cfg, err)
	}
}

func TestCAFileRefusesUnsafeFiles(t *testing.T) {
	fake := newFakeJira(t)
	good := fake.caFile(t)
	dir := plugintest.TempDir(t)
	goodPEM, _ := os.ReadFile(good)
	link := filepath.Join(dir, "link.pem")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(dir, "open.pem")
	if err := os.WriteFile(open, goodPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o620); err != nil {
		t.Fatal(err)
	}
	text := filepath.Join(dir, "text.pem")
	if err := os.WriteFile(text, []byte("not a pem bundle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, open, text} {
		if _, err := loadConfig(lookupFrom(map[string]string{caFileEnv: path})); err == nil {
			t.Errorf("%s: accepted", filepath.Base(path))
		} else if strings.Contains(err.Error(), dir) {
			t.Errorf("the error names a local path: %v", err)
		}
	}
}

func TestAnInvalidConfigurationFailsEveryCallBeforeAnyRequest(t *testing.T) {
	fake := newFakeJira(t)
	env := fake.env(t)
	env[tokenEnv] = "bad token"
	tr := newTracker(lookupFrom(env))
	if tr.configErr == nil {
		t.Fatal("a token with a space was accepted")
	}
	c, _ := contract.Lookup("tracker")
	args := map[string]any{"config": siteCfg(), "selector": "oldest", "limit": 1, "key": "ENG-1", "keys": []string{"ENG-1"}}
	for i := range c.Tools {
		tool := &c.Tools[i]
		in := map[string]any{}
		for name := range tool.Input.Properties {
			if v, ok := args[name]; ok {
				in[name] = v
			}
		}
		raw, _ := json.Marshal(in)
		_, err := tr.bind(tool)(context.Background(), raw)
		if msg := failure(t, err, mcpserver.Failed); !strings.Contains(msg, "JIRA_API_TOKEN") {
			t.Fatalf("%s: message %q", tool.Name, msg)
		}
	}
	if n := len(fake.seen()); n != 0 {
		t.Fatalf("%d requests, want none", n)
	}
}

// TestTheTokenIsRedactedFromLogsAndErrors serves real MCP requests in process
// while Jira answers with a body that carries the token. The fake is reached
// through the origin seam, so the server is built from the in-package tracker.
func TestTheTokenIsRedactedFromLogsAndErrors(t *testing.T) {
	fake := newFakeJira(t)
	env := fake.env(t)
	t.Setenv(emailEnv, fakeEmail)
	t.Setenv(tokenEnv, fakeToken)
	t.Setenv(caFileEnv, env[caFileEnv])
	c, _ := contract.Lookup("tracker")
	var stderr, out bytes.Buffer
	s := mcpserver.New(serverName, serverVersion, c.ID)
	s.SecretEnv(tokenEnv)
	s.SetStderr(&stderr)
	tr := newTracker(mcpserver.Env)
	if tr.configErr != nil {
		t.Fatal(tr.configErr)
	}
	tr.c.origin = fake.srv.URL
	for i := range c.Tools {
		tool := &c.Tools[i]
		s.Tool(tool.Name, tool.InputSchema(), tr.bind(tool), mcpserver.MaxOutputBytes(tool.MaxOutputBytes))
	}
	cfg := `{"site":"` + fakeSite + `","project":"ENG"}`
	lines := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tracker_get","arguments":{"config":` + cfg + `,"key":"ENG-1"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"tracker_get","arguments":{"config":` + cfg + `,"key":"` + fakeToken + `"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"tracker_list","arguments":{"config":` + cfg + `,"selector":"label:` + fakeToken + ` x","limit":1}}}`,
	}
	fake.set(func(f *fakeJira) { f.status = 401 })
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
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
	if !reflect.DeepEqual(reasons, []string{"auth-missing", "failed", "failed"}) {
		t.Fatalf("reasons %v", reasons)
	}
	if n := len(fake.seen()); n != 1 {
		t.Fatalf("%d requests, want only the valid key's", n)
	}
	// The whole server, built as main builds it, logs an invalid token setting
	// without its value.
	stderr.Reset()
	t.Setenv(tokenEnv, fakeToken+" x")
	newServer(mcpserver.Env, &stderr)
	if text := stderr.String(); strings.Contains(text, fakeToken) || !strings.Contains(text, "JIRA_API_TOKEN") {
		t.Fatalf("stderr %q", text)
	}
}

// renderManifest fills plugin.json.tmpl for a server binary and checks it
// decodes as a plugin manifest.
func renderManifest(t *testing.T, server string) []byte {
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
	return []byte(strings.ReplaceAll(out, "{{SERVER_SHA256}}", hex.EncodeToString(sum[:])))
}

func TestTheManifestTemplateDeclaresTheContract(t *testing.T) {
	server := filepath.Join(plugintest.TempDir(t), "forgecell-jira")
	if err := os.WriteFile(server, []byte("stand-in"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := plugin.Decode(renderManifest(t, server))
	if err != nil {
		t.Fatalf("the rendered template is not a valid manifest: %v", err)
	}
	if m.Kind != "tracker" || m.Contract != "forgecell.tracker/v1" || m.Name != "jira" || len(m.Server.Args) != 0 {
		t.Fatalf("manifest %+v", m)
	}
	if !reflect.DeepEqual(m.Needs.Env, []string{emailEnv, tokenEnv, caFileEnv}) || !reflect.DeepEqual(m.Needs.Network, []string{"atlassian.net"}) {
		t.Fatalf("needs %+v", m.Needs)
	}
	c, _ := contract.Lookup("tracker")
	if len(m.Tools) != len(c.Tools) {
		t.Fatalf("tools %+v", m.Tools)
	}
	for i, tool := range c.Tools {
		if m.Tools[i].Name != tool.Name || m.Tools[i].Access != plugin.AccessFor(tool.Access) {
			t.Fatalf("tool %+v, want %s %s", m.Tools[i], tool.Name, plugin.AccessFor(tool.Access))
		}
	}
}
