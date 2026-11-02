package ui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/DanielNetzer/forgecell/lab/internal/forgesync"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/molecule"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin"
	"github.com/DanielNetzer/forgecell/lab/internal/plugin/contract"
	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// Keyed Issues (plugins slice 4d). A Lab whose active approved Formula reads
// Issues from a tracker plugin lists them through the plugin instead of the
// GitHub sync:
//
//	GET /api/issues → tracker_list("oldest", cursor, 100), at most 5 pages
//
// The listing runs in the background, at most one at a time, and is cached per
// Formula identity for issuesKeyedTTL, or the plugin's needs.minPollSeconds
// when that is longer. A poll never waits on the plugin: before the first
// listing it gets an empty view marked loading, and while a later listing runs
// it gets the cached view marked stale. The v1 listing carries no Issue URL,
// so a keyed row links to no tracker page (url stays ""; the client links only
// an https tracker URL). A listing past the page bound is
// marked truncated; a failed one keeps the last good view, marked stale, until
// the TTL has passed again, so a poll never retries the plugin sooner. Each
// row's startability is the loop's own (molecule.QueueInventory.SummarizeKey).
// Plugin error text is never served.

const (
	// issuesKeyedTTL is the shortest time a keyed listing is reused.
	issuesKeyedTTL = 60 * time.Second
	// issuesKeyedMaxPoll bounds needs.minPollSeconds as the manifest does.
	issuesKeyedMaxPoll = 3600 * time.Second
	// issuesKeyedMaxPages and issuesKeyedPageSize bound one listing to 500 items.
	issuesKeyedMaxPages = 5
	issuesKeyedPageSize = tracker.MaxListLimit
	// issuesKeyedListTimeout bounds one whole listing.
	issuesKeyedListTimeout = 2 * time.Minute
	// issuesKeyedMaxCursor bounds a next cursor, as the adapter does.
	issuesKeyedMaxCursor = 1024
	// issuesKeyedMaxLabels bounds the labels kept per row.
	issuesKeyedMaxLabels = 20
	// issuesKeyedMaxSelectors bounds the described selectors kept.
	issuesKeyedMaxSelectors = 32
	// issuesKeyedMaxSelectorRunes is the contract's selector bound.
	issuesKeyedMaxSelectorRunes = 200
)

// Fixed texts of the keyed Issues view; none carries plugin output.
const (
	issuesKeyedReasonFailed = "The tracker plugin could not list open Issues just now; check it with forgecell plugin doctor."
	issuesKeyedNoteStale    = "the tracker listing is not current; nothing is startable until it is"
)

var errIssuesKeyedCursor = errors.New("the tracker's next cursor does not advance the listing")

// issuesKeyedItem is one listed open Issue, validated and bounded.
type issuesKeyedItem struct {
	key, title, milestone, updatedAt string
	labels                           []string
}

// issuesKeyedListing is one finished keyed listing.
type issuesKeyedListing struct {
	items     []issuesKeyedItem
	truncated bool
	total     int
	fetchedAt time.Time
}

// issuesKeyedCache is the keyed listing of one Formula identity. mu is never
// held through a plugin call, so a poll never waits on the plugin.
type issuesKeyedCache struct {
	mu       sync.Mutex
	identity string
	listing  *issuesKeyedListing
	// finished is when the latest listing for identity ended, failed or not;
	// failed reports that it failed. ttl is that listing's cache time.
	finished time.Time
	failed   bool
	ttl      time.Duration
	// running is set while the one listing runs; done closes when it ends.
	running bool
	done    chan struct{}
}

// issuesKeyedPoll is the cache time of f's listing: issuesKeyedTTL, or the
// pinned tracker manifest's needs.minPollSeconds when that is longer. A
// manifest that cannot be loaded gives issuesKeyedTTL.
func issuesKeyedPoll(lab string, f formula.Formula) time.Duration {
	ref, ok := f.TrackerPlugin()
	if !ok {
		return issuesKeyedTTL
	}
	m, _, err := plugin.Load(lab, ref.Manifest, ref.SHA256)
	if err != nil {
		return issuesKeyedTTL
	}
	poll := time.Duration(m.Needs.MinPollSeconds) * time.Second
	if poll <= issuesKeyedTTL || poll > issuesKeyedMaxPoll {
		return issuesKeyedTTL
	}
	return poll
}

