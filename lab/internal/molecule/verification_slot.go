package molecule

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"
)

// verificationSlotFile is the Lab's single verification slot, a flock in the
// ledgers directory. Its loopLockSuffix keeps it out of the ledger inventory's
// entry cap, and like every flockLedgerFile lock it is never removed: the
// operating system releases it when its holder exits, including on a crash.
const verificationSlotFile = "verification-slot" + loopLockSuffix

// verificationSlotPollInterval is how often a queued Molecule retries the slot.
const verificationSlotPollInterval = 2 * time.Second

// QueuedForChecksLabel is how a Molecule waiting for the slot is reported.
const QueuedForChecksLabel = "queued for checks"

// tryVerificationSlot takes the slot without blocking; tests replace it.
var tryVerificationSlot = func(lab string) (func(), error) { return flockLedgerFile(lab, verificationSlotFile) }

// verificationSlotTicker paces a queued Molecule's retries; tests replace it
// with a ticker they drive.
var verificationSlotTicker = func() (<-chan time.Time, func()) {
	t := time.NewTicker(verificationSlotPollInterval)
	return t.C, t.Stop
}

// acquireVerificationSlot takes the Lab's verification slot. When another
// Molecule holds it, queued runs once before the first retry and the wait polls
// until the slot is free or ctx ends. The wait has no deadline of its own: queue
// time is not check time. A lock error other than contention is returned at once.
func acquireVerificationSlot(ctx context.Context, lab string, queued func() error) (func(), error) {
	release, err := trySlot(lab)
	if !errors.Is(err, errLocked) {
		return release, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = queued(); err != nil {
		return nil, err
	}
	tick, stop := verificationSlotTicker()
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick:
		}
		// A cancellation that raced the tick wins, so a stopped Molecule never starts checks.
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		release, err = trySlot(lab)
		if !errors.Is(err, errLocked) {
			return release, err
		}
	}
}

// trySlot is tryVerificationSlot with any path dropped from its error, so a
// ledger note never carries the Lab's absolute location.
func trySlot(lab string) (func(), error) {
	release, err := tryVerificationSlot(lab)
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return nil, fmt.Errorf("%s %s: %w", pathErr.Op, verificationSlotFile, pathErr.Err)
	}
	return release, err
}

// QueuedForChecks reports a running Molecule that waits for the Lab's
// verification slot.
func QueuedForChecks(r Record) bool {
	return (r.Status == "running" || r.Status == "allocating") && r.Readiness != nil && r.Readiness.QueuedForChecks()
}

// maxQueueScan bounds how many running Molecules QueuedMolecules reads.
const maxQueueScan = 64

// QueuedMolecules lists the running Molecules in lab that wait for the
// verification slot. It reads at most maxQueueScan running ledgers in check-pending
// and reports truncated when there were more; unreadable ledgers are skipped.
func QueuedMolecules(lab string) (ids []string, truncated bool) {
	ids, read := []string{}, 0
	for _, a := range ReadQueueInventory(lab).Attempts {
		if (a.Status != "running" && a.Status != "allocating") || a.Phase != "check-pending" {
			continue
		}
		if read == maxQueueScan {
			return ids, true
		}
		read++
		r, err := readMolecule(lab, a.ID)
		if err == nil && QueuedForChecks(r) {
			ids = append(ids, r.ID)
		}
	}
	return ids, false
}

// recordSlotEvent appends a verification slot event and saves the ledger.
func recordSlotEvent(r *Record, kind string) error {
	state, err := r.Readiness.RecordVerificationSlot(kind, stamp())
	if err != nil {
		return err
	}
	r.Readiness = &state
	return save(*r)
}
