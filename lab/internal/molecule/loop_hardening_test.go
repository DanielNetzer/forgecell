package molecule

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
)

func TestHardeningIssueLockNamesDoNotCollide(t *testing.T) {
	lab := t.TempDir()
	one, err := lockIssue(lab, "a-b/c", 7)
	if err != nil {
		t.Fatal(err)
	}
	defer one()
	two, err := lockIssue(lab, "a/b-c", 7)
	if err != nil {
		t.Fatalf("a-b/c and a/b-c share a start lock: %v", err)
	}
	defer two()
	if _, err = lockIssue(lab, "A-B/C", 7); !errors.Is(err, errLocked) {
		t.Fatalf("the lock is case sensitive: %v", err)
	}
	if other, err := lockIssue(lab, "a-b/c", 8); err != nil {
		t.Fatalf("another Issue of the same repository was locked out: %v", err)
	} else {
		other()
	}
	entries, err := os.ReadDir(filepath.Join(lab, "ledgers"))
	if err != nil || len(entries) != 3 {
		t.Fatalf("lock files: %v %v", entries, err)
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), loopLockSuffix)
		if name == e.Name() || !safeLoopID(strings.ReplaceAll(name, ".", "_")) || strings.Contains(name, "/") {
			t.Fatalf("unexpected lock file name %q", e.Name())
		}
	}
	for _, bad := range []string{"", "no-slash", "a/b/c", "a/ b"} {
		if _, err = lockIssue(lab, bad, 1); err == nil || errors.Is(err, errLocked) {
			t.Fatalf("repository %q: %v", bad, err)
		}
	}
}

func TestHardeningLoopPinsTheApprovalToThePlansFrozenBase(t *testing.T) {
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	unmoved := loopMolecule(t, o, 11)
	r := loopMolecule(t, o, 12)

	// With HEAD still on the plan's base the approval carries no note.
	report := loopOnce(t, o, loopFixedList(loopOpen(11, 0)))
	item := loopItem(t, report.Passes[0], 11)
	if !item.Approved || item.Failed || item.MoleculeID != unmoved.ID || strings.Contains(item.Reason, "moved") {
		t.Fatalf("%+v", item)
	}

	testGit(t, o.SourceRoot, "commit", "--allow-empty", "-qm", "moved after intake")
	head := testGit(t, o.SourceRoot, "rev-parse", "HEAD")
	if head == r.Workspace.BaseCommit {
		t.Fatal("test premise failed: HEAD did not move")
	}

	// A manual approval compares the current HEAD and refuses; the loop's is pinned to the plan's base.
	manual := o
	manual.Approve = r.Readiness.Plans[0].Digest
	loaded, err := formula.Load(o.LabDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resume(context.Background(), manual, loaded, r.Issue.Number, r.Issue.Repo); err == nil || !strings.Contains(err.Error(), "source base changed") {
		t.Fatalf("a manual approval ignored the moved HEAD: %v", err)
	}

	report = loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	item = loopItem(t, report.Passes[0], 12)
	if !item.Approved || item.Failed || item.State != LoopWaiting || item.Phase != "review-waiting" || strings.Contains(item.Reason, "base changed") {
		t.Fatalf("a moved HEAD stopped the approval: %+v", item)
	}
	if !strings.Contains(item.Reason, "has moved since the plan froze base "+r.Workspace.BaseCommit) || !strings.Contains(item.Reason, head) {
		t.Fatalf("the report does not record the moved HEAD: %s", item.Reason)
	}
	if report.NonProgress() != 0 || loopStarts(loopLines(t, harnessLog)) != 2 {
		t.Fatalf("non-progress %d, harness starts %d", report.NonProgress(), loopStarts(loopLines(t, harnessLog)))
	}
}

func TestHardeningLoopRefusesAPlanWhoseBaseIsNotTheWorkspaceBase(t *testing.T) {
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := loopMolecule(t, o, 12)
	ledger := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
	r.Workspace.BaseCommit = strings.Repeat("0", len(r.Workspace.BaseCommit))
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}

	report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	item := loopItem(t, report.Passes[0], 12)
	if item.State != LoopStalled || item.Approved || item.Failed || !strings.Contains(item.Reason, "frozen base") || !strings.Contains(item.Reason, "forgecell ledger "+r.ID) {
		t.Fatalf("%+v", item)
	}
	if report.NonProgress() != 1 || loopStarts(loopLines(t, harnessLog)) != 0 {
		t.Fatalf("non-progress %d, harness starts %d", report.NonProgress(), loopStarts(loopLines(t, harnessLog)))
	}
	if after, err := os.ReadFile(ledger); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a refused approval changed the ledger: %v", err)
	}
}

