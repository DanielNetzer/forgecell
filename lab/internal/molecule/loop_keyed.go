package molecule

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/mcpadapter"
	"github.com/DanielNetzer/forgecell/lab/internal/tracker"
)

// The keyed loop (plugins slice 4b): RunLoop over a Formula whose intake.source
// is a declared tracker plugin. It lists open Issues by key through the
// tracker, starts an eligible key under the key's start lock with RunKeyed and
// approves a Molecule at its scope gate only when the Formula's policy does,
// with resumeByPolicyKeyed. The numeric loop is unchanged.

// Bounds of a keyed listing: at most keyedListMaxPages pages of
// keyedListPageSize items, 500 in all. A listing with pages left past them is
// truncated.
const (
	keyedListPageSize = tracker.MaxListLimit
	keyedListMaxPages = 5
	// maxKeyedListCursor bounds a next cursor, as the adapter bounds a cursor it
	// sends.
	maxKeyedListCursor = 1024
)

// keyedLoop is a keyed loop's own state.
type keyedLoop struct {
	// selector is the raw tracker selector; never empty.
	selector string
	// resolved is the Lab's real path, which the plugin adapter needs.
	resolved string
}

// formulaUsesTrackerPlugin reports an approved Formula whose intake.source is a
// declared tracker plugin. Anything it cannot load is false, so the numeric
// path reports it exactly as before.
func formulaUsesTrackerPlugin(lab, id string) bool {
	loaded, err := formula.Load(lab, id)
	if err != nil || !loaded.Approved {
		return false
	}
	_, ok := loaded.Formula.TrackerPlugin()
	return ok
}

// loadLoopFormulaKeyed is loadLoopFormula for the keyed loop: the approved,
// ready Formula must read Issues from a declared tracker plugin.
func loadLoopFormulaKeyed(lab, id string) (formula.Loaded, error) {
	loaded, err := formula.Load(lab, id)
	if err != nil {
		return loaded, err
	}
	if !loaded.Approved {
		return loaded, fmt.Errorf("approve a Formula with init before running")
	}
	f := loaded.Formula
	if err = f.ValidateReadiness(); err != nil {
		return loaded, err
	}
	if _, ok := f.TrackerPlugin(); !ok {
		return loaded, fmt.Errorf("a keyed loop needs a Formula whose intake.source is a declared tracker plugin")
	}
	return loaded, nil
}

// keyedLoopRepository is the keyed loop's repository binding.
func keyedLoopRepository(f formula.Formula) (string, error) {
	return intake.BindKeyProject(f.Intake.Repo, f.NestedProjectAllowed())
}

// runLoopKeyed is RunLoop's keyed branch. Like the numeric loop it takes the
// Lab's marker once the approved Formula has loaded and before any Issue is
// listed; the marker is acquired with a zero selector and then records the
// raw tracker selector.
func runLoopKeyed(ctx context.Context, lo LoopOptions, lab string) (LoopReport, error) {
	var report LoopReport
	loaded, err := loadLoopFormulaKeyed(lab, lo.Template.FormulaID)
	if err != nil {
		return report, err
	}
	repo, err := keyedLoopRepository(loaded.Formula)
	if err != nil {
		return report, err
	}
	selector := lo.TrackerSelect
	if selector == "" {
		selector = intake.SelectOldest
	}
	if lo.Marker != nil {
		if err = lo.Marker.Acquire(lab, intake.Selector{}, lo.MaxConcurrent); err != nil {
			return report, err
		}
		lo.Marker.recordTrackerSelector(selector)
	} else if err = refuseWhileLoopMarkerHeld(lab); err != nil {
		return report, err
	}
	// The adapter needs the Lab's real path; Molecules keep the Lab as given.
	resolved, err := filepath.EvalSymlinks(lab)
	if err != nil {
		return report, fmt.Errorf("the Lab directory cannot be resolved")
	}
	lo.Template.LabDir = lab
	l := &loopRun{work: ctx, o: lo, lab: lab, repo: repo, loaded: loaded, marker: lo.Marker, keyed: &keyedLoop{selector: selector, resolved: resolved}}
	if lo.Work != nil {
		l.work = lo.Work
	}
	report.Repository, report.TrackerSelect = repo, selector
	return l.passes(ctx, report)
}

// reloadFormulaKeyed is reloadFormula for the keyed loop: the run stays bound to
// one repository binding.
func (l *loopRun) reloadFormulaKeyed() error {
	loaded, err := loadLoopFormulaKeyed(l.lab, l.o.Template.FormulaID)
	if err == nil {
		var repo string
		if repo, err = keyedLoopRepository(loaded.Formula); err == nil && !strings.EqualFold(repo, l.repo) {
			return fmt.Errorf("the approved Formula's intake.repo changed from %s to %s during the run; restart the loop", l.repo, oneLine(repo))
		}
	}
	if err != nil {
		return fmt.Errorf("the approved Formula could not be reloaded, so nothing was started or advanced: %w", err)
	}
	l.loaded = loaded
	return nil
}

