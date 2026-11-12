package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/delivery"
	"github.com/DanielNetzer/forgecell/lab/internal/fixround"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
)

// findingsSyncWait waits for the latest background pass to end.
func findingsSyncWait(h *handler) {
	s := h.findingsSync
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}

// findingsSyncClock is the injected clock of the background pass.
type findingsSyncClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *findingsSyncClock) get() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *findingsSyncClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// findingsSyncCollector replaces fixround.Collect: it records each call and
// the most collections in flight at once, and fails while err is set.
type findingsSyncCollector struct {
	mu       sync.Mutex
	calls    []string
	inflight int
	most     int
	err      error
	during   func(ctx context.Context, id string) error
	// record is what each collection returns.
	record delivery.FindingsRecord
}

func (c *findingsSyncCollector) returns(record delivery.FindingsRecord) {
	c.mu.Lock()
	c.record = record
	c.mu.Unlock()
}

func (c *findingsSyncCollector) collected() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.calls...)
}

// findingsSyncFixture is a Lab UI with n published Molecules whose snapshots
// declare a reviewer, none collected yet. Proposing, dismissing and pushing
// fail the test.
func findingsSyncFixture(t *testing.T, n int, source string) (*handler, []Summary, *findingsSyncClock, *findingsSyncCollector) {
	t.Helper()
	lab := t.TempDir()
	var records []molecule.Record
	var list []Summary
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("mol-%d-abcdef", 100+i)
		records = append(records, findingsTestRecord(lab, id, true))
		findingsTestReceipt(t, lab, id)
		list = append(list, Summary{ID: id, Lane: LaneFinished, Delivery: "Draft published"})
	}
	hh, _ := findingsTestHandler(t, lab, source, records...)
	h := hh.(*handler)
	clock := &findingsSyncClock{now: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	h.findingsNow = clock.get
	h.findingsPoll = func(string, formula.Formula) time.Duration { return 120 * time.Second }
	c := &findingsSyncCollector{}
	h.findingsCollect = func(ctx context.Context, _, id string, _ formula.Formula) (delivery.FindingsRecord, error) {
		c.mu.Lock()
		c.calls = append(c.calls, id)
		c.inflight++
		c.most = max(c.most, c.inflight)
		err, during, record := c.err, c.during, c.record
		c.mu.Unlock()
		if during != nil {
			err = during(ctx, id)
		}
		c.mu.Lock()
		c.inflight--
		c.mu.Unlock()
		return record, err
	}
	h.fixRoundPropose = func(context.Context, string, string, []string) (fixround.Proposal, error) {
		t.Error("the background pass proposed a fix round")
		return fixround.Proposal{}, errors.New("no")
	}
	h.fixRoundPush = func(context.Context, string, string, string, bool) error {
		t.Error("the background pass pushed")
		return errors.New("no")
	}
	h.findingsDismiss = func(string, string, string, string) error {
		t.Error("the background pass dismissed a finding")
		return errors.New("no")
	}
	h.findingsUndismiss = func(string, string, string) error {
		t.Error("the background pass undismissed a finding")
		return errors.New("no")
	}
	return h, list, clock, c
}

// findingsSyncRun kicks one pass and waits for it.
func findingsSyncRun(h *handler, list []Summary) {
	h.findingsSyncKick(list)
	findingsSyncWait(h)
}

func TestFindingsSyncHonoursMinPollSeconds(t *testing.T) {
	h, list, clock, c := findingsSyncFixture(t, 1, "/src")
	findingsSyncRun(h, list)
	if got := c.collected(); len(got) != 1 {
		t.Fatalf("first pass collected %v", got)
	}
	// A second pass within a minute does not start; one within the plugin's
	// 120 s minPollSeconds starts but collects nothing.
	clock.advance(30 * time.Second)
	findingsSyncRun(h, list)
	clock.advance(89 * time.Second)
	findingsSyncRun(h, list)
	if got := c.collected(); len(got) != 1 {
		t.Fatalf("collected again before minPollSeconds: %v", got)
	}
	clock.advance(time.Minute)
	findingsSyncRun(h, list)
	if got := c.collected(); len(got) != 2 {
		t.Fatalf("no collection once minPollSeconds passed: %v", got)
	}
	// A collection the CLI recorded counts too: a round collected just now is
	// not collected again.
	r := findingsTestRecord(h.opts.LabDir, list[0].ID, true)
	round := findingsTestRound(findingsTestCommit, delivery.FindingsPending, true)
	clock.advance(10 * time.Minute)
	round.CollectedAt = clock.get().Format(time.RFC3339)
	findingsTestWrite(t, h.opts.LabDir, r.ID, nil, round)
	clock.advance(time.Minute)
	findingsSyncRun(h, list)
	if got := c.collected(); len(got) != 2 {
		t.Fatalf("collected within minPollSeconds of the recorded round: %v", got)
	}
	if c.most != 1 {
		t.Fatalf("%d collections ran at once", c.most)
	}
}

