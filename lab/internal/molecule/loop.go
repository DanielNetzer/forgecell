package molecule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/harness"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	LoopWaiting = "waiting"
	LoopActive  = "active"
	LoopStopped = "stopped"
	LoopLocked  = "locked"
	LoopUnknown = "unknown"
	// LoopStalled marks an Issue the loop could not move forward although it should
	// have: an approval that never applied, or a ledger intake never finished. It is
	// counted as non-progress, unlike a wait at a human gate.
	LoopStalled     = "stalled"
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
	// Drain, once closed, stops the loop dispatching further Issues and ends it
	// after the pass. Unlike cancelling ctx it never interrupts Molecules already
	// in flight.
	Drain <-chan struct{}
	// Work, when set, is the context Molecule work runs on, so ctx can cancel the
	// Issue listing without interrupting Molecules already in flight. It defaults
	// to ctx.
	Work context.Context
}

// TransientLoopError marks a pass failure that a later pass may not repeat: the
// Issue listing or the local ledger inventory. Formula and configuration errors
// are never transient.
type TransientLoopError struct{ Err error }

func (e TransientLoopError) Error() string { return e.Err.Error() }
func (e TransientLoopError) Unwrap() error { return e.Err }

// IsTransientLoopError reports whether err is a TransientLoopError.
func IsTransientLoopError(err error) bool {
	var t TransientLoopError
	return errors.As(err, &t)
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
	// Proposal is the saved recovery proposal of an interrupted Molecule.
	Proposal *LoopProposal
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

// NonProgress counts the Issues of the final pass that ended stalled or in a state
// the loop could not determine. Earlier passes are not counted: an Issue that
// stalled and then advanced made progress.
func (r LoopReport) NonProgress() int {
	if len(r.Passes) == 0 {
		return 0
	}
	return r.Passes[len(r.Passes)-1].NonProgress()
}

func (p LoopPass) NonProgress() int {
	n := 0
	for _, item := range p.Items {
		if item.State == LoopStalled || item.State == LoopUnknown {
			n++
		}
	}
	return n
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
	work   context.Context
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
	loaded, err := loadLoopFormula(lab, lo.Template.FormulaID)
	if err != nil {
		return report, err
	}
	if lo.ListIssues == nil {
		selector := lo.Select
		lo.ListIssues = func(ctx context.Context, repository string) (intake.OpenIssues, error) {
			return intake.ReadSelectedOpenIssues(ctx, repository, selector)
		}
	}
	lo.Template.LabDir = lab
	l := &loopRun{work: ctx, o: lo, lab: lab, repo: loaded.Formula.Intake.Repo, loaded: loaded}
	if lo.Work != nil {
		l.work = lo.Work
	}
	report.Repository = l.repo
	for {
		pass, err := l.pass(ctx)
		if err != nil {
			return report, err
		}
		report.Passes = append(report.Passes, pass)
		if lo.Once || l.draining(ctx) || !pass.progressed() {
			return report, nil
		}
	}
}

func loadLoopFormula(lab, id string) (formula.Loaded, error) {
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
	if f.Intake.Source != "github-issues" {
		return loaded, fmt.Errorf("unsupported intake source")
	}
	return loaded, nil
}

// reloadFormula re-reads the approved Formula before every pass. Each Molecule
// snapshots the Formula it started under and an approval needs the live Formula
// to match that snapshot, so a Formula approved again during a long run must be
// the one later approvals compare against. The run stays bound to one repository.
func (l *loopRun) reloadFormula() error {
	loaded, err := loadLoopFormula(l.lab, l.o.Template.FormulaID)
	if err != nil {
		return fmt.Errorf("the approved Formula could not be reloaded, so nothing was started or advanced: %w", err)
	}
	if !strings.EqualFold(loaded.Formula.Intake.Repo, l.repo) {
		return fmt.Errorf("the approved Formula's intake.repo changed from %s to %s during the run; restart the loop", l.repo, loaded.Formula.Intake.Repo)
	}
	l.loaded = loaded
	return nil
}

func (l *loopRun) pass(ctx context.Context) (LoopPass, error) {
	var pass LoopPass
	listed, err := l.o.ListIssues(ctx, l.repo)
	if err != nil {
		return pass, TransientLoopError{err}
	}
	if err = l.reloadFormula(); err != nil {
		return pass, err
	}
	if listed.Truncated && l.o.Select.Kind != "" && l.o.Select.Kind != intake.SelectOldest {
		return pass, fmt.Errorf("the open Issue listing was truncated after %d Issues, so %s cannot be applied to every open Issue; nothing was started or advanced", len(listed.Issues), l.o.Select)
	}
	if listed.Truncated {
		pass.Notes = append(pass.Notes, fmt.Sprintf("open Issue listing truncated after %d Issues; later Issues were not considered", len(listed.Issues)))
	}
	eligible := l.o.Select.Apply(listed.Issues)
	inventory := ReadQueueInventory(l.lab)
	if !inventory.Complete {
		return pass, TransientLoopError{fmt.Errorf("the local ledger inventory is incomplete, so no Molecule was started or advanced: %s", inventoryCause(inventory))}
	}
	pruneRecoveryProposals(l.lab, l.analysisTimeout())
	pass.Items = make([]LoopItem, len(eligible))
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(l.o.MaxConcurrent, len(eligible)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				pass.Items[i] = l.process(l.work, inventory, eligible[i])
			}
		}()
	}
	for i, issue := range eligible {
		if !l.draining(ctx) {
			select {
			case work <- i:
				continue
			case <-ctx.Done():
			case <-l.o.Drain:
			}
		}
		pass.Items[i] = LoopItem{Repository: issue.Repository, Issue: issue.Number, State: LoopInterrupted, Reason: "the loop was interrupted before this Issue was started"}
	}
	close(work)
	wg.Wait()
	return pass, nil
}