// issuesKeyedState is what one poll reads from the cache.
type issuesKeyedState struct {
	listing *issuesKeyedListing
	failed  bool
	running bool
}

// issuesKeyedCurrent reads the cache for f and starts the one background
// listing when the cached one has expired and none runs. It never waits.
func (h *handler) issuesKeyedCurrent(f keyedActiveFormula) issuesKeyedState {
	c := &h.issuesKeyed
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.identity != f.identity {
		// Another Formula's listing is never shown; one still running stores
		// nothing when it ends.
		c.identity, c.listing, c.finished, c.failed, c.ttl = f.identity, nil, time.Time{}, false, 0
	}
	ttl := c.ttl
	if ttl < issuesKeyedTTL {
		ttl = issuesKeyedTTL
	}
	if !c.running && (c.finished.IsZero() || h.forge.now().Sub(c.finished) >= ttl) {
		h.issuesKeyedStart(f)
	}
	return issuesKeyedState{listing: c.listing, failed: c.failed, running: c.running}
}

// issuesKeyedStart starts the background listing for f on the jobs' context,
// so it stops with the server. c.mu is held by the caller.
func (h *handler) issuesKeyedStart(f keyedActiveFormula) {
	c := &h.issuesKeyed
	g := h.jobs
	g.mu.Lock()
	if g.ctx.Err() != nil {
		g.mu.Unlock()
		return
	}
	g.wg.Add(1)
	g.mu.Unlock()
	done := make(chan struct{})
	c.running, c.done = true, done
	go func() {
		defer g.wg.Done()
		defer close(done)
		listing, ttl, err := h.issuesKeyedList(g.ctx, f)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.running = false
		if c.identity != f.identity {
			return
		}
		c.finished, c.ttl, c.failed = h.forge.now(), ttl, err != nil
		if err == nil {
			listing.fetchedAt = c.finished
			c.listing = &listing
		}
	}()
}

// issuesKeyedList prepares f's tracker through the trackerFor seam and lists
// it. The listing's calls belong to no Molecule, so no observer records them.
func (h *handler) issuesKeyedList(ctx context.Context, f keyedActiveFormula) (listing issuesKeyedListing, ttl time.Duration, err error) {
	defer func() {
		if recover() != nil {
			listing, err = issuesKeyedListing{}, errGatePanic
		}
	}()
	ttl = issuesKeyedTTL
	lab, ok := h.keyedLabDir()
	if !ok {
		return listing, ttl, errors.New("the Lab directory cannot be resolved")
	}
	if h.issuesKeyedPoll != nil {
		ttl = h.issuesKeyedPoll(lab, f.formula)
	}
	keyed, isPlugin, err := h.trackerFor(lab, f.formula, mcpadapter.Options{})
	if err != nil || !isPlugin || keyed == nil {
		return listing, ttl, errors.New("the tracker plugin could not be prepared")
	}
	call, cancel := context.WithTimeout(ctx, issuesKeyedListTimeout)
	defer cancel()
	listing, err = issuesKeyedPages(call, keyed)
	return listing, ttl, err
}

