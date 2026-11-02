package molecule

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/mcp"
)

// fakeKeyed is an in-process tracker.Keyed. pages serves List by cursor ("" is
// the first page); issues serves Get by key. It records every List selector
// and cursor and every Get key.
type fakeKeyed struct {
	pattern     string
	describeErr error
	pages       map[string]contract.TrackerPage
	issues      map[string]contract.TrackerIssue
	getErr      error

	mu    sync.Mutex
	lists []string
	gets  []string
}

func (f *fakeKeyed) Describe(context.Context) (contract.TrackerDescription, error) {
	if f.describeErr != nil {
		return contract.TrackerDescription{}, f.describeErr
	}
	return contract.TrackerDescription{DisplayName: "Fake", KeyPattern: f.pattern, States: []string{"open", "closed"}, Selectors: []string{"oldest", "filter:"}}, nil
}

func (f *fakeKeyed) List(_ context.Context, selector, cursor string, limit int) (contract.TrackerPage, error) {
	f.mu.Lock()
	f.lists = append(f.lists, selector+"|"+cursor)
	f.mu.Unlock()
	if limit != 100 {
		return contract.TrackerPage{}, errors.New("unexpected limit")
	}
	return f.pages[cursor], nil
}

func (f *fakeKeyed) Get(_ context.Context, key string) (contract.TrackerIssue, error) {
	f.mu.Lock()
	f.gets = append(f.gets, key)
	f.mu.Unlock()
	if f.getErr != nil {
		return contract.TrackerIssue{}, f.getErr
	}
	issue, ok := f.issues[key]
	if !ok {
		return contract.TrackerIssue{}, errors.New("not found")
	}
	return issue, nil
}

func (f *fakeKeyed) States(context.Context, []string) (contract.TrackerStates, error) {
	return contract.TrackerStates{}, errors.New("not used")
}

func (f *fakeKeyed) listCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lists...)
}

func (f *fakeKeyed) getCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.gets...)
}

func fakeTrackerIssue(key, state string) contract.TrackerIssue {
	return contract.TrackerIssue{Key: key, Title: "Change code " + key, Body: "Write changed", State: state, URL: "https://tracker.example.test/browse/" + key, UpdatedAt: "2026-11-01T10:00:00Z", Labels: []string{}}
}

func TestTrackerKeyPatternAnchors(t *testing.T) {
	t.Parallel()
	for pattern, want := range map[string]string{
		"^ENG-[0-9]+$": "^ENG-[0-9]+$",
		"ENG-[0-9]+":   "^(?:ENG-[0-9]+)$",
		"^ENG-[0-9]+":  "^(?:^ENG-[0-9]+)$",
	} {
		got, err := TrackerKeyPattern(context.Background(), &fakeKeyed{pattern: pattern})
		if err != nil || got != want {
			t.Errorf("%s: got %q, %v; want %q", pattern, got, err, want)
		}
	}
	if _, err := TrackerKeyPattern(context.Background(), &fakeKeyed{describeErr: errors.New("plugin fake tracker_describe: failed")}); err == nil || err.Error() != "plugin fake tracker_describe: failed" {
		t.Fatalf("describe failure: %v", err)
	}
}

func TestKeyedReaderMapsTheTrackerIssue(t *testing.T) {
	t.Parallel()
	f := &fakeKeyed{issues: map[string]contract.TrackerIssue{
		"ENG-1": fakeTrackerIssue("ENG-1", "open"),
		"ENG-2": fakeTrackerIssue("ENG-2", "closed"),
		"ENG-3": fakeTrackerIssue("ENG-3", "archived"),
	}}
	read := KeyedReader(f, nil)
	for key, state := range map[string]string{"ENG-1": "OPEN", "ENG-2": "CLOSED", "ENG-3": ""} {
		issue, err := read(context.Background(), "acme/widgets", key, "/ignored")
		if err != nil {
			t.Fatal(err)
		}
		want := Issue{Key: key, Number: 0, Repo: "acme/widgets", Title: "Change code " + key, Body: "Write changed", State: state, URL: "https://tracker.example.test/browse/" + key, UpdatedAt: "2026-11-01T10:00:00Z"}
		if issue != want {
			t.Errorf("%s: %+v, want %+v", key, issue, want)
		}
	}
	// An unknown state never reads as open, so RunKeyed refuses it.
	if readKeyedIssueMatches(Issue{Key: "ENG-3", Repo: "acme/widgets", URL: "https://tracker.example.test/browse/ENG-3"}, "acme/widgets", "ENG-3") {
		t.Fatal("an Issue with no mapped state matched")
	}
}

func TestKeyedReaderRecordsAFailedGet(t *testing.T) {
	t.Parallel()
	lab := t.TempDir()
	fail := &mcpadapter.Error{Plugin: "fake", Tool: "tracker_get", Reason: mcp.Failed, Diagnostics: "boom at " + lab + "/plugins"}
	calls := NewPluginCallLog(lab)
	read := KeyedReader(&fakeKeyed{getErr: fail}, calls)
	if _, err := read(context.Background(), "acme/widgets", "ENG-1", ""); !errors.Is(err, fail) {
		t.Fatalf("err = %v", err)
	}
	var r Record
	if !absorbPluginCalls(&r, calls) || len(r.Notes) != 1 {
		t.Fatalf("notes = %q", r.Notes)
	}
	if note := r.Notes[0]; !strings.HasPrefix(note, PluginDiagnosticsNote+"plugin fake tracker_get: ") || strings.Contains(note, lab) || !strings.Contains(note, "boom at ") {
		t.Fatalf("note = %q", note)
	}
	// A nil log and a plain error add nothing and never panic.
	if _, err := KeyedReader(&fakeKeyed{getErr: errors.New("plain")}, nil)(context.Background(), "acme/widgets", "ENG-1", ""); err == nil {
		t.Fatal("a failed Get succeeded")
	}
}