func TestHardeningLoopReportsAHeldApprovalLockAsNonProgressAndNeverRemovesIt(t *testing.T) {
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := loopMolecule(t, o, 12)
	lock := filepath.Join(o.LabDir, "ledgers", r.ID+".lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(lock, "owner")
	if err := os.WriteFile(owner, []byte("999999"), 0600); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}

	report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	item := loopItem(t, report.Passes[0], 12)
	if item.State != LoopStalled || item.Approved || item.Failed || item.MoleculeID != r.ID || !strings.Contains(item.Reason, lock) || !strings.Contains(item.Reason, "never removes") {
		t.Fatalf("%+v", item)
	}
	if report.NonProgress() != 1 || report.Failed() {
		t.Fatalf("non-progress %d, failed %t", report.NonProgress(), report.Failed())
	}
	if _, err = os.Stat(owner); err != nil {
		t.Fatalf("the loop touched the held lock: %v", err)
	}
	after, err := os.ReadFile(ledger)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the ledger changed while its approval lock was held: %v", err)
	}
	assertNothingApproved(t, loadRecord(t, o.LabDir, r.ID))
	if loopStarts(loopLines(t, harnessLog)) != 0 {
		t.Fatal("an approval ran while its lock was held")
	}

	if err = os.RemoveAll(lock); err != nil {
		t.Fatal(err)
	}
	if item = loopItem(t, loopOnce(t, o, loopFixedList(loopOpen(12, 0))).Passes[0], 12); !item.Approved || item.State != LoopWaiting {
		t.Fatalf("the Molecule did not advance once the lock was gone: %+v", item)
	}
}

