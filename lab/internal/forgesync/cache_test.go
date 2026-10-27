package forgesync

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFetcher counts syncs; gate, when set, holds each listing until closed.
type fakeFetcher struct {
	lists, states atomic.Int32
	started       chan struct{}
	gate          chan struct{}
	err           error
	gotIssues     []int64
	gotPRs        []int64
	mu            sync.Mutex
}

func (f *fakeFetcher) OpenIssues(ctx context.Context, repository string) (Listing, error) {
	f.lists.Add(1)
	if f.started != nil {
		f.started <- struct{}{}
	}
	if f.gate != nil {
		<-f.gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return Listing{}, f.err
	}
	return Listing{Issues: []Issue{{Number: 1, Found: true, State: IssueOpen, Labels: []string{}}}, Total: 1}, nil
}

func (f *fakeFetcher) States(ctx context.Context, repository string, issues, prs []int64) (States, error) {
	f.states.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotIssues, f.gotPRs = issues, prs
	out := States{Issues: map[int64]Issue{}, PRs: map[int64]PR{}}
	for _, n := range issues {
		out.Issues[n] = Issue{Number: n, Found: true, State: IssueClosed, Labels: []string{}}
	}
	for _, n := range prs {
		out.PRs[n] = PR{Number: n, Found: true, State: PRMerged}
	}
	return out, nil
}

func (f *fakeFetcher) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)} }

func TestCacheHonoursTheTTLAndRefreshBypassesIt(t *testing.T) {
	f, clk := &fakeFetcher{}, newClock()
	c := NewCache(f, clk.now)
	want := Want{Repository: "o/r", Issues: []int64{1, 2}, PRs: []int64{5}}
	r := c.Get(context.Background(), want, false)
	if r.Snapshot == nil || r.Reason != "" || f.lists.Load() != 1 {
		t.Fatalf("first: %+v lists %d", r, f.lists.Load())
	}
	// Listed Issues are not read again by number.
	if !slices.Equal(f.gotIssues, []int64{2}) || !slices.Equal(f.gotPRs, []int64{5}) {
		t.Fatalf("states asked for %v %v", f.gotIssues, f.gotPRs)
	}
	if r.Snapshot.Issue(1).State != IssueOpen || r.Snapshot.Issue(2).State != IssueClosed || r.Snapshot.PRs[5].State != PRMerged || r.Snapshot.Issue(3) != nil {
		t.Fatalf("snapshot %+v", r.Snapshot)
	}
	clk.add(59 * time.Second)
	if r = c.Get(context.Background(), want, false); f.lists.Load() != 1 || r.Age != 59*time.Second {
		t.Fatalf("within the TTL: lists %d age %v", f.lists.Load(), r.Age)
	}
	if c.Get(context.Background(), want, true); f.lists.Load() != 2 {
		t.Fatalf("refresh: lists %d", f.lists.Load())
	}
	clk.add(TTL)
	if r = c.Get(context.Background(), want, false); f.lists.Load() != 3 || r.Age != 0 {
		t.Fatalf("after the TTL: lists %d age %v", f.lists.Load(), r.Age)
	}
}

// A failed sync keeps the last good snapshot and reports its age with the fixed
// reason; a failure is throttled by the TTL as well.
func TestCacheKeepsTheStaleSnapshotOnFailure(t *testing.T) {
	f, clk := &fakeFetcher{}, newClock()
	c := NewCache(f, clk.now)
	want := Want{Repository: "o/r"}
	good := c.Get(context.Background(), want, false).Snapshot
	clk.add(4 * time.Minute)
	f.fail(fail(ReasonRateLimited))
	r := c.Get(context.Background(), want, false)
	if r.Snapshot != good || r.Reason != ReasonRateLimited || r.Age != 4*time.Minute {
		t.Fatalf("stale: %+v", r)
	}
	clk.add(time.Second)
	if r = c.Get(context.Background(), want, false); f.lists.Load() != 2 || r.Reason != ReasonRateLimited || r.Snapshot != good {
		t.Fatalf("failure not throttled: lists %d %+v", f.lists.Load(), r)
	}
	f.fail(nil)
	if r = c.Get(context.Background(), want, true); r.Reason != "" || r.Snapshot == good {
		t.Fatalf("recovery: %+v", r)
	}
}

func TestCacheWithoutASnapshotReportsOnlyTheReason(t *testing.T) {
	f, clk := &fakeFetcher{err: fail(ReasonNotAuthenticated)}, newClock()
	r := NewCache(f, clk.now).Get(context.Background(), Want{Repository: "o/r"}, false)
	if r.Snapshot != nil || r.Reason != ReasonNotAuthenticated {
		t.Fatalf("%+v", r)
	}
}

// A snapshot of one repository is never served for another.
func TestCacheIsPerRepository(t *testing.T) {
	f, clk := &fakeFetcher{}, newClock()
	c := NewCache(f, clk.now)
	c.Get(context.Background(), Want{Repository: "o/r"}, false)
	f.fail(fail(ReasonWrongHost))
	r := c.Get(context.Background(), Want{Repository: "o/other"}, false)
	if f.lists.Load() != 2 || r.Snapshot != nil || r.Reason != ReasonWrongHost {
		t.Fatalf("other repository: lists %d %+v", f.lists.Load(), r)
	}
}