// issuesKeyedPages pages List("oldest", cursor, 100) for at most
// issuesKeyedMaxPages pages and keeps each valid open key once, in the
// tracker's order. Pages left past the bound make the listing truncated; a
// page larger than asked for or a cursor that would not advance fails it.
func issuesKeyedPages(ctx context.Context, t tracker.Keyed) (issuesKeyedListing, error) {
	var out issuesKeyedListing
	seen := map[string]bool{}
	cursor := ""
	for range issuesKeyedMaxPages {
		page, err := t.List(ctx, intake.SelectOldest, cursor, issuesKeyedPageSize)
		if err != nil {
			return issuesKeyedListing{}, err
		}
		if len(page.Items) > issuesKeyedPageSize {
			return issuesKeyedListing{}, errors.New("the tracker listed more Issues than a page holds")
		}
		for _, item := range page.Items {
			if item.State != "open" || intake.ValidateIssueKey(item.Key) != nil || seen[item.Key] {
				continue
			}
			seen[item.Key] = true
			out.items = append(out.items, issuesKeyedItemOf(item))
		}
		if page.NextCursor == nil {
			out.total = len(out.items)
			if page.Total != nil && *page.Total > int64(out.total) && *page.Total < 1<<31 {
				out.total = int(*page.Total)
			}
			return out, nil
		}
		next := *page.NextCursor
		if next == "" || next == cursor || len(next) > issuesKeyedMaxCursor || len(page.Items) == 0 {
			return issuesKeyedListing{}, errIssuesKeyedCursor
		}
		cursor = next
	}
	out.truncated, out.total = true, len(out.items)
	return out, nil
}

// issuesKeyedItemOf bounds one listed item's display text.
func issuesKeyedItemOf(item contract.TrackerItem) issuesKeyedItem {
	out := issuesKeyedItem{key: item.Key, title: issuesKeyedText(item.Title), labels: []string{}}
	if item.Milestone != nil {
		out.milestone = issuesKeyedText(*item.Milestone)
	}
	if at, err := time.Parse(time.RFC3339, item.UpdatedAt); err == nil {
		out.updatedAt = at.UTC().Format(time.RFC3339)
	}
	for _, l := range item.Labels {
		if len(out.labels) == issuesKeyedMaxLabels {
			break
		}
		if l = issuesKeyedText(l); l != "" {
			out.labels = append(out.labels, l)
		}
	}
	return out
}

// issuesKeyedText is untrusted tracker text as one bounded line: invalid
// UTF-8, control, format and separator characters are dropped.
func issuesKeyedText(s string) string {
	if len(s) > 4*maxStepRunes*utf8.UTFMax {
		s = s[:4*maxStepRunes*utf8.UTFMax]
	}
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Bidi_Control) {
			return -1
		}
		return r
	}, s)
	return bounded(strings.TrimSpace(s))
}

// issuesKeyedRepository is the keyed Formula's repository binding, as the
// keyed loop binds it, or "".
func issuesKeyedRepository(f formula.Formula) string {
	repo, err := intake.BindKeyProject(f.Intake.Repo, f.NestedProjectAllowed())
	if err != nil {
		return ""
	}
	return repo
}

// issuesKeyedView serves GET /api/issues for a tracker-plugin Lab.
func (h *handler) issuesKeyedView(w http.ResponseWriter, f keyedActiveFormula) {
	repo := issuesKeyedRepository(f.formula)
	out := IssuesView{Repository: repo, Keyed: true, Issues: []IssueRow{}}
	st := h.issuesKeyedCurrent(f)
	out.Loading = st.listing == nil && st.running
	if st.failed {
		out.Reason = issuesKeyedReasonFailed
	}
	if st.listing == nil {
		h.json(w, http.StatusOK, out)
		return
	}
	l := st.listing
	out.Checked, out.Stale = true, st.running || st.failed
	out.FetchedAt = l.fetchedAt.UTC().Format(time.RFC3339)
	out.AgeSeconds = max(int64(h.forge.now().Sub(l.fetchedAt)/time.Second), 0)
	out.Total, out.Shown, out.Truncated = l.total, len(l.items), l.truncated || l.total > len(l.items)
	if out.Truncated {
		out.Note = "showing " + strconv.Itoa(out.Shown) + " of " + strconv.Itoa(out.Total)
		if l.truncated {
			out.Note = "showing the first " + strconv.Itoa(out.Shown)
		}
	}
	queue := molecule.ReadQueueInventory(h.opts.LabDir)
	list := h.list(ledgerETag(h.opts.LabDir))
	out.Complete = queue.Complete && list.Complete && repo != ""
	lanes := map[string]Summary{}
	for _, s := range list.Molecules {
		lanes[s.ID] = s
	}
	// Molecules are joined to a key by the queue's raw key, never the display key.
	byKey := map[string][]forgesync.Molecule{}
	for _, a := range queue.Attempts {
		s, ok := lanes[a.ID]
		if a.Key == "" || a.Number != 0 || !ok || !strings.EqualFold(a.Repository, repo) {
			continue
		}
		byKey[a.Key] = append(byKey[a.Key], forgesync.Molecule{ID: s.ID, Lane: s.Lane, StartedAt: s.StartedAt})
	}
	for _, item := range l.items {
		summary := queue.SummarizeKey(repo, item.key)
		open := forgesync.Issue{Found: true, State: forgesync.IssueOpen, Title: item.title, Labels: item.labels, Milestone: item.milestone}
		state := forgesync.SyncState(&open, nil, forgesync.Work{Molecules: byKey[item.key], SummaryStartable: summary.Startable()})
		if !out.Complete {
			state.Startable = false
		}
		if out.Stale {
			state.Startable = false
			state.Notes = append(state.Notes, issuesKeyedNoteStale)
		}
		row := IssueRow{Key: item.key, Title: item.title, Labels: item.labels, Milestone: item.milestone, UpdatedAt: item.updatedAt, State: state, Startable: state.Startable, SummaryState: summary.State, SummaryStartable: summary.Startable()}
		if id := state.MoleculeID; moleculeID.MatchString(id) {
			row.LatestMolecule, row.MoleculeLink = id, "/?m="+id
		}
		out.Issues = append(out.Issues, row)
	}
	h.json(w, http.StatusOK, out)
}

