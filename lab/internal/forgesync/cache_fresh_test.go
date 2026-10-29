package forgesync

import (
	"context"
	"slices"
	"testing"
	"time"
)

// Right after publishing, the wanted set names a new pull request: the first
// read with it syncs again within the TTL, so the view sees the PR instead of
// "the published pull request was not checked" until the TTL expires.
func TestCacheReadsANewlyWantedPRWithinTheTTL(t *testing.T) {
	f, clk := &fakeFetcher{}, newClock()
	c := NewCache(f, clk.now)
	before := Want{Repository: "o/r", PRs: []int64{3}}
	if r := c.Get(context.Background(), before, false); r.Snapshot == nil || f.lists.Load() != 1 {
		t.Fatalf("first: %+v lists %d", r, f.lists.Load())
	}
	clk.add(5 * time.Second)
	published := Want{Repository: "o/r", PRs: []int64{3, 4}}
	r := c.Get(context.Background(), published, false)
	if f.lists.Load() != 2 || r.Snapshot == nil || r.Snapshot.PRs[4].State != PRMerged || r.Age != 0 {
		t.Fatalf("new PR within the TTL: lists %d %+v", f.lists.Load(), r)
	}
	if !slices.Equal(f.gotPRs, []int64{3, 4}) {
		t.Fatalf("states asked for %v", f.gotPRs)
	}
	// The same set again, a subset, or a set in another order never syncs again.
	clk.add(5 * time.Second)
	for _, want := range []Want{published, before, {Repository: "o/r", PRs: []int64{4, 3}}, {Repository: "o/r"}} {
		if c.Get(context.Background(), want, false); f.lists.Load() != 2 {
			t.Fatalf("known PRs %v synced again: lists %d", want.PRs, f.lists.Load())
		}
	}
}

// A number already asked for never forces another read, even when the read
// failed or did not return it: a failure or a cut-off read waits for the TTL.
func TestCacheNeverRereadsAnAskedPRBeforeTheTTL(t *testing.T) {
	f, clk := &fakeFetcher{}, newClock()
	c := NewCache(f, clk.now)
	good := c.Get(context.Background(), Want{Repository: "o/r"}, false).Snapshot
	f.fail(fail(ReasonRateLimited))
	want := Want{Repository: "o/r", PRs: []int64{9}}
	r := c.Get(context.Background(), want, false)
	if f.lists.Load() != 2 || r.Reason != ReasonRateLimited || r.Snapshot != good {
		t.Fatalf("failed read of a new PR: lists %d %+v", f.lists.Load(), r)
	}
	f.fail(nil)
	for i := 0; i < 3; i++ {
		clk.add(time.Second)
		if r = c.Get(context.Background(), want, false); f.lists.Load() != 2 || r.Snapshot != good || r.Reason != ReasonRateLimited {
			t.Fatalf("an asked PR forced another read: lists %d %+v", f.lists.Load(), r)
		}
	}
	clk.add(TTL)
	if r = c.Get(context.Background(), want, false); f.lists.Load() != 3 || r.Reason != "" || r.Snapshot.PRs[9].State != PRMerged {
		t.Fatalf("after the TTL: lists %d %+v", f.lists.Load(), r)
	}
}

// The numbers asked for belong to one repository: another repository's are
// forgotten, and the remembered set is bounded, so a full set falls back to the
// TTL instead of reading early.
func TestCacheAskedPRsArePerRepositoryAndBounded(t *testing.T) {
	f, clk := &fakeFetcher{}, newClock()
	c := NewCache(f, clk.now)
	c.Get(context.Background(), Want{Repository: "o/r", PRs: []int64{1}}, false)
	c.Get(context.Background(), Want{Repository: "o/other", PRs: []int64{2}}, false)
	if f.lists.Load() != 2 {
		t.Fatalf("other repository: lists %d", f.lists.Load())
	}
	c.mu.Lock()
	stale := c.asked[1]
	c.mu.Unlock()
	if stale {
		t.Fatal("another repository's asked PRs were kept")
	}

	many := make([]int64, 0, maxAskedPRs+10)
	for n := int64(1); n <= maxAskedPRs+10; n++ {
		many = append(many, n)
	}
	c.Get(context.Background(), Want{Repository: "o/r", PRs: many}, false)
	if f.lists.Load() != 3 {
		t.Fatalf("repository switch back: lists %d", f.lists.Load())
	}
	c.mu.Lock()
	size := len(c.asked)
	c.mu.Unlock()
	if size != maxAskedPRs {
		t.Fatalf("asked holds %d numbers, want the bound %d", size, maxAskedPRs)
	}
	clk.add(time.Second)
	if c.Get(context.Background(), Want{Repository: "o/r", PRs: []int64{maxAskedPRs + 100}}, false); f.lists.Load() != 3 {
		t.Fatalf("a full asked set read early: lists %d", f.lists.Load())
	}
}