func TestFindingsSyncDefaultPollIsAMinute(t *testing.T) {
	f, err := formula.Parse([]byte(findingsTestYAML(true)))
	if err != nil {
		t.Fatal(err)
	}
	// The pinned manifest is absent, so it adds nothing to the minute.
	if got := findingsSyncPoll(t.TempDir(), f); got != findingsSyncMinPoll {
		t.Fatalf("poll %v", got)
	}
}

func TestFindingsSyncStopsAfterEightOldestFirst(t *testing.T) {
	h, list, clock, c := findingsSyncFixture(t, 10, "/src")
	findingsSyncRun(h, list)
	first := c.collected()
	if len(first) != findingsSyncMaxPerPass || c.most != 1 {
		t.Fatalf("first pass collected %v, %d at once", first, c.most)
	}
	clock.advance(2 * time.Minute)
	findingsSyncRun(h, list)
	second := c.collected()[len(first):]
	if len(second) != findingsSyncMaxPerPass {
		t.Fatalf("second pass collected %v", second)
	}
	// The two never collected come first, then the oldest collections.
	left := []string{list[8].ID, list[9].ID}
	if second[0] != left[0] || second[1] != left[1] {
		t.Fatalf("second pass order %v, want %v first", second, left)
	}
	sorted := append([]string{}, first...)
	sort.Strings(sorted)
	if strings.Join(first, ",") != strings.Join(sorted, ",") {
		t.Fatalf("first pass order %v", first)
	}
}

func TestFindingsSyncBacksOffOnFailure(t *testing.T) {
	h, list, clock, c := findingsSyncFixture(t, 1, "/src")
	h.findingsPoll = func(string, formula.Formula) time.Duration { return 0 }
	c.err = errors.New("the reviewer failed")
	at := func(want int) {
		t.Helper()
		findingsSyncRun(h, list)
		if got := len(c.collected()); got != want {
			t.Fatalf("at %v: %d collections, want %d", clock.get().Format(time.TimeOnly), got, want)
		}
	}
	at(1) // fails: the next waits twice the minute
	clock.advance(time.Minute)
	at(1)
	clock.advance(time.Minute)
	at(2) // fails again: the wait doubles to 4 minutes
	clock.advance(3 * time.Minute)
	at(2)
	clock.advance(time.Minute)
	at(3) // 8 minutes next
	clock.advance(7 * time.Minute)
	at(3)
	clock.advance(time.Minute)
	at(4)
	// The wait never passes an hour.
	for i := 0; i < 8; i++ {
		clock.advance(time.Hour)
		at(5 + i)
	}
	// A good collection resets it to the minute.
	c.mu.Lock()
	c.err = nil
	c.mu.Unlock()
	clock.advance(time.Hour)
	at(13)
	clock.advance(time.Minute)
	at(14)
}

// findingsSyncAt runs one pass and checks the collections so far.
func findingsSyncAt(t *testing.T, h *handler, list []Summary, clock *findingsSyncClock, c *findingsSyncCollector, want int) {
	t.Helper()
	findingsSyncRun(h, list)
	if got := len(c.collected()); got != want {
		t.Fatalf("at %v: %d collections, want %d", clock.get().Format(time.TimeOnly), got, want)
	}
}