// trackerFor is the tracker a keyed loop reads through: LoopOptions.Tracker
// when set, otherwise the Formula's plugin tracker, whose every call reaches
// calls (nil for the listing, whose calls belong to no Molecule).
func (l *loopRun) trackerFor(calls *PluginCallLog) (tracker.Keyed, error) {
	if l.o.Tracker != nil {
		return l.o.Tracker, nil
	}
	opts := mcpadapter.Options{LookupEnv: l.o.TrackerLookupEnv}
	if calls != nil {
		opts.Observer = calls.Observe
	}
	t, ok, err := mcpadapter.TrackerFor(l.keyed.resolved, l.loaded.Formula, opts)
	if err == nil && !ok {
		err = fmt.Errorf("the approved Formula's intake.source is not a tracker plugin")
	}
	return t, err
}

// keyedListing is a bounded listing of open Issue keys, in the tracker's order.
type keyedListing struct {
	keys      []string
	truncated bool
	// invalid counts listed open items whose key is not a valid Issue key.
	invalid int
}

// listOpenKeys pages List(selector, cursor, 100) for at most keyedListMaxPages
// pages and keeps the open items, each key once. Pages left past the bound make
// the listing truncated; a cursor that would not advance is refused.
func listOpenKeys(ctx context.Context, t tracker.Keyed, selector string) (keyedListing, error) {
	var out keyedListing
	seen := map[string]bool{}
	cursor := ""
	for range keyedListMaxPages {
		page, err := t.List(ctx, selector, cursor, keyedListPageSize)
		if err != nil {
			return keyedListing{}, err
		}
		if len(page.Items) > keyedListPageSize {
			return keyedListing{}, errors.New("the tracker listed more Issues than a page holds")
		}
		for _, item := range page.Items {
			if item.State != "open" {
				continue
			}
			if !validLoopKey(item.Key) {
				out.invalid++
				continue
			}
			if !seen[item.Key] {
				seen[item.Key] = true
				out.keys = append(out.keys, item.Key)
			}
		}
		if page.NextCursor == nil {
			return out, nil
		}
		next := *page.NextCursor
		if next == "" || next == cursor || len(next) > maxKeyedListCursor || len(page.Items) == 0 {
			return keyedListing{}, errors.New("the tracker's next cursor does not advance the listing")
		}
		cursor = next
	}
	out.truncated = true
	return out, nil
}

// passKeyed is pass for the keyed loop.
func (l *loopRun) passKeyed(ctx context.Context) (LoopPass, error) {
	var pass LoopPass
	l.marker.beginPass()
	// The listing runs through the reloaded Formula's tracker.
	if err := l.reloadFormula(); err != nil {
		return pass, err
	}
	t, err := l.trackerFor(nil)
	if err != nil {
		return pass, err
	}
	selector := l.keyed.selector
	listed, err := listOpenKeys(ctx, t, selector)
	if err != nil {
		return pass, TransientLoopError{err}
	}
	if listed.truncated && selector != intake.SelectOldest {
		return pass, fmt.Errorf("the open Issue listing was truncated after %d Issues, so %s cannot be applied to every open Issue; nothing was started or advanced", len(listed.keys), oneLine(selector))
	}
	if listed.truncated {
		pass.Notes = append(pass.Notes, fmt.Sprintf("open Issue listing truncated after %d Issues; later Issues were not considered", len(listed.keys)))
	}
	if listed.invalid > 0 {
		pass.Notes = append(pass.Notes, fmt.Sprintf("the tracker listed %d open Issue(s) whose key is not a valid Issue key; they were not considered", listed.invalid))
	}
	inventory := ReadQueueInventory(l.lab)
	if !inventory.Complete {
		return pass, TransientLoopError{fmt.Errorf("the local ledger inventory is incomplete, so no Molecule was started or advanced: %s", inventoryCause(inventory))}
	}
	pruneRecoveryProposals(l.lab, l.analysisTimeout())
	keys := listed.keys
	l.dispatch(ctx, &pass, len(keys), func(i int) LoopItem {
		return LoopItem{Repository: l.repo, Key: keys[i]}
	}, func(ctx context.Context, i int) LoopItem {
		return l.processKeyed(ctx, inventory, keys[i])
	})
	l.noteStatusWrite(&pass)
	return pass, nil
}

// noteStatusWrite reports a loop status write the marker refused or that
// failed during the pass, so a stale status is never left unnoticed.
func (l *loopRun) noteStatusWrite(pass *LoopPass) {
	if err := l.marker.takeStatusWriteError(); err != nil {
		pass.Notes = append(pass.Notes, "the loop status could not be written, so forgecell status may show an earlier status: "+oneLine(err.Error()))
	}
}

