package molecule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode"
)

const (
	LoopWaiting     = "waiting"
	LoopActive      = "active"
	LoopStopped     = "stopped"
	LoopLocked      = "locked"
	LoopUnknown     = "unknown"
	LoopInterrupted = "interrupted"
)

// LoopOptions configures the Lab loop. Template carries the per-Molecule seams
// (Analyze, ReadIssue, PrepareWorkspace) and defaults; the loop sets the Issue
// and approval digest itself and never delivers, checks, amends or recovers.
type LoopOptions struct {
	Template      Options
	Select        intake.Selector
	MaxConcurrent int
	Once          bool
	ListIssues    func(context.Context, string) (intake.OpenIssues, error)
}

// LoopItem reports one eligible Issue. Failed marks a Molecule this pass started
// or approved that ended failed or blocked; waiting is never a failure.
type LoopItem struct {
	Repository string
	Issue      int64
	MoleculeID string
	Started    bool
	Approved   bool
	State      string
	Status     string
	Phase      string
	Reason     string
	Failed     bool
}

type LoopPass struct {
	Items []LoopItem
	Notes []string
}

type LoopReport struct {
	Repository string
	Passes     []LoopPass
}

func (r LoopReport) Failed() bool {
	for _, pass := range r.Passes {
		for _, item := range pass.Items {
			if item.Failed {
				return true
			}
		}
	}
	return false
}

func (p LoopPass) progressed() bool {
	for _, item := range p.Items {
		if item.Started || item.Approved {
			return true
		}
	}
	return false
}

type loopRun struct {
	o      LoopOptions
	lab    string
	repo   string
	loaded formula.Loaded
}

// RunLoop starts intake for eligible open Issues and advances a Molecule through
// its scope gate only when the Formula's policy approves it. Ledgers hold all
// state, so a Molecule that already has one is never started again. Without
// Once it repeats passes until one starts or approves nothing.
func RunLoop(ctx context.Context, lo LoopOptions) (LoopReport, error) {
	var report LoopReport
	if lo.MaxConcurrent < 1 {
		return report, fmt.Errorf("max concurrent must be at least 1")
	}
	lab, err := filepath.Abs(lo.Template.LabDir)
	if err != nil {
		return report, err
	}
	loaded, err := formula.Load(lab, lo.Template.FormulaID)
	if err != nil {
		return report, err
	}
	if !loaded.Approved {
		return report, fmt.Errorf("approve a Formula with init before running")
	}
	f := loaded.Formula
	if err = f.ValidateReadiness(); err != nil {
		return report, err
	}
	if f.Intake.Source != "github-issues" {
		return report, fmt.Errorf("unsupported intake source")
	}
	if lo.ListIssues == nil {
		lo.ListIssues = intake.ReadOpenIssues
	}
	lo.Template.LabDir = lab
	l := &loopRun{o: lo, lab: lab, repo: f.Intake.Repo, loaded: loaded}
	report.Repository = l.repo
	for {
		pass, err := l.pass(ctx)
		if err != nil {
			return report, err
		}
		report.Passes = append(report.Passes, pass)
		if lo.Once || ctx.Err() != nil || !pass.progressed() {
			return report, nil
		}
	}
}

func (l *loopRun) pass(ctx context.Context) (LoopPass, error) {
	var pass LoopPass
	listed, err := l.o.ListIssues(ctx, l.repo)
	if err != nil {
		return pass, err
	}
	if listed.Truncated {
		pass.Notes = append(pass.Notes, fmt.Sprintf("open Issue listing truncated after %d Issues; later Issues were not considered", len(listed.Issues)))
	}
	eligible := l.o.Select.Apply(listed.Issues)
	inventory := ReadQueueInventory(l.lab)
	if !inventory.Complete {
		return pass, fmt.Errorf("the local ledger inventory is incomplete, so no Molecule was started or advanced: %s", inventoryCause(inventory))
	}
	pass.Items = make([]LoopItem, len(eligible))
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(l.o.MaxConcurrent, len(eligible)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				pass.Items[i] = l.process(ctx, inventory, eligible[i])
			}
		}()
	}
	for i, issue := range eligible {
		if ctx.Err() == nil {
			select {
			case work <- i:
				continue
			case <-ctx.Done():
			}
		}
		pass.Items[i] = LoopItem{Repository: issue.Repository, Issue: issue.Number, State: LoopInterrupted, Reason: "the loop was interrupted before this Issue was started"}
	}
	close(work)
	wg.Wait()
	return pass, nil
}

func (l *loopRun) process(ctx context.Context, inventory QueueInventory, issue intake.OpenIssue) LoopItem {
	item := LoopItem{Repository: issue.Repository, Issue: issue.Number}
	summary := inventory.Summarize(issue.Repository, issue.Number)
	if summary.State == "untouched" {
		return l.start(ctx, item)
	}
	return l.existing(ctx, item, summary)
}