func TestHardeningWaitingAtAHumanGateAndASkippedLockAreNotNonProgress(t *testing.T) {
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	loopMolecule(t, o, 11)
	locked := loopMolecule(t, o, 12)
	unlock, err := lockMolecule(o.LabDir, locked.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	report := loopOnce(t, o, loopFixedList(loopOpen(11, 0), loopOpen(12, 1)))
	if got := loopItem(t, report.Passes[0], 11); got.State != LoopWaiting {
		t.Fatalf("%+v", got)
	}
	if got := loopItem(t, report.Passes[0], 12); got.State != LoopLocked {
		t.Fatalf("%+v", got)
	}
	if report.NonProgress() != 0 || report.Failed() {
		t.Fatalf("non-progress %d, failed %t", report.NonProgress(), report.Failed())
	}
}

func TestHardeningNonProgressCountsOnlyTheFinalPass(t *testing.T) {
	report := LoopReport{Passes: []LoopPass{
		{Items: []LoopItem{{State: LoopStalled}, {State: LoopWaiting}}},
		{Items: []LoopItem{{State: LoopWaiting}}},
	}}
	if report.NonProgress() != 0 {
		t.Fatalf("an earlier stall was counted: %d", report.NonProgress())
	}
	report.Passes = append(report.Passes, LoopPass{Items: []LoopItem{{State: LoopStalled}, {State: LoopStalled}, {State: LoopLocked}}})
	if report.NonProgress() != 2 {
		t.Fatalf("got %d", report.NonProgress())
	}
	report.Passes[2].Items = append(report.Passes[2].Items, LoopItem{State: LoopUnknown}, LoopItem{State: LoopActive}, LoopItem{State: LoopInterrupted}, LoopItem{State: LoopStopped})
	if report.NonProgress() != 3 {
		t.Fatalf("an undetermined Issue must count as non-progress, and active, interrupted and stopped ones must not: %d", report.NonProgress())
	}
	if (LoopReport{}).NonProgress() != 0 {
		t.Fatal("an empty report has non-progress")
	}
}

func TestHardeningAmendAndRecoverRefuseWhileTheMoleculeFlockIsHeld(t *testing.T) {
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	r := loopMolecule(t, o, 12)
	digest := r.Readiness.Plans[0].Digest
	ledger := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockMolecule(o.LabDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Amend(context.Background(), o.LabDir, r.ID, digest, readiness.Plan{}, ""); !errors.Is(err, errLocked) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("amend ran while a loop held the Molecule: %v", err)
	}
	if _, err = Recover(context.Background(), o.LabDir, r.ID, digest, true); !errors.Is(err, errLocked) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("recover ran while a loop held the Molecule: %v", err)
	}
	after, err := os.ReadFile(ledger)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a refused command changed the ledger: %v", err)
	}
	for _, name := range []string{".lock", ".recovery-lock"} {
		if _, err = os.Lstat(filepath.Join(o.LabDir, "ledgers", r.ID+name)); !os.IsNotExist(err) {
			t.Fatalf("a refused command left %s behind: %v", name, err)
		}
	}
	unlock()

	// Once the flock is free the commands get past it, fail on their own checks, and release everything.
	if _, err = Amend(context.Background(), o.LabDir, r.ID, digest, readiness.Plan{}, ""); err == nil || errors.Is(err, errLocked) {
		t.Fatalf("amend: %v", err)
	}
	if _, err = Recover(context.Background(), o.LabDir, r.ID, digest, true); err == nil || errors.Is(err, errLocked) {
		t.Fatalf("recover: %v", err)
	}
	for _, name := range []string{".lock", ".recovery-lock"} {
		if _, err = os.Lstat(filepath.Join(o.LabDir, "ledgers", r.ID+name)); !os.IsNotExist(err) {
			t.Fatalf("a finished command left %s behind: %v", name, err)
		}
	}
	again, err := lockMolecule(o.LabDir, r.ID)
	if err != nil {
		t.Fatalf("a command kept the Molecule flock: %v", err)
	}
	again()
	after, err = os.ReadFile(ledger)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a failed command changed the ledger: %v", err)
	}
}

func TestHardeningManualApprovalTakesTheMoleculeFlockBeforeTheLockDirectory(t *testing.T) {
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	r := loopMolecule(t, o, 13)
	o.Approve = r.Readiness.Plans[0].Digest
	loaded, err := formula.Load(o.LabDir, "")
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(o.LabDir, "ledgers", r.ID+".lock")

	unlock, err := lockMolecule(o.LabDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resume(context.Background(), o, loaded, r.Issue.Number, r.Issue.Repo); !errors.Is(err, errLocked) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("a manual approval ran while a loop held the Molecule: %v", err)
	}
	if _, err = os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("a refused approval left its lock directory behind: %v", err)
	}
	unlock()

	// Past the flock the approval fails on its own check (a wrong Issue number) and releases both locks.
	if _, err = resume(context.Background(), o, loaded, r.Issue.Number+1, r.Issue.Repo); err == nil || errors.Is(err, errLocked) || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("approval did not get past the flock: %v", err)
	}
	if _, err = os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("a finished approval left its lock directory behind: %v", err)
	}
	again, err := lockMolecule(o.LabDir, r.ID)
	if err != nil {
		t.Fatalf("an approval kept the Molecule flock: %v", err)
	}
	again()

	// A held lock directory refuses the approval and still releases the flock it took first.
	if err = os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = resume(context.Background(), o, loaded, r.Issue.Number, r.Issue.Repo); err == nil || errors.Is(err, errLocked) || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("an approval ignored the held lock directory: %v", err)
	}
	if _, err = os.Lstat(dir); err != nil {
		t.Fatalf("the approval removed a lock directory it did not create: %v", err)
	}
	again, err = lockMolecule(o.LabDir, r.ID)
	if err != nil {
		t.Fatalf("a refused approval kept the Molecule flock: %v", err)
	}
	again()
	if after, err := os.ReadFile(ledger); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a refused approval changed the ledger: %v", err)
	}
}