func TestFindingsSyncRetriesAnErroredReviewerWithBackoff(t *testing.T) {
	h, list, clock, c := findingsSyncFixture(t, 1, "/src")
	h.findingsPoll = func(string, formula.Formula) time.Duration { return 0 }
	lab, id := h.opts.LabDir, list[0].ID
	// The reviewer errored on its first collection: a complete round on the
	// head that holds an error entry is wanted again.
	errored := findingsTestRound(findingsTestCommit, delivery.FindingsError, true)
	errored.Reviewers[0].Error = "the reviewer failed"
	findingsTestWrite(t, lab, id, nil, errored)
	c.returns(delivery.FindingsRecord{Rounds: []delivery.FindingsRound{errored}})
	at := func(want int) { t.Helper(); findingsSyncAt(t, h, list, clock, c, want) }
	at(1) // collected without error, but the reviewer errored: 2 minutes next
	clock.advance(time.Minute)
	at(1)
	clock.advance(time.Minute)
	at(2) // errored again: the wait doubles to 4 minutes
	clock.advance(3 * time.Minute)
	at(2)
	clock.advance(time.Minute)
	at(3) // 8 minutes next
	clock.advance(7 * time.Minute)
	at(3)
	// The reviewer answers: a good round resets the wait, and a complete
	// round on the head without errors is not collected again.
	good := findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true)
	c.returns(delivery.FindingsRecord{Rounds: []delivery.FindingsRound{good}})
	clock.advance(time.Minute)
	at(4)
	findingsTestWrite(t, lab, id, nil, good)
	clock.advance(time.Hour)
	at(4)
	h.findingsSync.mu.Lock()
	e := h.findingsSync.entries[id]
	h.findingsSync.mu.Unlock()
	if e == nil || e.failed || e.ttl != findingsSyncMinPoll {
		t.Fatalf("entry after a good round %+v", e)
	}
}

func TestFindingsSyncBacksOffATruncatedRound(t *testing.T) {
	h, list, clock, c := findingsSyncFixture(t, 1, "/src")
	h.findingsPoll = func(string, formula.Formula) time.Duration { return 0 }
	cut := findingsTestRound(findingsTestCommit, delivery.FindingsComplete, false, findingsTestFinding("a", "high", "src/a.go", "Nil deref"))
	findingsTestWrite(t, h.opts.LabDir, list[0].ID, nil, cut)
	c.returns(delivery.FindingsRecord{Rounds: []delivery.FindingsRound{cut}})
	at := func(want int) { t.Helper(); findingsSyncAt(t, h, list, clock, c, want) }
	at(1)
	clock.advance(time.Minute)
	at(1) // not a minute later: a failure waits twice the minute
	clock.advance(time.Minute)
	at(2)
	clock.advance(time.Minute)
	at(2) // the wait doubled
	clock.advance(3 * time.Minute)
	at(3)
	// A persistently truncated reviewer settles at one collection an hour.
	for i := 0; i < 10; i++ {
		clock.advance(time.Hour)
		at(4 + i)
	}
	clock.advance(59 * time.Minute)
	at(13)
	h.findingsSync.mu.Lock()
	ttl := h.findingsSync.entries[list[0].ID].ttl
	h.findingsSync.mu.Unlock()
	if ttl != issuesKeyedMaxPoll {
		t.Fatalf("wait %v, want %v", ttl, issuesKeyedMaxPoll)
	}
	// Neither an empty record nor a clean complete round is a failure.
	for _, r := range []delivery.FindingsRecord{{}, {Rounds: []delivery.FindingsRound{findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true)}}} {
		if findingsSyncFailedRound(r) {
			t.Fatalf("failed round %+v", r)
		}
	}
}

// Opening the Lab while a reviewer fails collects once, not again a minute
// later: a failed round waits twice the interval. Collect now still runs.
func TestFindingsSyncDoesNotRecollectAFailedRoundWithinTheWindow(t *testing.T) {
	h, list, clock, c := findingsSyncFixture(t, 1, "/src")
	h.findingsPoll = func(string, formula.Formula) time.Duration { return 0 }
	errored := findingsTestRound(findingsTestCommit, delivery.FindingsError, true)
	errored.Reviewers[0].Error = "the reviewer failed"
	c.returns(delivery.FindingsRecord{Rounds: []delivery.FindingsRound{errored}})
	at := func(want int) { t.Helper(); findingsSyncAt(t, h, list, clock, c, want) }
	at(1)
	clock.advance(findingsSyncMinPoll)
	at(1)
	clock.advance(findingsSyncMinPoll - time.Second)
	at(1)
	clock.advance(time.Second)
	at(2)
	h.findingsSync.mu.Lock()
	ttl := h.findingsSync.entries[list[0].ID].ttl
	h.findingsSync.mu.Unlock()
	if ttl < findingsSyncFailedWait(findingsSyncMinPoll) {
		t.Fatalf("wait %v after two failures", ttl)
	}
	// The throttle is the background pass's alone: Collect now is not held back.
	if w := do(t, h, gatePost("/api/molecules/"+list[0].ID+"/findings/collect", `{}`)); w.Code != http.StatusAccepted {
		t.Fatalf("collect now: %d %s", w.Code, w.Body)
	}
	waitJob(t, h, findingsJobKey(list[0].ID))
	if got := len(c.collected()); got != 3 {
		t.Fatalf("%d collections after Collect now, want 3", got)
	}
}

