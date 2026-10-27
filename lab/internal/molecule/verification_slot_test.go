package molecule

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DanielNetzer/forgecell/lab/internal/formula"
	"github.com/DanielNetzer/forgecell/lab/internal/process"
	"github.com/DanielNetzer/forgecell/lab/internal/readiness"
	"github.com/DanielNetzer/forgecell/lab/internal/resources"
	"github.com/DanielNetzer/forgecell/lab/internal/verification"
)

// driveSlotTicker replaces the queue's ticker with one the test drives: queued
// is closed when a waiter first starts polling, and tick delivers one retry.
func driveSlotTicker(t *testing.T) (queued <-chan struct{}, tick chan<- time.Time) {
	t.Helper()
	original := verificationSlotTicker
	t.Cleanup(func() { verificationSlotTicker = original })
	started, ticks := make(chan struct{}), make(chan time.Time)
	var once sync.Once
	verificationSlotTicker = func() (<-chan time.Time, func()) {
		once.Do(func() { close(started) })
		return ticks, func() {}
	}
	return started, ticks
}

// limitSlots makes the Lab allow n verification slots for the test.
func limitSlots(t *testing.T, n int) {
	t.Helper()
	original := verificationSlotLimit
	t.Cleanup(func() { verificationSlotLimit = original })
	verificationSlotLimit = func(Options) (int, string) { return n, "" }
}

// slotHeld reports whether some holder other than the caller has the Lab's slot.
func slotHeld(t *testing.T, lab string) bool {
	t.Helper()
	release, err := flockLedgerFile(lab, verificationSlotFile)
	if errors.Is(err, errLocked) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	release()
	return false
}

func eventKinds(r Record) []string {
	kinds := []string{}
	for _, e := range r.Readiness.Events {
		kinds = append(kinds, e.Kind)
	}
	return kinds
}

// TestVerificationSlotSerializesMolecules runs two Molecules of one Lab together.
// The first to reach its checks waits inside the fake verifier until the other
// has queued, so the waiter must enter only after the first exits. Serial: it
// replaces the verifier and ticker seams.
func TestVerificationSlotSerializesMolecules(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	limitSlots(t, 1)
	queued, tick := driveSlotTicker(t)
	original := runVerification
	t.Cleanup(func() { runVerification = original })
	var mu sync.Mutex
	var log []string
	active, most, entries := 0, 0, 0
	runVerification = func(ctx context.Context, source, destination string, p readiness.Plan, c verification.CaptureResult, observe func(verification.Observation) error) verification.Result {
		mu.Lock()
		active++
		most = max(most, active)
		entries++
		first := entries == 1
		log = append(log, "enter "+p.MoleculeID)
		mu.Unlock()
		if !slotHeld(t, o.LabDir) {
			t.Errorf("checks of %s started without the verification slot", p.MoleculeID)
		}
		// Check budgets are process timeouts set inside the verifier, so no deadline
		// covering the queue may reach it.
		if _, deadline := ctx.Deadline(); deadline {
			t.Errorf("the verification context of %s carries a deadline", p.MoleculeID)
		}
		if first {
			<-queued
		}
		result := original(ctx, source, destination, p, c, observe)
		mu.Lock()
		active--
		log = append(log, "exit "+p.MoleculeID)
		mu.Unlock()
		return result
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case tick <- time.Time{}:
			case <-done:
				return
			}
		}
	}()
	_, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 2, Once: true, ListIssues: loopFixedList(loopOpen(11, 0), loopOpen(12, 1))})
	close(done)
	if err != nil {
		t.Fatal(err)
	}
	if most != 1 || len(log) != 4 || !strings.HasPrefix(log[0], "enter ") || log[1] != "exit "+strings.TrimPrefix(log[0], "enter ") || log[3] != "exit "+strings.TrimPrefix(log[2], "enter ") {
		t.Fatalf("verifications overlapped or did not both run: most=%d log=%v", most, log)
	}
	waiter := strings.TrimPrefix(log[2], "enter ")
	records := loopRecords(t, o.LabDir)
	for _, number := range []int64{11, 12} {
		if len(records[number]) != 1 {
			t.Fatalf("Issue %d: %d Molecules", number, len(records[number]))
		}
		r := records[number][0]
		if statePhase(r) != "review-waiting" || !r.Verification.RequiredChecksPassed {
			t.Fatalf("%s: phase %s notes %v", r.ID, statePhase(r), r.Notes)
		}
		kinds := eventKinds(r)
		started := slices.Index(kinds, readiness.VerificationStarted)
		if started < 0 || kinds[len(kinds)-1] != "verification-review-waiting" {
			t.Fatalf("%s events %v", r.ID, kinds)
		}
		q := slices.Index(kinds, readiness.VerificationQueued)
		if r.ID == waiter {
			if q < 0 || q != started-1 || r.Readiness.Events[q].At == "" {
				t.Fatalf("the waiter's ledger must show verification-queued, then verification-started: %v", kinds)
			}
			for _, obs := range r.Verification.Checks {
				if obs.Result.TimedOut {
					t.Fatalf("queue time was counted against check %s", obs.ID)
				}
			}
		} else if q >= 0 {
			t.Fatalf("the first Molecule never waited, yet its ledger shows it queued: %v", kinds)
		}
	}
	if slotHeld(t, o.LabDir) {
		t.Fatal("the slot outlived both verifications")
	}
	if _, err = os.Lstat(filepath.Join(o.LabDir, "ledgers", verificationSlotFile)); err != nil {
		t.Fatalf("the slot file must stay in place: %v", err)
	}
}