func reapprove(t *testing.T, lab, id string, edit func(string) string) {
	t.Helper()
	loaded, err := formula.Load(lab, "")
	if err != nil {
		t.Fatal(err)
	}
	approveFixture(t, lab, edit(loaded.Formula.YAML), id)
}

func TestHardeningLoopReloadsTheFormulaEveryPass(t *testing.T) {
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	calls := 0
	list := func(context.Context, string) (intake.OpenIssues, error) {
		calls++
		switch calls {
		case 1:
			return intake.OpenIssues{Issues: []intake.OpenIssue{loopOpen(11, 0)}}, nil
		case 2:
			reapprove(t, o.LabDir, "reapproved", func(yaml string) string { return yaml + "# approved again\n" })
		}
		return intake.OpenIssues{Issues: []intake.OpenIssue{loopOpen(11, 0), loopOpen(12, 1)}}, nil
	}
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, ListIssues: list})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Passes) != 3 || report.Failed() || report.NonProgress() != 0 {
		t.Fatalf("passes %d, failed %t, non-progress %d", len(report.Passes), report.Failed(), report.NonProgress())
	}
	item := loopItem(t, report.Passes[1], 12)
	if !item.Started || !item.Approved || item.Failed || item.Phase != "review-waiting" {
		t.Fatalf("a Molecule started after the re-approval was not approved: %+v", item)
	}
	if loopStarts(loopLines(t, harnessLog)) != 2 {
		t.Fatalf("harness starts %d", loopStarts(loopLines(t, harnessLog)))
	}
}

func TestHardeningLoopStopsBeforeAnyWorkWhenTheFormulaCannotBeReloaded(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(t *testing.T, o Options)
		want   string
	}{
		"no longer approved": {func(t *testing.T, o Options) {
			if err := os.Remove(filepath.Join(o.LabDir, "lab.json")); err != nil {
				t.Fatal(err)
			}
		}, "could not be reloaded"},
		"another repository": {func(t *testing.T, o Options) {
			reapprove(t, o.LabDir, "elsewhere", func(yaml string) string { return strings.Replace(yaml, "repo: acme/widgets", "repo: acme/gadgets", 1) })
		}, "intake.repo changed"},
	} {
		t.Run(name, func(t *testing.T) {
			o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
			analyses := countAnalysis(&o)
			calls := 0
			list := func(context.Context, string) (intake.OpenIssues, error) {
				calls++
				if calls == 1 {
					return intake.OpenIssues{Issues: []intake.OpenIssue{loopOpen(11, 0)}}, nil
				}
				tc.change(t, o)
				return intake.OpenIssues{Issues: []intake.OpenIssue{loopOpen(11, 0), loopOpen(12, 1)}}, nil
			}
			report, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 1, ListIssues: list})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if len(report.Passes) != 1 || len(analyses()) != 1 || len(loopRecords(t, o.LabDir)[12]) != 0 {
				t.Fatalf("passes %d, analyses %v: work started after a failed reload", len(report.Passes), analyses())
			}
		})
	}
}