func TestFindingsSyncStopsOnCancel(t *testing.T) {
	h, list, _, c := findingsSyncFixture(t, 3, "/src")
	c.during = func(ctx context.Context, id string) error {
		h.jobs.cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	findingsSyncRun(h, list)
	if got := c.collected(); len(got) != 1 {
		t.Fatalf("collected after cancel: %v", got)
	}
	// A cancelled Lab UI starts no further pass.
	h.findingsSync.mu.Lock()
	h.findingsSync.lastPass = time.Time{}
	h.findingsSync.mu.Unlock()
	findingsSyncRun(h, list)
	if got := c.collected(); len(got) != 1 {
		t.Fatalf("a stopped Lab UI collected: %v", got)
	}
}

func TestFindingsSyncDoesNothingOnAReadOnlyLab(t *testing.T) {
	h, list, _, c := findingsSyncFixture(t, 2, "")
	findingsSyncRun(h, list)
	if got := c.collected(); len(got) != 0 || h.findingsSync.done != nil {
		t.Fatalf("a read-only Lab collected: %v", got)
	}
	// Sample data never collects either.
	demo := testHandler(t, Options{Demo: true, SourceRoot: "/src"}).(*handler)
	demo.findingsSyncKick(list)
	if demo.findingsSync.done != nil {
		t.Fatal("sample data started a pass")
	}
}

func TestFindingsSyncSkipsMoleculesWithACompleteRoundOnTheHead(t *testing.T) {
	h, list, _, c := findingsSyncFixture(t, 6, "/src")
	lab := h.opts.LabDir
	a := findingsTestFinding("a", "high", "src/a.go", "Nil deref")
	// 0: complete on the head, skipped. 1: complete on an older commit only.
	// 2: newest round pending on the head. 3: newest round incomplete.
	findingsTestWrite(t, lab, list[0].ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, true, a))
	findingsTestWrite(t, lab, list[1].ID, nil, findingsTestRound(strings.Repeat("6", 40), delivery.FindingsComplete, true, a))
	findingsTestWrite(t, lab, list[2].ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsPending, true))
	findingsTestWrite(t, lab, list[3].ID, nil, findingsTestRound(findingsTestCommit, delivery.FindingsComplete, false, a))
	// 4: no reviewers in its snapshot. 5: no published receipt. Neither runs.
	h.records = func(_, id string) (molecule.Record, error) {
		return findingsTestRecord(lab, id, id != list[4].ID), nil
	}
	if err := removeFile(lab, list[5].ID); err != nil {
		t.Fatal(err)
	}
	// Recorded rounds are older than the minute.
	clockLater(h, time.Hour)
	findingsSyncRun(h, list)
	got := c.collected()
	sort.Strings(got)
	want := []string{list[1].ID, list[2].ID, list[3].ID}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("collected %v, want %v", got, want)
	}
	// A retired, merged or unpublished listing never reaches the pass.
	h2, list2, _, c2 := findingsSyncFixture(t, 3, "/src")
	list2[0].Lane, list2[1].Inbox.Merged, list2[2].Delivery = LaneRetired, true, "Delivery in progress · receipt committed"
	findingsSyncRun(h2, list2)
	if got := c2.collected(); len(got) != 0 {
		t.Fatalf("collected %v", got)
	}
}

// clockLater moves the pass's clock d past the recorded rounds.
func clockLater(h *handler, d time.Duration) {
	at := time.Date(2026, 11, 1, 11, 30, 0, 0, time.UTC).Add(d)
	h.findingsNow = func() time.Time { return at }
}

// removeFile removes a test Molecule's receipt.
func removeFile(lab, id string) error {
	return os.Remove(filepath.Join(lab, "deliveries", id+".json"))
}