// ---- Keyed loop selectors -----------------------------------------------------

// issuesKeyedSelectorCache is the tracker's described selectors for one Formula
// identity, described once (in the background for GET /api/loop, or by a loop
// start) and never per poll. A failed describe is cached too; only a loop
// start retries it. mu is never held through a describe.
type issuesKeyedSelectorCache struct {
	describing sync.Mutex
	mu         sync.Mutex
	identity   string
	tried      bool
	selectors  []string
	background string
	done       chan struct{}
}

func (c *issuesKeyedSelectorCache) cached(identity string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tried || c.identity != identity {
		return nil, false
	}
	return c.selectors, true
}

// issuesKeyedSelectors is f's described selectors, or false when the tracker
// could not describe any. retry describes again after a cached failure.
func (h *handler) issuesKeyedSelectors(ctx context.Context, f keyedActiveFormula, retry bool) ([]string, bool) {
	c := &h.keyedSelectors
	c.describing.Lock()
	defer c.describing.Unlock()
	if selectors, tried := c.cached(f.identity); tried && (len(selectors) > 0 || !retry) {
		return selectors, len(selectors) > 0
	}
	lab, ok := h.keyedLabDir()
	if !ok {
		return nil, false
	}
	call, cancel := context.WithTimeout(ctx, keyedDescribeTimeout)
	defer cancel()
	var selectors []string
	if keyed, isPlugin, err := h.trackerFor(lab, f.formula, mcpadapter.Options{}); err == nil && isPlugin && keyed != nil {
		if d, err := keyedDescribe(call, keyed); err == nil {
			selectors = issuesKeyedCleanSelectors(d.Selectors)
		}
	}
	if len(selectors) == 0 && ctx.Err() != nil {
		return nil, false
	}
	c.mu.Lock()
	c.identity, c.tried, c.selectors = f.identity, true, selectors
	c.mu.Unlock()
	return selectors, len(selectors) > 0
}

// issuesKeyedSelectorsInBackground starts one background describe per Formula
// identity on the jobs' context.
func (h *handler) issuesKeyedSelectorsInBackground(f keyedActiveFormula) {
	c := &h.keyedSelectors
	c.mu.Lock()
	if c.background == f.identity {
		c.mu.Unlock()
		return
	}
	done := make(chan struct{})
	c.background, c.done = f.identity, done
	c.mu.Unlock()
	g := h.jobs
	g.mu.Lock()
	if g.ctx.Err() != nil {
		g.mu.Unlock()
		close(done)
		return
	}
	g.wg.Add(1)
	g.mu.Unlock()
	go func() {
		defer g.wg.Done()
		defer close(done)
		h.issuesKeyedSelectors(g.ctx, f, false)
	}()
}