func (l *loopRun) processKeyed(ctx context.Context, inventory QueueInventory, key string) LoopItem {
	item := LoopItem{Repository: l.repo, Key: key}
	summary := inventory.SummarizeKey(l.repo, key)
	if summary.Startable() {
		return l.startKeyed(ctx, item)
	}
	return l.existing(ctx, item, summary)
}

// startKeyed is start for a keyed Issue: it creates a Molecule only while
// holding the key's start lock and only if a fresh read of the ledgers, taken
// under that lock, still shows the key untouched. It calls RunKeyed, not
// RunIntakeKeyed, since the loop already holds the lock; each Molecule records
// its own plugin calls.
func (l *loopRun) startKeyed(ctx context.Context, item LoopItem) LoopItem {
	unlock, err := lockIssueKeyAs(l.lab, item.Repository, item.Key, issueOwnerLoop)
	if errors.Is(err, errLocked) {
		holder := "another process"
		if name, e := issueProjectKeyLockName(item.Repository, item.Key); e == nil {
			holder = issueLockHolderNamed(l.lab, name)
		}
		item.State = LoopLocked
		item.Reason = holder + " holds this Issue's start lock, so no Molecule was started"
		return item
	}
	if err != nil {
		item.State, item.Failed = LoopUnknown, true
		item.Reason = "the Issue start lock could not be taken: " + oneLine(pathlessErr(err).Error())
		return item
	}
	release := sync.OnceFunc(unlock)
	defer release()
	fresh := ReadQueueInventory(l.lab)
	if !fresh.Complete {
		item.State, item.Failed = LoopUnknown, true
		item.Reason = "the local ledger inventory became incomplete, so no Molecule was started: " + inventoryCause(fresh)
		return item
	}
	if summary := fresh.SummarizeKey(item.Repository, item.Key); !summary.Startable() {
		release()
		return l.existing(ctx, item, summary)
	}
	calls := NewPluginCallLog(l.keyed.resolved, l.lab)
	t, err := l.trackerFor(calls)
	if err != nil {
		item.State, item.Failed = LoopStopped, true
		item.Reason = "intake could not start: " + oneLine(err.Error())
		return item
	}
	o := l.o.Template
	o.Issue, o.Approve, o.AllowKeyedApproval = "", "", false
	o.ReadIssueKey, o.PluginCalls = KeyedReader(t, calls), calls
	// As on the numeric path, the status lists the key while intake runs and
	// then its Molecule, never neither.
	l.marker.TrackIntakeKey(item.Key)
	untrackIntake := sync.OnceFunc(func() { l.marker.UntrackIntakeKey(item.Key) })
	defer untrackIntake()
	r, err := RunKeyed(ctx, o, item.Key)
	release()
	if r.ID != "" {
		l.marker.Track(r.ID)
		defer l.marker.Untrack(r.ID)
	}
	untrackIntake()
	if r.ID == "" {
		item.State, item.Failed = LoopStopped, true
		item.Reason = "intake could not start: " + oneLine(fmt.Sprint(err))
		return item
	}
	item.MoleculeID, item.Started = r.ID, true
	item = l.advance(ctx, item)
	if err != nil {
		item.Failed = true
		item.Reason += "; intake reported: " + oneLine(err.Error())
	}
	return item
}

// approveKeyed is advance's policy approval of a keyed Molecule: it re-reads the
// Issue by its key through a fresh tracker whose calls the Molecule records.
func (l *loopRun) approveKeyed(ctx context.Context, o Options, r Record) error {
	if l.keyed == nil {
		return fmt.Errorf("a keyed Molecule is approved only by a loop over its tracker plugin")
	}
	calls := NewPluginCallLog(l.keyed.resolved, l.lab)
	t, err := l.trackerFor(calls)
	if err != nil {
		return err
	}
	o.Issue = ""
	o.ReadIssueKey, o.PluginCalls = KeyedReader(t, calls), calls
	_, err = resumeByPolicyKeyed(ctx, o, l.loaded, r.Issue.Key, r.Issue.Repo)
	return err
}

// probeStartLock takes the item's start lock, the key's for a keyed item, so
// interruptedLedger can tell that no loop is starting it.
func probeStartLock(lab string, item LoopItem) (func(), error) {
	if item.Key == "" {
		return lockIssue(lab, item.Repository, item.Issue)
	}
	if err := intake.ValidateIssueKey(item.Key); err != nil {
		return nil, err
	}
	name, err := issueProjectKeyLockName(item.Repository, item.Key)
	if err != nil {
		return nil, err
	}
	return flockLedgerFile(lab, name)
}

// keyedRunHint is the run command for a keyed Issue, its key shell-quoted. A
// ledger key that is not a valid Issue key gets no command.
func keyedRunHint(key string) string {
	if !validLoopKey(key) {
		return "forgecell run with the Issue's key (the recorded key is not a valid tracker key, so it is not shown)"
	}
	return "forgecell run " + shellWord(key)
}