func (l *loopRun) draining(ctx context.Context) bool {
	select {
	case <-l.o.Drain:
		return true
	default:
		return ctx.Err() != nil
	}
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
				if reason, stuck := l.interruptedLedger(a, item); stuck {
					item.State, item.Reason = LoopStalled, reason
					if a.Phase != "" {
						item = l.proposeRecovery(item)
					}
					return item
				}
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

// defaultAnalysisTimeout is the harness timeout when the Formula declares none.
const defaultAnalysisTimeout = 10 * time.Minute

// analysisTimeout is how long a ledger may go unwritten while intake or analysis
// is still working: the Formula's analysis binding timeout.
func (l *loopRun) analysisTimeout() time.Duration { return AnalysisTimeout(l.loaded.Formula) }

// AnalysisTimeout is the Formula's analysis binding timeout, or the default.
func AnalysisTimeout(f formula.Formula) time.Duration {
	if b, _ := f.ExecutionBinding(formula.IntakeRole); b.TimeoutMS > 0 {
		return time.Duration(b.TimeoutMS) * time.Millisecond
	}
	return defaultAnalysisTimeout
}

// interruptedLedger recognises a ledger a crash left running or allocating. It
// reports one only when nothing could still be working on it: the ledger has not
// been written for longer than the analysis timeout (a live manual forgecell run
// keeps writing it), no live process owns its approval lock, and, for a ledger
// that never reached a scope plan, no loop holds the Issue's start lock, which a
// live loop holds for the whole of its intake. Anything uncertain keeps the plain
// active report.
func (l *loopRun) interruptedLedger(a QueueAttempt, item LoopItem) (string, bool) {
	if a.Status != "running" && a.Status != "allocating" {
		return "", false
	}
	timeout := l.analysisTimeout()
	if !ledgerAbandoned(l.lab, a.ID, timeout) {
		return "", false
	}
	if a.Phase != "" {
		return interruptedApprovalReason(a, timeout), true
	}
	unlock, err := lockIssue(l.lab, item.Repository, item.Issue)
	if err != nil {
		return "", false
	}
	unlock()
	return unfinishedIntakeReason(a, timeout), true
}

// ledgerAbandoned is true when the ledger has not been written for longer than
// the analysis timeout and no live process owns its approval lock.
func ledgerAbandoned(lab, id string, timeout time.Duration) bool {
	info, err := os.Lstat(filepath.Join(lab, "ledgers", id+".json"))
	if err != nil || !info.Mode().IsRegular() || time.Since(info.ModTime()) <= timeout {
		return false
	}
	return !approvalOwnerMayBeLive(lab, id)
}

// approvalOwnerMayBeLive reads the approval lock directory's owner file. Anything
// it cannot read or parse counts as a live owner, so a state it does not
// understand is never reported as abandoned.
func approvalOwnerMayBeLive(lab, id string) bool {
	lock := filepath.Join(lab, "ledgers", id+".lock")
	if _, err := os.Lstat(lock); err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	raw, err := os.ReadFile(filepath.Join(lock, "owner"))
	if err != nil {
		return true
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid < 1 {
		return true
	}
	return confirmProcessExited(pid) != nil
}

func unfinishedIntakeReason(a QueueAttempt, timeout time.Duration) string {
	return fmt.Sprintf("the ledger reports status %s but holds no scope plan, so intake never reached the scope gate; it has not been written for over %s, and no loop is starting this Issue, so a crash most likely interrupted it. forgecell recover needs a plan digest and does not apply to a ledger without a plan, so inspect it with forgecell ledger %s once no Lab process is running. The loop started no other Molecule for this Issue", a.Status, timeout, a.ID)
}

func interruptedApprovalReason(a QueueAttempt, timeout time.Duration) string {
	return fmt.Sprintf("the ledger reports status %s in readiness phase %s, but it has not been written for over %s and no live Lab process owns its approval lock, so a crash most likely interrupted it. The loop did not start, approve or invoke anything; inspect it with forgecell ledger %s, and if every Lab process has stopped, forgecell recover %s --plan DIGEST --confirm-stopped may apply", a.Status, oneLine(a.Phase), timeout, a.ID, a.ID)
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
	item.Status, item.Phase = r.Status, statePhase(r)
	// The approval compares the source checkout against the plan's frozen base. An
	// empty base means HEAD, which moves whenever the checkout does, so the loop pins
	// the base to the plan's own. A plan frozen on a different base than the
	// Molecule's workspace could never be applied.
	if len(r.Readiness.Plans) == 0 {
		item.State = LoopStalled
		item.Reason = "the policy approved this plan but the ledger holds no plan to pin the approval's base to, so the approval was not applied"
		return item
	}
	planBase := r.Readiness.Plans[len(r.Readiness.Plans)-1].Plan.Inputs.BaseCommit
	if planBase == "" || planBase != r.Workspace.BaseCommit {
		item.State = LoopStalled
		item.Reason = fmt.Sprintf("the policy approved this plan but its frozen base %s differs from the Molecule workspace base %s, so the approval was refused; inspect forgecell ledger %s", oneLine(planBase), oneLine(r.Workspace.BaseCommit), r.ID)
		return item
	}
	lock := filepath.Join(l.lab, "ledgers", r.ID+".lock")
	if _, err = os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		item.State = LoopStalled
		if err != nil {
			item.Reason = "the policy approved this plan but its approval lock " + oneLine(lock) + " could not be checked: " + oneLine(err.Error())
			return item
		}
		item.Reason = fmt.Sprintf("the policy approved this plan but its approval lock %s exists, so the approval was not applied: another approval, amend or recover holds it, or an interrupted approval left it behind. The loop never removes it; if no Lab process is running, inspect it and forgecell ledger %s", oneLine(lock), r.ID)
		return item
	}
	o := l.o.Template
	o.Issue, o.Approve, o.Base = strconv.FormatInt(r.Issue.Number, 10), eval.PlanDigest, planBase
	moved := l.sourceMovedNote(ctx, r)
	decisions := len(r.PolicyDecisions)
	_, applyErr := resumeByPolicy(ctx, o, l.loaded, r.Issue.Number, r.Issue.Repo)
	after, err := readMolecule(l.lab, item.MoleculeID)
	if err != nil {
		return unreadable(item, err, true)
	}
	item.Approved = len(after.PolicyDecisions) > decisions
	item = settle(item, after)
	switch {
	case applyErr == nil:
	case item.Approved || statePhase(after) != "scope-waiting":
		item.Failed = true
		item.Reason += "; the approval transaction reported: " + oneLine(applyErr.Error())
	default:
		item.State = LoopStalled
		item.Reason = "the policy approved this plan but the approval was not applied: " + oneLine(applyErr.Error())
	}
	if moved != "" {
		item.Reason += "; " + moved
	}
	return item
}

// sourceMovedNote reports a source checkout whose HEAD is no longer the base the
// Molecule was frozen on. It is information, never a refusal: the loop's approval
// is pinned to the plan's base, unlike a manual approval, which checks the current
// HEAD.
func (l *loopRun) sourceMovedNote(ctx context.Context, r Record) string {
	head, err := git(ctx, r.Workspace.SourceRoot, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil || head == r.Workspace.BaseCommit {
		return ""
	}
	return fmt.Sprintf("the source checkout's HEAD (%s) has moved since the plan froze base %s; the loop's approval is pinned to the plan's base, so it was not affected, but a manual approval would be refused until intake is refreshed", oneLine(head), oneLine(r.Workspace.BaseCommit))
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

// oneLine bounds ledger text for a terminal report. It removes terminal escape
// sequences and replaces control, line-break and direction-override characters,
// like every other text shown from an untrusted source.
func oneLine(s string) string {
	return harness.ReviewDisplayText(s, 300)
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

// lockedByLoop explains a failed Molecule flock to a manual command. The flock is
// always taken before the approval lock directory, never after it.
func lockedByLoop(err error) error {
	if errors.Is(err, errLocked) {
		return fmt.Errorf("Molecule is locked by a running loop or another command; wait for it to finish, then run this again: %w", err)
	}
	return fmt.Errorf("the Molecule lock could not be taken: %w", err)
}

// loopLockSuffix names the advisory lock files in the ledgers directory. They
// are never removed, so the ledger inventory does not count them toward its
// directory entry cap.
const loopLockSuffix = ".loop-lock"

// lockMolecule takes an exclusive advisory lock on the Molecule. It is separate
// from the approval transaction's own lock directory.
func lockMolecule(lab, id string) (func(), error) {
	if !safeLoopID(id) {
		return nil, fmt.Errorf("invalid Molecule id")
	}
	return flockLedgerFile(lab, id+loopLockSuffix)
}

// lockIssue takes an exclusive advisory lock on starting a Molecule for one
// Issue. GitHub names are case-insensitive, so the repository is lowercased to
// match the inventory's case-insensitive join. The lock name carries a hash of
// the repository, not the repository itself: joining owner and repository with a
// separator that GitHub names also contain would give a-b/c and a/b-c one lock.
func lockIssue(lab, repository string, number int64) (func(), error) {
	lower := strings.ToLower(repository)
	owner, repo, ok := strings.Cut(lower, "/")
	if !ok || !safeLoopID(strings.ReplaceAll(owner, ".", "_")) || !safeLoopID(strings.ReplaceAll(repo, ".", "_")) {
		return nil, fmt.Errorf("invalid Issue identity")
	}
	sum := sha256.Sum256([]byte(lower))
	return flockLedgerFile(lab, "issue-"+hex.EncodeToString(sum[:16])+"-"+strconv.FormatInt(number, 10)+loopLockSuffix)
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
