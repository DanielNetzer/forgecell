package molecule

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/intake"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

const triggersTimeout = time.Minute

// triggersInterrupted leaves Issue 12's Molecule running at its scope gate with a
// plan, written long ago and owned by nobody. attempt shapes the harness attempt
// the ledger records: "" for none, "unreconciled", "reconciled" or "tree".
func triggersInterrupted(t *testing.T, o Options, attempt string) Record {
	t.Helper()
	old := time.Now().Add(-24 * time.Hour)
	r := crashedLedger(t, o, 12, "running", true, old)
	digest := r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest
	if attempt != "" {
		a := HarnessAttempt{ID: "attempt-1", PlanDigest: digest, Evidence: &VerificationFile{Path: "evidence/harness-attempt-1.json", SHA256: strings.Repeat("a", 64)}, Result: process.Result{Reconciled: attempt != "unreconciled"}}
		if attempt == "tree" {
			a.Capture = &verification.CaptureResult{Tree: strings.Repeat("b", 40)}
		}
		r.HarnessAttempts = []HarnessAttempt{a}
		if err := save(r); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(o.LabDir, "ledgers", r.ID+".json"), old, old); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func triggersProposalFile(lab, id string) string {
	return filepath.Join(lab, "recovery-proposals", id+".json")
}

func triggersLedgerBytes(t *testing.T, lab, id string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(lab, "ledgers", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestTriggersRecoverProposalIsSavedForAnAbandonedMoleculeAndNeverRun(t *testing.T) {
	for name, owner := range map[string]string{"no lock directory": "", "a dead owner": "dead"} {
		t.Run(name, func(t *testing.T) {
			o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
			r := triggersInterrupted(t, o, "tree")
			digest := r.Readiness.Plans[len(r.Readiness.Plans)-1].Digest
			lock := filepath.Join(o.LabDir, "ledgers", r.ID+".lock")
			if owner == "dead" {
				if err := os.Mkdir(lock, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(lock, "owner"), []byte(strconv.Itoa(deadProcessID(t))), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ledgerBefore := triggersLedgerBytes(t, o.LabDir, r.ID)
			list := loopFixedList(loopOpen(12, 0))

			report := loopOnce(t, o, list)
			item := loopItem(t, report.Passes[0], 12)
			if item.State != LoopStalled || item.Started || item.Approved || item.Failed || item.Proposal == nil || item.Proposal.Existing {
				t.Fatalf("%+v", item)
			}
			want := "forgecell recover " + shellWord(r.ID) + " --plan " + shellWord(digest) + " --confirm-stopped --lab " + shellWord(o.LabDir)
			if item.Proposal.Command != want || item.Proposal.Path != triggersProposalFile(o.LabDir, r.ID) {
				t.Fatalf("proposal %+v, want command %q", item.Proposal, want)
			}
			planFile := filepath.Join(o.LabDir, "recovery-proposals", "adopt-plans", r.ID+".json")
			wantAmend := "forgecell amend " + shellWord(r.ID) + " --parent " + shellWord(digest) + " --plan " + shellWord(planFile) + " --lab " + shellWord(o.LabDir)
			if len(item.Proposal.Followup) != 1 || item.Proposal.Followup[0] != wantAmend || strings.Contains(item.Proposal.Followup[0], "#") {
				t.Fatalf("follow-up %q, want exactly %q", item.Proposal.Followup, wantAmend)
			}
			rawPlan, err := os.ReadFile(planFile)
			if err != nil {
				t.Fatal(err)
			}
			var adopt readiness.Plan
			if err = json.Unmarshal(rawPlan, &adopt); err != nil || adopt.Continuation != "adopt-failed-tree" || adopt.AdoptAttempt != "attempt-1" {
				t.Fatalf("adopt plan %+v: %v", adopt, err)
			}
			if info, err := os.Stat(planFile); err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("adopt plan file: %v %v", info, err)
			}
			info, err := os.Stat(item.Proposal.Path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("proposal file: %v %v", info, err)
			}
			read, err := ReadRecoveryProposals(o.LabDir, triggersTimeout)
			if err != nil || len(read) != 1 || read[0].MoleculeID != r.ID || read[0].PlanDigest != digest || read[0].Command != want || read[0].Issue != 12 || len(read[0].Followup) != 1 || read[0].Followup[0].Command != wantAmend {
				t.Fatalf("read back %+v, %v", read, err)
			}

			if string(triggersLedgerBytes(t, o.LabDir, r.ID)) != string(ledgerBefore) {
				t.Fatal("the loop changed the ledger")
			}
			if _, err = os.Lstat(filepath.Join(o.LabDir, "ledgers", r.ID+".recovery-lock")); !os.IsNotExist(err) {
				t.Fatal("a recovery ran or left a lock")
			}
			if _, err = os.Lstat(lock); (owner == "") != os.IsNotExist(err) {
				t.Fatalf("the loop created or removed the approval lock directory: %v", err)
			}
			if len(loopLines(t, harnessLog)) != 0 || len(loopRecords(t, o.LabDir)[12]) != 1 {
				t.Fatal("the loop invoked a harness or started another Molecule")
			}
			unlockMolecule, err := lockMolecule(o.LabDir, r.ID)
			if err != nil {
				t.Fatalf("the Molecule lock outlived the step: %v", err)
			}
			unlockMolecule()
			unlock, err := lockIssue(o.LabDir, "acme/widgets", 12)
			if err != nil {
				t.Fatalf("the Issue lock outlived the step: %v", err)
			}
			unlock()

			// A second pass leaves the file's bytes alone and says it is already proposed.
			before, err := os.ReadFile(item.Proposal.Path)
			if err != nil {
				t.Fatal(err)
			}
			again := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
			after, err := os.ReadFile(item.Proposal.Path)
			if err != nil || string(before) != string(after) {
				t.Fatalf("the proposal changed between passes: %v", err)
			}
			if again.Proposal == nil || !again.Proposal.Existing || again.State != LoopStalled {
				t.Fatalf("%+v", again)
			}
			entries, _ := os.ReadDir(filepath.Join(o.LabDir, "recovery-proposals"))
			if len(entries) != 2 {
				t.Fatalf("%d entries in the proposal directory, want the proposal and the adopt-plans directory", len(entries))
			}
		})
	}
}

func TestTriggersChangedProposalIsReplacedAtomically(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := triggersInterrupted(t, o, "reconciled")
	list := loopFixedList(loopOpen(12, 0))
	item := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
	if item.Proposal == nil || len(item.Proposal.Followup) != 0 {
		t.Fatalf("a proposal without a captured tree must carry no adopt step: %+v", item.Proposal)
	}
	file := triggersProposalFile(o.LabDir, r.ID)
	if err := os.WriteFile(file, []byte(`{"schemaVersion":"v1","moleculeId":"`+r.ID+`","planDigest":"`+strings.Repeat("0", 64)+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if read, err := ReadRecoveryProposals(o.LabDir, triggersTimeout); err != nil || len(read) != 0 {
		t.Fatalf("a proposal for a digest the ledger no longer holds was listed: %+v %v", read, err)
	}
	again := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
	if again.Proposal == nil || again.Proposal.Existing {
		t.Fatalf("%+v", again.Proposal)
	}
	if read, err := ReadRecoveryProposals(o.LabDir, triggersTimeout); err != nil || len(read) != 1 {
		t.Fatalf("%+v %v", read, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(file))
	if len(entries) != 1 {
		t.Fatalf("a temporary file was left behind: %d entries", len(entries))
	}
}

func TestTriggersNoProposalWhenRecoveryDoesNotApply(t *testing.T) {
	list := loopFixedList(loopOpen(12, 0))
	for name, attempt := range map[string]string{"no harness attempt": "", "unreconciled attempt": "unreconciled"} {
		t.Run(name, func(t *testing.T) {
			o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
			triggersInterrupted(t, o, attempt)
			item := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
			if item.State != LoopStalled || item.Proposal != nil || !strings.Contains(item.Reason, "no recovery proposal was made") || !strings.Contains(item.Reason, "no live Lab process owns its approval lock") {
				t.Fatalf("%+v", item)
			}
			if _, err := os.Lstat(filepath.Join(o.LabDir, "recovery-proposals")); !os.IsNotExist(err) {
				t.Fatal("a proposal directory was created")
			}
			if read, err := ReadRecoveryProposals(o.LabDir, triggersTimeout); err != nil || len(read) != 0 {
				t.Fatalf("%+v %v", read, err)
			}
		})
	}

	t.Run("a possibly live owner", func(t *testing.T) {
		for name, owner := range map[string]string{"this live process": strconv.Itoa(os.Getpid()), "an unparsable owner": "not a pid"} {
			o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
			r := triggersInterrupted(t, o, "tree")
			lock := filepath.Join(o.LabDir, "ledgers", r.ID+".lock")
			if err := os.Mkdir(lock, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(lock, "owner"), []byte(owner), 0600); err != nil {
				t.Fatal(err)
			}
			item := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
			if item.State != LoopActive || item.Proposal != nil {
				t.Fatalf("%s: %+v", name, item)
			}
			if _, err := os.Lstat(filepath.Join(o.LabDir, "recovery-proposals")); !os.IsNotExist(err) {
				t.Fatalf("%s: a proposal directory was created", name)
			}
		}
	})

	t.Run("a ledger written within the analysis timeout", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		r := triggersInterrupted(t, o, "tree")
		now := time.Now()
		if err := os.Chtimes(filepath.Join(o.LabDir, "ledgers", r.ID+".json"), now, now); err != nil {
			t.Fatal(err)
		}
		if item := loopItem(t, loopOnce(t, o, list).Passes[0], 12); item.State != LoopActive || item.Proposal != nil {
			t.Fatalf("%+v", item)
		}
	})

	t.Run("intake that never reached a plan", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		crashedLedger(t, o, 12, "running", false, time.Now().Add(-24*time.Hour))
		item := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
		if item.State != LoopStalled || item.Proposal != nil || strings.Contains(item.Reason, "no recovery proposal") {
			t.Fatalf("%+v", item)
		}
		if _, err := os.Lstat(filepath.Join(o.LabDir, "recovery-proposals")); !os.IsNotExist(err) {
			t.Fatal("a proposal directory was created")
		}
	})

	t.Run("a stall for another reason", func(t *testing.T) {
		o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
		r := loopMolecule(t, o, 12)
		if err := os.Mkdir(filepath.Join(o.LabDir, "ledgers", r.ID+".lock"), 0700); err != nil {
			t.Fatal(err)
		}
		item := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
		if item.Proposal != nil {
			t.Fatalf("%+v", item)
		}
		if _, err := os.Lstat(filepath.Join(o.LabDir, "recovery-proposals")); !os.IsNotExist(err) {
			t.Fatal("a proposal directory was created")
		}
	})
}

func TestTriggersDoctorListsAProposalOnlyWhileTheLedgerIsStillInterrupted(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := triggersInterrupted(t, o, "tree")
	if item := loopItem(t, loopOnce(t, o, loopFixedList(loopOpen(12, 0))).Passes[0], 12); item.Proposal == nil {
		t.Fatalf("%+v", item)
	}
	if read, err := ReadRecoveryProposals(o.LabDir, triggersTimeout); err != nil || len(read) != 1 {
		t.Fatalf("%+v %v", read, err)
	}
	r.Status = "failed"
	if err := save(r); err != nil {
		t.Fatal(err)
	}
	if read, err := ReadRecoveryProposals(o.LabDir, triggersTimeout); err != nil || len(read) != 0 {
		t.Fatalf("a recovered Molecule's proposal was still listed: %+v %v", read, err)
	}
	if _, err := os.Stat(triggersProposalFile(o.LabDir, r.ID)); err != nil {
		t.Fatalf("reading deleted the proposal: %v", err)
	}

	// The loop, not doctor, removes a proposal that no longer applies.
	loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	if _, err := os.Stat(triggersProposalFile(o.LabDir, r.ID)); !os.IsNotExist(err) {
		t.Fatalf("the proposal outlived the interruption: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.LabDir, "recovery-proposals", "adopt-plans", r.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("the adopt plan outlived the proposal: %v", err)
	}
}

func TestTriggersReadListsOnlyWhatTheLoopWouldStillReportAsAbandoned(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := triggersInterrupted(t, o, "tree")
	if item := loopItem(t, loopOnce(t, o, loopFixedList(loopOpen(12, 0))).Passes[0], 12); item.Proposal == nil {
		t.Fatalf("%+v", item)
	}
	ledger := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
	old := time.Now().Add(-24 * time.Hour)
	listed := func() int {
		t.Helper()
		read, err := ReadRecoveryProposals(o.LabDir, triggersTimeout)
		if err != nil {
			t.Fatal(err)
		}
		return len(read)
	}
	if listed() != 1 {
		t.Fatal("the abandoned Molecule's proposal was not listed")
	}

	now := time.Now()
	if err := os.Chtimes(ledger, now, now); err != nil {
		t.Fatal(err)
	}
	if listed() != 0 {
		t.Fatal("a ledger written within the analysis timeout still listed a proposal")
	}
	if err := os.Chtimes(ledger, old, old); err != nil {
		t.Fatal(err)
	}

	lock := filepath.Join(o.LabDir, "ledgers", r.ID+".lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lock, "owner"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	if listed() != 0 {
		t.Fatal("a live approval-lock owner still listed a proposal")
	}
	if err := os.WriteFile(filepath.Join(lock, "owner"), []byte(strconv.Itoa(deadProcessID(t))), 0600); err != nil {
		t.Fatal(err)
	}
	if listed() != 1 {
		t.Fatal("a dead owner hid the proposal")
	}

	plan := filepath.Join(o.LabDir, "recovery-proposals", "adopt-plans", r.ID+".json")
	if err := os.WriteFile(plan, []byte(`{"continuation":"something else"}`), 0600); err != nil {
		t.Fatal(err)
	}
	read, err := ReadRecoveryProposals(o.LabDir, triggersTimeout)
	if err != nil || len(read) != 1 || len(read[0].Followup) != 0 {
		t.Fatalf("an edited adopt plan kept its runnable follow-up: %+v %v", read, err)
	}
}

func TestTriggersPruneKeepsAProposalWhoseLedgerCannotBeRead(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := triggersInterrupted(t, o, "tree")
	loopOnce(t, o, loopFixedList(loopOpen(12, 0)))
	file := triggersProposalFile(o.LabDir, r.ID)
	ledger := filepath.Join(o.LabDir, "ledgers", r.ID+".json")
	good, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(ledger, []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	pruneRecoveryProposals(o.LabDir, triggersTimeout)
	if _, err = os.Stat(file); err != nil {
		t.Fatalf("a proposal was removed because its ledger was unreadable: %v", err)
	}
	if err = os.WriteFile(ledger, good, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	pruneRecoveryProposals(o.LabDir, triggersTimeout)
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("a proposal for a vanished ledger was kept: %v", err)
	}
}

func TestTriggersProposalCommandsQuoteEveryArgument(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	r := triggersInterrupted(t, o, "tree")
	p, why := buildRecoveryProposal(o.LabDir, r.ID)
	if why != "" {
		t.Fatal(why)
	}
	for _, command := range []string{p.Command, p.Followup[0].Command} {
		for _, arg := range []string{shellWord(r.ID), shellWord(p.PlanDigest), shellWord(o.LabDir)} {
			if !strings.Contains(command, " "+arg) {
				t.Fatalf("%q does not quote %s", command, arg)
			}
		}
	}
	if _, why = buildRecoveryProposal(o.LabDir, "../ledgers/"+r.ID); why == "" {
		t.Fatal("a Molecule id outside the safe alphabet built a proposal")
	}
}

func TestTriggersListingAndInventoryFailuresAreTransientAndFormulaFailuresAreNot(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	failing := func(context.Context, string) (intake.OpenIssues, error) {
		return intake.OpenIssues{}, errors.New("gh: network unreachable")
	}
	_, err := RunLoop(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: failing})
	if err == nil || !IsTransientLoopError(err) || !strings.Contains(err.Error(), "network unreachable") {
		t.Fatalf("a listing failure was not transient: %v", err)
	}

	bad := o
	bad.LabDir = filepath.Join(t.TempDir(), "no-lab")
	_, err = RunLoop(loopBounded(t), LoopOptions{Template: bad, MaxConcurrent: 1, Once: true, ListIssues: loopFixedList()})
	if err == nil || IsTransientLoopError(err) {
		t.Fatalf("a Formula failure must end a watch: %v", err)
	}
	if _, err = RunLoop(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 0, Once: true, ListIssues: loopFixedList()}); err == nil || IsTransientLoopError(err) {
		t.Fatalf("a configuration failure must end a watch: %v", err)
	}
}

func TestTriggersCancellingTheContextInterruptsAHungListing(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	ctx, cancel := context.WithCancel(loopBounded(t))
	entered := make(chan struct{})
	hung := func(ctx context.Context, _ string) (intake.OpenIssues, error) {
		close(entered)
		<-ctx.Done()
		return intake.OpenIssues{}, ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		_, err := RunLoop(ctx, LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: hung, Work: context.WithoutCancel(ctx)})
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled listing reported success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancelling did not interrupt the hung listing")
	}
}

func TestTriggersCancelledContextLeavesInFlightMoleculesOnTheWorkContext(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	gate := newLoopGate()
	o.Analyze = triggersGatedAnalyze(gate, o.Analyze)
	ctx, cancel := context.WithCancel(loopBounded(t))
	done := loopBackground(ctx, LoopOptions{Template: o, MaxConcurrent: 1, Once: true, ListIssues: loopFixedList(loopOpen(11, 0)), Work: context.WithoutCancel(ctx)})
	gate.waitEntered(t)
	cancel()
	gate.open()
	report := loopWait(t, done)
	if item := loopItem(t, report.Passes[0], 11); !item.Started || !item.Approved || item.Failed {
		t.Fatalf("cancelling the listing context interrupted the in-flight Molecule: %+v", item)
	}
}

func TestTriggersDrainDispatchesNothingFurtherAndLetsInFlightMoleculesFinish(t *testing.T) {
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	gate := newLoopGate()
	analyze := o.Analyze
	o.Analyze = triggersGatedAnalyze(gate, analyze)
	drain := make(chan struct{})
	list := loopFixedList(loopOpen(11, 0), loopOpen(12, 1))

	done := loopBackground(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 1, ListIssues: list, Drain: drain})
	gate.waitEntered(t)
	close(drain)
	gate.open()
	report := loopWait(t, done)

	if len(report.Passes) != 1 {
		t.Fatalf("the loop ran %d passes after a drain request", len(report.Passes))
	}
	first, second := loopItem(t, report.Passes[0], 11), loopItem(t, report.Passes[0], 12)
	if !first.Started || !first.Approved || first.Failed || first.State != LoopWaiting {
		t.Fatalf("the in-flight Molecule did not finish: %+v", first)
	}
	if second.State != LoopInterrupted || second.Started || second.MoleculeID != "" {
		t.Fatalf("%+v", second)
	}
	if loopStarts(loopLines(t, harnessLog)) != 1 || len(loopRecords(t, o.LabDir)[12]) != 0 {
		t.Fatal("work was dispatched after the drain request")
	}
	if report.NonProgress() != 0 {
		t.Fatalf("non-progress %d", report.NonProgress())
	}
	for _, r := range loopRecords(t, o.LabDir)[11] {
		unlock, err := lockMolecule(o.LabDir, r.ID)
		if err != nil {
			t.Fatalf("a Molecule lock outlived the drain: %v", err)
		}
		unlock()
	}
}

func TestTriggersAClosedDrainEndsAnUnboundedLoopAfterOnePassWithoutStartingAnything(t *testing.T) {
	o, harnessLog := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	drain := make(chan struct{})
	close(drain)
	report, err := RunLoop(loopBounded(t), LoopOptions{Template: o, MaxConcurrent: 2, ListIssues: loopFixedList(loopOpen(11, 0), loopOpen(12, 1)), Drain: drain})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Passes) != 1 {
		t.Fatalf("%d passes", len(report.Passes))
	}
	for _, item := range report.Passes[0].Items {
		if item.State != LoopInterrupted || item.Started {
			t.Fatalf("%+v", item)
		}
	}
	if len(loopRecords(t, o.LabDir)) != 0 || len(loopLines(t, harnessLog)) != 0 {
		t.Fatal("a drained loop started work")
	}
}

func TestTriggersRepeatedPassesNeverApproveAHumanGateOrDeliver(t *testing.T) {
	o, harnessLog := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	ghLog, gitLog := loopTranscripts(t)
	list := loopFixedList(loopOpen(12, 0))
	var first Record
	for pass := 0; pass < 3; pass++ {
		item := loopItem(t, loopOnce(t, o, list).Passes[0], 12)
		if item.Approved || item.State != LoopWaiting || (pass == 0) != item.Started {
			t.Fatalf("pass %d: %+v", pass, item)
		}
		if pass == 0 {
			first = loadRecord(t, o.LabDir, item.MoleculeID)
		}
	}
	records := loopRecords(t, o.LabDir)[12]
	if len(records) != 1 {
		t.Fatalf("%d Molecules", len(records))
	}
	if len(records[0].PolicyDecisions) != 0 || records[0].ID != first.ID {
		t.Fatalf("%d decisions", len(records[0].PolicyDecisions))
	}
	if len(loopLines(t, harnessLog)) != 0 {
		t.Fatal("coding started behind a human gate")
	}
	for _, line := range append(loopLines(t, ghLog), loopLines(t, gitLog)...) {
		for _, banned := range []string{"push", "merge", "pull", "send-pack"} {
			if strings.Contains(line, banned) {
				t.Fatalf("a pass ran %q", line)
			}
		}
	}
}

func triggersGatedAnalyze(gate *loopGate, next func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error)) func(context.Context, formula.Binding, Issue, readiness.RepositorySnapshot) (readiness.Analysis, error) {
	return func(ctx context.Context, b formula.Binding, i Issue, s readiness.RepositorySnapshot) (readiness.Analysis, error) {
		if err := gate.hold(ctx); err != nil {
			return readiness.Analysis{}, err
		}
		return next(ctx, b, i, s)
	}
}
