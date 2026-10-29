package forgesync

import (
	"context"
	"sync"
	"time"
)

// TTL is how long one read of GitHub serves a server: at most one sync per TTL,
// failed or not, unless a caller asks for a refresh or wants a pull request no
// attempt has asked for yet.
const TTL = 60 * time.Second

// maxAskedPRs bounds the pull request numbers the cache remembers asking for.
// Once full, a new number waits for the TTL like any other read: the bound
// stops early reads, never the TTL.
const maxAskedPRs = MaxStateNumbers

// syncTimeout bounds one whole sync, every gh call in it included.
const syncTimeout = 2 * time.Minute

// Snapshot is one successful sync of a repository.
type Snapshot struct {
	Repository string
	Listing    Listing
	// Issues holds every Issue read, listed open ones and those read by number.
	Issues map[int64]Issue
	PRs    map[int64]PR
	// StatesTruncated means some wanted numbers were past MaxStateNumbers and
	// were not read.
	StatesTruncated bool
	FetchedAt       time.Time
}

// Issue is the read Issue n, or nil when it was not read.
func (s *Snapshot) Issue(n int64) *Issue {
	if s == nil {
		return nil
	}
	if i, ok := s.Issues[n]; ok {
		return &i
	}
	return nil
}

// Want is what one sync reads: the repository's open Issues, plus these Issues
// and pull requests by number.
type Want struct {
	Repository string
	Issues     []int64
	PRs        []int64
}

// Result is the cache's answer. Snapshot is the last good sync of the
// repository, or nil when there is none; Reason is the fixed reason the latest
// attempt failed, or "" when it succeeded. Age is the snapshot's age.
type Result struct {
	Snapshot *Snapshot
	Reason   string
	Age      time.Duration
}

// Cache serves one server's GitHub state. It reads GitHub at most once per TTL,
// a refresh bypasses that, and concurrent callers share one in-flight sync. A
// failed sync keeps the last good snapshot.
type Cache struct {
	fetcher Fetcher
	now     func() time.Time

	mu            sync.Mutex
	snap          *Snapshot
	reason        string
	attempted     time.Time
	attemptedRepo string
	// asked holds the pull request numbers the attempts for attemptedRepo
	// wanted, at most maxAskedPRs. A want naming one not in it is read again
	// within the TTL, once: the first view after publishing sees its PR.
	asked  map[int64]bool
	flight *flight
	// joined, when set by a test, receives once per caller that joins a flight:
	// a readiness marker, so tests never poll.
	joined chan struct{}
}

type flight struct {
	done    chan struct{}
	waiters int
}

// NewCache returns a cache reading through fetcher; now is the clock, so tests
// never wait on the wall clock.
func NewCache(fetcher Fetcher, now func() time.Time) *Cache {
	if now == nil {
		now = time.Now
	}
	return &Cache{fetcher: fetcher, now: now}
}

// Get returns GitHub state for want, syncing when the latest attempt for its
// repository is older than TTL, refresh is set, or want names a pull request
// no attempt for the repository has asked for. A caller arriving while a sync
// runs waits for it, or for ctx, instead of starting another.
func (c *Cache) Get(ctx context.Context, want Want, refresh bool) Result {
	c.mu.Lock()
	if f := c.flight; f != nil {
		f.waiters++
		joined := c.joined
		c.mu.Unlock()
		if joined != nil {
			joined <- struct{}{}
		}
		select {
		case <-f.done:
		case <-ctx.Done():
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.result(want.Repository)
	}
	fresh := !c.attempted.IsZero() && c.attemptedRepo == want.Repository && c.now().Sub(c.attempted) < TTL
	if fresh && !refresh && !c.newPR(want.PRs) {
		defer c.mu.Unlock()
		return c.result(want.Repository)
	}
	f := &flight{done: make(chan struct{})}
	c.flight = f
	c.mu.Unlock()
	return c.run(want, f)
}

// run is the one shared sync. Its deferred function always records the
// outcome, clears the in-flight entry and closes done, so the cache never
// wedges; a fetcher panic is recovered and recorded as the fixed ReasonFailed.
func (c *Cache) run(want Want, f *flight) (r Result) {
	var snap *Snapshot
	reason := ReasonFailed
	defer func() {
		if recover() != nil {
			snap, reason = nil, ReasonFailed
		}
		c.mu.Lock()
		if c.attemptedRepo != want.Repository {
			c.asked = nil
		}
		c.attempted, c.attemptedRepo, c.reason = c.now(), want.Repository, reason
		// Failed or not, the numbers count as asked, so a failure or a read cut
		// off at MaxStateNumbers never forces another read before the TTL.
		c.recordAsked(want.PRs)
		if reason == "" {
			c.snap = snap
		} else if c.snap != nil && c.snap.Repository != want.Repository {
			c.snap = nil
		}
		c.flight = nil
		r = c.result(want.Repository)
		c.mu.Unlock()
		close(f.done)
	}()
	snap, reason = c.sync(want)
	return r
}

// newPR reports a wanted pull request no attempt for attemptedRepo asked for,
// while asked is below its bound; the caller holds mu.
func (c *Cache) newPR(prs []int64) bool {
	if len(c.asked) >= maxAskedPRs {
		return false
	}
	for _, n := range prs {
		if !c.asked[n] {
			return true
		}
	}
	return false
}

// recordAsked adds prs to asked, up to maxAskedPRs; the caller holds mu.
func (c *Cache) recordAsked(prs []int64) {
	if c.asked == nil {
		c.asked = map[int64]bool{}
	}
	for _, n := range prs {
		if len(c.asked) >= maxAskedPRs {
			return
		}
		c.asked[n] = true
	}
}

// Current reports GitHub state fit to allow a start: the latest sync succeeded
// and its snapshot is at most twice the TTL old. Anything else fails closed.
func (r Result) Current() bool {
	return r.Snapshot != nil && r.Reason == "" && r.Age <= 2*TTL
}

// result is the answer for repository; the caller holds mu.
func (c *Cache) result(repository string) Result {
	var r Result
	if c.attemptedRepo == repository {
		r.Reason = c.reason
	}
	if c.snap != nil && c.snap.Repository == repository {
		r.Snapshot = c.snap
		if age := c.now().Sub(c.snap.FetchedAt); age > 0 {
			r.Age = age
		}
	}
	return r
}

// sync reads GitHub once. It runs on its own bounded context, so a caller that
// goes away does not cancel the sync the others share.
func (c *Cache) sync(want Want) (*Snapshot, string) {
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()
	listing, err := c.fetcher.OpenIssues(ctx, want.Repository)
	if err != nil {
		return nil, ReasonOf(err)
	}
	snap := &Snapshot{Repository: want.Repository, Listing: listing, Issues: map[int64]Issue{}, PRs: map[int64]PR{}}
	for _, i := range listing.Issues {
		snap.Issues[i.Number] = i
	}
	var need []int64
	for _, n := range want.Issues {
		if _, listed := snap.Issues[n]; !listed {
			need = append(need, n)
		}
	}
	if len(need) > 0 || len(want.PRs) > 0 {
		states, err := c.fetcher.States(ctx, want.Repository, need, want.PRs)
		if err != nil {
			return nil, ReasonOf(err)
		}
		for n, i := range states.Issues {
			snap.Issues[n] = i
		}
		for n, p := range states.PRs {
			snap.PRs[n] = p
		}
		snap.StatesTruncated = states.Truncated
	}
	snap.FetchedAt = c.now()
	return snap, ""
}