// TestVerificationSlotFreeDuringReview: the slot covers the checks only. While
// the first Molecule's fake review harness is held open by a marker, a second
// Molecule of the same Lab takes the slot without queueing, runs its checks and
// reaches its own review. Serial: it replaces the verifier and ticker seams.
func TestVerificationSlotFreeDuringReview(t *testing.T) {
	o, _ := loopFixture(t, defaultScopeAtom, defaultReviewAtom, "")
	path := filepath.Join(o.LabDir, "formulas", "sample.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(string(raw), "atoms:\n", reviewFxRole+"atoms:\n", 1)
	yaml = strings.Replace(yaml, "  - {id: gate,", "  - "+reviewFxAtom+"\n  - {id: gate,", 1)
	if err = os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	approveFixture(t, o.LabDir, yaml, "review-atom")
	inReview, releaseReview := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(releaseReview) }) }
	defer unblock()
	var mu sync.Mutex
	reviews := 0
	o.ReviewDiff = func(context.Context, Workspace, string, string) (string, error) {
		return "diff --git a/code.txt b/code.txt\n-base\n+changed\n", nil
	}
	o.InvokeReview = func(context.Context, formula.Binding, string, time.Duration, map[string]any, []string) process.Result {
		mu.Lock()
		reviews++
		first := reviews == 1
		mu.Unlock()
		if first {
			close(inReview)
			<-releaseReview
		}
		return process.Result{OK: true, Stdout: reviewFxClean}
	}
	originalVerify, originalTicker := runVerification, verificationSlotTicker
	defer func() { runVerification, verificationSlotTicker = originalVerify, originalTicker }()
	runVerification = func(ctx context.Context, source, destination string, p readiness.Plan, c verification.CaptureResult, observe func(verification.Observation) error) verification.Result {
		if !slotHeld(t, o.LabDir) {
			t.Errorf("checks of %s started without the verification slot", p.MoleculeID)
		}
		return originalVerify(ctx, source, destination, p, c, observe)
	}
	// Queueing behind a review is the defect under test: report it and let the
	// first review finish so the waiter can proceed instead of hanging.
	verificationSlotTicker = func() (<-chan time.Time, func()) {
		t.Error("the second Molecule queued behind the first Molecule's review")
		unblock()
		ticks := make(chan time.Time)
		close(ticks)
		return ticks, func() {}
	}
	approved := func(r Record) Options {
		run := o
		run.Issue, run.Approve = fmt.Sprint(r.Issue.Number), r.Readiness.Plans[0].Digest
		return run
	}
	first, second := loopMolecule(t, o, 11), loopMolecule(t, o, 12)
	type outcome struct {
		r   Record
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		r, err := Run(context.Background(), approved(first))
		got <- outcome{r, err}
	}()
	<-inReview
	if slotHeld(t, o.LabDir) {
		t.Error("the slot is held while the first Molecule is only in its review")
	}
	b, err := Run(context.Background(), approved(second))
	if err != nil || statePhase(b) != "review-waiting" || len(b.ReviewAttempts) != 1 {
		t.Fatalf("the second Molecule did not verify during the first's review: phase %s err %v notes %v", statePhase(b), err, b.Notes)
	}
	if slices.Contains(eventKinds(b), readiness.VerificationQueued) || !slices.Contains(eventKinds(b), readiness.VerificationStarted) {
		t.Fatalf("the second Molecule's events %v", eventKinds(b))
	}
	unblock()
	a := <-got
	if a.err != nil || statePhase(a.r) != "review-waiting" || len(a.r.ReviewAttempts) != 1 {
		t.Fatalf("the first Molecule: phase %s err %v notes %v", statePhase(a.r), a.err, a.r.Notes)
	}
	if slotHeld(t, o.LabDir) {
		t.Fatal("the slot outlived both verifications")
	}
}