// crashedLedger leaves the Molecule of an Issue in the given ledger status, with no
// scope plan when planned is false, and sets the ledger file's modification time.
func crashedLedger(t *testing.T, o Options, number int64, status string, planned bool, written time.Time) Record {
	t.Helper()
	r := loopMolecule(t, o, number)
	r.Status = status
	if !planned {
		r.Readiness, r.Analysis = nil, nil
	}
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(o.LabDir, "ledgers", r.ID+".json"), written, written); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHardeningCrashedStartLedgerIsReportedAsStalledNotRecoverable(t *testing.T) {
	for _, status := range []string{"running", "allocating"} {
		t.Run(status, func(t *testing.T) {
			o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
			r := crashedLedger(t, o, 12, status, false, time.Now().Add(-24*time.Hour))
			analyses := countAnalysis(&o)
			list := loopFixedList(loopOpen(12, 0))

			report := loopOnce(t, o, list)
			item := loopItem(t, report.Passes[0], 12)
			if item.State != LoopStalled || item.Started || item.Approved || item.Failed || item.MoleculeID != r.ID || item.Status != status {
				t.Fatalf("%+v", item)
			}
			if !strings.Contains(item.Reason, "never reached the scope gate") || !strings.Contains(item.Reason, "forgecell ledger "+r.ID) || strings.Contains(item.Reason, "recover "+r.ID) {
				t.Fatalf("the reason does not explain the crashed start: %s", item.Reason)
			}
			if report.NonProgress() != 1 || report.Failed() {
				t.Fatalf("non-progress %d, failed %t", report.NonProgress(), report.Failed())
			}
			if len(analyses()) != 0 || len(loopRecords(t, o.LabDir)[12]) != 1 {
				t.Fatal("the loop started another Molecule for a stuck Issue")
			}

			// A loop that still holds the Issue's start lock is starting this Molecule: keep the plain active report.
			unlock, err := lockIssue(o.LabDir, "acme/widgets", 12)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			item = loopItem(t, loopOnce(t, o, list).Passes[0], 12)
			if item.State != LoopActive || !strings.Contains(item.Reason, "forgecell recover "+r.ID) {
				t.Fatalf("%+v", item)
			}
		})
	}
}

func TestHardeningLedgerWrittenWithinTheAnalysisTimeoutIsStillActive(t *testing.T) {
	for _, status := range []string{"running", "allocating"} {
		for _, planned := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s planned=%t", status, planned), func(t *testing.T) {
				o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
				r := crashedLedger(t, o, 12, status, planned, time.Now())
				report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
				item := loopItem(t, report.Passes[0], 12)
				if item.State != LoopActive || item.MoleculeID != r.ID || !strings.Contains(item.Reason, "forgecell recover "+r.ID) {
					t.Fatalf("a ledger a manual run is still writing was reported as abandoned: %+v", item)
				}
				if report.NonProgress() != 0 {
					t.Fatalf("non-progress %d", report.NonProgress())
				}
			})
		}
	}
}