// issuesKeyedCleanSelectors keeps the described selectors that are one clean
// line of at most the contract's bound, each once, at most
// issuesKeyedMaxSelectors of them; a bare ':' is no selector.
func issuesKeyedCleanSelectors(described []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range described {
		if len(out) == issuesKeyedMaxSelectors {
			break
		}
		if s == ":" || seen[s] || !issuesKeyedSelectorText(s) {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// issuesKeyedSelectorText reports a non-empty UTF-8 line of at most the
// contract's bound with no control, format or separator character.
func issuesKeyedSelectorText(s string) bool {
	if s == "" || len(s) > maxSelectBytes || !utf8.ValidString(s) || utf8.RuneCountInString(s) > issuesKeyedMaxSelectorRunes {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp, unicode.Bidi_Control)
	})
}

// issuesKeyedAdmits reports a selector the tracker described, or one that
// extends a described prefix ending in ':' with a non-empty value, as the
// adapter admits it.
func issuesKeyedAdmits(described []string, selector string) bool {
	if !issuesKeyedSelectorText(selector) {
		return false
	}
	for _, d := range described {
		if strings.HasSuffix(d, ":") {
			if len(selector) > len(d) && strings.HasPrefix(selector, d) {
				return true
			}
			continue
		}
		if selector == d {
			return true
		}
	}
	return false
}

// ---- Keyed loop passes --------------------------------------------------------

// observeKeyedPass is observePass for this UI's keyed loop. The keyed engine
// lists through the tracker, not ListIssues, so the pass is read from the Lab's
// loop status instead: a key is dispatched while its intake runs or its
// Molecule is in flight. Of a dispatched key's other Molecules, those started
// before the loop are left alone. An incomplete inventory, or a status that is
// not this loop's, records no pass, so no Molecule is named the loop's.
func (h *handler) observeKeyedPass() {
	l := h.loop
	l.mu.Lock()
	running, keyed, token := l.running, l.keyed, l.token
	l.mu.Unlock()
	if !running || !keyed {
		return
	}
	var pass loopPass
	lab := molecule.ReadLoopStatus(h.opts.LabDir)
	queue := molecule.ReadQueueInventory(h.opts.LabDir)
	if s := lab.Status; lab.Running && s != nil && token != "" && s.Token == token && queue.Complete {
		pass = loopPass{}
		inFlight := map[string]bool{}
		for i, id := range s.InFlight {
			if i == molecule.MaxLoopStatusInFlight {
				break
			}
			if !moleculeID.MatchString(id) {
				continue
			}
			inFlight[id] = true
			if r, err := h.records(h.opts.LabDir, id); err == nil && r.Issue.IsKeyed() && intake.ValidateIssueKey(r.Issue.Key) == nil {
				pass[keyedPassKey(r.Issue.Repo, r.Issue.Key)] = map[string]bool{}
			}
		}
		if f, ok := h.keyedFormula(h.opts.LabDir); ok {
			if repo := issuesKeyedRepository(f.formula); repo != "" {
				for i, in := range s.Intake {
					if i == molecule.MaxLoopStatusInFlight {
						break
					}
					if in.Key != "" && intake.ValidateIssueKey(in.Key) == nil {
						pass[keyedPassKey(repo, in.Key)] = map[string]bool{}
					}
				}
			}
		}
		for _, a := range queue.Attempts {
			alone, ok := pass[keyedPassKey(a.Repository, a.Key)]
			if a.Key == "" || !ok || inFlight[a.ID] {
				continue
			}
			if at, err := time.Parse(time.RFC3339Nano, a.StartedAt); err != nil || at.Before(s.StartedAt) {
				alone[a.ID] = true
			}
		}
	}
	l.mu.Lock()
	if l.running && l.keyed && l.token == token {
		l.pass = pass
	}
	l.mu.Unlock()
}

// issuesKeyedKeyText is a recorded key the loop views may show, or "".
func issuesKeyedKeyText(key string) string {
	if intake.ValidateIssueKey(key) != nil {
		return ""
	}
	return key
}