// Concurrent requests share one in-flight sync, refresh or not.
func TestCacheSingleFlight(t *testing.T) {
	f := &fakeFetcher{started: make(chan struct{}, 1), gate: make(chan struct{})}
	clk := newClock()
	c := NewCache(f, clk.now)
	const callers = 8
	c.joined = make(chan struct{}, callers)
	results := make(chan Result, callers)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); results <- c.Get(context.Background(), Want{Repository: "o/r"}, true) }()
	<-f.started
	for i := 1; i < callers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- c.Get(context.Background(), Want{Repository: "o/r"}, true) }()
	}
	// Readiness marker: every other caller has joined the flight. The deadline
	// only guards against a hang; the test never waits on it otherwise.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := 1; i < callers; i++ {
		select {
		case <-c.joined:
		case <-ctx.Done():
			close(f.gate)
			t.Fatalf("only %d of %d callers joined the flight", i-1, callers-1)
		}
	}
	close(f.gate)
	wg.Wait()
	close(results)
	for r := range results {
		if r.Snapshot == nil {
			t.Fatalf("a caller got %+v", r)
		}
	}
	if n := f.lists.Load(); n != 1 {
		t.Fatalf("%d syncs for %d concurrent callers", n, callers)
	}
}

// panicFetcher panics on its first listings, while a waiter has joined, then
// answers like fakeFetcher.
type panicFetcher struct {
	fakeFetcher
	panics atomic.Int32
}

func (p *panicFetcher) OpenIssues(ctx context.Context, repository string) (Listing, error) {
	if p.panics.Add(-1) >= 0 {
		p.lists.Add(1)
		if p.started != nil {
			p.started <- struct{}{}
		}
		if p.gate != nil {
			<-p.gate
		}
		panic("fetcher exploded")
	}
	return p.fakeFetcher.OpenIssues(ctx, repository)
}

// A fetcher panic never wedges the cache: the flight is cleared, its waiters are
// released with the fixed reason, and the next Get syncs again.
func TestCacheRecoversAFetcherPanic(t *testing.T) {
	p := &panicFetcher{}
	p.panics.Store(1)
	p.started, p.gate = make(chan struct{}, 1), make(chan struct{})
	c := NewCache(p, newClock().now)
	c.joined = make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	first, waiter := make(chan Result, 1), make(chan Result, 1)
	go func() { first <- c.Get(context.Background(), Want{Repository: "o/r"}, false) }()
	<-p.started
	go func() { waiter <- c.Get(ctx, Want{Repository: "o/r"}, false) }()
	select {
	case <-c.joined:
	case <-ctx.Done():
		t.Fatal("the waiter never joined")
	}
	close(p.gate)
	for _, ch := range []chan Result{first, waiter} {
		select {
		case r := <-ch:
			if r.Snapshot != nil || r.Reason != ReasonFailed {
				t.Fatalf("after a panic: %+v", r)
			}
		case <-ctx.Done():
			t.Fatal("the cache wedged after a fetcher panic")
		}
	}
	c.mu.Lock()
	wedged := c.flight != nil
	c.mu.Unlock()
	if wedged {
		t.Fatal("the in-flight entry was not cleared")
	}
	p.started, p.gate = nil, nil
	if r := c.Get(ctx, Want{Repository: "o/r"}, true); r.Snapshot == nil || r.Reason != "" || p.lists.Load() != 2 {
		t.Fatalf("after recovery: %+v lists %d", r, p.lists.Load())
	}
}

// Current fails closed on a failed sync and on a snapshot older than 2x TTL.
func TestResultCurrent(t *testing.T) {
	s := &Snapshot{}
	for _, c := range []struct {
		r    Result
		want bool
	}{
		{Result{Snapshot: s}, true},
		{Result{Snapshot: s, Age: 2 * TTL}, true},
		{Result{Snapshot: s, Age: 2*TTL + time.Second}, false},
		{Result{Snapshot: s, Reason: ReasonOffline}, false},
		{Result{Reason: ReasonOffline}, false},
		{Result{}, false},
	} {
		if got := c.r.Current(); got != c.want {
			t.Errorf("%+v: %v", c.r, got)
		}
	}
}

// A caller that gives up while a sync runs returns without cancelling it.
func TestCacheWaiterHonoursItsContext(t *testing.T) {
	f := &fakeFetcher{started: make(chan struct{}, 1), gate: make(chan struct{})}
	c := NewCache(f, newClock().now)
	done := make(chan Result, 1)
	go func() { done <- c.Get(context.Background(), Want{Repository: "o/r"}, false) }()
	<-f.started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := c.Get(ctx, Want{Repository: "o/r"}, false); r.Snapshot != nil {
		t.Fatalf("cancelled waiter: %+v", r)
	}
	close(f.gate)
	if r := <-done; r.Snapshot == nil {
		t.Fatalf("the shared sync was cancelled: %+v", r)
	}
}