func deadProcessID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestHardeningInterruptedApprovalIsStalledOnlyWithoutALiveOwner(t *testing.T) {
	old := time.Now().Add(-24 * time.Hour)
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := crashedLedger(t, o, 12, "running", true, old)
	list := loopFixedList(loopOpen(12, 0))
	lock := filepath.Join(o.LabDir, "ledgers", r.ID+".lock")
	setOwner := func(owner string) {
		t.Helper()
		if err := os.RemoveAll(lock); err != nil {
			t.Fatal(err)
		}
		if owner == "" {
			return
		}
		if err := os.Mkdir(lock, 0700); err != nil {
			t.Fatal(err)
		}
		if owner != "none" {
			if err := os.WriteFile(filepath.Join(lock, "owner"), []byte(owner), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	run := func() (LoopItem, LoopReport) {
		t.Helper()
		report := loopOnce(t, o, list)
		return loopItem(t, report.Passes[0], 12), report
	}

	for name, owner := range map[string]string{"no lock directory": "", "a dead owner": fmt.Sprint(deadProcessID(t))} {
		setOwner(owner)
		item, report := run()
		if item.State != LoopStalled || item.Started || item.Approved || item.Failed || item.Status != "running" || item.MoleculeID != r.ID {
			t.Fatalf("%s: %+v", name, item)
		}
		if !strings.Contains(item.Reason, "no live Lab process owns its approval lock") || !strings.Contains(item.Reason, "forgecell ledger "+r.ID) || !strings.Contains(item.Reason, "scope-waiting") {
			t.Fatalf("%s: the reason does not explain the interruption: %s", name, item.Reason)
		}
		if report.NonProgress() != 1 || report.Failed() {
			t.Fatalf("%s: non-progress %d, failed %t", name, report.NonProgress(), report.Failed())
		}
	}

	for name, owner := range map[string]string{"this live process": fmt.Sprint(os.Getpid()), "an ownerless lock": "none", "an unparsable owner": "not a pid"} {
		setOwner(owner)
		item, report := run()
		if item.State != LoopActive || report.NonProgress() != 0 || !strings.Contains(item.Reason, "forgecell recover "+r.ID) {
			t.Fatalf("%s: a lock that may be live was reported as abandoned: %+v", name, item)
		}
	}
	if _, err := os.Lstat(lock); err != nil {
		t.Fatalf("the loop removed the lock directory: %v", err)
	}
}

func TestHardeningLoopLockFilesDoNotCountTowardTheInventoryCap(t *testing.T) {
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	r := loopMolecule(t, o, 12)
	dir := filepath.Join(o.LabDir, "ledgers")
	for i := 0; i < 4200; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("issue-%04d%s", i, loopLockSuffix)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	inventory := ReadQueueInventory(o.LabDir)
	if !inventory.Complete || inventory.Truncated || len(inventory.Attempts) != 1 || inventory.Attempts[0].ID != r.ID {
		t.Fatalf("lock files crowded the inventory out: %+v", inventory)
	}
	report := loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	if item := loopItem(t, report.Passes[0], 12); item.MoleculeID != r.ID || item.State != LoopWaiting {
		t.Fatalf("%+v", item)
	}
}

func TestHardeningTheEntryCapStillBoundsEverythingElse(t *testing.T) {
	lab := t.TempDir()
	dir := filepath.Join(lab, "ledgers")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4097; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("n%d.txt", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	inventory := ReadQueueInventory(lab)
	if inventory.Complete || !inventory.Truncated || !strings.Contains(strings.Join(inventory.Diagnostics, ";"), "4096") {
		t.Fatalf("%+v", inventory)
	}
}

func TestHardeningTruncatedListingUnderASelectorStartsNothing(t *testing.T) {
	for _, input := range []string{"label:bug", "milestone:v1"} {
		t.Run(input, func(t *testing.T) {
			o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
			analyses := countAnalysis(&o)
			selector, err := intake.ParseSelector(input)
			if err != nil {
				t.Fatal(err)
			}
			list := func(context.Context, string) (intake.OpenIssues, error) {
				return intake.OpenIssues{Issues: []intake.OpenIssue{loopOpen(12, 0, "bug")}, Truncated: true}, nil
			}
			report, err := RunLoop(context.Background(), LoopOptions{Template: o, Select: selector, MaxConcurrent: 1, Once: true, ListIssues: list})
			if err == nil || !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), input) {
				t.Fatalf("a truncated listing was used under %s: %v", input, err)
			}
			if len(report.Passes) != 0 || len(analyses()) != 0 || len(loopRecords(t, o.LabDir)) != 0 {
				t.Fatal("work started on a partial listing")
			}
		})
	}
}

func TestHardeningOneLineStripsTerminalControlText(t *testing.T) {
	override := string(rune(0x202e))
	got := oneLine("a\x1b[31mred\x1b[0m\nb\x07" + override + "c\x1b]0;title\x07d")
	if got != "ared b cd" {
		t.Fatalf("got %q", got)
	}
	if runes := []rune(oneLine(strings.Repeat("x", 1000))); len(runes) != 300 {
		t.Fatalf("a long text was not bounded: %d runes", len(runes))
	}
}

func TestHardeningDefaultListerCarriesTheSelector(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	ghLog, _ := loopTranscripts(t)
	selector, err := intake.ParseSelector("label:needs triage")
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunLoop(context.Background(), LoopOptions{Template: o, Select: selector, MaxConcurrent: 1, Once: true})
	if err != nil || len(report.Passes) != 1 || len(report.Passes[0].Items) != 0 {
		t.Fatalf("%+v %v", report, err)
	}
	calls := loopLines(t, ghLog)
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "&labels=needs+triage&page=1") {
		t.Fatalf("the label was not part of the query: %v", calls)
	}
}