// TestVerificationSlotCancelWhileQueued: a queued Molecule whose context ends
// stops waiting with an interrupted outcome, starts no attempt and runs no check,
// and stays amendable. While it waits, every reader reports it queued. Serial:
// it replaces the verifier and ticker seams.
func TestVerificationSlotCancelWhileQueued(t *testing.T) {
	o, _ := fixture(t)
	limitSlots(t, 1)
	release, err := flockLedgerFile(o.LabDir, verificationSlotFile)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	original, originalTicker := runVerification, verificationSlotTicker
	defer func() { runVerification, verificationSlotTicker = original, originalTicker }()
	calls := 0
	runVerification = func(ctx context.Context, source, destination string, p readiness.Plan, c verification.CaptureResult, observe func(verification.Observation) error) verification.Result {
		calls++
		return original(ctx, source, destination, p, c, observe)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	verificationSlotTicker = func() (<-chan time.Time, func()) {
		// The queued event is saved before the first poll, so readers see it now.
		ids, truncated := QueuedMolecules(o.LabDir)
		if len(ids) != 1 || truncated {
			t.Errorf("queued Molecules %v truncated %v", ids, truncated)
		} else if r, err := ReadRecord(o.LabDir, ids[0]); err != nil || !QueuedForChecks(r) {
			t.Errorf("the saved ledger does not read as queued: %v", err)
		} else {
			// A queued ledger whose process died reads the same, so the loop report keeps
			// the recovery hint on both the restart and the classify paths.
			l := &loopRun{lab: o.LabDir}
			item := l.existing(context.Background(), LoopItem{Repository: r.Issue.Repo, Issue: r.Issue.Number}, ReadQueueInventory(o.LabDir).Summarize(r.Issue.Repo, r.Issue.Number))
			state, reason, _ := classify(r)
			for _, got := range []LoopItem{item, {State: state, Reason: reason}} {
				if got.State != LoopActive || !strings.HasPrefix(got.Reason, QueuedForChecksLabel+": ") || !strings.Contains(got.Reason, "liveness is unverified") || !strings.Contains(got.Reason, "forgecell recover "+r.ID) {
					t.Errorf("loop report %s: %s", got.State, got.Reason)
				}
			}
		}
		cancel()
		return make(chan time.Time), func() {}
	}
	r, err := Run(context.Background(), o)
	if err != nil || r.Readiness == nil {
		t.Fatalf("intake: %v", err)
	}
	o.Approve = r.Readiness.Plans[0].Digest
	r, err = Run(ctx, o)
	if err != nil {
		t.Fatalf("a cancelled wait must close the attempt, not escape: %v", err)
	}
	saved, err := ReadRecord(o.LabDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []Record{r, saved} {
		kinds := eventKinds(rec)
		if statePhase(rec) != "interrupted" || rec.Status == "running" || QueuedForChecks(rec) || !slices.Equal(kinds[len(kinds)-2:], []string{readiness.VerificationQueued, "interrupted"}) {
			t.Fatalf("status %s phase %s events %v", rec.Status, statePhase(rec), kinds)
		}
		if len(rec.VerificationAttempts) != 0 || rec.Atoms[3].Status != "blocked" || !strings.Contains(rec.Atoms[3].Detail, "queued for checks") {
			t.Fatalf("attempts %+v check atom %+v", rec.VerificationAttempts, rec.Atoms[3])
		}
		if !slices.Contains(rec.Notes, "Queued for checks at position 1 of 1: every verification slot of this Lab is in use (1 slot(s)).") {
			t.Fatalf("the queue position is not in the ledger: %v", rec.Notes)
		}
	}
	if calls != 0 {
		t.Fatalf("%d verifications ran for a cancelled waiter", calls)
	}
	if ids, _ := QueuedMolecules(o.LabDir); len(ids) != 0 {
		t.Fatalf("a stopped Molecule still reads as queued: %v", ids)
	}
	parent := saved.Readiness.Plans[len(saved.Readiness.Plans)-1]
	amended, err := Amend(context.Background(), o.LabDir, saved.ID, parent.Digest, copyPlan(t, parent.Plan), "")
	if err != nil || statePhase(amended) != "scope-waiting" {
		t.Fatalf("the Molecule must stay amendable: %v", err)
	}
}

// TestVerificationSlotReleasedOnRefusal: an early exit after the slot was taken
// still releases it. Serial: it replaces startVerification.
func TestVerificationSlotReleasedOnRefusal(t *testing.T) {
	o, _ := fixture(t)
	original := startVerification
	startVerification = func(*Record, readiness.Plan, string) error { return fmt.Errorf("injected refusal") }
	defer func() { startVerification = original }()
	r, err := runApprovedFixture(t, o)
	if err != nil || statePhase(r) != "failed" {
		t.Fatalf("phase %s: %v", statePhase(r), err)
	}
	if kinds := eventKinds(r); !slices.Equal(kinds[len(kinds)-2:], []string{readiness.VerificationStarted, "verification-failed"}) {
		t.Fatalf("events %v", kinds)
	}
	if slotHeld(t, o.LabDir) {
		t.Fatal("a refused verification kept the slot")
	}
}

// Serial: helper-process entry point that reads its environment.
func TestVerificationSlotHolderHelper(t *testing.T) {
	lab := os.Getenv("VERIFICATION_SLOT_LAB")
	if lab == "" {
		t.Skip("helper process for TestVerificationSlotSurvivesKilledHolder")
	}
	if _, err := flockLedgerFile(lab, verificationSlotFile); err != nil {
		fmt.Println("error:", err)
		os.Exit(3)
	}
	fmt.Println("locked")
	time.Sleep(time.Minute)
}

// TestVerificationSlotSurvivesKilledHolder: a slot held by a separate process
// that is then killed is acquired by the next waiter, and the slot file is never
// removed. Serial: it launches a helper process and replaces the ticker seam.
func TestVerificationSlotSurvivesKilledHolder(t *testing.T) {
	lab := resolvedTempDir(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestVerificationSlotHolderHelper$")
	cmd.Env = append(os.Environ(), "VERIFICATION_SLOT_LAB="+lab)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("the helper did not take the slot: %q %v", line, err)
	}
	queuedSignal, tick := driveSlotTicker(t)
	type outcome struct {
		release func()
		err     error
	}
	got, queuedCalls := make(chan outcome, 1), 0
	go func() {
		release, err := acquireVerificationSlot(context.Background(), lab, func() error { queuedCalls++; return nil })
		got <- outcome{release, err}
	}()
	<-queuedSignal
	tick <- time.Time{}
	select {
	case o := <-got:
		t.Fatalf("the slot was granted while its holder lived: %v", o.err)
	default:
	}
	if err = cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	var o outcome
	for acquired := false; !acquired; {
		select {
		case tick <- time.Time{}:
		case o = <-got:
			acquired = true
		}
	}
	if o.err != nil || queuedCalls != 1 {
		t.Fatalf("the slot outlived its killed holder: %v (queued %d times)", o.err, queuedCalls)
	}
	defer o.release()
	if _, err = os.Lstat(filepath.Join(lab, "ledgers", verificationSlotFile)); err != nil {
		t.Fatalf("the slot file was removed: %v", err)
	}
}

// TestVerificationSlotsRunTwoMoleculesConcurrently: with two slots, two Molecules of
// one Lab are inside their checks at the same time, neither queues, and each records
// the slot it ran in. Serial: it replaces the verifier and ticker seams.
func TestVerificationSlotsRunTwoMoleculesConcurrently(t *testing.T) {
	o, _ := loopFixture(t, policyScopeAtom, defaultReviewAtom, "")
	// The Lab sets two slots explicitly (an unset Lab keeps one); the seam keeps the
	// guard's live sample out of the test.
	if err := os.WriteFile(filepath.Join(o.LabDir, resources.ConfigFile), []byte(`{"verificationSlots": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	limitSlots(t, 2)
	queued, tick := driveSlotTicker(t)
	original := runVerification
	t.Cleanup(func() { runVerification = original })
	var mu sync.Mutex
	entered, most, active := 0, 0, 0
	both := make(chan struct{})
	runVerification = func(ctx context.Context, source, destination string, p readiness.Plan, c verification.CaptureResult, observe func(verification.Observation) error) verification.Result {
		mu.Lock()
		entered++
		active++
		most = max(most, active)
		if entered == 2 {
			close(both)
		}
		mu.Unlock()
		// Each waits until the other is inside its checks too; a Molecule that queued
		// instead is the defect under test.
		select {
		case <-both:
		case <-queued:
			t.Errorf("%s queued although a second slot was free", p.MoleculeID)
		}
		result := original(ctx, source, destination, p, c, observe)
		mu.Lock()
		active--
		mu.Unlock()
		return result
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case tick <- time.Time{}:
			case <-done:
				return
			}
		}
	}()
	_, err := RunLoop(context.Background(), LoopOptions{Template: o, MaxConcurrent: 2, Once: true, ListIssues: loopFixedList(loopOpen(11, 0), loopOpen(12, 1))})
	close(done)
	if err != nil {
		t.Fatal(err)
	}
	if most != 2 {
		t.Fatalf("the two verifications did not overlap: most=%d", most)
	}
	slots := []int{}
	for _, number := range []int64{11, 12} {
		records := loopRecords(t, o.LabDir)[number]
		if len(records) != 1 {
			t.Fatalf("Issue %d: %d Molecules", number, len(records))
		}
		r := records[0]
		if statePhase(r) != "review-waiting" || slices.Contains(eventKinds(r), readiness.VerificationQueued) {
			t.Fatalf("%s: phase %s events %v", r.ID, statePhase(r), eventKinds(r))
		}
		a := r.VerificationAttempts[len(r.VerificationAttempts)-1]
		if a.Slot == nil || a.Slots != 2 {
			t.Fatalf("%s attempt slot %v of %d", r.ID, a.Slot, a.Slots)
		}
		slots = append(slots, *a.Slot)
	}
	slices.Sort(slots)
	if !slices.Equal(slots, []int{0, 1}) {
		t.Fatalf("slots %v, want 0 and 1", slots)
	}
}

// TestVerificationSlotsAcquireInOrderAndQueue: slots are taken lowest first up to the
// limit, which is asked again before every try; once every allowed slot is held a
// waiter queues, even while a slot beyond the current limit is free.
func TestVerificationSlotsAcquireInOrderAndQueue(t *testing.T) {
	t.Parallel()
	lab := resolvedTempDir(t)
	two := func() int { return 2 }
	free := func() error { return fmt.Errorf("queued while a slot was free") }
	first, slot, err := acquireVerificationSlots(context.Background(), lab, two, free)
	if err != nil || slot != 0 {
		t.Fatalf("first: slot %d %v", slot, err)
	}
	second, slot, err := acquireVerificationSlots(context.Background(), lab, two, free)
	if err != nil || slot != 1 {
		t.Fatalf("second: slot %d %v", slot, err)
	}
	for _, name := range []string{verificationSlotFile, verificationSlotName(1)} {
		if _, err := os.Lstat(filepath.Join(lab, "ledgers", name)); err != nil {
			t.Fatalf("slot file %s: %v", name, err)
		}
	}
	waitQueued := func(limit func() int) bool {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		queued := false
		_, _, err := acquireVerificationSlots(ctx, lab, limit, func() error { queued = true; cancel(); return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a waiter took a held slot: %v", err)
		}
		return queued
	}
	if !waitQueued(two) {
		t.Fatal("a third Molecule did not queue behind two held slots")
	}
	second()
	// Slot 1 is free, but the guard now allows one slot, which slot 0 holds.
	if !waitQueued(func() int { return 1 }) {
		t.Fatal("a waiter took a slot beyond the current limit")
	}
	first()
	again, slot, err := acquireVerificationSlots(context.Background(), lab, func() int { return 1 }, free)
	if err != nil || slot != 0 {
		t.Fatalf("after release: slot %d %v", slot, err)
	}
	again()
	bounded, slot, err := acquireVerificationSlots(context.Background(), lab, func() int { return 99 }, free)
	if err != nil || slot != 0 {
		t.Fatalf("an oversized limit: slot %d %v", slot, err)
	}
	bounded()
}

// TestVerificationSlotLimitRefusedUnderLowResources: the Lab's limit (default 1, or
// resources.json) is cut to one slot below a soft floor or without memory for the
// extra slot, and a config that cannot be used falls back to one slot without naming
// the Lab's path.
func TestVerificationSlotLimitRefusedUnderLowResources(t *testing.T) {
	t.Parallel()
	lab := resolvedTempDir(t)
	guard := func(freeDisk, memory uint64) *resources.Guard {
		return &resources.Guard{Floors: resources.DefaultFloors().Clamp(), Reader: resources.ReaderFunc(func() resources.Readings {
			return resources.Readings{Volumes: []resources.Volume{{Path: "/lab", Free: freeDisk, Total: 100 * resources.GiB}}, Memory: memory, MemoryKnown: true}
		})}
	}
	// A Lab without the setting keeps one slot, even with plenty of resources.
	if n, reason := labVerificationSlots(Options{LabDir: lab, Resources: guard(50*resources.GiB, 16*resources.GiB)}); n != 1 || reason != "" {
		t.Fatalf("default: %d %q", n, reason)
	}
	if err := os.WriteFile(filepath.Join(lab, resources.ConfigFile), []byte(`{"verificationSlots": 2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		guard  *resources.Guard
		want   int
		reason string
	}{
		{"plenty", guard(50*resources.GiB, 16*resources.GiB), 2, ""},
		{"low disk", guard(10*resources.GiB, 16*resources.GiB), 1, "extra verification slots are refused"},
		{"low memory", guard(50*resources.GiB, 2*resources.GiB), 1, "extra verification slots are refused"},
		{"memory for one", guard(50*resources.GiB, 5*resources.GiB), 1, "allows 1 verification slot(s)"},
	}
	for _, c := range cases {
		n, reason := labVerificationSlots(Options{LabDir: lab, Resources: c.guard})
		if n != c.want || (c.reason == "") != (reason == "") || !strings.Contains(reason, c.reason) {
			t.Errorf("%s: %d %q", c.name, n, reason)
		}
	}
	if err := os.WriteFile(filepath.Join(lab, resources.ConfigFile), []byte(`{"verificationSlots": 3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, _ := labVerificationSlots(Options{LabDir: lab, Resources: guard(50*resources.GiB, 16*resources.GiB)}); n != 3 {
		t.Fatalf("configured: %d", n)
	}
	if err := os.WriteFile(filepath.Join(lab, resources.ConfigFile), []byte(`{"verificationSlots": 100}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, reason := labVerificationSlots(Options{LabDir: lab}); n != 1 || !strings.Contains(reason, "one verification slot is used") || strings.Contains(reason, lab) {
		t.Fatalf("invalid config: %d %q", n, reason)
	}
	if note := queueNote(2, 3, 1, "resources: low memory"); note != "Queued for checks at position 2 of 3: every verification slot of this Lab is in use (1 slot(s)). resources: low memory." {
		t.Fatalf("queue note %q", note)
	}
}

// TestVerificationSlotLabsAreIndependent: a slot held in one Lab never queues a
// Molecule of another, and a held slot reports a context that ends while queued.
func TestVerificationSlotLabsAreIndependent(t *testing.T) {
	t.Parallel()
	a, b := resolvedTempDir(t), resolvedTempDir(t)
	holdA, err := acquireVerificationSlot(context.Background(), a, func() error { return fmt.Errorf("lab a was free") })
	if err != nil {
		t.Fatal(err)
	}
	defer holdA()
	holdB, err := acquireVerificationSlot(context.Background(), b, func() error { return fmt.Errorf("lab b queued behind lab a") })
	if err != nil {
		t.Fatal(err)
	}
	holdB()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	queued := false
	if _, err = acquireVerificationSlot(ctx, a, func() error { queued = true; return nil }); !errors.Is(err, context.Canceled) || queued {
		t.Fatalf("a cancelled waiter must not queue or take a held slot: %v queued=%v", err, queued)
	}
	if _, err = acquireVerificationSlot(context.Background(), a, func() error { return fmt.Errorf("save failed") }); err == nil || err.Error() != "save failed" {
		t.Fatalf("a queue event that cannot be saved must stop the wait: %v", err)
	}
}