// existing never starts anything: it reports the Molecules the ledgers hold for an
// Issue and lets advance apply the scope gate's policy to the latest one.
func (l *loopRun) existing(ctx context.Context, item LoopItem, summary QueueSummary) LoopItem {
	switch {
	case summary.Active > 0:
		for _, a := range summary.Attempts {
			if a.Status == "running" || a.Status == "allocating" {
				item.MoleculeID, item.Status = a.ID, a.Status
				item.Reason = activeReason(a.Status, a.ID)
				break
			}
		}
		item.State = LoopActive
	case summary.Latest == nil:
		item.State = LoopUnknown
		item.Reason = "the recorded Molecules for this Issue cannot be ordered, so none was advanced"
	default:
		item.MoleculeID = summary.Latest.ID
		return l.advance(ctx, item)
	}
	return item
}

// start creates a Molecule only while holding the Issue's start lock and only if
// a fresh read of the ledgers, taken under that lock, still shows the Issue
// untouched. The pass-start inventory can be stale: another loop may have
// created a Molecule since. The lock is held for intake alone and released
// before the gate step, which takes the Molecule lock instead.
func (l *loopRun) start(ctx context.Context, item LoopItem) LoopItem {
	unlock, err := lockIssue(l.lab, item.Repository, item.Issue)
	if errors.Is(err, errLocked) {
		item.State = LoopLocked
		item.Reason = "another loop holds this Issue's start lock, so no Molecule was started"
		return item
	}
	if err != nil {
		item.State, item.Failed = LoopUnknown, true
		item.Reason = "the Issue start lock could not be taken: " + oneLine(err.Error())
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
	if summary := fresh.Summarize(item.Repository, item.Issue); summary.State != "untouched" {
		release()
		return l.existing(ctx, item, summary)
	}
	o := l.o.Template
	o.Issue, o.Approve = strconv.FormatInt(item.Issue, 10), ""
	r, err := Run(ctx, o)
	release()
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

// advance applies the scope gate's policy to a Molecule waiting at that gate and
// otherwise only reports the ledger's state. It holds the per-Molecule lock for
// the whole approval and re-reads the ledger once the lock is held.
func (l *loopRun) advance(ctx context.Context, item LoopItem) LoopItem {
	r, err := readMolecule(l.lab, item.MoleculeID)
	if err != nil {
		return unreadable(item, err, item.Started)
	}
	if !atScopeGate(r) {
		return settle(item, r)
	}
	unlock, err := lockMolecule(l.lab, r.ID)
	if errors.Is(err, errLocked) {
		item.State = LoopLocked
		item.Reason = "another loop holds this Molecule's lock, so it was skipped"
		return item
	}
	if err != nil {
		item.State, item.Failed = LoopUnknown, item.Started
		item.Reason = "the Molecule lock could not be taken: " + oneLine(err.Error())
		return item
	}
	defer unlock()
	if r, err = readMolecule(l.lab, item.MoleculeID); err != nil {
		return unreadable(item, err, item.Started)
	}
	if !atScopeGate(r) {
		return settle(item, r)
	}
	eval := EvaluateGatePolicy(r, formula.ScopeApproval)
	if eval.Outcome != PolicyApprove {
		item.State, item.Status, item.Phase = LoopWaiting, r.Status, statePhase(r)
		item.Reason = fmt.Sprintf("the scope gate needs a human (%s): %s: %s", eval.Outcome, eval.Reason, oneLine(eval.Detail))
		return item
	}
	o := l.o.Template
	o.Issue, o.Approve = strconv.FormatInt(r.Issue.Number, 10), eval.PlanDigest
	decisions := len(r.PolicyDecisions)
	_, applyErr := resumeByPolicy(ctx, o, l.loaded, r.Issue.Number, r.Issue.Repo)
	after, err := readMolecule(l.lab, item.MoleculeID)
	if err != nil {
		return unreadable(item, err, true)
	}
	item.Approved = len(after.PolicyDecisions) > decisions
	item = settle(item, after)
	if applyErr == nil {
		return item
	}
	if item.Approved || statePhase(after) != "scope-waiting" {
		item.Failed = true
		item.Reason += "; the approval transaction reported: " + oneLine(applyErr.Error())
	} else {
		item.Reason = "the policy approved this plan but the approval was not applied: " + oneLine(applyErr.Error())
	}
	return item
}

func unreadable(item LoopItem, err error, failed bool) LoopItem {
	item.State, item.Failed = LoopUnknown, failed
	item.Reason = "the ledger could not be read: " + oneLine(err.Error())
	return item
}

func atScopeGate(r Record) bool {
	return r.Status == "waiting" && statePhase(r) == "scope-waiting"
}

func settle(item LoopItem, r Record) LoopItem {
	state, reason, stopped := classify(r)
	item.MoleculeID, item.Status, item.Phase = r.ID, r.Status, statePhase(r)
	item.State, item.Reason = state, reason
	item.Failed = stopped && (item.Started || item.Approved)
	return item
}

// classify reads a ledger without changing it. Only failed and blocked
// Molecules that are not waiting on a question count as stopped.
func classify(r Record) (state, reason string, stopped bool) {
	phase := statePhase(r)
	switch r.Status {
	case "running", "allocating":
		return LoopActive, activeReason(r.Status, r.ID), false
	case "waiting":
		switch phase {
		case "scope-waiting":
			return LoopWaiting, "waiting at the scope gate", false
		case "review-waiting":
			return LoopWaiting, "waiting at the review gate for a human; the loop never delivers or merges", false
		}
		return LoopWaiting, "waiting (readiness phase " + oneLine(phase) + ")", false
	case "blocked", "failed":
		if r.Readiness == nil && r.Analysis != nil && len(r.Analysis.Questions) > 0 {
			return LoopWaiting, fmt.Sprintf("clarification required: the analysis has %d open question(s); update the Issue body, then run forgecell run %d", len(r.Analysis.Questions), r.Issue.Number), false
		}
		return LoopStopped, stoppedReason(r), true
	}
	return LoopUnknown, "unrecognized ledger status " + strconv.Quote(oneLine(r.Status)), false
}

func activeReason(status, id string) string {
	return fmt.Sprintf("the ledger reports status %s and process liveness is unverified, so the loop did not start, approve or invoke anything; if every Lab process has stopped, inspect it with forgecell ledger %s and recover it with forgecell recover %s", status, id, id)
}

func stoppedReason(r Record) string {
	for _, a := range r.Atoms {
		if (a.Status == "blocked" || a.Status == "failed") && a.Detail != "" {
			return oneLine(a.ID + ": " + a.Detail)
		}
	}
	if n := len(r.Notes); n > 0 {
		return oneLine(r.Notes[n-1])
	}
	return "no detail was recorded"
}

// oneLine bounds ledger text for a terminal report and removes control characters.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)), " ")
	if runes := []rune(s); len(runes) > 300 {
		return string(runes[:300]) + "…"
	}
	return s
}

func readMolecule(lab, id string) (Record, error) {
	var r Record
	if !safeLoopID(id) {
		return r, fmt.Errorf("invalid Molecule id")
	}
	file := filepath.Join(lab, "ledgers", id+".json")
	info, err := os.Lstat(file)
	if err != nil {
		return r, err
	}
	if !info.Mode().IsRegular() || info.Size() > 5_000_000 {
		return r, fmt.Errorf("ledger is not a bounded regular file")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if r.ID != id || r.LabDir != lab {
		return r, fmt.Errorf("ledger identity mismatch")
	}
	return r, nil
}

func safeLoopID(id string) bool {
	return id != "" && strings.Trim(id, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") == ""
}

var errLocked = errors.New("locked by another loop")

// lockMolecule takes an exclusive advisory lock on the Molecule. It is separate
// from the approval transaction's own lock directory.
func lockMolecule(lab, id string) (func(), error) {
	if !safeLoopID(id) {
		return nil, fmt.Errorf("invalid Molecule id")
	}
	return flockLedgerFile(lab, id+".loop-lock")
}

// lockIssue takes an exclusive advisory lock on starting a Molecule for one
// Issue. GitHub names are case-insensitive, so the lock name is lowercased to
// match the inventory's case-insensitive join.
func lockIssue(lab, repository string, number int64) (func(), error) {
	name := "issue-" + strings.ReplaceAll(strings.ToLower(repository), "/", "-") + "-" + strconv.FormatInt(number, 10)
	if !safeLoopID(strings.ReplaceAll(name, ".", "_")) {
		return nil, fmt.Errorf("invalid Issue identity")
	}
	return flockLedgerFile(lab, name+".loop-lock")
}

// flockLedgerFile never blocks. The operating system drops the lock when the file
// is closed or the process exits, including on a crash, so no stale lock can
// outlive its holder. Lock files are never removed: unlinking one while another
// process waits on it would let two holders each lock a different inode.
func flockLedgerFile(lab, name string) (func(), error) {
	dir := filepath.Join(lab, "ledgers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return func() { file.Close() }, nil
}

// inventoryCause names why an inventory is incomplete: at most three
// diagnostics, bounded for a terminal.
func inventoryCause(q QueueInventory) string {
	if len(q.Diagnostics) == 0 {
		return "no diagnostic was recorded"
	}
	shown := q.Diagnostics[:min(3, len(q.Diagnostics))]
	parts := make([]string, len(shown))
	for i, d := range shown {
		parts[i] = oneLine(d)
	}
	cause := strings.Join(parts, "; ")
	if more := len(q.Diagnostics) - len(shown); more > 0 {
		cause += fmt.Sprintf("; and %d more", more)
	}
	return cause
}
